package action

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// Worker drain.
//
// Pausing a batch only stops that batch from starting more children; jobs
// already on a worker keep running, and standalone actions and other batches
// ignore it. A worker deploy needs every worker idle, so drain is the global
// switch for it: while it is on no new worker job is admitted by anything (a
// batch starting a file, a standalone conversion, a ladder rung or retry, a
// podcast clean), and every job already on a worker finishes and publishes
// the way it always does. A file the server admitted but has not encoded is
// held too: before its source hash (or while it waits for a hash slot), or at
// its worker submit when the hash is running or done; only a file with a job
// on a worker counts as in flight. Nothing is cancelled, and deferred work keeps its
// place: a child waits with worker_drain and submits when drain is turned
// off. Drain is one durable row, so a restart keeps it, and it never changes
// a batch's own pause, which stays as it was.
const drainWaitCondition = "worker_drain"

// drainPollDelay is how often a child held by drain looks again; the drain
// can last a deploy, and a held child has nothing to look at in between.
const drainPollDelay = 15 * time.Second

// drainSettle (a variable so tests can end it at once) is how long after drain starts it is not yet reported ready: an
// admission that passed the gate just before the switch persists its job
// identity a moment later, and a status read in between would not see it.
var drainSettle = 15 * time.Second

func (e *Engine) drainSettleWindow() time.Duration {
	switch {
	case e.deps.DrainSettle < 0:
		return 0
	case e.deps.DrainSettle > 0:
		return e.deps.DrainSettle
	}
	return drainSettle
}

// drainHold reports whether new worker jobs are held. A store that cannot be
// read holds them too, since admitting through an unknown state could send a
// job into a deploy.
func (e *Engine) drainHold() (held bool, reason string) {
	if e.deps.Store == nil {
		return false, ""
	}
	drain, err := e.deps.Store.GetWorkerDrain()
	if err != nil {
		return true, "the drain state could not be read"
	}
	if drain.Enabled {
		return true, "worker drain is on"
	}
	return false, ""
}

// drainBlockedResult defers an admission without contacting the worker.
func drainBlockedResult(phase, reason string) StepResult {
	return StepResult{
		Status:           StepWaitingExternal,
		WaitingCondition: drainWaitCondition,
		WaitingReason:    fmt.Sprintf("Worker drain is holding new jobs (%s); deferring %s admission until it is resumed", reason, phase),
	}
}

// SetWorkerDrain turns drain on or off and returns the status afterwards.
// Turning it off wakes the reconciler so held children submit now.
func (e *Engine) SetWorkerDrain(ctx context.Context, enabled bool) (transcode.DrainStatus, error) {
	if e.deps.Store == nil {
		return transcode.DrainStatus{}, fmt.Errorf("store is required for worker drain")
	}
	if _, err := e.deps.Store.SetWorkerDrain(enabled); err != nil {
		return transcode.DrainStatus{}, err
	}
	e.syncWorkerHolds(ctx)
	if !enabled {
		e.WakeReconciler()
	}
	return e.WorkerDrainStatus(ctx), nil
}

// WorkerDrainStatus is the drain state with what is left before a deploy.
func (e *Engine) WorkerDrainStatus(ctx context.Context) transcode.DrainStatus {
	return e.drainStatus(ctx, e.observeWorkers(ctx))
}

