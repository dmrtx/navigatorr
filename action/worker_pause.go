package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// Node pause.
//
// Worker drain stops every node so one can be deployed. A pause is for one
// node: the operator wants its CPU for something else, and the other nodes
// keep encoding. While a node is paused it takes no new job: the transcode
// queue assigns files to the other nodes (a paused node has no video capacity,
// see workerVideoCapacity), a file already assigned to it that has not been
// sent is released back to the queue, and the node holds the jobs it has
// accepted but not started (the same /v1/hold the drain uses). There are two
// modes for what is encoding on the node (worker_suspend.go has the third):
//
//   - graceful: it finishes and publishes as usual. The node is idle when the
//     last one ends.
//   - immediate: each one is cancelled on the node and its file goes back to
//     the transcode queue as if it had never started, keeping its place in the
//     queue. It is not a cancellation: the action stays active, a batch keeps
//     counting the file, and another node (or this one once resumed) encodes
//     it from the start.
//   - suspend: it is frozen where it is and continues on resume; it stays
//     assigned to the node and nothing is requeued or cancelled.
//
// Podcast work follows the pause of the podcast node for new jobs only; a
// podcast job already running is left to finish in both modes. A pause is one
// durable row per node, so a restart keeps it, and a sweep on every reconciler
// tick finishes what an unreachable node or a busy action could not take at
// the time.
const pauseWaitCondition = "worker_paused"

// These keys record that a file went back to the queue, and where from.
const (
	requeuedFromKey      = "requeued_from_worker"
	requeuedAtKey        = "requeued_at"
	requeueCountKey      = "pause_requeues"
	requeueLeftoversKey  = "requeue_leftover_candidates"
	requeueRetryCountKey = "manual_retry_count"
	// requeuePendingKey marks the job being moved, saved before it is
	// cancelled, so a resume that finds the reset undone finishes it instead of
	// reading the cancelled job as the action's own end.
	requeuePendingKey = "pause_requeue_pending"
)

// requeueGeneration is the suffix that keeps the job identities of a file sent
// back to the queue apart from the ones the node already holds as cancelled
// (the rungs of a quality or size ladder build theirs from the action id).
func requeueGeneration(state map[string]any) string {
	if n := getInt(state, requeueCountKey); n > 0 {
		return fmt.Sprintf("-g%d", n)
	}
	return ""
}

// pauseSettle (a variable so tests can end it at once) is how long after a
// pause starts the node is not yet reported idle: an admission that passed the
// gate just before the pause persists its job identity a moment later.
var pauseSettle = 15 * time.Second

func (e *Engine) pauseSettleWindow() time.Duration {
	switch {
	case e.deps.DrainSettle < 0:
		return 0
	case e.deps.DrainSettle > 0:
		return e.deps.DrainSettle
	}
	return pauseSettle
}

// workerPauses is the effective pause of each node by name: the operator's
// pause if the node has one, else the hold of its running builds (see
// builds.go). Everything that decides dispatch, requeue, the worker's hold and
// its freeze reads this view; only the operator's own requests read
// operatorPauses. A store that cannot be read gives an error, which callers
// treat as paused: sending a job to a node that may be paused is the worse
// mistake.
func (e *Engine) workerPauses() (map[string]store.WorkerPause, error) {
	if e.deps.Store == nil {
		return nil, nil
	}
	operator, err := e.operatorPauses()
	if err != nil {
		return nil, err
	}
	holds, err := e.buildHolds()
	if err != nil {
		return nil, err
	}
	if len(holds) == 0 {
		return operator, nil
	}
	out := make(map[string]store.WorkerPause, len(operator)+len(holds))
	for name, hold := range holds {
		out[name] = hold
	}
	for name, pause := range operator {
		out[name] = pause
	}
	return out, nil
}

