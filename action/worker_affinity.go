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
// started before workers had names, when the only worker was video-worker:
// it belongs to the worker of that name. At startup such actions are
// assigned the first configured worker when that one has another name (see
// AssignEarlierWorkerActions), so they keep their worker when the config moves
// from the single http block to a named list.
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

// executorFor is the executor of the named worker; an empty name is
// video-worker (see above). A worker that is not configured gives an executor
// whose every call fails saying so.
func (e *Engine) executorFor(name string) transcode.Executor {
	if name == "" {
		name = transcode.DefaultWorkerName
	}
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
	// Podcast work has one worker, and with one worker there is no choice to
	// make: its readiness is checked by the steps, as the jobs are sent.
	if all := e.deps.Workers.All(); ec.ActionName == "clean_podcast_ads" || len(all) == 1 {
		w := all[0]
		if ec.ActionName == "clean_podcast_ads" {
			w, _ = e.deps.Workers.Podcast()
		}
		return e.recordAssignedWorker(ctx, ec, w.Name)
	}
	e.bindMu.Lock()
	defer e.bindMu.Unlock()
	name, err := e.pickVideoWorker(ctx)
	if err != nil {
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_unreachable", WaitingReason: err.Error()}, true
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
// the first in config order on a tie. A worker with no free slot can still be
// chosen when every ready worker is full: it queues the job itself.
func (e *Engine) pickVideoWorker(ctx context.Context) (string, error) {
	workers := e.deps.Workers.All()
	ready := e.workersReady(ctx, workers)
	active := map[string]int{}
	if e.deps.Store != nil {
		counts, err := e.deps.Store.CountActiveWorkerActions("transcode_media")
		if err != nil {
			return "", fmt.Errorf("Could not count the jobs assigned to each worker: %v", err)
		}
		active = counts
	}
	best, bestFree := -1, 0
	for i, w := range workers {
		if !ready[i] {
			continue
		}
		free := e.workerVideoCapacity(ctx, w) - active[w.Name]
		if best < 0 || free > bestFree {
			best, bestFree = i, free
		}
	}
	if best < 0 {
		return "", fmt.Errorf("No video worker is ready; the job waits for one before it is assigned")
	}
	return workers[best].Name, nil
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

// AssignEarlierWorkerActions records the worker of actions started before
// workers had names, so they stay with it when the config names its workers.
// Those actions belong to video-worker; when the first configured worker has
// another name, every worker action and batch item that holds a worker job and
// names no worker is assigned that first worker, and video-worker's slot
// settings become its own. An action that holds no job yet is assigned a
// worker when it next runs. It runs before the reconciler starts and is a
// no-op once nothing is left to assign.
func (e *Engine) AssignEarlierWorkerActions() error {
	if e.deps.Store == nil || !e.workersConfigured() {
		return nil
	}
	first := e.deps.Workers.All()[0].Name
	if first == transcode.DefaultWorkerName {
		return nil
	}
	n, err := e.deps.Store.AssignUnnamedWorker(first, transcode.DefaultWorkerName)
	if err != nil {
		return fmt.Errorf("assigning earlier worker actions to worker %s: %w", first, err)
	}
	if n > 0 {
		log.Printf("worker affinity: %d earlier worker actions and batch items are assigned to worker %s", n, first)
	}
	return nil
}

// ActionWorkerExecutor is the executor of the worker named in an action's
// state, for adapters that read a job's logs or details from its worker.
func (e *Engine) ActionWorkerExecutor(state map[string]any) transcode.Executor {
	return e.executorFor(assignedWorker(state))
}