func (e *Engine) drainStatus(ctx context.Context, obs transcode.WorkerObservation) transcode.DrainStatus {
	out := transcode.DrainStatus{CheckedAt: time.Now().UTC()}
	if e.deps.Store == nil {
		out.Message = "Worker drain needs the action store."
		out.NotReady = []string{out.Message}
		return out
	}
	drain, err := e.deps.Store.GetWorkerDrain()
	if err != nil {
		out.Enabled = true
		out.Message = "The drain state could not be read; new jobs are held until it can."
		out.NotReady = []string{out.Message}
		return out
	}
	out.Enabled, out.Since, out.Revision = drain.Enabled, drain.Since, drain.Revision
	rows, err := e.deps.Store.ListDrainActions()
	if err != nil {
		out.Message = "The open worker jobs could not be read, so the worker cannot be reported ready."
		out.NotReady = []string{out.Message}
		return out
	}
	unstarted, err := e.deps.Store.CountUnstartedBatchItems()
	if err != nil {
		out.Message = "The queued batch files could not be read, so the worker cannot be reported ready."
		out.NotReady = []string{out.Message}
		return out
	}
	unmatched := 0 // open jobs on a worker that is not configured
	byWorker := map[string]*transcode.DrainWorker{}
	for _, w := range e.deps.Workers.All() {
		reachable := false
		for _, o := range obs.Workers {
			if o.Name == w.Name {
				reachable = o.Reachable
			}
		}
		byWorker[w.Name] = &transcode.DrainWorker{Name: w.Name, Reachable: reachable}
	}
	// Jobs the database says are encoding, per worker: a worker that holds its
	// queue may keep no more running jobs than these.
	encoding := map[string]int{}
	for _, r := range rows {
		if classifyDrainRow(r) == transcode.DrainJobRunning && !r.WorkerQueued {
			encoding[e.resolveWorker(r.Worker)]++
		}
	}
	for _, r := range rows {
		state := classifyDrainRow(r)
		if state == "" {
			continue
		}
		name := e.resolveWorker(r.Worker)
		// A job still in its worker's queue, on a worker that confirmed it is
		// holding that queue, will not start before Resume: it is held back
		// like a file the server has not sent, and it does not block a deploy.
		// The stored status can lag the worker, so the worker's own count of
		// running jobs must not exceed the encodes the database knows of;
		// otherwise a queued job may already have started.
		if state == transcode.DrainJobRunning && r.WorkerQueued {
			if active, ok := e.workerHolding(name); ok && active <= encoding[name] {
				state = transcode.DrainJobQueued
			}
		}
		job := transcode.DrainJob{ActionID: r.ID, ParentID: r.ParentID, Worker: name, State: state, Waiting: state == transcode.DrainJobRunning && r.WorkerQueued}
		if r.Path != "" {
			job.File, job.Path = filepath.Base(r.Path), r.Path
		}
		out.Jobs = append(out.Jobs, job)
		w := byWorker[name]
		switch state {
		case transcode.DrainJobRunning:
			out.Running++
			if job.Waiting {
				out.Waiting++
			}
			if w != nil {
				w.Running++
				if job.Waiting {
					w.Waiting++
				}
				w.InFlight = append(w.InFlight, job)
			} else {
				unmatched++
			}
		case transcode.DrainJobPublicationPending:
			out.PublicationPending++
			if w != nil {
				w.PublicationPending++
				w.InFlight = append(w.InFlight, job)
			} else {
				unmatched++
			}
		case transcode.DrainJobQueued:
			out.Queued++
		}
	}
	out.Queued += unstarted
	// Publication-pending jobs first: they are the ones that need a person.
	rank := map[string]int{transcode.DrainJobPublicationPending: 0, transcode.DrainJobRunning: 1, transcode.DrainJobQueued: 2}
	sort.SliceStable(out.Jobs, func(i, j int) bool { return rank[out.Jobs[i].State] < rank[out.Jobs[j].State] })
	if len(out.Jobs) > 50 {
		out.Jobs = out.Jobs[:50]
	}

	settling := false
	if at, err := time.Parse(time.RFC3339, drain.Since); err == nil && time.Since(at) < e.drainSettleWindow() {
		settling = true
	}
	ready := out.Enabled && !settling
	for _, w := range e.deps.Workers.All() {
		dw := byWorker[w.Name]
		switch {
		case !out.Enabled:
			dw.Reason = "Drain is off."
		case settling:
			dw.Reason = "Drain just started; waiting for admissions in flight."
		case !dw.Reachable:
			dw.Reason = "Worker unreachable; its jobs cannot be confirmed."
		case dw.PublicationPending > 0:
			dw.Reason = fmt.Sprintf("%d finished encode(s) not saved yet.", dw.PublicationPending)
		case dw.Running > 0:
			dw.Reason = fmt.Sprintf("%d job(s) still running.", dw.Running)
		default:
			dw.ReadyForDeploy = true
		}
		ready = ready && dw.ReadyForDeploy
		if out.Enabled && !dw.ReadyForDeploy && !settling {
			out.NotReady = append(out.NotReady, w.Name+": "+dw.Reason)
		}
		out.Workers = append(out.Workers, *dw)
	}
	if out.Enabled && settling {
		out.NotReady = append(out.NotReady, "Drain just started; waiting for admissions in flight.")
	}
	if unmatched > 0 {
		// A job on a worker this server does not know cannot be confirmed idle.
		ready = false
		out.NotReady = append(out.NotReady, fmt.Sprintf("%d open job(s) are on a worker that is not configured.", unmatched))
	}
	if out.Enabled && len(out.Workers) == 0 {
		out.NotReady = append(out.NotReady, "No worker is configured.")
	}
	out.ReadyForDeploy = ready && len(out.Workers) > 0
	out.Message = drainMessage(out, settling)
	return out
}