// NodeHeldBy says what keeps new jobs off a node: "operator" (a pause the
// operator set; it wins), "builds" (a running build holds it) or "" when the
// node takes jobs. It reads stored state only, so it is cheap enough for the
// activity view. An unreadable pause state holds the node, as workerPaused does.
func (e *Engine) NodeHeldBy(name string) string {
	pauses, err := e.workerPauses()
	if err != nil {
		return "operator"
	}
	name = e.resolveWorker(name)
	if _, ok := pauses[name]; !ok {
		return ""
	}
	operator, _ := e.operatorPauses()
	if _, ok := operator[name]; ok {
		return "operator"
	}
	return "builds"
}

// workerPaused reports whether new jobs for the node are held. A node name is
// resolved like any stored worker name.
func (e *Engine) workerPaused(name string) bool {
	pauses, err := e.workerPauses()
	if err != nil {
		return true
	}
	_, paused := pauses[e.resolveWorker(name)]
	return paused
}

// pauseHold reports whether a new job for the node is held, with the reason.
func (e *Engine) pauseHold(name string) (held bool, reason string) {
	pauses, err := e.workerPauses()
	if err != nil {
		return true, "the pause state could not be read"
	}
	node := e.resolveWorker(name)
	if _, ok := pauses[node]; ok {
		return true, "node " + node + " is paused"
	}
	return false, ""
}

// allWorkersPaused reports whether every configured worker is paused.
func (e *Engine) allWorkersPaused() bool {
	workers := e.deps.Workers.All()
	if len(workers) == 0 {
		return false
	}
	pauses, err := e.workerPauses()
	if err != nil || len(pauses) == 0 {
		return false
	}
	for _, w := range workers {
		if _, ok := pauses[w.Name]; !ok {
			return false
		}
	}
	return true
}

// pauseBlockedResult defers an admission to a paused node without contacting
// it.
func pauseBlockedResult(phase, reason string) StepResult {
	return StepResult{
		Status:           StepWaitingExternal,
		WaitingCondition: pauseWaitCondition,
		WaitingReason:    fmt.Sprintf("Worker pause is holding new jobs (%s); deferring %s admission until it is resumed", reason, phase),
	}
}

// SetNodePause pauses a node in a mode ("graceful", "immediate" or "suspend"),
// or resumes it with an empty mode, and returns the node's status afterwards.
// Repeating a request changes nothing. An immediate pause starts sending the
// node's encodes back to the queue at once, in the background. A suspend
// returns transcode.ErrSuspendUnavailable, with nothing changed, when the node
// does not confirm the freeze.
func (e *Engine) SetNodePause(ctx context.Context, node, mode string) (transcode.NodePauseStatus, error) {
	if e.deps.Store == nil {
		return transcode.NodePauseStatus{}, errors.New("store is required for worker pause")
	}
	w, ok := e.deps.Workers.Get(node)
	if !ok {
		return transcode.NodePauseStatus{}, &transcode.WorkerNotConfiguredError{Name: node}
	}
	// One request per node at a time: a suspend that is written, synced,
	// confirmed and possibly undone must not be interleaved with another
	// request, or its undo would overwrite what that request set.
	state := e.workerStateFor(w.Name)
	lock := &state.pauseMu
	lock.Lock()
	defer lock.Unlock()
	if mode != store.WorkerPauseImmediate {
		// The next immediate pause is a new one: it clears the cache again.
		state.cacheClearedFor = ""
	}
	if mode == store.WorkerPauseSuspend {
		// The operator's own pause is what a failed suspend goes back to; a
		// build's hold is not theirs to restore or clear.
		pauses, err := e.operatorPauses()
		if err != nil {
			return transcode.NodePauseStatus{}, err
		}
		if _, err := e.suspendNode(ctx, w, pauses[w.Name].Mode); err != nil {
			return transcode.NodePauseStatus{}, err
		}
		e.WakeReconciler()
		return e.NodePauseStatuses(ctx)[w.Name], nil
	}
	if _, err := e.deps.Store.SetWorkerPause(w.Name, mode); err != nil {
		return transcode.NodePauseStatus{}, err
	}
	e.syncWorkerHold(ctx, w)
	e.WakeReconciler()
	return e.NodePauseStatuses(ctx)[w.Name], nil
}

