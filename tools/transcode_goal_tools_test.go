package tools

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// The goal change tool carries the media engine choice with smaller files and
// refuses it for the other goals.
func TestGoalChangeToolCarriesTheMediaEngineChoice(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "goal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateActionInstance(store.ActionInstance{ID: "goal-batch", ActionName: "transcode_batch", Status: "completed", InputsJSON: `{"mode":"quality"}`, OutputsJSON: "{}", StateJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "goal-batch", ItemKey: "file", FilePath: "/media/one.mkv", DisplayLabel: "One", Status: "failed", Decision: "transcode"}); err != nil {
		t.Fatal(err)
	}
	e := action.NewEngine(action.EngineDeps{Store: st})
	s := server.NewMCPServer("test", "1")
	registerTranscodeGoalTool(s, e)
	preview := callTool(t, s, "transcode_goal_change", map[string]any{"id": "goal-batch"})
	var read struct {
		SelectionVersion string `json:"selection_version"`
	}
	if text := resultText(t, preview); json.Unmarshal([]byte(text), &read) != nil || read.SelectionVersion == "" {
		t.Fatal("preview without a version", text)
	}
	call := func(key string, extra map[string]any) *mcp.CallToolResult {
		args := map[string]any{"id": "goal-batch", "selection_version": read.SelectionVersion, "idempotency_key": key}
		for k, v := range extra {
			args[k] = v
		}
		return callTool(t, s, "transcode_goal_change", args)
	}
	for name, extra := range map[string]map[string]any{
		"quality with the engine":   {"mode": "quality", "allow_hardware": true},
		"quality with a margin":     {"mode": "quality", "hardware_margin_percent": float64(20)},
		"size, margin too large":    {"mode": "size", "hardware_margin_percent": float64(60)},
		"size, margin and off":      {"mode": "size", "allow_hardware": false, "hardware_margin_percent": float64(20)},
		"size, engine not a bool":   {"mode": "size", "allow_hardware": "no"},
		"size, margin not a number": {"mode": "size", "hardware_margin_percent": "20"},
	} {
		if res := call("refused-"+strings.ReplaceAll(name, " ", "-"), extra); !res.IsError {
			t.Fatalf("%s: accepted", name)
		}
	}
	res := call("engine-off", map[string]any{"mode": "size", "allow_hardware": false})
	var receipt struct {
		CommandID string `json:"command_id"`
	}
	if res.IsError || json.Unmarshal([]byte(resultText(t, res)), &receipt) != nil || receipt.CommandID == "" {
		t.Fatal("goal change refused", resultText(t, res))
	}
	command, _ := st.GetActionInstance(receipt.CommandID)
	if command == nil || !strings.Contains(command.InputsJSON, `"allow_hardware":false`) {
		t.Fatalf("engine choice not carried: %+v", command)
	}
}