// classifyDrainRow says what an unfinished worker action is to the drain:
// "queued" while held, "publication_pending" for a finished encode that could
// not be saved, "running" while a job it submitted is open on its worker, and
// "" for everything else (before its job, after it, or in a review).
func classifyDrainRow(r store.DrainActionRow) string {
	switch {
	case r.WaitingCondition == drainWaitCondition:
		return transcode.DrainJobQueued
	case r.RecoveryRequired:
		return transcode.DrainJobPublicationPending
	case r.Status == StatusWaitingDecision, r.Done:
		return ""
	case r.Reconcile:
		// A submit may have been accepted; count it until the worker says.
		return transcode.DrainJobRunning
	case r.Rejected && !r.Submitted:
		return ""
	case r.Submitted || r.JobID != "" || r.ExternalRef != "":
		return transcode.DrainJobRunning
	}
	return ""
}

func drainMessage(s transcode.DrainStatus, settling bool) string {
	switch {
	case !s.Enabled:
		return "Drain is off. Workers admit new jobs."
	case s.ReadyForDeploy:
		return "Ready for deploy: no worker has an open job."
	case settling:
		return "Drain just started. Checking for admissions in flight."
	case s.PublicationPending > 0 && s.Running > 0:
		return fmt.Sprintf("Draining: %d running, %d finished but not saved. Use Try again on the unsaved ones once storage is reachable.", s.Running, s.PublicationPending)
	case s.PublicationPending > 0:
		return fmt.Sprintf("Draining: %d finished encode(s) not saved yet. Use Try again once storage is reachable.", s.PublicationPending)
	case s.Running > 0:
		return fmt.Sprintf("Draining: %d job(s) still running. Nothing new is admitted.", s.Running)
	}
	return "Draining: waiting for the workers to be reachable."
}

// drainSlotRecheck is how often a source hash waiting for a hash slot looks
// at the drain switch (a variable so tests can shorten it).
var drainSlotRecheck = 2 * time.Second

// acquireHashSlotUnlessDraining waits for a hash slot like acquireHashSlot, but
// gives up the wait, without reading a byte, when drain is turned on: the file
// has not started hashing, so it has not started its encode either, and it is
// held until drain ends. A hash that already holds its slot is never
// interrupted; it finishes and the file is held at its worker submit instead.
func (e *Engine) acquireHashSlotUnlessDraining(ctx context.Context) (release func(), held *StepResult, err error) {
	if e.hashSlots == nil {
		return func() {}, nil, nil
	}
	for {
		waitCtx, cancel := context.WithTimeout(ctx, drainSlotRecheck)
		release, err = e.acquireHashSlot(waitCtx)
		cancel()
		if err == nil {
			return release, nil, nil
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if hold, reason := e.drainHold(); hold {
			res := drainBlockedResult("source hash", reason)
			return nil, &res, nil
		}
	}
}
