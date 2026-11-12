package action

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// Suspend mode of a node pause.
//
// The node is paused like a graceful pause (nothing new starts, files not yet
// begun go back to the queue), but the encodes that are running are frozen on
// the node instead of finishing or being cancelled, and continue where they
// were when the node is resumed. The server's part is only to keep the worker
// told: the freezing is done by the worker's own runners (see
// internal/transcodeworker/suspend.go). A frozen job stays assigned to its
// node and is never requeued, cancelled, retried or reported failed: the
// worker keeps answering its status as running, with Suspended set, and the
// action keeps polling it.
//
// Raising a suspend to immediate keeps the freeze until the cancelled jobs are
// gone from the node (an encode must not run again for the seconds before the
// sweep cancels it); lowering it to graceful continues the encodes, which then
// finish as in any graceful pause.

// workerSuspender is what an HTTP worker offers for suspending its encodes.
type workerSuspender interface {
	Suspend(ctx context.Context) (httpclient.SuspendStatus, error)
	SetSuspend(ctx context.Context, suspend bool) (httpclient.SuspendStatus, error)
}

type workerSuspendState struct {
	mu          sync.Mutex
	known, on   bool
	active      int
	frozen      int
	countsKnown bool
	unsupported time.Time
}

// workerSuspendWanted is whether the worker should have its encodes frozen
// now. ok is false when that cannot be decided (the pause state or the open
// jobs could not be read): the worker is then left as it is, because freezing
// or releasing on a guess is the worse mistake either way.
func (e *Engine) workerSuspendWanted(name string) (want, ok bool) {
	pauses, err := e.workerPauses()
	if err != nil {
		return false, false
	}
	pause, paused := pauses[e.resolveWorker(name)]
	switch {
	case !paused:
		return false, true
	case pause.Mode == store.WorkerPauseSuspend:
		return true, true
	case pause.Mode == store.WorkerPauseImmediate:
		// Until the cancels have emptied the node.
		rows, err := e.deps.Store.ListDrainActions()
		if err != nil {
			return false, false
		}
		for _, r := range rows {
			if e.resolveWorker(r.Worker) == e.resolveWorker(name) && classifyDrainRow(r) == transcode.DrainJobRunning && !r.WorkerQueued {
				return true, true
			}
		}
	}
	return false, true
}

// syncWorkerSuspend brings one worker's suspend in line with its node's pause.
// Like the hold it reads the worker's state first and sends only a change, so
// a confirmation is always a fresh one. It does nothing for a worker that
// cannot suspend.
func (e *Engine) syncWorkerSuspend(ctx context.Context, w transcode.NamedWorker) {
	suspender, ok := w.Executor.(workerSuspender)
	if !ok {
		return
	}
	st := &e.workerStateFor(w.Name).suspend
	st.mu.Lock()
	defer st.mu.Unlock()
	want, decided := e.workerSuspendWanted(w.Name)
	if !decided {
		return
	}
	if !want && !st.unsupported.IsZero() && time.Since(st.unsupported) < holdUnsupportedRecheck {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), holdCallTimeout)
	defer cancel()
	got, err := suspender.Suspend(ctx)
	if err == nil && got.Suspend != want {
		got, err = suspender.SetSuspend(ctx, want)
		if err == nil && got.Suspend != want {
			err = errors.New("worker reports the other suspend after the change")
		}
	}
	switch {
	case errors.Is(err, httpclient.ErrSuspendUnsupported):
		st.known, st.on, st.countsKnown, st.unsupported = false, false, false, time.Now()
		if want {
			log.Printf("worker %s: suspend wanted but the worker does not support it", w.Name)
		}
	case err != nil:
		st.known, st.countsKnown = false, false
		log.Printf("worker %s: suspend %t not confirmed: %v", w.Name, want, err)
	default:
		st.known, st.on, st.unsupported = true, want, time.Time{}
		st.active, st.frozen, st.countsKnown = got.Active, got.Frozen, got.CountsKnown
	}
}

// workerSuspendConfirmed reports whether the worker confirmed it is suspended.
func (e *Engine) workerSuspendConfirmed(name string) bool {
	st := &e.workerStateFor(name).suspend
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.known && st.on
}

// workerFrozen is how many running jobs the suspended worker says are frozen
// out of the ones it runs. ok is false unless the worker confirmed a suspend
// and reports the counts.
func (e *Engine) workerFrozen(name string) (active, frozen int, ok bool) {
	st := &e.workerStateFor(name).suspend
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.known || !st.on || !st.countsKnown {
		return 0, 0, false
	}
	return st.active, st.frozen, true
}

// suspendNode puts a node into suspend mode, or reports why it cannot. A node
// that does not confirm the freeze is returned to the pause it had, so the
// operator never believes a CPU is free while an encode still runs on it.
func (e *Engine) suspendNode(ctx context.Context, w transcode.NamedWorker, previous string) (transcode.NodePauseStatus, error) {
	if _, ok := w.Executor.(workerSuspender); !ok {
		return transcode.NodePauseStatus{}, fmt.Errorf("%w: node %s has no remote worker to suspend", transcode.ErrSuspendUnavailable, w.Name)
	}
	if _, err := e.deps.Store.SetWorkerPause(w.Name, store.WorkerPauseSuspend); err != nil {
		return transcode.NodePauseStatus{}, err
	}
	e.syncWorkerHold(ctx, w)
	if !e.workerSuspendConfirmed(w.Name) {
		// Back to what it was, and the worker told so.
		if _, err := e.deps.Store.SetWorkerPause(w.Name, previous); err != nil {
			log.Printf("worker %s: undoing an unconfirmed suspend: %v", w.Name, err)
		}
		e.syncWorkerHold(ctx, w)
		return transcode.NodePauseStatus{}, fmt.Errorf("%w: node %s did not confirm the suspend (unreachable, or its worker is not updated); nothing changed", transcode.ErrSuspendUnavailable, w.Name)
	}
	return transcode.NodePauseStatus{}, nil
}
