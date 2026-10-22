package action

import (
	"context"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// Batch fan-out follows the workers.
//
// Each worker's parallel jobs are set from the maintenance UI and its video
// pool (the ceiling, or the video share of a split) is how many conversions it
// runs at once; a worker's max_parallel_jobs in the server config caps that.
// A batch keeps as many children in flight as its ready workers have free
// slots for it. The server's transcode.max_parallel_jobs is an optional cap on
// top: 0 follows the workers, a positive value never lets one batch exceed it.

// workerPoolTTL bounds how stale a cached video pool can get. The pool only
// changes when the operator saves new slot settings.
const workerPoolTTL = 30 * time.Second

type workerPoolReading struct {
	at    time.Time
	video int // 0 while the worker has never reported a pool
}

type workerSlotReader interface {
	Slots(ctx context.Context) (transcode.WorkerSlots, error)
}

// batchAvailableSlots is how many more children the batch may start now: for
// each ready worker, its capacity minus the batch's running items assigned to
// it, less the running items not yet assigned a worker. When no worker is
// ready every worker counts, so the children are started and wait for their
// worker as a single worker's always have. activeCount is the batch's running
// items.
func (e *Engine) batchAvailableSlots(ctx context.Context, items []store.TranscodeBatchItem, activeCount int) int {
	limit := 0
	if e.deps.Config != nil {
		limit = e.deps.Config.Transcode.MaxParallelJobs
	}
	workers := e.deps.Workers.All()
	if len(workers) == 0 {
		// Without a worker the children start and fail as they always have.
		return max(limit, 1) - activeCount
	}
	ready := make([]bool, len(workers))
	if len(workers) == 1 {
		ready[0] = true
	} else {
		ready = e.workersReady(ctx, workers)
		anyReady := false
		for _, r := range ready {
			anyReady = anyReady || r
		}
		if !anyReady {
			for i := range ready {
				ready[i] = true
			}
		}
	}
	running := map[string]int{}
	for _, it := range items {
		if it.Status == "running" {
			running[it.Worker]++
		}
	}
	// An item with no worker is a child not yet assigned one (earlier items
	// that hold a job are stamped at startup): it takes room on whichever
	// worker it is given, so it counts against the total. An item counts on
	// its worker by its resolved name.
	unassigned := running[""]
	delete(running, "")
	for name, n := range running {
		if resolved := e.resolveWorker(name); resolved != name {
			delete(running, name)
			running[resolved] += n
		}
	}
	available := -unassigned
	for i, w := range workers {
		if ready[i] {
			available += max(0, e.workerVideoCapacity(ctx, w)-running[w.Name])
		}
	}
	if limit > 0 {
		available = min(available, limit-activeCount)
	}
	return available
}

// workerVideoCapacity is how many conversions the server keeps on a worker at
// once: its video pool, capped by its max_parallel_jobs when set. Without a
// pool reported (an older worker, or one never reached) it is the worker's
// max_parallel_jobs, else transcode.max_parallel_jobs, else 1.
func (e *Engine) workerVideoCapacity(ctx context.Context, w transcode.NamedWorker) int {
	pool := e.workerVideoPool(ctx, w)
	switch {
	case pool > 0 && w.MaxParallelJobs > 0:
		return min(pool, w.MaxParallelJobs)
	case pool > 0:
		return pool
	case w.MaxParallelJobs > 0:
		return w.MaxParallelJobs
	}
	if e.deps.Config != nil && e.deps.Config.Transcode.MaxParallelJobs > 0 {
		return e.deps.Config.Transcode.MaxParallelJobs
	}
	return 1
}

// workerVideoPool is the video pool the worker last reported, re-read at most
// every workerPoolTTL. A failed read keeps the last value.
func (e *Engine) workerVideoPool(ctx context.Context, w transcode.NamedWorker) int {
	reader, ok := w.Executor.(workerSlotReader)
	if !ok {
		return 0
	}
	st := e.workerStateFor(w.Name)
	st.poolMu.Lock()
	defer st.poolMu.Unlock()
	now := time.Now()
	if !st.pool.at.IsZero() && now.Sub(st.pool.at) < workerPoolTTL {
		return st.pool.video
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	st.pool.at = now
	slots, err := reader.Slots(ctx)
	if err != nil {
		return st.pool.video
	}
	video := slots.Video.Total
	if !slots.Split || video < 1 {
		video = slots.Ceiling
	}
	if video > 0 {
		st.pool.video = video
	}
	return st.pool.video
}
