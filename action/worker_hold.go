package action

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// Pause all also has to stop the worker's own queue. A job the server already
// sent sits in the worker's queue as queued, and the worker starts the next
// one whenever a slot frees; drain alone only stops new submissions. While
// drain is on, every worker that supports it is told to hold its queue, so
// only the encodes already running finish and nothing else starts. The state
// is pushed when drain changes and re-sent from the worker observation until
// the worker confirms, so a worker that was down or restarted catches up.
// While a worker confirms the hold, its queued jobs are held back like any
// other file, not in flight, so they do not block a deploy.

// workerHolder is what an HTTP worker offers for holding its queue.
type workerHolder interface {
	SetHold(ctx context.Context, hold bool) (bool, error)
}

// holdUnsupportedRecheck is how long a worker that predates the hold is left
// alone before the engine asks again.
const holdUnsupportedRecheck = 10 * time.Minute

type workerHoldState struct {
	mu          sync.Mutex
	known, on   bool
	unsupported time.Time
}

// workerHoldWanted is whether the workers should hold their queues now. An
// unreadable drain state holds, as drainHold does.
func (e *Engine) workerHoldWanted() bool {
	held, _ := e.drainHold()
	return held
}

// syncWorkerHold brings one worker's hold in line with the drain state. It
// does nothing for a worker that cannot hold and stays quiet when the worker
// already confirmed the wanted value.
func (e *Engine) syncWorkerHold(ctx context.Context, w transcode.NamedWorker) {
	holder, ok := w.Executor.(workerHolder)
	if !ok {
		return
	}
	want := e.workerHoldWanted()
	st := &e.workerStateFor(w.Name).hold
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.known && st.on == want {
		return
	}
	if !st.unsupported.IsZero() && time.Since(st.unsupported) < holdUnsupportedRecheck {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	got, err := holder.SetHold(ctx, want)
	switch {
	case errors.Is(err, httpclient.ErrHoldUnsupported):
		st.known, st.on, st.unsupported = false, false, time.Now()
	case err != nil:
		st.known = false
		log.Printf("worker %s: hold %t not confirmed: %v", w.Name, want, err)
	case got != want:
		st.known = false
		log.Printf("worker %s: asked to hold=%t but it reports hold=%t", w.Name, want, got)
	default:
		st.known, st.on, st.unsupported = true, want, time.Time{}
	}
}

// syncWorkerHolds pushes the hold to every worker at once.
func (e *Engine) syncWorkerHolds(ctx context.Context) {
	var wg sync.WaitGroup
	for _, w := range e.deps.Workers.All() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.syncWorkerHold(ctx, w)
		}()
	}
	wg.Wait()
}

// workerHolding reports whether the worker confirmed it is holding its queue.
func (e *Engine) workerHolding(name string) bool {
	st := &e.workerStateFor(name).hold
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.known && st.on
}
