package action

import (
	"context"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// Batch fan-out follows the worker.
//
// The worker's parallel jobs are set from the maintenance UI and its video
// pool (the ceiling, or the video share of a split) is how many conversions it
// runs at once, so a batch keeps that many children in flight. The server's
// transcode.max_parallel_jobs is an optional cap on top: 0 follows the worker,
// a positive value never lets one batch exceed it.

// workerPoolTTL bounds how stale the cached video pool can get. The pool only
// changes when the operator saves new slot settings.
const workerPoolTTL = 30 * time.Second

type workerPoolReading struct {
	at    time.Time
	video int // 0 while the worker has never reported a pool
}

type workerSlotReader interface {
	Slots(ctx context.Context) (transcode.WorkerSlots, error)
}

// batchMaxParallel is how many children a batch keeps in flight: the worker's
// video pool capped by transcode.max_parallel_jobs when that is positive.
// Without a pool reported (an older worker, or one never reached) it is the
// configured cap, else 1.
func (e *Engine) batchMaxParallel(ctx context.Context) int {
	limit := 0
	if e.deps.Config != nil {
		limit = e.deps.Config.Transcode.MaxParallelJobs
	}
	pool := e.workerVideoPool(ctx)
	switch {
	case pool > 0 && limit > 0:
		return min(pool, limit)
	case pool > 0:
		return pool
	case limit > 0:
		return limit
	}
	return 1
}

// workerVideoPool is the video pool the worker last reported, re-read at most
// every workerPoolTTL. A failed read keeps the last value.
func (e *Engine) workerVideoPool(ctx context.Context) int {
	worker, ok := e.deps.Transcode.(workerSlotReader)
	if !ok {
		return 0
	}
	e.workerPoolMu.Lock()
	defer e.workerPoolMu.Unlock()
	now := time.Now()
	if !e.workerPool.at.IsZero() && now.Sub(e.workerPool.at) < workerPoolTTL {
		return e.workerPool.video
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	e.workerPool.at = now
	slots, err := worker.Slots(ctx)
	if err != nil {
		return e.workerPool.video
	}
	video := slots.Video.Total
	if !slots.Split || video < 1 {
		video = slots.Ceiling
	}
	if video > 0 {
		e.workerPool.video = video
	}
	return e.workerPool.video
}
