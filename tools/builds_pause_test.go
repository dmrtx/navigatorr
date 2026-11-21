package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/internal/nodeswire"
	"github.com/jakenesler/navigatorr/nodes"
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
	reg, err := nodes.Open(nodes.Options{Path: filepath.Join(t.TempDir(), "nodes.db"), Topology: nodeswire.Topology(set), Transcode: nodeswire.TranscodeView(engine)})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	nodeswire.Wire(engine, reg)
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
	_, err = reg.CreateBuild(context.Background(), nodes.BuildInput{Target: "node-a", Via: nodes.BuildViaNodeLease, Source: "ci", Title: "run"})
	var refused *nodes.BuildsPausedError
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
	if _, err := reg.CreateBuild(context.Background(), nodes.BuildInput{Target: "node-a", Via: nodes.BuildViaNodeLease, Source: "ci", Title: "run"}); err != nil {
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

func TestWorkerPauseToolRefusesMixedOrMistypedBuildsRequests(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "builds.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: "node-a", Executor: &observedHealthWorker{}, Podcast: true}})
	if err != nil {
		t.Fatal(err)
	}
	engine := action.NewEngine(action.EngineDeps{Store: st, Workers: set, DrainSettle: -1})
	reg, err := nodes.Open(nodes.Options{Path: filepath.Join(t.TempDir(), "nodes.db"), Topology: nodeswire.Topology(set), Transcode: nodeswire.TranscodeView(engine)})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	nodeswire.Wire(engine, reg)
	s := server.NewMCPServer("test", "0")
	registerWorkerPause(s, engine)
	for _, bad := range []map[string]any{
		{"node": "node-a", "mode": "", "builds_paused": true},
		{"node": "node-a", "mode": "off", "builds_paused": false},
		{"node": "node-a", "builds_paused": "true"},
		{"node": "node-a", "builds_paused": nil},
		{"node": "node-a", "builds_paused": float64(1)},
	} {
		if res := callTool(t, s, "worker_pause", bad); !res.IsError {
			t.Fatalf("must be an error, not a silent read: %+v", bad)
		}
	}
	if engine.NodeBuildsPaused("node-a") {
		t.Fatal("nothing changed")
	}
}

// unavailableRecords is a builds registry that does not answer.
type unavailableRecords struct{}

func (unavailableRecords) NodeBuildRecords(string) []transcode.NodeBuild { return nil }
func (unavailableRecords) BuildsPauses() (map[string]string, error) {
	return nil, fmt.Errorf("%w: nothing answered", action.ErrBuildRecordsUnavailable)
}
func (unavailableRecords) SetBuildsPause(context.Context, string, bool) error {
	return errors.New("builds registry unreachable; nothing changed")
}
func (unavailableRecords) BuildsPaused(string) (bool, error) {
	return false, fmt.Errorf("%w: nothing answered", action.ErrBuildRecordsUnavailable)
}

// worker_health and worker_pause show an unreachable registry as unavailable:
// not as paused and not as "no builds, not paused".
func TestWorkerToolsShowAnUnavailableBuildsRegistry(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "builds.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: "node-a", Executor: &observedHealthWorker{}, Podcast: true}})
	if err != nil {
		t.Fatal(err)
	}
	engine := action.NewEngine(action.EngineDeps{Store: st, Workers: set, DrainSettle: -1})
	engine.SetBuildRecords(unavailableRecords{})
	s := server.NewMCPServer("test", "0")
	registerWorkerHealth(s, engine)
	registerWorkerPause(s, engine)

	var health transcode.WorkerObservation
	if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "worker_health", map[string]any{}))), &health); err != nil {
		t.Fatal(err)
	}
	if !health.BuildsUnavailable || health.BuildsPaused || len(health.Workers) != 1 || !health.Workers[0].BuildsUnavailable || health.Workers[0].BuildsPaused {
		t.Fatalf("worker_health: %+v", health)
	}
	var pause struct {
		Nodes []transcode.NodePauseStatus `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "worker_pause", map[string]any{}))), &pause); err != nil {
		t.Fatal(err)
	}
	if len(pause.Nodes) != 1 || !pause.Nodes[0].BuildsUnavailable || pause.Nodes[0].BuildsPaused {
		t.Fatalf("worker_pause: %+v", pause)
	}
	// A write that fails says so and changes nothing.
	res := callTool(t, s, "worker_pause", map[string]any{"node": "node-a", "builds_paused": true})
	if !res.IsError || !strings.Contains(resultText(t, res), "nothing changed") {
		t.Fatalf("write: %v %s", res.IsError, resultText(t, res))
	}
}
