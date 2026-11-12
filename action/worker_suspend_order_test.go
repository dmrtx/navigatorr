package action

import (
	"context"
	"errors"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// queueWorker is a worker that can hold its queue and suspend, and writes down
// every moment its queue was open (neither held nor suspended).
type queueWorker struct {
	*suspendingExecutor
	hold          bool
	failHoldOn    bool // SetHold(true) fails without applying
	loseHoldReply bool // SetHold(true) applies but the answer is lost, once
	opened        []string
}

func (q *queueWorker) checkOpen(after string) {
	if !q.hold && !q.suspendingExecutor.suspend {
		q.opened = append(q.opened, after)
	}
}

func (q *queueWorker) Hold(context.Context) (httpclient.HoldStatus, error) {
	return httpclient.HoldStatus{Hold: q.hold, Active: q.active, ActiveKnown: true}, nil
}

func (q *queueWorker) SetHold(_ context.Context, hold bool) (httpclient.HoldStatus, error) {
	if hold && q.failHoldOn {
		return httpclient.HoldStatus{}, errors.New("worker went away")
	}
	q.hold = hold
	q.checkOpen("hold " + map[bool]string{true: "on", false: "off"}[hold])
	if hold && q.loseHoldReply {
		q.loseHoldReply = false
		return httpclient.HoldStatus{}, errors.New("reply lost")
	}
	return httpclient.HoldStatus{Hold: q.hold, Active: q.active, ActiveKnown: true}, nil
}

func (q *queueWorker) SetSuspend(ctx context.Context, suspend bool) (httpclient.SuspendStatus, error) {
	st, err := q.suspendingExecutor.SetSuspend(ctx, suspend)
	q.checkOpen("suspend " + map[bool]string{true: "on", false: "off"}[suspend])
	return st, err
}

func newQueueWorker() *queueWorker {
	q := &queueWorker{suspendingExecutor: newSuspendingM1(transcode.StatusRunning)}
	q.active = 1
	return q
}

// Leaving a suspend must never open the queue. If the hold that takes over is
// lost (the worker forgot it) and cannot be set again, or is set but the answer
// is lost, the suspend stays and the next sweep finishes the change. A suspended
// node always carries the hold as well: the server holds a suspended node like
// any paused one.
func TestLeavingASuspendNeverOpensTheQueue(t *testing.T) {
	for _, mode := range []string{store.WorkerPauseGraceful, store.WorkerPauseImmediate} {
		for _, failure := range []string{"set fails", "answer lost"} {
			t.Run(mode+" "+failure, func(t *testing.T) {
				m1 := newQueueWorker()
				e := pauseEngine(t, m1, &mockTranscodeExecutor{})
				if mode == store.WorkerPauseGraceful {
					seedEncoding(t, e, "enc", "m1", "job-enc")
				} // immediate with nothing running releases the freeze
				pauseNode(t, e, "m1", store.WorkerPauseSuspend)
				if !m1.hold || !m1.suspend {
					t.Fatalf("a suspended node is held and frozen: hold=%v suspend=%v", m1.hold, m1.suspend)
				}

				// The worker lost its hold (restart with the record gone) and cannot
				// take it back right now.
				m1.hold = false
				if failure == "set fails" {
					m1.failHoldOn = true
				} else {
					m1.loseHoldReply = true
				}
				m1.opened = nil
				if _, err := e.SetNodePause(context.Background(), "m1", mode); err != nil {
					t.Fatal(err)
				}
				if failure == "set fails" && !m1.suspend {
					t.Fatal("the freeze was lifted although the hold is not in place")
				}
				if failure == "answer lost" && !m1.suspend {
					t.Fatal("the freeze was lifted on an unconfirmed hold")
				}
				if len(m1.opened) != 0 {
					t.Fatalf("the queue was open: %v", m1.opened)
				}

				// The worker answers again: the next sweep sets the hold, confirms it
				// and only then lifts the freeze.
				m1.failHoldOn = false
				e.syncWorkerHold(context.Background(), mustWorker(t, e, "m1"))
				if !m1.hold {
					t.Fatal("the hold must be set on the next sweep")
				}
				if len(m1.opened) != 0 {
					t.Fatalf("the queue was open: %v", m1.opened)
				}
				if m1.suspend {
					t.Fatalf("with the hold in place the freeze is lifted")
				}
			})
		}
	}
}

// A suspended node carries the worker's hold, so a finished encode is not
// published by "Try again" until the node resumes, as in a graceful pause. This
// pins that behaviour: the hold is on with the suspend and off after the resume.
func TestSuspendedNodeIsHeldLikeAnyPausedNode(t *testing.T) {
	m1 := newQueueWorker()
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	seedEncoding(t, e, "enc", "m1", "job-enc")
	pauseNode(t, e, "m1", store.WorkerPauseSuspend)
	if !m1.hold || !m1.suspend {
		t.Fatalf("hold=%v suspend=%v", m1.hold, m1.suspend)
	}
	pauseNode(t, e, "m1", "")
	if m1.hold || m1.suspend {
		t.Fatalf("resume lifts both: hold=%v suspend=%v", m1.hold, m1.suspend)
	}
	if len(m1.opened) == 0 {
		t.Fatal("after the resume the queue is open")
	}
}