// startNodePauseSweep runs a sweep in the background unless one is running,
// for the reconciler tick and for a pause that was just set.
func (e *Engine) startNodePauseSweep(ctx context.Context) {
	if !e.pauseSweeping.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer e.pauseSweeping.Store(false)
		e.sweepPausedNodes(ctx)
	}()
}

// sweepPausedNodes sends back to the queue what a paused node should not
// keep: a file assigned to it that has not been sent, a job the node accepted
// but has not started, and, for an immediate pause, every job encoding on it.
// An action that is busy or a node that cannot be reached is left for the next
// pass.
func (e *Engine) sweepPausedNodes(ctx context.Context) {
	pauses, err := e.workerPauses()
	if err != nil || len(pauses) == 0 {
		return
	}
	rows, err := e.deps.Store.ListDrainActions()
	if err != nil {
		log.Printf("worker pause: listing open jobs: %v", err)
		return
	}
	for _, r := range rows {
		if ctx.Err() != nil {
			return
		}
		if r.ActionName != "transcode_media" || r.Status == StatusWaitingDecision {
			continue
		}
		// Past its encode, or only waiting to be saved: it finishes.
		if r.Done || r.RecoveryRequired {
			continue
		}
		sent := r.JobID != "" || r.ExternalRef != "" || r.Submitted || r.Reconcile
		if r.Worker == "" && !sent {
			continue
		}
		pause, ok := pauses[e.resolveWorker(r.Worker)]
		if !ok {
			continue
		}
		// A graceful pause only touches what has not begun encoding; the
		// recorded status can lag, so a queued one is checked on the worker.
		if sent && pause.Mode != store.WorkerPauseImmediate && !r.WorkerQueued {
			continue
		}
		if _, err := e.requeueForPause(ctx, r.ID, pause); err != nil {
			log.Printf("worker pause: %s: %v", r.ID, err)
		}
	}
	e.clearPausedCaches(ctx, pauses)
}

// cacheClearer is a node's executor that can be told to delete the cached
// sources no running job holds.
type cacheClearer interface {
	SetHoldClearCache(ctx context.Context) (httpclient.HoldStatus, error)
}

// clearPausedCaches empties the source cache of every node under an immediate
// pause, once its encodes have been sent back: the node is wanted for something
// else and keeps no spare copies of files. Finished candidates waiting for
// approval are not touched. A graceful pause or a suspend leaves the cache alone.
// It is done once per pause, under the node's pause lock and only while the
// pause in the store is still the immediate one, so a resume (or another pause
// change) that lands first is never followed by a hold from this sweep.
func (e *Engine) clearPausedCaches(ctx context.Context, pauses map[string]store.WorkerPause) {
	for name, pause := range pauses {
		if pause.Mode != store.WorkerPauseImmediate {
			continue
		}
		w, ok := e.deps.Workers.Get(name)
		if !ok {
			continue
		}
		c, ok := w.Executor.(cacheClearer)
		if !ok {
			continue
		}
		e.clearPausedCache(ctx, w, pause, c)
	}
}

func (e *Engine) clearPausedCache(ctx context.Context, w transcode.NamedWorker, listed store.WorkerPause, c cacheClearer) {
	state := e.workerStateFor(w.Name)
	state.pauseMu.Lock()
	defer state.pauseMu.Unlock()
	pauses, err := e.workerPauses()
	if err != nil {
		return
	}
	current, ok := pauses[w.Name]
	if !ok || current.Mode != store.WorkerPauseImmediate || current.Revision != listed.Revision {
		return
	}
	token := fmt.Sprintf("%s/%d", current.Since, current.Revision)
	if state.cacheClearedFor == token {
		return
	}
	if _, err := c.SetHoldClearCache(ctx); err != nil {
		log.Printf("worker pause: clearing the source cache of %s: %v", w.Name, err)
		return
	}
	state.cacheClearedFor = token
}

