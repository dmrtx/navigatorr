package action

import (
	"context"
	"errors"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

func pauseBuilds(t *testing.T, e *Engine, node string, paused bool) transcode.NodePauseStatus {
	t.Helper()
	st, err := e.SetNodeBuildsPause(context.Background(), node, paused)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func createBuild(e *Engine, target, priority string) (store.Build, error) {
	return e.CreateBuild(context.Background(), BuildInput{Target: target, Source: "ci-tool", Title: "run", Priority: priority})
}

func TestBuildsPausedRefusesANewBuildOnThatNodeOnly(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	if st := pauseBuilds(t, e, "m1", true); !st.BuildsPaused || st.BuildsPausedSince == "" || st.Paused {
		t.Fatalf("the builds pause is not a transcode pause: %+v", st)
	}
	_, err := createBuild(e, "m1", "")
	var refused *BuildsPausedError
	if !errors.As(err, &refused) || refused.Node != "m1" || err.Error() != "builds paused on m1" {
		t.Fatalf("a new build on the paused node is refused: %v", err)
	}
	// Nothing was created and no hold was taken.
	if list, _ := e.ListBuilds(store.BuildFilter{}); len(list) != 0 {
		t.Fatalf("a refused build is not recorded: %+v", list)
	}
	if e.workerPaused("m1") {
		t.Fatal("a refused build takes no hold")
	}
	// A low priority build is refused too, not left waiting.
	if _, err := createBuild(e, "m1", store.BuildPriorityLow); !errors.As(err, &refused) {
		t.Fatalf("a low build is refused: %v", err)
	}
	// Another node and a target that is not a node are unaffected.
	if _, err := createBuild(e, "m4", ""); err != nil {
		t.Fatal("another node still takes builds", err)
	}
	if b, err := createBuild(e, "laptop", ""); err != nil || b.HoldsTranscodeNode {
		t.Fatal("a non-node target still takes builds", err)
	}
	// A request that is invalid stays a validation error, not a refusal.
	var invalid *BuildInvalidError
	if _, err := e.CreateBuild(context.Background(), BuildInput{Target: "m1"}); !errors.As(err, &invalid) {
		t.Fatalf("validation comes first: %v", err)
	}
	// Resume: the node takes builds again.
	if st := pauseBuilds(t, e, "m1", false); st.BuildsPaused {
		t.Fatalf("%+v", st)
	}
	if _, err := createBuild(e, "m1", ""); err != nil {
		t.Fatal("resumed", err)
	}
}

func TestBuildsPauseNeverTouchesBuildsAlreadyRegistered(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	running, err := createBuild(e, "m1", "")
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := createBuild(e, "m1", store.BuildPriorityLow)
	if err != nil || waiting.Status != store.BuildWaiting {
		t.Fatal("a low build waits behind the running one", waiting.Status, err)
	}
	status := func() map[string]string {
		out := map[string]string{}
		list, _ := e.ListBuilds(store.BuildFilter{})
		for _, b := range list {
			out[b.ID] = b.Status
		}
		return out
	}
	for _, paused := range []bool{true, false} {
		st := pauseBuilds(t, e, "m1", paused)
		got := status()
		if got[running.ID] != store.BuildRunning || got[waiting.ID] != store.BuildWaiting {
			t.Fatalf("paused=%v changed a registered build: %+v", paused, got)
		}
		if len(st.Builds) != 2 || !e.workerPaused("m1") {
			t.Fatalf("paused=%v: the running build still holds the node: %+v", paused, st)
		}
		// The registered builds keep heartbeating while builds are paused.
		if _, err := e.HeartbeatBuild(context.Background(), running.ID, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	// With builds paused, the waiting build already registered still starts
	// when the node frees up, and the running one can finish normally.
	pauseBuilds(t, e, "m1", true)
	finishBuild(t, e, running.ID, store.BuildSucceeded)
	if got := status(); got[waiting.ID] != store.BuildRunning || got[running.ID] != store.BuildSucceeded {
		t.Fatalf("a registered waiting build is promoted as usual: %+v", got)
	}
}

func TestBuildsPauseAndTranscodePauseAreIndependent(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	flags := func() (bool, bool) {
		st := e.NodePauseStatuses(context.Background())["m1"]
		return st.Paused, st.BuildsPaused
	}
	for _, c := range []struct{ transcode, builds bool }{{false, false}, {true, false}, {true, true}, {false, true}, {false, false}} {
		if c.transcode {
			pauseNode(t, e, "m1", store.WorkerPauseGraceful)
		} else {
			pauseNode(t, e, "m1", "")
		}
		pauseBuilds(t, e, "m1", c.builds)
		if tp, bp := flags(); tp != c.transcode || bp != c.builds {
			t.Fatalf("want transcode=%v builds=%v, got %v %v", c.transcode, c.builds, tp, bp)
		}
	}
	// A transcode pause alone refuses nothing: builds register on a paused node.
	pauseNode(t, e, "m1", store.WorkerPauseGraceful)
	if _, err := createBuild(e, "m1", ""); err != nil {
		t.Fatal("a transcode pause does not pause builds", err)
	}
	// And the builds pause does not stop transcode (no operator row).
	pauseNode(t, e, "m1", "")
	pauseBuilds(t, e, "m1", true)
	if pauses, _ := e.operatorPauses(); len(pauses) != 0 {
		t.Fatalf("a builds pause writes no transcode pause: %+v", pauses)
	}
}

func TestBuildsPauseUnknownNodeAndObservationFlag(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	var unknown *transcode.WorkerNotConfiguredError
	if _, err := e.SetNodeBuildsPause(context.Background(), "nowhere", true); !errors.As(err, &unknown) {
		t.Fatalf("an unknown node: %v", err)
	}
	pauseBuilds(t, e, "m4", true)
	byName := map[string]bool{}
	for _, w := range e.WorkerObservation(context.Background()).Workers {
		byName[w.Name] = w.BuildsPaused
	}
	if !byName["m4"] || byName["m1"] {
		t.Fatalf("worker_health carries the flag per node: %+v", byName)
	}
	if !e.NodeBuildsPaused("m4") || e.NodeBuildsPaused("m1") {
		t.Fatal("NodeBuildsPaused")
	}
}
