package tools

import (
	"context"
	"encoding/json"
	"fmt"
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
			if entry["destructive"] != true {
				t.Errorf("expected destructive=true, got %v", entry["destructive"])
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

func TestIdempotencyKeyMCPToolSchemaAndProtocol(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "action_idempotency_schema_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := &config.Config{}
	engine := action.NewEngine(action.EngineDeps{Store: st, Config: cfg})

	s := server.NewMCPServer("test-server", "1.0.0")
	registerActionTools(s, engine)

	// 1. Verify idempotency_key appears in the MCP tool schema properties
	tool := s.GetTool("action_run")
	if tool == nil {
		t.Fatalf("tool action_run was not registered")
	}
	schemaProps := tool.Tool.InputSchema.Properties
	if _, ok := schemaProps["idempotency_key"]; !ok {
		t.Fatalf("CRITICAL: idempotency_key property is missing from action_run MCP schema: %+v", schemaProps)
	}
	if _, ok := schemaProps["allow_cleanup"]; ok {
		t.Fatalf("CRITICAL: allow_cleanup should be completely removed from action_run schema")
	}

	// 2. Register template that enters waiting state to simulate active concurrent calls
	engine.RegisterTemplate(action.ActionTemplate{
		Name: "active_concurrent_flow",
		Steps: []action.StepDefinition{
			{
				Name: "wait_step",
				Run: func(ctx context.Context, ec *action.ExecutionContext) (action.StepResult, error) {
					return action.StepResult{
						Status:        action.StepWaitingDecision,
						WaitingReason: "Waiting for user confirmation",
					}, nil
				},
			},
		},
	})

	// 3. First call with idempotency_key = radarr:327:size_optimization
	idempotencyKey := "radarr:327:size_optimization"
	res1 := callTool(t, s, "action_run", map[string]any{
		"action":          "active_concurrent_flow",
		"idempotency_key": idempotencyKey,
	})
	txt1 := resultText(t, res1)
	var m1 map[string]any
	if err := json.Unmarshal([]byte(txt1), &m1); err != nil {
		t.Fatalf("decoding response 1: %v", err)
	}
	id1, ok1 := m1["id"].(string)
	if !ok1 || id1 == "" {
		t.Fatalf("missing action ID in response 1: %s", txt1)
	}
	if m1["idempotency_key"] != idempotencyKey {
		t.Errorf("expected idempotency_key %q in output, got %v", idempotencyKey, m1["idempotency_key"])
	}

	// Verify key reached SQLite store
	inst, err := st.GetActionInstance(id1)
	if err != nil || inst == nil {
		t.Fatalf("could not retrieve action instance %s from store: %v", id1, err)
	}
	if inst.IdempotencyKey != idempotencyKey {
		t.Errorf("store expected IdempotencyKey %q, got %q", idempotencyKey, inst.IdempotencyKey)
	}

	// 4. Second concurrent call with same key while first is active -> must return identical action
	res2 := callTool(t, s, "action_run", map[string]any{
		"action":          "active_concurrent_flow",
		"idempotency_key": idempotencyKey,
	})
	txt2 := resultText(t, res2)
	var m2 map[string]any
	if err := json.Unmarshal([]byte(txt2), &m2); err != nil {
		t.Fatalf("decoding response 2: %v", err)
	}
	id2, ok2 := m2["id"].(string)
	if !ok2 || id2 == "" {
		t.Fatalf("missing action ID in response 2: %s", txt2)
	}

	if id1 != id2 {
		t.Errorf("deduplication failed: expected identical action ID %s, got %s", id1, id2)
	}

	// 5. Also verify idempotency_key passed inside inputs JSON string
	res3 := callTool(t, s, "action_run", map[string]any{
		"action": "active_concurrent_flow",
		"inputs": fmt.Sprintf(`{"idempotency_key":%q}`, idempotencyKey),
	})
	txt3 := resultText(t, res3)
	var m3 map[string]any
	_ = json.Unmarshal([]byte(txt3), &m3)
	id3 := m3["id"].(string)
	if id3 != id1 {
		t.Errorf("deduplication via inputs JSON failed: expected %s, got %s", id1, id3)
	}
}
