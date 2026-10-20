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
		"budget with quality":       {"mode": "quality", "size_budget_percent": float64(30)},
		"budget percent too small":  {"mode": "size", "size_budget_percent": float64(4)},
		"budget percent too large":  {"mode": "size", "size_budget_percent": float64(86)},
		"budget percent not number": {"mode": "size", "size_budget_percent": "30"},
		"budget bytes too small":    {"mode": "size", "size_budget_bytes": float64(1000)},
		"budget bytes fractional":   {"mode": "size", "size_budget_bytes": 60000000.5},
		"siblings not a string":     {"mode": "size", "size_budget_source": float64(1)},
		"multiplier not a number":   {"mode": "size", "size_budget_source": "siblings", "size_budget_sibling_multiplier": "1.5"},
		"budget percent and bytes":  {"mode": "size", "size_budget_percent": float64(30), "size_budget_bytes": float64(60000000)},
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
	res = call("budget-percent", map[string]any{"mode": "size", "size_budget_percent": float64(33)})
	if res.IsError || json.Unmarshal([]byte(resultText(t, res)), &receipt) != nil || receipt.CommandID == "" {
		t.Fatal("goal change with a size budget refused", resultText(t, res))
	}
	if command, _ = st.GetActionInstance(receipt.CommandID); command == nil || !strings.Contains(command.InputsJSON, `"size_budget_percent":33`) {
		t.Fatalf("size budget not carried: %+v", command)
	}
	res = call("budget-bytes", map[string]any{"mode": "size", "size_budget_bytes": float64(2000000000)})
	if res.IsError || json.Unmarshal([]byte(resultText(t, res)), &receipt) != nil || receipt.CommandID == "" {
		t.Fatal("goal change with a byte budget refused", resultText(t, res))
	}
	if command, _ = st.GetActionInstance(receipt.CommandID); command == nil || !strings.Contains(command.InputsJSON, `"size_budget_bytes":2000000000`) {
		t.Fatalf("byte budget not carried: %+v", command)
	}
	// Leaving the choice out is the documented default, not the frozen one: the
	// command names no engine choice and the policy freeze applies its default.
	res = call("engine-default", map[string]any{"mode": "size"})
	if res.IsError || json.Unmarshal([]byte(resultText(t, res)), &receipt) != nil || receipt.CommandID == "" {
		t.Fatal("goal change without an engine choice refused", resultText(t, res))
	}
	if command, _ = st.GetActionInstance(receipt.CommandID); command == nil || strings.Contains(command.InputsJSON, "allow_hardware") || strings.Contains(command.InputsJSON, "hardware_margin_percent") {
		t.Fatalf("an omitted engine choice was filled in: %+v", command)
	}
}

// The read-only preview shows the media engine choice each selected file was
// frozen with, from its own goal-change policy when it has one, else the
// batch's, so a caller sees it before choosing a goal.
func TestGoalChangePreviewShowsTheFrozenMediaEngineChoice(t *testing.T) {
	preview := func(t *testing.T, inputs, state string, keys ...string) map[string]any {
		t.Helper()
		st, err := store.Open(filepath.Join(t.TempDir(), "goal.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if err := st.CreateActionInstance(store.ActionInstance{ID: "goal-batch", ActionName: "transcode_batch", Status: "completed", InputsJSON: inputs, OutputsJSON: "{}", StateJSON: state}); err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "goal-batch", ItemKey: key, FilePath: "/media/" + key + ".mkv", DisplayLabel: key, Status: "failed", Decision: "transcode"}); err != nil {
				t.Fatal(err)
			}
		}
		s := server.NewMCPServer("test", "1")
		registerTranscodeGoalTool(s, action.NewEngine(action.EngineDeps{Store: st}))
		res := callTool(t, s, "transcode_goal_change", map[string]any{"id": "goal-batch"})
		var out map[string]any
		if res.IsError || json.Unmarshal([]byte(resultText(t, res)), &out) != nil {
			t.Fatal("preview failed", resultText(t, res))
		}
		return out
	}
	policy := func(rules string) string {
		return `{"mode":"size","size":{` + rules + `}}`
	}
	engineOn := policy(`"hardware_search":{"margin_percent":30}`)
	engineOff := policy(`"search_extension":null`)

	t.Run("an on choice with its margin", func(t *testing.T) {
		got := preview(t, `{"mode":"size"}`, `{"mode_policy":`+engineOn+`}`, "a", "b")
		if got["allow_hardware"] != true || got["hardware_margin_percent"] != float64(30) || got["hardware_mixed"] != false {
			t.Fatalf("frozen choice not shown: %v", got)
		}
	})
	t.Run("a batch frozen as x265 only shows off, not the default", func(t *testing.T) {
		got := preview(t, `{"mode":"size"}`, `{"mode_policy":`+engineOff+`}`, "a")
		if got["allow_hardware"] != false || got["hardware_mixed"] != false {
			t.Fatalf("x265-only batch shown as on: %v", got)
		}
		if _, has := got["hardware_margin_percent"]; has && got["hardware_margin_percent"] != nil {
			t.Fatalf("margin shown with the engine off: %v", got)
		}
	})
	t.Run("a file with its own goal change policy overrides the batch", func(t *testing.T) {
		state := `{"mode_policy":` + engineOff + `,"batch_item_settings":{"b":{"mode":"size","mode_policy":` + engineOn + `}}}`
		got := preview(t, `{"mode":"size"}`, state, "a", "b")
		if got["hardware_mixed"] != true {
			t.Fatalf("files frozen differently not reported as mixed: %v", got)
		}
		got = preview(t, `{"mode":"size"}`, state, "b")
		if got["allow_hardware"] != true || got["hardware_margin_percent"] != float64(30) || got["hardware_mixed"] != false {
			t.Fatalf("the file's own policy was not read: %v", got)
		}
	})
	t.Run("other goals and unfrozen batches show nothing", func(t *testing.T) {
		for name, state := range map[string]string{
			"quality":  `{"mode_policy":{"mode":"quality"}}`,
			"unfrozen": `{}`,
		} {
			got := preview(t, `{"mode":"quality"}`, state, "a")
			for _, key := range []string{"allow_hardware", "hardware_margin_percent", "hardware_mixed"} {
				if _, has := got[key]; has {
					t.Fatalf("%s: %s shown: %v", name, key, got)
				}
			}
		}
	})
}
