package action

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

// A heartbeat caught between its store update and its append must hold the
// finish back: the finish returns only after the lines landed, so nothing is
// added to the output once it has returned. The test is deterministic: the seam
// says "in the gap", the finish starts, and the seam then waits for the finish
// to return or for 50 ms. Without the engine's lock the finish returns inside the
// gap, the line lands afterwards and the two snapshots differ every time; with
// it the wait times out, the line lands and the finish goes on.
func TestBuildFinishWaitsForAHeartbeatInTheGap(t *testing.T) {
	e, _ := buildEngine(t, &mockTranscodeExecutor{})
	b := startBuild(t, e, "laptop", "", 0)

	inGap := make(chan struct{})
	finishReturned := make(chan struct{})
	var timedOut atomic.Bool
	var entered atomic.Bool
	gap := func() {
		if !entered.CompareAndSwap(false, true) {
			return
		}
		close(inGap)
		select {
		case <-finishReturned:
		case <-time.After(50 * time.Millisecond):
			timedOut.Store(true)
		}
	}
	buildHeartbeatGap.Store(&gap)
	t.Cleanup(func() { buildHeartbeatGap.Store(nil) })

	heartbeat := make(chan error, 1)
	go func() {
		_, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, "in the gap")
		heartbeat <- err
	}()
	<-inGap

	var atEnd []string
	finished := make(chan error, 1)
	go func() {
		// build_cancel and POST /finish both end a build through FinishBuild.
		_, _, err := e.FinishBuild(context.Background(), b.ID, store.BuildCancelled, nil)
		_, atEnd, _, _ = e.BuildLog(b.ID)
		close(finishReturned)
		finished <- err
	}()
	if err := <-heartbeat; err != nil {
		t.Fatalf("the heartbeat was accepted before the finish: %v", err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if !timedOut.Load() {
		t.Fatal("the finish returned while a heartbeat was between its update and its append: the lock is missing")
	}
	_, lines, _, err := e.BuildLog(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lines, []string{"in the gap"}) || !reflect.DeepEqual(atEnd, lines) {
		t.Fatalf("lines landed after the finish returned: %q at the end, %q now", atEnd, lines)
	}
	// And the build stays closed to more.
	if _, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, "late"); err == nil {
		t.Fatal("a heartbeat after the finish is refused")
	}
}
