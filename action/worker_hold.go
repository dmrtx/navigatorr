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

// Pause all also has to stop the worker's own queue, and so does a node's own
// pause for that node. A job the server already sent sits in the worker's
// queue as queued, and the worker starts the next one whenever a slot frees;
// drain alone only stops new submissions. While drain is on, every worker that
// supports it is told to hold its queue, so only the encodes already running
// finish and nothing else starts.
//
// The worker is asked on every observation and every reconciler tick, not
// only when drain changes: the hold is re-sent to a worker that lost it
// (restart, state directory replaced) and lifted from one that kept it after
// a Resume it could not hear. While a worker confirms the hold, its queued
// jobs are held back like any other file, not in flight, so they do not block
// a deploy; but only when the worker's own count of running jobs matches what
// the database says is encoding, so a job that started a moment before the
// hold still blocks.

// workerHolder is what an HTTP worker offers for holding its queue.
type workerHolder interface {
	Hold(ctx context.Context) (httpclient.HoldStatus, error)
	SetHold(ctx context.Context, hold bool) (httpclient.HoldStatus, error)
}

// holdUnsupportedRecheck is how long a worker that predates the hold is left
// alone before the engine asks again.
const holdUnsupportedRecheck = 10 * time.Minute

// holdCallTimeout bounds one hold request. It is independent of the caller's
// deadline, which the admission checks may already have used up.
const holdCallTimeout = 4 * time.Second

type workerHoldState struct {
	mu          sync.Mutex
	known, on   bool
	active      int
	activeKnown bool
	unsupported time.Time
}

// workerHoldWanted is whether the worker should hold its queue now: drain is
// on, or the node is paused. An unreadable state holds, as drainHold does.
//
// A node in suspend mode that has confirmed its suspend does not get the hold:
// the suspend already keeps its queue closed (internal/transcodeworker
// queueHeld), and the hold would also refuse PublishRetained, which a finished
// encode must be able to do while the node is suspended. The drain still holds
// it, so a drain on a suspended node keeps refusing publications. Until the
// suspend is confirmed the hold stays, so the queue is never open.
func (e *Engine) workerHoldWanted(name string) bool {
	if held, _ := e.drainHold(); held {
		return true
	}
	held, _ := e.pauseHold(name)
	if held && e.suspendKeepsTheQueue(name) {
		return false
	}
	return held
}

// syncWorkerHold brings one worker's hold in line with the drain state. It
// does nothing for a worker that cannot hold. It reads the worker's hold
// first and only sends a change when it differs, so a confirmation is always
// a fresh one.
func (e *Engine) syncWorkerHold(ctx context.Context, w transcode.NamedWorker) {
	// The suspend follows the same triggers as the hold. Taking a suspend, it
	// goes first: the hold is only released once the suspend is confirmed, so
	// the queue is never open in between. Every other change lifts the freeze
	// after the hold is in place.
	if intent, ok := e.workerSuspendIntent(w.Name); ok && intent == suspendHold {
		e.syncWorkerSuspend(ctx, w)
	} else {
		defer e.syncWorkerSuspend(ctx, w)
	}
	holder, ok := w.Executor.(workerHolder)
	if !ok {
		return
	}
	st := &e.workerStateFor(w.Name).hold
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.unsupported.IsZero() && time.Since(st.unsupported) < holdUnsupportedRecheck {
		return
	}
	want := e.workerHoldWanted(w.Name) // read under the lock: the last toggle wins
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), holdCallTimeout)
	defer cancel()
	got, err := holder.Hold(ctx)
	if err == nil && got.Hold != want {
		got, err = holder.SetHold(ctx, want)
		if err == nil && got.Hold != want {
			err = errors.New("worker reports the other hold after the change")
		}
	}
	switch {
	case errors.Is(err, httpclient.ErrHoldUnsupported):
		st.known, st.on, st.activeKnown, st.unsupported = false, false, false, time.Now()
	case err != nil:
		st.known, st.activeKnown = false, false
		log.Printf("worker %s: hold %t not confirmed: %v", w.Name, want, err)
	default:
		st.known, st.on, st.active, st.activeKnown, st.unsupported = true, want, got.Active, got.ActiveKnown, time.Time{}
	}
}

// syncWorkerHolds checks every worker's hold at once.
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

// startWorkerHoldSync runs a sync in the background unless one is running,
// for the reconciler tick: a worker down during a Resume would otherwise stay
// held until something happened to observe it.
func (e *Engine) startWorkerHoldSync(ctx context.Context) {
	if !e.holdSyncing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer e.holdSyncing.Store(false)
		e.syncWorkerHolds(ctx)
	}()
}

// workerHolding reports whether the worker confirmed it is holding its queue,
// and how many jobs it said it was running when it did (ok is false when it
// does not report that).
func (e *Engine) workerHolding(name string) (active int, ok bool) {
	st := &e.workerStateFor(name).hold
	st.mu.Lock()
	known, on, activeKnown, count := st.known, st.on, st.activeKnown, st.active
	st.mu.Unlock()
	if known && on && activeKnown {
		return count, true
	}
	// A confirmed suspend closes the queue as the hold does.
	if running, _, ok := e.workerFrozen(name); ok {
		return running, true
	}
	return 0, false
}
