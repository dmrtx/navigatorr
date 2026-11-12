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
	// maybeOn is set when a suspend was sent and the worker has not been seen
	// off since: its encodes may be frozen although no answer confirmed it.
	maybeOn bool
}

// suspendIntent is what the server wants of a worker's suspend flag.
type suspendIntent int

const (
	suspendRelease suspendIntent = iota // the encodes run
	suspendHold                         // the encodes are frozen
	suspendKeep                         // leave the flag as the worker has it
)

// suspendIntentFor decides the flag from the node's pause mode and whether
// encodes still run on the node. An immediate pause never freezes anything by
// itself: it only keeps a freeze that is already there (suspend -> immediate),
// until the cancels have emptied the node, so an encode does not run again
// for the moments before it is moved. Off -> immediate and graceful ->
// immediate find the flag off and leave it off.
func suspendIntentFor(paused bool, mode string, encodesOnNode bool) suspendIntent {
	switch {
	case !paused:
		return suspendRelease
	case mode == store.WorkerPauseSuspend:
		return suspendHold
	case mode == store.WorkerPauseImmediate && encodesOnNode:
		return suspendKeep
	}
	return suspendRelease
}

// workerSuspendIntent reads the node's pause and open jobs and decides. ok is
// false when that cannot be decided (the pause state or the open jobs could
// not be read): the worker is then left as it is, because freezing or
// releasing on a guess is the worse mistake either way.
func (e *Engine) workerSuspendIntent(name string) (intent suspendIntent, ok bool) {
	pauses, err := e.workerPauses()
	if err != nil {
		return suspendRelease, false
	}
	pause, paused := pauses[e.resolveWorker(name)]
	encodes := false
	if paused && pause.Mode == store.WorkerPauseImmediate {
		rows, err := e.deps.Store.ListDrainActions()
		if err != nil {
			return suspendRelease, false
		}
		for _, r := range rows {
			if e.resolveWorker(r.Worker) == e.resolveWorker(name) && classifyDrainRow(r) == transcode.DrainJobRunning && !r.WorkerQueued {
				encodes = true
				break
			}
		}
	}
	return suspendIntentFor(paused, pause.Mode, encodes), true
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
	intent, decided := e.workerSuspendIntent(w.Name)
	if !decided {
		return
	}
	want := intent == suspendHold
	if intent == suspendRelease && !st.unsupported.IsZero() && time.Since(st.unsupported) < holdUnsupportedRecheck {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), holdCallTimeout)
	defer cancel()
	got, err := suspender.Suspend(ctx)
	if err == nil && intent == suspendKeep {
		want = got.Suspend
	}
	if err == nil && got.Suspend && !want && !e.holdCoversTheQueue(w) {
		// Lifting the freeze would open the queue: the hold that must take
		// over was not confirmed (the change failed or its answer was lost).
		// The suspend stays on; the next sweep tries the hold again.
		log.Printf("worker %s: suspend kept because its hold is not confirmed yet", w.Name)
		want = true
	}
	if err == nil && got.Suspend != want {
		if want {
			st.maybeOn = true
		}
		got, err = suspender.SetSuspend(ctx, want)
		if err == nil && got.Suspend != want {
			err = errors.New("worker reports the other suspend after the change")
		}
	}
	switch {
	case errors.Is(err, httpclient.ErrSuspendUnsupported):
		st.known, st.on, st.countsKnown, st.unsupported, st.maybeOn = false, false, false, time.Now(), false
		if want {
			log.Printf("worker %s: suspend wanted but the worker does not support it", w.Name)
		}
	case err != nil:
		st.known, st.countsKnown = false, false
		log.Printf("worker %s: suspend %t not confirmed: %v", w.Name, want, err)
	default:
		st.known, st.on, st.unsupported, st.maybeOn = true, got.Suspend, time.Time{}, got.Suspend
		st.active, st.frozen, st.countsKnown = got.Active, got.Frozen, got.CountsKnown
	}
}

// holdCoversTheQueue is whether the worker's queue is closed by something
// other than the suspend: the node does not want a hold at all, the worker has
// none to give (it cannot hold), or it confirmed the hold. Only then may a
// suspend be lifted, so the queue is never open between the two.
func (e *Engine) holdCoversTheQueue(w transcode.NamedWorker) bool {
	if _, ok := w.Executor.(workerHolder); !ok || !e.workerHoldWanted(w.Name) {
		return true
	}
	st := &e.workerStateFor(w.Name).hold
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.known && st.on
}

// workerMaybeFrozen is whether a suspend was sent to the worker and no later
// answer showed it released.
func (e *Engine) workerMaybeFrozen(name string) bool {
	st := &e.workerStateFor(name).suspend
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.maybeOn
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
			// The pause stays stored as suspend, which is what the status
			// shows ("suspended, not confirmed"), so say exactly that: this is
			// not a refusal that changed nothing.
			log.Printf("worker %s: undoing an unconfirmed suspend: %v", w.Name, err)
			e.syncWorkerHold(ctx, w)
			return transcode.NodePauseStatus{}, fmt.Errorf("%w: node %s did not confirm the suspend and the pause could not be restored, so it stays in suspend (its queue is held and the worker may still be running its encodes); resume the node or choose another mode to lift it", transcode.ErrSuspendNotUndone, w.Name)
		}
		e.syncWorkerHold(ctx, w)
		if e.workerMaybeFrozen(w.Name) {
			// The suspend may have reached the worker although no answer came
			// back, and the release could not be confirmed either: "nothing
			// changed" would be untrue.
			return transcode.NodePauseStatus{}, fmt.Errorf("%w: node %s did not confirm the suspend, the previous pause was restored, but the worker did not confirm releasing it and may still have its encodes frozen; the next sweep releases it (resume the node if it does not)", transcode.ErrSuspendReleasePending, w.Name)
		}
		return transcode.NodePauseStatus{}, fmt.Errorf("%w: node %s did not confirm the suspend (unreachable, or its worker is not updated); nothing changed", transcode.ErrSuspendUnavailable, w.Name)
	}
	return transcode.NodePauseStatus{}, nil
}
