package action

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/jakenesler/navigatorr/transcode"
)

// Worker affinity.
//
// An action that runs worker jobs (a conversion, a podcast clean) is assigned
// one worker the first time one of its steps runs, before it reads any worker
// capability or plans anything for that worker. The worker's name is stored in
// the action's state under "worker" and never changes: every capability read,
// submit, status poll, cancel and candidate check of that action goes to that
// worker. A worker that is offline makes its actions wait; a worker that is no
// longer configured makes them wait as well, naming it, until it is configured
// again or the operator cancels them. Nothing moves to another worker.
//
// An action with no worker in its state that already holds a worker job was
// started before workers had names, when the only worker was video-worker.
// It belongs to the legacy worker: video-worker while one of that name is
// configured, else the first configured worker, which is where the single
// http block's worker goes when the config becomes a named list. The same
// rule maps a stored "video-worker" once no worker has that name. At startup
// every such action and batch item is stamped with the legacy worker (see
// AssignEarlierWorkerActions), and the engine applies the same rule wherever
// it reads a worker name, so such a job never reaches another worker.
//
// A conversion goes to the ready worker with the most free video slots (its
// video pool minus the conversions already assigned to it), config order
// breaking ties. Podcast work always goes to the podcast worker, which keeps
// the ad catalog.

// workerStateKey is the action state key holding the assigned worker's name.
const workerStateKey = "worker"

// actionUsesWorker reports whether an action runs jobs on a worker and so is
// assigned one.
func actionUsesWorker(name string) bool {
	return name == "transcode_media" || name == "clean_podcast_ads"
}

// workerState is what the engine caches per worker.
type workerState struct {
	obsMu sync.Mutex
	obs   transcode.WorkerObservation

	poolMu sync.Mutex
	pool   workerPoolReading

	hold workerHoldState
}

func (e *Engine) workerStateFor(name string) *workerState {
	e.workerStatesMu.Lock()
	defer e.workerStatesMu.Unlock()
	if e.workerStates == nil {
		e.workerStates = make(map[string]*workerState)
	}
	st := e.workerStates[name]
	if st == nil {
		st = &workerState{}
		e.workerStates[name] = st
	}
	return st
}

// workersConfigured reports whether any worker is configured.
func (e *Engine) workersConfigured() bool { return e.deps.Workers.Len() > 0 }

// legacyWorker is the worker that jobs from before workers had names belong
// to (see above): video-worker when configured, else the first worker.
func (e *Engine) legacyWorker() string {
	if _, ok := e.deps.Workers.Get(transcode.DefaultWorkerName); ok || !e.workersConfigured() {
		return transcode.DefaultWorkerName
	}
	return e.deps.Workers.All()[0].Name
}

// resolveWorker maps a stored worker name to the worker it means: an empty
// name, or video-worker once no worker has that name, is the legacy worker.
func (e *Engine) resolveWorker(name string) string {
	if name == "" || name == transcode.DefaultWorkerName {
		return e.legacyWorker()
	}
	return name
}

// holdsWorkerJob reports whether an action's state holds a job on a worker,
// submitted or possibly submitted. It matches the store's stamp of earlier
// actions (store.AssignLegacyWorker).
func holdsWorkerJob(state map[string]any) bool {
	return getString(state, "job_id") != "" || getString(state, "external_reference") != "" || getBool(state, "transcode_submitted") || getBool(state, "transcode_reconcile")
}

// executorFor is the executor of the named worker (see resolveWorker). A
// worker that is not configured gives an executor whose every call fails
// saying so.
func (e *Engine) executorFor(name string) transcode.Executor {
	name = e.resolveWorker(name)
	w, ok := e.deps.Workers.Get(name)
	if !ok {
		return unavailableExecutor{err: &transcode.WorkerNotConfiguredError{Name: name}}
	}
	return w.Executor
}

// actionWorker is the executor of the worker the action was assigned.
func (e *Engine) actionWorker(ec *ExecutionContext) transcode.Executor {
	return e.executorFor(assignedWorker(ec.State))
}

