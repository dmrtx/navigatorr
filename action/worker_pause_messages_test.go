package action

import (
	"context"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// The sentences the node cards show are part of the product: pin them for
// every pause state, with and without a running build on the node.
func TestNodePauseMessages(t *testing.T) {
	const (
		notPaused = "Transcode not paused. The node takes new jobs."
		idle      = "Transcode paused and idle: no encode runs on this node."
		pausing   = "Transcode paused: 1 job(s) still finishing. Nothing new starts here."
		suspended = "Transcode suspended: 1 encode(s) frozen where they were; they continue when the node is resumed."
		busy      = "Busy with 1 running build(s). "
	)
	message := func(e *Engine) string {
		return e.NodePauseStatuses(context.Background())["m1"].Message
	}

	t.Run("not paused", func(t *testing.T) {
		e := pauseEngine(t, &mockTranscodeExecutor{}, &mockTranscodeExecutor{})
		if got := message(e); got != notPaused {
			t.Fatal(got)
		}
	})
	t.Run("paused and idle", func(t *testing.T) {
		e := pauseEngine(t, &mockTranscodeExecutor{}, &mockTranscodeExecutor{})
		pauseNode(t, e, "m1", store.WorkerPauseGraceful)
		if got := message(e); got != idle {
			t.Fatal(got)
		}
		startBuild(t, e, "m1", "", 0)
		// The operator's pause wins, the build is listed beside it: no build prefix.
		if got := message(e); got != idle {
			t.Fatal(got)
		}
	})
	t.Run("pausing", func(t *testing.T) {
		m1 := &mockTranscodeExecutor{statusFunc: jobStatusFunc(transcode.StatusRunning)}
		e := pauseEngine(t, m1, &mockTranscodeExecutor{})
		seedEncoding(t, e, "enc", "m1", "job-enc")
		pauseNode(t, e, "m1", store.WorkerPauseGraceful)
		if got := message(e); got != pausing {
			t.Fatal(got)
		}
		startBuild(t, e, "m1", "", 0)
		if got := message(e); got != pausing {
			t.Fatal(got)
		}
	})
	t.Run("suspended", func(t *testing.T) {
		m1 := newSuspendingM1(transcode.StatusRunning)
		m1.active = 1
		e := pauseEngine(t, m1, &mockTranscodeExecutor{})
		seedEncoding(t, e, "enc", "m1", "job-enc")
		pauseNode(t, e, "m1", store.WorkerPauseSuspend)
		e.sweepPausedNodes(context.Background())
		got := message(e)
		if got != suspended {
			t.Fatal(got)
		}
		// With a build running the sentence still makes no claim about the CPU.
		startBuild(t, e, "m1", "", 0)
		if got := message(e); got != suspended {
			t.Fatal(got)
		}
	})
	t.Run("held by a build alone", func(t *testing.T) {
		e := pauseEngine(t, &mockTranscodeExecutor{}, &mockTranscodeExecutor{})
		startBuild(t, e, "m1", "graceful", 0)
		if got := message(e); got != busy+idle {
			t.Fatal(got)
		}
	})
}
