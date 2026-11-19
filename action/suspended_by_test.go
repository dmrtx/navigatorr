package action

import (
	"context"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// An encode frozen for a lease says which one, from the list its worker
// reported; with no lease the operator's pause and a freeze the worker did not
// explain are told apart.
func TestSuspendedByNamesTheLeaseTheWorkerReported(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))

	cause, why := e.suspendedBy("m1")
	if cause["kind"] != "worker" || cause["node"] != "m1" || !strings.Contains(why, "no record") {
		t.Fatalf("unknown freeze: %v %q", cause, why)
	}

	// A running build record alone says nothing: only the worker's list counts.
	startBuild(t, e, "m1", "suspend", 0)
	if cause, _ = e.suspendedBy("m1"); cause["kind"] != "worker" {
		t.Fatalf("a record is not a lease: %v", cause)
	}

	e.workerStateFor("m1").setLeases([]transcode.WorkerLease{
		{ID: "l-2", Title: "later", Source: "other", Status: "running", StartedAt: "2026-11-13T10:05:00Z"},
		{ID: "l-1", Title: "integration run", Source: "ci-tool", Status: "running", StartedAt: "2026-11-13T10:00:00Z"},
		{ID: "l-3", Title: "waiting one", Source: "x", Status: "waiting"},
	})
	cause, why = e.suspendedBy("m1")
	if cause["kind"] != "build" || cause["id"] != "l-1" || cause["title"] != "integration run" || cause["source"] != "ci-tool" {
		t.Fatalf("the longest running lease holds m1: %v", cause)
	}
	if !strings.Contains(why, "integration run") || !strings.Contains(why, "ci-tool") || !strings.Contains(why, "when it ends") {
		t.Fatalf("the reason names the lease: %q", why)
	}
	if other, _ := e.suspendedBy("m4"); other["kind"] != "worker" {
		t.Fatalf("m4 is not held by a lease on m1: %v", other)
	}

	if _, err := e.SetNodePause(context.Background(), "m1", "suspend"); err != nil {
		t.Fatal(err)
	}
	cause, why = e.suspendedBy("m1")
	if cause["kind"] != "operator" || !strings.Contains(why, "operator") {
		t.Fatalf("the operator's pause: %v %q", cause, why)
	}
}

func TestClipTitleKeepsAShortTitleAndCutsALongOne(t *testing.T) {
	if got := clipTitle("short"); got != "short" {
		t.Fatal(got)
	}
	long := strings.Repeat("é", 200)
	got := clipTitle(long)
	if n := len([]rune(got)); n != suspendedByTitleMax || !strings.HasSuffix(got, "…") {
		t.Fatalf("%d %q", n, got)
	}
}
