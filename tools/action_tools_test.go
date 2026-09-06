package tools

import (
	"context"
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

	var badMap map[string]any
	_ = json.Unmarshal([]byte(badTxt), &badMap)
	badActID := badMap["id"].(string)

	// 5. action_catalog tool
	catalogRes := callTool(t, s, "action_catalog", map[string]any{})
	catalogTxt := resultText(t, catalogRes)
	var catalog []map[string]any
	if err := json.Unmarshal([]byte(catalogTxt), &catalog); err != nil {
		t.Fatalf("decoding action_catalog: %v", err)
	}
	if len(catalog) < 2 {
		t.Errorf("expected at least 2 catalog entries, got %d", len(catalog))
	}
	foundSMR := false
	for _, entry := range catalog {
		if entry["name"] == "safe_media_replacement" {
			foundSMR = true
			if entry["version"] != float64(1) {
				t.Errorf("expected version 1, got %v", entry["version"])
			}
			if entry["destructive"] != false {
				t.Errorf("expected destructive=false, got %v", entry["destructive"])
			}
		}
	}
	if !foundSMR {
		t.Errorf("safe_media_replacement not found in action_catalog")
	}

	// 6. action_retry tool on failed action
	retryRes := callTool(t, s, "action_retry", map[string]any{
		"id": badActID,
	})
	retryTxt := resultText(t, retryRes)
	if !strings.Contains(retryTxt, badActID) {
		t.Errorf("expected retried instance ID %s in response, got: %s", badActID, retryTxt)
	}

	// 7. action_run with idempotency_key deduplication
	engine.RegisterTemplate(action.ActionTemplate{
		Name: "wait_test",
		Steps: []action.StepDefinition{
			{
				Name: "wait",
				Run: func(ctx context.Context, ec *action.ExecutionContext) (action.StepResult, error) {
					return action.StepResult{
						Status:        action.StepWaitingDecision,
						WaitingReason: "Confirm action",
					}, nil
				},
			},
		},
	})

	idempotentKey := "test:idempotency:key:123"
	run1 := callTool(t, s, "action_run", map[string]any{
		"action":          "wait_test",
		"idempotency_key": idempotentKey,
	})
	txt1 := resultText(t, run1)
	var m1 map[string]any
	_ = json.Unmarshal([]byte(txt1), &m1)
	id1 := m1["id"].(string)

	run2 := callTool(t, s, "action_run", map[string]any{
		"action":          "wait_test",
		"idempotency_key": idempotentKey,
	})
	txt2 := resultText(t, run2)
	var m2 map[string]any
	_ = json.Unmarshal([]byte(txt2), &m2)
	id2 := m2["id"].(string)

	if id1 != id2 {
		t.Errorf("idempotency_key failed: expected same instance ID %s, got %s", id1, id2)
	}
}
