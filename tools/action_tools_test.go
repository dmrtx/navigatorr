package tools

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

func TestActionToolsLifecycle(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "action_tool_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := &config.Config{}
	engine := action.NewEngine(action.EngineDeps{Store: st, Config: cfg})

	s := server.NewMCPServer("test", "0.0.0")
	registerActionTools(s, engine)

	// 1. action_run with validate_torrent and safe files
	runRes := callTool(t, s, "action_run", map[string]any{
		"action": "validate_torrent",
		"inputs": `{"files":["Movie.1080p.mkv","Movie.1080p.eng.srt"]}`,
	})
	txt := resultText(t, runRes)
	var resMap map[string]any
	if err := json.Unmarshal([]byte(txt), &resMap); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if resMap["status"] != "completed" {
		t.Fatalf("expected action_run to complete, got: %s", txt)
	}
	actID, ok := resMap["id"].(string)
	if !ok || actID == "" {
		t.Fatalf("missing action id in response: %v", resMap)
	}

	// 2. action_status
	statRes := callTool(t, s, "action_status", map[string]any{
		"id": actID,
	})
	statTxt := resultText(t, statRes)
	if !strings.Contains(statTxt, actID) || !strings.Contains(statTxt, `"status": "completed"`) {
		t.Errorf("action_status unexpected response: %s", statTxt)
	}

	// 3. action_list
	listRes := callTool(t, s, "action_list", map[string]any{
		"status": "all",
		"limit":  10,
	})
	listTxt := resultText(t, listRes)
	if !strings.Contains(listTxt, actID) {
		t.Errorf("expected %s in action_list, got: %s", actID, listTxt)
	}

	// 4. action_run with dangerous files -> must fail safely
	badRunRes := callTool(t, s, "action_run", map[string]any{
		"action": "validate_torrent",
		"inputs": `{"files":["Movie.1080p.mkv","virus.exe"]}`,
	})
	badTxt := resultText(t, badRunRes)
	if !strings.Contains(badTxt, `"status": "failed"`) {
		t.Errorf("expected dangerous files to fail action, got: %s", badTxt)
	}
}