// assignedWorker is the worker named in an action's state, "" before one is
// assigned.
func assignedWorker(state map[string]any) string { return getString(state, workerStateKey) }

// podcastExecutor is the podcast worker's executor, nil without workers.
func (e *Engine) podcastExecutor() transcode.Executor {
	w, ok := e.deps.Workers.Podcast()
	if !ok {
		return nil
	}
	return w.Executor
}

// unavailableExecutor stands in for a worker that is no longer configured. It
// never reaches a network.
type unavailableExecutor struct{ err error }

func (u unavailableExecutor) Doctor(context.Context) error { return u.err }
func (u unavailableExecutor) Capabilities(context.Context) (transcode.WorkerCapabilities, error) {
	return transcode.WorkerCapabilities{}, u.err
}
func (u unavailableExecutor) Submit(context.Context, transcode.Request) (transcode.Job, error) {
	return transcode.Job{}, u.err
}
func (u unavailableExecutor) Status(context.Context, string) (transcode.JobStatus, error) {
	return transcode.JobStatus{}, u.err
}
func (u unavailableExecutor) Cancel(context.Context, string) error { return u.err }

// ensureWorkerAssigned assigns the action a worker before its first step that
// could use one. It reports waiting=true, with the result to return, when no
// worker can take it yet; the action then has no worker and is assigned one
// on a later run. Without any worker configured it assigns nothing and the
// steps fail as they always have.
func (e *Engine) ensureWorkerAssigned(ctx context.Context, ec *ExecutionContext) (StepResult, bool) {
	if !actionUsesWorker(ec.ActionName) || assignedWorker(ec.State) != "" || !e.workersConfigured() {
		return StepResult{}, false
	}
	// A job from before workers had names stays on the worker that has it.
	if holdsWorkerJob(ec.State) {
		return e.recordAssignedWorker(ctx, ec, e.legacyWorker())
	}
	// Podcast work has one worker: its readiness is checked by the steps, as
	// the jobs are sent.
	if ec.ActionName == "clean_podcast_ads" {
		w, _ := e.deps.Workers.Podcast()
		return e.recordAssignedWorker(ctx, ec, w.Name)
	}
	e.bindMu.Lock()
	defer e.bindMu.Unlock()
	// A conversion waits for its turn in the transcode queue (see
	// transcode_queue.go) before it is given a worker.
	if res, waiting := e.transcodeQueueTurn(ctx, ec); waiting {
		return res, true
	}
	// With one worker there is no choice to make: its readiness is checked by
	// the steps, as the jobs are sent.
	if all := e.deps.Workers.All(); len(all) == 1 {
		return e.recordAssignedWorker(ctx, ec, all[0].Name)
	}
	name, condition, err := e.pickVideoWorker(ctx)
	if err != nil {
		return StepResult{Status: StepWaitingExternal, WaitingCondition: condition, WaitingReason: err.Error()}, true
	}
	// Recorded under bindMu, so the next pick counts this one.
	return e.recordAssignedWorker(ctx, ec, name)
}

// recordAssignedWorker stores the action's worker before any step uses it.
func (e *Engine) recordAssignedWorker(ctx context.Context, ec *ExecutionContext, name string) (StepResult, bool) {
	ec.State[workerStateKey] = name
	ec.Outputs[workerStateKey] = name
	if err := e.persistExecutionState(ctx, ec); err != nil {
		delete(ec.State, workerStateKey)
		delete(ec.Outputs, workerStateKey)
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_unreachable", WaitingReason: fmt.Sprintf("Could not record the assigned worker: %v", err)}, true
	}
	return StepResult{}, false
}