// requeueForPause sends one conversion back to the transcode queue because
// its node is paused. It reports whether it did. Under the pause's mode it
// leaves alone what is not its to move: an encode a graceful pause lets
// finish, a job past its encode, and an action another execution holds.
//
// The job is cancelled on the node first and only a confirmed cancel is
// followed by the reset, so a node that cannot be reached keeps the job it
// still runs. The reset is the state of a conversion that was just asked for,
// plus its place in the queue, so the next run assigns it a node and plans it
// for that node.
func (e *Engine) requeueForPause(ctx context.Context, id string, pause store.WorkerPause) (bool, error) {
	ctx, release, err := e.claimExecution(ctx, id, false)
	if errors.Is(err, errActionBusy) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer release()
	// The pause may have been lifted or changed since the sweep listed it.
	pauses, err := e.workerPauses()
	if err != nil {
		return false, err
	}
	current, ok := pauses[pause.Worker]
	if !ok {
		return false, nil
	}
	pause = current
	inst, err := e.deps.Store.GetActionInstanceIfExists(id)
	if err != nil || inst == nil {
		return false, err
	}
	switch inst.Status {
	case StatusPending, StatusRunning, StatusWaitingExternal:
	default:
		return false, nil
	}
	if inst.ActionName != "transcode_media" {
		return false, nil
	}
	tmpl, ok := e.GetTemplate(inst.ActionName)
	if !ok {
		return false, nil
	}
	ec := parseExecutionContext(inst, e)
	if node := e.instanceWorker(ec.State); node == "" || node != pause.Worker {
		return false, nil
	}
	// Past the encode: validation, acceptance or a candidate waiting to be saved.
	if wait := actionStepIndex(tmpl, "wait_transcode"); wait >= 0 && inst.CurrentStep > wait {
		return false, nil
	}
	if getBool(ec.State, "transcode_done") || getBool(ec.State, "recovery_required") {
		return false, nil
	}
	jobID := getString(ec.State, "job_id")
	if jobID == "" {
		jobID = getString(ec.State, "external_reference")
	}
	return e.finishPauseRequeue(ctx, inst, ec, jobID, pause.Mode)
}

// finishPauseRequeue also resumes a durable move before normal reconciliation,
// even if the pause was lifted. An intent does not prove the worker stopped.
func (e *Engine) finishPauseRequeue(ctx context.Context, inst *store.ActionInstance, ec *ExecutionContext, jobID, mode string) (bool, error) {
	node := e.instanceWorker(ec.State)
	if jobID != "" && holdsWorkerJob(ec.State) {
		if getString(ec.State, requeuePendingKey) == jobID {
			mode = store.WorkerPauseImmediate
		}
		cancelled, err := e.cancelForPause(ctx, ec, jobID, mode)
		if err != nil {
			return false, err
		}
		if !cancelled {
			delete(ec.State, requeuePendingKey)
			return false, e.persistExecutionState(ctx, ec)
		}
		// The worker removes what it wrote for the job; this removes what the
		// server knows of, and the paths that stay are kept on the action.
		e.retireCandidate(ctx, inst, ec, candidateRetiredRequeued)
		if path := getString(ec.State, "candidate_path"); path != "" && !getBool(ec.State, candidateRemovedKey) {
			leftover := map[string]any{}
			for _, key := range []string{"candidate_path", "resolved_path"} {
				leftover[key] = ec.State[key]
			}
			leftover["job_id"], leftover[workerStateKey] = jobID, node
			ec.State[requeueLeftoversKey] = append(requeueLeftoverEntries(ec.State), leftover)
		}
	}
	if err := e.resetForRequeue(ctx, inst, ec, jobID, node); err != nil {
		return false, err
	}
	e.noteWorkerSlotFreed()
	return true, nil
}

