package action

import (
	"context"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// An encode frozen for a running build says which build, and the same build
// is in the structured answer; with no build the operator's pause and a
// freeze the server has no record of are told apart.
func TestSuspendedByNamesTheBuildThatHoldsTheNode(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))

	// Nothing holds m1: the worker froze it for something the server was not told.
	cause, why := e.suspendedBy("m1")
	if cause["kind"] != "worker" || cause["node"] != "m1" || !strings.Contains(why, "no record") {
		t.Fatalf("unknown freeze: %v %q", cause, why)
	}

	b := startBuild(t, e, "m1", "suspend", 0)
	cause, why = e.suspendedBy("m1")
	if cause["kind"] != "build" || cause["id"] != b.ID || cause["title"] != "integration run" || cause["source"] != "ci-tool" || cause["holder"] != "runner1" {
		t.Fatalf("a build holds m1: %v", cause)
	}
	if !strings.Contains(why, "integration run") || !strings.Contains(why, "ci-tool") || !strings.Contains(why, "when it ends") {
		t.Fatalf("the reason names the build: %q", why)
	}
	// Another node is not held by it.
	if other, _ := e.suspendedBy("m4"); other["kind"] != "worker" {
		t.Fatalf("m4 is not held by a build on m1: %v", other)
	}

	// The operator's pause wins over the build, as the node's hold does.
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
