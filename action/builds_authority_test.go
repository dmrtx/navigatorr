package action

import (
	"context"
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
	if b.Status != "running" {
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