// pickVideoWorker chooses the ready worker with the most free video slots,
// the first in config order on a tie. A worker with no free slot is never
// chosen, so no worker holds a backlog: the transcode queue keeps the order.
// With nothing to choose it returns the waiting condition: worker_unreachable
// when no worker is ready, worker_busy when every ready worker is full.
func (e *Engine) pickVideoWorker(ctx context.Context) (string, string, error) {
	workers := e.deps.Workers.All()
	ready := e.workersReady(ctx, workers)
	active := map[string]int{}
	if e.deps.Store != nil {
		counts, err := e.deps.Store.CountActiveWorkerActions("transcode_media")
		if err != nil {
			return "", "worker_unreachable", fmt.Errorf("Could not count the jobs assigned to each worker: %v", err)
		}
		active = counts
	}
	best, bestFree, anyReady := -1, 0, false
	for i, w := range workers {
		if !ready[i] {
			continue
		}
		anyReady = true
		free := e.workerVideoCapacity(ctx, w) - active[w.Name]
		if free <= 0 {
			continue
		}
		if best < 0 || free > bestFree {
			best, bestFree = i, free
		}
	}
	switch {
	case best >= 0:
		return workers[best].Name, "", nil
	case anyReady:
		return "", "worker_busy", fmt.Errorf("Every ready video worker is full; the job waits for a free slot before it is assigned")
	}
	return "", "worker_unreachable", fmt.Errorf("No video worker is ready; the job waits for one before it is assigned")
}

// assignableWorkers are the workers a conversion could be assigned now: the
// ready ones. Checks made before a file is assigned a worker (cover art, HFR
// quality) ask only these, so an offline worker never decides them; the
// assigned worker is checked again before anything is sent to it. When no
// worker is ready, or only one is configured, it is every worker, and those
// checks fail as they would against a single offline worker.
func (e *Engine) assignableWorkers(ctx context.Context) []transcode.NamedWorker {
	workers := e.deps.Workers.All()
	if len(workers) <= 1 {
		return workers
	}
	ready := e.workersReady(ctx, workers)
	var out []transcode.NamedWorker
	for i, w := range workers {
		if ready[i] {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return workers
	}
	return out
}

// workersReady checks every worker at once, so an offline worker never
// delays the others. Each check is the cached worker observation.
func (e *Engine) workersReady(ctx context.Context, workers []transcode.NamedWorker) []bool {
	ready := make([]bool, len(workers))
	var wg sync.WaitGroup
	for i, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready[i] = e.workerObservation(ctx, w).Ready
		}()
	}
	wg.Wait()
	return ready
}

// AssignEarlierWorkerActions stamps the legacy worker (see above) on every
// worker action and batch item that holds a worker job and names no worker,
// or names video-worker once no worker has that name; video-worker's slot
// settings then become the legacy worker's. An action that holds no job yet
// is assigned a worker when it next runs. It runs before the reconciler starts
// and is a no-op once nothing is left to stamp.
func (e *Engine) AssignEarlierWorkerActions() error {
	if e.deps.Store == nil || !e.workersConfigured() {
		return nil
	}
	legacy := e.legacyWorker()
	n, err := e.deps.Store.AssignLegacyWorker(legacy, transcode.DefaultWorkerName)
	if err != nil {
		return fmt.Errorf("assigning earlier worker actions to worker %s: %w", legacy, err)
	}
	if n > 0 {
		log.Printf("worker affinity: %d earlier worker actions and batch items are assigned to worker %s", n, legacy)
	}
	return nil
}

// ActionWorkerExecutor is the executor of the worker named in an action's
// state, for adapters that read a job's logs or details from its worker.
func (e *Engine) ActionWorkerExecutor(state map[string]any) transcode.Executor {
	return e.executorFor(e.instanceWorker(state))
}

// instanceWorker is the worker an action belongs to: the assigned one, the
// legacy worker for a job from before workers had names, or "" before it is
// assigned one.
func (e *Engine) instanceWorker(state map[string]any) string {
	name := assignedWorker(state)
	if name == "" && !holdsWorkerJob(state) {
		return ""
	}
	return e.resolveWorker(name)
}

// workerMatches reports whether a record planned for worker may run on the
// action's worker: the same one after resolving stored names, or a record
// from before workers had names. A record planned for another worker is
// never run.
func (e *Engine) workerMatches(worker string, state map[string]any) bool {
	return worker == "" || e.resolveWorker(worker) == e.resolveWorker(assignedWorker(state))
}
