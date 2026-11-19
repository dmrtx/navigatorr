package action

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// readerAnswers is every answer the pause readers give for the nodes, so two
// moments can be compared.
func readerAnswers(e *Engine) map[string]any {
	out := map[string]any{}
	for _, n := range []string{"m1", "m4"} {
		held, reason := e.pauseHold(n)
		intent, ok := e.workerSuspendIntent(n)
		out[n] = []any{e.workerPaused(n), held, reason, e.NodeHeldBy(n), intent, ok}
	}
	pauses, err := e.workerPauses()
	out["pauses"] = []any{len(pauses), err}
	out["all"] = e.allWorkersPaused()
	statuses := e.NodePauseStatuses(context.Background())
	for n, st := range statuses {
		st.CheckedAt = time.Time{}
		out["status-"+n] = st
	}
	out["stored"] = len(e.NodePauseStatesStored(context.Background()))
	return out
}

// A running build record is a record only: none of the readers of the pause
// view (hold, dispatch gate, suspend intent, statuses, sweep) answers
// differently, and no worker is touched.
func TestRunningBuildRecordChangesNoPauseReader(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, _ := buildEngine(t, m1)
	seedEncoding(t, e, "enc", "m1", "job-enc")

	before := readerAnswers(e)
	b := startBuild(t, e, "m1", store.WorkerPauseImmediate, 120)
	if b.Status != store.BuildRunning || b.HoldsTranscodeNode {
		t.Fatalf("%+v", b)
	}
	tick(e)
	e.sweepPausedNodes(context.Background())
	after := readerAnswers(e)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("a build record changed a pause answer:\nbefore %v\nafter  %v", before, after)
	}
	if m1.suspend || m1.cancelCalls != 0 {
		t.Fatalf("no worker was touched: suspend=%v cancels=%d", m1.suspend, m1.cancelCalls)
	}
	if st := e.NodePauseStatuses(context.Background())["m1"]; st.Paused || st.HeldBy != "" {
		t.Fatalf("%+v", st)
	}
}

// A low priority create comes back running: the server never queues it.
func TestLowBuildCreateReturnsRunning(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	seedEncoding(t, e, "enc", "m1", "job-enc")
	b := lowBuild(t, e, "m1", 120)
	if b.Status != store.BuildRunning || b.Priority != store.BuildPriorityLow {
		t.Fatalf("%+v", b)
	}
}

func TestBuildsPausedCreateIs409Code(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	pauseBuilds(t, e, "m1", true)
	_, err := createBuild(e, "m1", "")
	var refused *BuildsPausedError
	if !errors.As(err, &refused) {
		t.Fatalf("%v", err)
	}
}

// A node target must come from the node's own lease; any other target is a
// plain record.
func TestNodeTargetRequiresTheNodeLease(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	_, err := e.CreateBuild(context.Background(), BuildInput{Target: "m1", Source: "ci-tool", Title: "run"})
	var need *NodeLeaseRequiredError
	if !errors.As(err, &need) || need.Node != "m1" {
		t.Fatalf("a node target without via: %v", err)
	}
	if list, _ := e.ListBuilds(store.BuildFilter{}); len(list) != 0 {
		t.Fatalf("nothing recorded: %+v", list)
	}
	if _, err := e.CreateBuild(context.Background(), BuildInput{Target: "m1", Source: "ci-tool", Title: "run", Via: BuildViaNodeLease}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBuild(context.Background(), BuildInput{Target: "laptop", Source: "ci-tool", Title: "run"}); err != nil {
		t.Fatalf("a non-node target is a plain record: %v", err)
	}
}