// cancelForPause cancels the job on its node when the pause's mode lets it
// move: a queued job in either mode, an encoding one only in immediate mode.
// It reports false, with no error, for a job it must leave: finished, kept for
// promotion, waiting to be saved, or encoding under a graceful pause. A job
// the node does not have counts as cancelled.
func (e *Engine) cancelForPause(ctx context.Context, ec *ExecutionContext, jobID, mode string) (bool, error) {
	worker := e.actionWorker(ec)
	st, err := worker.Status(ctx, jobID)
	if err != nil {
		if he, ok := transcodeHTTPError(err); ok && he.StatusCode == 404 {
			// A submit the server never saw answered may still land on the node
			// later; moving the file now could encode it twice. Leave it until
			// the node says what it has (or the node is resumed).
			return !getBool(ec.State, "transcode_reconcile"), nil
		}
		return false, fmt.Errorf("reading job %s before moving it: %w", jobID, err)
	}
	if st.RecoveryRequired || st.Retained {
		return false, nil
	}
	switch st.Status {
	case transcode.StatusQueued, transcode.StatusRunning:
		// A finished encode is publishing: cancelling it would throw the
		// result away. Only a still active job is protected this way; a
		// cancelled one has already lost its output on the node.
		if st.EncodeComplete {
			return false, nil
		}
		if st.Status == transcode.StatusRunning && mode != store.WorkerPauseImmediate {
			return false, nil
		}
	case transcode.StatusCancelled, transcode.StatusFailed:
		// Nothing runs on the node for it, and the action would only start it
		// again on this paused node: let it start over on another.
		return true, nil
	default:
		return false, nil
	}
	ec.State[requeuePendingKey] = jobID
	if err := e.persistExecutionState(ctx, ec); err != nil {
		return false, fmt.Errorf("recording the move of %s: %w", ec.InstanceID, err)
	}
	if err := e.cancelTranscodeJobConfirmed(ctx, worker, jobID); err != nil {
		return false, fmt.Errorf("cancelling job %s to move it: %w", jobID, err)
	}
	return true, nil
}

// resetForRequeue puts the action back in the queue. The new state keeps what
// a conversion keeps from its request (where it came from, its place in the
// queue) and a record of the attempt it replaces; the next attempt gets its
// own job identity, since the node still knows the cancelled one.
func (e *Engine) resetForRequeue(ctx context.Context, inst *store.ActionInstance, ec *ExecutionContext, jobID, node string) error {
	fresh := map[string]any{}
	for _, key := range []string{"request_origin", "mode_policy", transcodeQueueOrderKey, transcodeQueueSinceKey, "previous_job_id", requeueLeftoversKey, SubmittedSourceBytesKey, roomReserveKey, roomRefusedByKey, roomRefusedAtKey, runnerKilledKey, runnerKilledJobKey} {
		if v, ok := ec.State[key]; ok {
			fresh[key] = v
		}
	}
	if jobID != "" {
		fresh["previous_job_id"] = jobID
		fresh[requeueRetryCountKey] = getInt(ec.State, requeueRetryCountKey) + 1
	} else if n := getInt(ec.State, requeueRetryCountKey); n > 0 {
		fresh[requeueRetryCountKey] = n
	}
	fresh[requeueCountKey] = getInt(ec.State, requeueCountKey) + 1
	fresh[requeuedFromKey] = node
	fresh[requeuedAtKey] = e.now().Format(time.RFC3339Nano)

	inst.Status = StatusWaitingExternal
	inst.CurrentStep = 0
	inst.WaitingCondition = "worker_busy"
	inst.WaitingReason = fmt.Sprintf("Node %s was paused; back in the transcode queue to start again on a free node", node)
	if why := getString(ec.State, requeueWhyKey); why != "" {
		inst.WaitingReason = fmt.Sprintf("Back in the transcode queue (%s): %s", node, why)
	}
	inst.WaitingOptionsJSON = "[]"
	inst.ErrorJSON = ""
	inst.StateJSON, inst.OutputsJSON = toJSON(fresh), toJSON(fresh)
	if err := e.updateInstance(ctx, inst); err != nil {
		return fmt.Errorf("putting %s back in the queue: %w", inst.ID, err)
	}
	// The batch row shows the file on no node until it is assigned again.
	if err := e.deps.Store.ClearTranscodeBatchItemJob(inst.ID); err != nil {
		log.Printf("worker pause: clearing the batch row of %s: %v", inst.ID, err)
	}
	ec.State, ec.Outputs = fresh, fresh
	return nil
}

