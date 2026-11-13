package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/mark3labs/mcp-go/server"
)

func TestBuildMCPToolsListReadAndCancel(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "builds.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{
		{Name: "m1", Executor: &observedHealthWorker{}},
		{Name: "m4", Executor: &observedHealthWorker{}, Podcast: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := action.NewEngine(action.EngineDeps{Store: st, Workers: set, DrainSettle: -1})
	s := server.NewMCPServer("test", "0")
	registerBuilds(s, engine)
	registerWorkerHealth(s, engine)

	onNode, err := engine.CreateBuild(context.Background(), action.BuildInput{Target: "m1", Source: "mdm-stack", Holder: "runner1", Kind: "gradle", Title: "unit run", PreemptMode: store.WorkerPauseGraceful})
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := engine.CreateBuild(context.Background(), action.BuildInput{Target: "laptop", Source: "mdm-stack", Title: "kit run"})
	if err != nil {
		t.Fatal(err)
	}

	var list struct {
		Builds []store.Build `json:"builds"`
	}
	if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "build_list", map[string]any{}))), &list); err != nil || len(list.Builds) != 2 {
		t.Fatal(err, list)
	}
	if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "build_list", map[string]any{"target": "m1", "status": "running", "source": "mdm-stack", "limit": float64(5)}))), &list); err != nil || len(list.Builds) != 1 || list.Builds[0].ID != onNode.ID || !list.Builds[0].HoldsTranscodeNode {
		t.Fatal(err, list)
	}
	if res := callTool(t, s, "build_list", map[string]any{"limit": float64(0)}); !res.IsError {
		t.Fatal("a limit below 1 must be refused")
	}

	var got store.Build
	if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "build_status", map[string]any{"id": elsewhere.ID}))), &got); err != nil || got.ID != elsewhere.ID || got.HoldsTranscodeNode || got.Status != store.BuildRunning {
		t.Fatal(err, got)
	}
	if res := callTool(t, s, "build_status", map[string]any{"id": "bld_missing"}); !res.IsError {
		t.Fatal("an unknown build is an error")
	}

	// worker_health lists the node's running builds.
	if text := resultText(t, callTool(t, s, "worker_health", map[string]any{})); !strings.Contains(text, onNode.ID) || strings.Contains(text, elsewhere.ID) {
		t.Fatalf("worker_health must list the node's builds only: %s", text)
	}

	// Cancelling frees the node on this side and is repeatable.
	if !engine.NodePauseStatuses(context.Background())["m1"].Paused {
		t.Fatal("the build holds m1")
	}
	for i := 0; i < 2; i++ {
		if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "build_cancel", map[string]any{"id": onNode.ID}))), &got); err != nil || got.Status != store.BuildCancelled || got.EndedAt == "" {
			t.Fatal(err, got)
		}
	}
	if engine.NodePauseStatuses(context.Background())["m1"].Paused {
		t.Fatal("cancelling frees the node")
	}
	if res := callTool(t, s, "build_cancel", map[string]any{"id": "bld_missing"}); !res.IsError {
		t.Fatal("an unknown build is an error")
	}
}
