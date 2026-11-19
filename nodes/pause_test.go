package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestBuildsPausedRefusesANewBuildOnThatNodeOnly(t *testing.T) {
	e, _ := buildSvc(t)
	if v := pauseBuilds(t, e, "nodeA", true); !v.BuildsPaused || v.BuildsPausedSince == "" {
		t.Fatalf("%+v", v)
	}
	_, err := createBuild(e, "nodeA", "")
	var refused *BuildsPausedError
	if !errors.As(err, &refused) || refused.Node != "nodeA" || err.Error() != "builds paused on nodeA" {
		t.Fatalf("a new build on the paused node is refused: %v", err)
	}
	// Nothing was created.
	if list, _ := e.ListBuilds(BuildFilter{}); len(list) != 0 {
		t.Fatalf("a refused build is not recorded: %+v", list)
	}
	// A low priority build is refused too, not left waiting.
	if _, err := createBuild(e, "nodeA", BuildPriorityLow); !errors.As(err, &refused) {
		t.Fatalf("a low build is refused: %v", err)
	}
	// Another node and a target that is not a node are unaffected.
	if _, err := createBuild(e, "nodeB", ""); err != nil {
		t.Fatal("another node still takes builds", err)
	}
	if _, err := createBuild(e, "laptop", ""); err != nil {
		t.Fatal("a non-node target still takes builds", err)
	}
	// A request that is invalid stays a validation error, not a refusal.
	var invalid *InvalidError
	if _, err := e.CreateBuild(context.Background(), BuildInput{Target: "nodeA"}); !errors.As(err, &invalid) {
		t.Fatalf("validation comes first: %v", err)
	}
	// Resume: the node takes builds again.
	if v := pauseBuilds(t, e, "nodeA", false); v.BuildsPaused {
		t.Fatalf("%+v", v)
	}
	if _, err := createBuild(e, "nodeA", ""); err != nil {
		t.Fatal("resumed", err)
	}
}

func TestBuildsPauseNeverTouchesBuildsAlreadyRegistered(t *testing.T) {
	e, _ := buildSvc(t)
	running, err := createBuild(e, "nodeA", "")
	if err != nil {
		t.Fatal(err)
	}
	low, err := createBuild(e, "nodeA", BuildPriorityLow)
	if err != nil || low.Status != BuildRunning {
		t.Fatal("a low build is running at once", low.Status, err)
	}
	status := func() map[string]string {
		out := map[string]string{}
		list, _ := e.ListBuilds(BuildFilter{})
		for _, b := range list {
			out[b.ID] = b.Status
		}
		return out
	}
	for _, paused := range []bool{true, false} {
		pauseBuilds(t, e, "nodeA", paused)
		got := status()
		if got[running.ID] != BuildRunning || got[low.ID] != BuildRunning {
			t.Fatalf("paused=%v changed a registered build: %+v", paused, got)
		}
		// The registered builds keep heartbeating while builds are paused.
		if _, err := e.HeartbeatBuild(context.Background(), running.ID, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	// With builds paused, the registered builds can still finish normally.
	pauseBuilds(t, e, "nodeA", true)
	finishBuild(t, e, running.ID, BuildSucceeded)
	if got := status(); got[low.ID] != BuildRunning || got[running.ID] != BuildSucceeded {
		t.Fatalf("registered builds finish as usual: %+v", got)
	}
}

func TestBuildsPauseUnknownNodeAndRepeat(t *testing.T) {
	e, _ := buildSvc(t)
	var unknown *UnknownNodeError
	if err := e.SetBuildsPause(context.Background(), "nowhere", true); !errors.As(err, &unknown) {
		t.Fatalf("an unknown node: %v", err)
	}
	first := pauseBuilds(t, e, "nodeB", true)
	again := pauseBuilds(t, e, "nodeB", true)
	if first.BuildsPausedSince == "" || again.BuildsPausedSince != first.BuildsPausedSince {
		t.Fatalf("pausing twice keeps the first start: %+v %+v", first, again)
	}
	if p, _ := e.BuildsPaused("nodeB"); !p {
		t.Fatal("BuildsPaused")
	}
	if p, _ := e.BuildsPaused("nodeA"); p {
		t.Fatal("nodeA is not paused")
	}
}

// The node view carries the transcode state it reads through the interface and
// never writes to it.
func TestNodeViewShowsTheTranscodeStateItIsGiven(t *testing.T) {
	e, _ := buildSvc(t)
	e.tv = fakeTranscode{"nodeA": {Paused: true, Mode: "graceful", HeldBy: "operator"}}
	v, err := e.Node(context.Background(), "nodeA")
	if err != nil || v.Transcode == nil || !v.Transcode.Paused || v.Transcode.HeldBy != "operator" {
		t.Fatalf("%+v %v", v, err)
	}
	if v, _ := e.Node(context.Background(), "nodeB"); v.Transcode != nil {
		t.Fatalf("no state reported for nodeB: %+v", v)
	}
	all, _ := e.Nodes(context.Background())
	if len(all) != 2 || all[0].Name != "nodeA" || all[1].Name != "nodeB" {
		t.Fatalf("%+v", all)
	}
}

// With the build-pause state unreadable, the view says paused, as the create
// path refuses.
func TestBuildsPauseStatusFailsClosedWhenTheStateCannotBeRead(t *testing.T) {
	e, _ := buildSvc(t)
	if p, _ := e.BuildsPaused("nodeA"); p {
		t.Fatal("readable and unpaused")
	}
	if err := e.reg.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.BuildsPaused("nodeA"); err == nil {
		t.Fatal("an unreadable state is an error")
	}
	if _, err := createBuild(e, "nodeA", ""); err == nil {
		t.Fatal("a create fails closed")
	}
	all, _ := e.Nodes(context.Background())
	if !all[0].BuildsPaused || !all[1].BuildsPaused {
		t.Fatalf("the view must fail closed: %+v", all)
	}
}

// An unpaused node's JSON has no pause start, so a quiet node stays small.
func TestUnpausedNodeViewHasNoPauseStart(t *testing.T) {
	e, _ := buildSvc(t)
	v, _ := e.Node(context.Background(), "nodeA")
	data, _ := json.Marshal(v)
	if strings.Contains(string(data), "builds_paused_since") || !strings.Contains(string(data), `"builds_paused":false`) {
		t.Fatalf("%s", data)
	}
	v = pauseBuilds(t, e, "nodeA", true)
	data, _ = json.Marshal(v)
	if !strings.Contains(string(data), `"builds_paused":true`) || !strings.Contains(string(data), `"builds_paused_since"`) {
		t.Fatalf("%s", data)
	}
}