// requeueLeftoverStates keeps each retired job's ownership after the reset.
// Legacy path-only entries cannot prove ownership and are never removed.
func requeueLeftoverStates(state map[string]any) []map[string]any {
	var states []map[string]any
	for _, entry := range requeueLeftoverEntries(state) {
		if old, ok := entry.(map[string]any); ok {
			states = append(states, old)
		}
	}
	return states
}

func requeueLeftoverEntries(state map[string]any) []any {
	var list []any
	_ = json.Unmarshal([]byte(toJSON(state[requeueLeftoversKey])), &list)
	return list
}

// NodePauseStatuses is every configured node's pause by node name: the mode,
// and for a paused node what is left before the machine is free.
func (e *Engine) NodePauseStatuses(ctx context.Context) map[string]transcode.NodePauseStatus {
	return e.nodePauseStatuses(ctx, e.observeWorkers(ctx))
}

// NodePauseStatesStored is NodePauseStatuses without the worker health probes:
// the same effective pause, builds pause and build list, read from the store and
// memory only, so it makes no network call. Reachable is left false and the
// messages that depend on it are not meaningful; the mode, the paused flags and
// Builds are the same as NodePauseStatuses reports (see
// TestNodePauseStatesStoredEqualsObservedState). nodePauseStatuses only reads:
// it writes nothing to the store, expiry included.
func (e *Engine) NodePauseStatesStored(ctx context.Context) map[string]transcode.NodePauseStatus {
	return e.nodePauseStatuses(ctx, transcode.WorkerObservation{})
}

