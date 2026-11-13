package tools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/mark3labs/mcp-go/server"
)

func TestWorkerPauseToolTogglesTheBuildsPause(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "builds.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{
		{Name: "node-a", Executor: &observedHealthWorker{}},
		{Name: "node-b", Executor: &observedHealthWorker{}, Podcast: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := action.NewEngine(action.EngineDeps{Store: st, Workers: set, DrainSettle: -1})
	s := server.NewMCPServer("test", "0")
	registerWorkerHealth(s, engine)
	registerWorkerPause(s, engine)

	read := func(args map[string]any) map[string]transcode.NodePauseStatus {
		t.Helper()
		res := callTool(t, s, "worker_pause", args)
		if res.IsError {
			t.Fatal(resultText(t, res))
		}
		var out struct {
			Nodes []transcode.NodePauseStatus `json:"nodes"`
		}
		if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil {
			t.Fatal(err)
		}
		m := map[string]transcode.NodePauseStatus{}
		for _, n := range out.Nodes {
			m[n.Node] = n
		}
		return m
	}

	if got := read(map[string]any{"node": "node-a", "builds_paused": true})["node-a"]; !got.BuildsPaused || got.Paused {
		t.Fatalf("the tool pauses builds only: %+v", got)
	}
	_, err = engine.CreateBuild(context.Background(), action.BuildInput{Target: "node-a", Source: "ci", Title: "run"})
	var refused *action.BuildsPausedError
	if !errors.As(err, &refused) {
		t.Fatalf("a build on the paused node is refused: %v", err)
	}
	// The transcode pause is independent, and a read with no arguments shows both.
	if got := read(map[string]any{"node": "node-a", "mode": "graceful"})["node-a"]; !got.Paused || !got.BuildsPaused || got.Mode != "graceful" {
		t.Fatalf("the transcode pause leaves builds paused: %+v", got)
	}
	all := read(map[string]any{})
	if !all["node-a"].BuildsPaused || all["node-b"].BuildsPaused {
		t.Fatalf("only node-a has builds paused: %+v", all)
	}
	if got := read(map[string]any{"node": "node-a", "mode": "off"})["node-a"]; got.Paused || !got.BuildsPaused {
		t.Fatalf("a transcode resume leaves builds paused: %+v", got)
	}
	if got := read(map[string]any{"node": "node-a", "builds_paused": false})["node-a"]; got.BuildsPaused {
		t.Fatalf("resume: %+v", got)
	}
	if _, err := engine.CreateBuild(context.Background(), action.BuildInput{Target: "node-a", Source: "ci", Title: "run"}); err != nil {
		t.Fatal("resumed", err)
	}

	for _, bad := range []map[string]any{
		{"builds_paused": true},
		{"node": "node-a", "mode": "graceful", "builds_paused": true},
		{"node": "nowhere", "builds_paused": true},
	} {
		if res := callTool(t, s, "worker_pause", bad); !res.IsError {
			t.Fatalf("must be refused: %+v", bad)
		}
	}

	// worker_health carries the flag per node.
	read(map[string]any{"node": "node-b", "builds_paused": true})
	var health struct {
		Workers []struct {
			Name         string `json:"name"`
			BuildsPaused bool   `json:"builds_paused"`
		} `json:"workers"`
	}
	if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "worker_health", map[string]any{}))), &health); err != nil {
		t.Fatal(err)
	}
	flags := map[string]bool{}
	for _, w := range health.Workers {
		flags[w.Name] = w.BuildsPaused
	}
	if !flags["node-b"] || flags["node-a"] {
		t.Fatalf("worker_health: %+v", flags)
	}
}