func (e *Engine) nodePauseStatuses(ctx context.Context, obs transcode.WorkerObservation) map[string]transcode.NodePauseStatus {
	now := time.Now().UTC()
	out := map[string]transcode.NodePauseStatus{}
	for _, w := range e.deps.Workers.All() {
		st := transcode.NodePauseStatus{Node: w.Name, CheckedAt: now}
		for _, o := range obs.Workers {
			if o.Name == w.Name {
				st.Reachable = o.Reachable
			}
		}
		out[w.Name] = st
	}
	if e.deps.Store == nil {
		return out
	}
	pauses, perr := e.workerPauses()
	operator, _ := e.operatorPauses()
	// An unreadable builds-pause state reads as paused, like the transcode
	// pause: the create path fails closed too, so the page must not say otherwise.
	buildPauses, bperr := e.deps.Store.ListBuildPauses()
	rows, rerr := e.deps.Store.ListDrainActions()
	encoding := map[string]int{}
	if rerr == nil {
		for _, r := range rows {
			if classifyDrainRow(r) == transcode.DrainJobRunning && !r.WorkerQueued {
				encoding[e.resolveWorker(r.Worker)]++
			}
		}
	}
	for name, st := range out {
		st.Builds = e.NodeBuilds(name)
		if since, ok := buildPauses[name]; ok {
			st.BuildsPaused, st.BuildsPausedSince = true, since.Since
		} else if bperr != nil {
			st.BuildsPaused = true
		}
		if perr != nil {
			st.Paused, st.Message = true, "The pause state could not be read; new jobs are held until it can."
			out[name] = st
			continue
		}
		pause, paused := pauses[name]
		if !paused {
			st.Message = "Transcode not paused. The node takes new jobs."
			out[name] = st
			continue
		}
		st.Paused, st.Mode, st.Since = true, pause.Mode, pause.Since
		st.HeldBy = "builds"
		if _, ok := operator[name]; ok {
			st.HeldBy = "operator"
		}
		if rerr != nil {
			st.Message = "The open jobs could not be read, so the node cannot be reported idle."
			out[name] = st
			continue
		}
		for _, r := range rows {
			if e.resolveWorker(r.Worker) != name {
				continue
			}
			state := classifyDrainRow(r)
			// A job still in the node's queue, on a node that confirmed it is
			// holding that queue, will not start before the resume.
			if state == transcode.DrainJobRunning && r.WorkerQueued {
				if active, ok := e.workerHolding(name); ok && active <= encoding[name] {
					state = transcode.DrainJobQueued
				}
			}
			switch state {
			case transcode.DrainJobRunning:
				st.Running++
			case transcode.DrainJobPublicationPending:
				st.PublicationPending++
			}
		}
		settling := false
		if at, err := time.Parse(time.RFC3339, pause.Since); err == nil && time.Since(at) < e.pauseSettleWindow() {
			settling = true
		}
		// A suspended node's frozen encodes are not running. The worker gives
		// counts, not job ids, so a frozen job cannot be attributed to a row:
		// the node counts as frozen only when every job the worker runs is
		// (frozen == active). Otherwise nothing is attributed and at least the
		// unfrozen remainder keeps the node from being reported idle, even if
		// the rows show fewer running jobs than the worker has.
		confirmed := false
		if pause.Mode == store.WorkerPauseSuspend {
			if active, frozen, ok := e.workerFrozen(name); ok {
				confirmed = true
				if frozen == active {
					st.Suspended = min(st.Running, frozen)
					st.Running -= st.Suspended
				} else {
					st.Running = max(st.Running, active-frozen, 1)
				}
			}
		}
		switch {
		case settling:
			st.Message = "Transcode paused just now; checking for admissions in flight."
		case !st.Reachable:
			st.Message = "Transcode paused, but the node is unreachable so its jobs cannot be confirmed."
		case st.PublicationPending > 0:
			st.Message = fmt.Sprintf("Transcode paused: %d finished encode(s) not saved yet. Use Try again once storage is reachable.", st.PublicationPending)
		case pause.Mode == store.WorkerPauseSuspend && !confirmed:
			st.Message = "Transcode suspended, but the node has not confirmed that its encodes are frozen."
		case pause.Mode == store.WorkerPauseSuspend && st.Running > 0:
			st.Message = fmt.Sprintf("Transcode suspended: %d job(s) not frozen yet.", st.Running)
		case st.Suspended > 0:
			st.Idle = true
			st.Message = fmt.Sprintf("Transcode suspended: %d encode(s) frozen where they were.", st.Suspended)
		case st.Running > 0 && pause.Mode == store.WorkerPauseImmediate:
			st.Message = fmt.Sprintf("Transcode paused: %d job(s) still active. Encoding jobs return to the queue; encoded files finish publishing.", st.Running)
		case st.Running > 0:
			st.Message = fmt.Sprintf("Transcode paused: %d job(s) still finishing. Nothing new starts here.", st.Running)
		default:
			st.Idle = true
			st.Message = "Transcode paused and idle: no encode runs on this node."
		}
		if st.HeldBy == "builds" {
			running := 0
			for _, b := range st.Builds {
				if b.Status == store.BuildRunning {
					running++
				}
			}
			st.Message = fmt.Sprintf("Busy with %d running build(s). ", running) + st.Message
		}
		out[name] = st
	}
	return out
}
