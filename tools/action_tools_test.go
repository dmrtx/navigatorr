package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
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
	foundImmutableBatch := false
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
		if entry["name"] == "transcode_batch" {
			foundImmutableBatch = true
			if entry["immutable_inputs"] != true {
				t.Errorf("expected transcode_batch immutable_inputs=true, got %v", entry["immutable_inputs"])
			}
		}
	}
	if !foundSMR {
		t.Errorf("safe_media_replacement not found in action_catalog")
	}
	if !foundImmutableBatch {
		t.Errorf("transcode_batch not found in action_catalog")
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

func TestCompactPromotionRecoveryHistoryAndMissingPlan(t *testing.T) {
	r := &action.ActionResult{ActionName: "promote_transcode_candidate", Status: action.StatusFailed}
	if compactPromotion(r) != nil {
		t.Fatal("missing plan should stay absent")
	}
	r.Status = action.StatusCompleted
	r.State = map[string]any{"promotion": map[string]any{"recovery_verified": false, "recovery_cleanup_started": true}}
	r.Outputs = map[string]any{"original_integrity": "verified_before_replacement", "recovery_retained": false}
	p := compactPromotion(r)
	if p["recovery_verified"] != false || p["recovery_verified_before_replacement"] != true || p["recovery_cleanup_completed"] != true {
		t.Fatalf("historical recovery evidence: %+v", p)
	}
	r.Status = action.StatusFailed
	delete(r.Outputs, "original_integrity")
	p = compactPromotion(r)
	if p["recovery_verified_before_replacement"] == true || p["recovery_cleanup_completed"] == true {
		t.Fatal("invented evidence on unfinished promotion")
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

func TestParseJSONObjectStrict(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "object", raw: `{"metric":"ssim"}`},
		{name: "empty object", raw: `{}`},
		{name: "malformed", raw: `{bad`, wantErr: "invalid JSON object"},
		{name: "array", raw: `["foo"]`, wantErr: "inputs must be a JSON object"},
		{name: "string scalar", raw: `"foo"`, wantErr: "inputs must be a JSON object"},
		{name: "number scalar", raw: `42`, wantErr: "inputs must be a JSON object"},
		{name: "null", raw: `null`, wantErr: "inputs must be a JSON object"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseJSONObject(tc.raw)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected parse error: %v", err)
				}
				if got == nil {
					t.Fatal("expected non-nil object")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestActionRunRejectsInvalidInputsJSON(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "action_invalid_run_inputs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	engine := action.NewEngine(action.EngineDeps{Store: st, Config: &config.Config{}})
	s := server.NewMCPServer("test", "0.0.0")
	registerActionTools(s, engine)

	for _, raw := range []string{`{bad`, `["foo"]`, `"foo"`} {
		res := callTool(t, s, "action_run", map[string]any{
			"action": "validate_torrent",
			"inputs": raw,
		})
		txt := resultText(t, res)
		if !strings.Contains(txt, "invalid action_run inputs") {
			t.Fatalf("action_run should reject %q explicitly, got %s", raw, txt)
		}
	}
}

type readyAdmissionWorker struct{ transcode.Executor }

func (readyAdmissionWorker) Ready(context.Context) error  { return nil }
func (readyAdmissionWorker) Health(context.Context) error { return nil }
func (readyAdmissionWorker) Doctor(context.Context) error { return nil }

func TestMCPWorkerGateDoesNotCreateOfflineJobsAndCancelIsRegistered(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "worker-gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := action.NewEngine(action.EngineDeps{Store: st, Config: &config.Config{}})
	s := server.NewMCPServer("test", "1")
	registerActionTools(s, e)
	for _, name := range []string{"transcode_media", "transcode_batch", "benchmark_transcode"} {
		result := callTool(t, s, "action_run", map[string]any{"action": name, "inputs": `{"path":"/offline.mkv"}`})
		if !result.IsError || !strings.Contains(resultText(t, result), "no job was submitted") {
			t.Fatal("offline submission not blocked", result)
		}
	}
	rows, _ := st.ListActionInstances("", 100)
	if len(rows) != 0 {
		t.Fatal("offline job persisted", rows)
	}
	for _, control := range []string{"action_retry", "action_resume"} {
		status := store.ActionStatusFailed
		if control == "action_resume" {
			status = store.ActionStatusWaitingExternal
		}
		id := "blocked-" + control
		if err := st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: "transcode_media", Status: status}); err != nil {
			t.Fatal(err)
		}
		result := callTool(t, s, control, map[string]any{"id": id})
		if !result.IsError || !strings.Contains(resultText(t, result), "no job was submitted") {
			t.Fatal("offline control was admitted", control, result)
		}
		inst, _ := st.GetActionInstance(id)
		if inst.Status != status || inst.CurrentStep != 0 {
			t.Fatal("blocked control changed job", inst)
		}
	}
	for _, decision := range []string{"approve", "accept_loss", "reject"} {
		id := "offline-batch-" + decision
		if err := st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: "transcode_batch", Status: store.ActionStatusWaitingDecision}); err != nil {
			t.Fatal(err)
		}
		result := callTool(t, s, "action_resume", map[string]any{"id": id, "decision": decision})
		if !result.IsError || !strings.Contains(resultText(t, result), "no job was submitted") {
			t.Fatal("batch continuation was admitted offline", result)
		}
		inst, _ := st.GetActionInstance(id)
		if inst.Status != store.ActionStatusWaitingDecision {
			t.Fatal("offline decision changed batch", inst)
		}
	}
	st.CreateActionInstance(store.ActionInstance{ID: "cancel-fixture", ActionName: "transcode_media", Status: "pending", InputsJSON: `{"path":"/fixture.mkv"}`})
	result := callTool(t, s, "action_cancel", map[string]any{"id": "cancel-fixture", "reason": "test"})
	if result.IsError {
		t.Fatal(resultText(t, result))
	}
	job, _ := st.GetActionInstance("cancel-fixture")
	if job.Status != "cancelled" {
		t.Fatal(job)
	}
}

func TestCatalogExamplesCanBeSentThroughActionRun(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "catalog_examples.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine := action.NewEngine(action.EngineDeps{Store: st, Config: &config.Config{}, Transcode: readyAdmissionWorker{}})
	s := server.NewMCPServer("test", "0.0.0")
	registerActionTools(s, engine)
	var catalog []action.ActionCatalogEntry
	if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "action_catalog", map[string]any{}))), &catalog); err != nil {
		t.Fatal(err)
	}
	var batch action.ActionCatalogEntry
	for _, entry := range catalog {
		if entry.Name == "transcode_batch" {
			batch = entry
		}
	}
	if len(batch.Examples) < 4 {
		t.Fatalf("missing filesystem/direct/tuning/preview examples: %+v", batch)
	}
	allowed := map[string]bool{}
	for _, key := range append(batch.RequiredInputs, batch.OptionalInputs...) {
		allowed[key] = true
	}
	var captured map[string]any
	// Exercise the real MCP transport and persisted inputs without invoking a
	// live library. The action suite separately exercises encoding/calibration.
	engine.RegisterTemplate(action.ActionTemplate{Name: batch.Name, ImmutableInputs: true, Steps: []action.StepDefinition{{Name: "capture", Run: func(_ context.Context, ec *action.ExecutionContext) (action.StepResult, error) {
		captured = ec.Inputs
		return action.StepResult{Status: action.StepCompleted}, nil
	}}}})
	for i, example := range batch.Examples {
		inputs, err := parseJSONObject(example.Inputs)
		if err != nil {
			t.Fatalf("example is not an action_run JSON string: %v", err)
		}
		for key := range inputs {
			if !allowed[key] {
				t.Fatalf("example advertises unsupported input %q", key)
			}
		}
		for _, key := range batch.RequiredInputs {
			if inputs[key] == nil {
				t.Fatalf("example missing %q", key)
			}
		}
		res := callTool(t, s, "action_run", map[string]any{"action": batch.Name, "inputs": example.Inputs, "idempotency_key": fmt.Sprintf("example-%d", i)})
		if res.IsError {
			t.Fatal(resultText(t, res))
		}
		if !reflect.DeepEqual(captured, inputs) {
			t.Fatalf("example intent changed in transit: %+v", captured)
		}
	}
}

func TestCompletedPreviewCannotBeChangedByResume(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "preview_guidance.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateActionInstance(store.ActionInstance{ID: "preview", ActionName: "transcode_batch", Status: action.StatusCompleted, InputsJSON: `{"service":"sonarr","series_id":10,"priority":"balanced","dry_run":true}`, StateJSON: `{}`, OutputsJSON: `{"dry_run":true,"counts":{"total":1,"queued":1}}`}); err != nil {
		t.Fatal(err)
	}
	engine := action.NewEngine(action.EngineDeps{Store: st, Config: &config.Config{}})
	s := server.NewMCPServer("test", "0.0.0")
	registerActionTools(s, engine)
	status := resultText(t, callTool(t, s, "action_status", map[string]any{"id": "preview"}))
	var got struct {
		Action ActionCompactSummary `json:"action"`
	}
	if err := json.Unmarshal([]byte(status), &got); err != nil {
		t.Fatal(err)
	}
	if got.Action.Batch == nil || got.Action.Batch.Outcome != "preview" || !strings.Contains(got.Action.Batch.NextStep, "new action_run") {
		t.Fatalf("preview suggests wrong continuation: %s", status)
	}
	callTool(t, s, "action_resume", map[string]any{"id": "preview", "inputs": `{"dry_run":false}`})
	inst, err := st.GetActionInstance("preview")
	if err != nil || inst.Status != action.StatusCompleted || !strings.Contains(inst.InputsJSON, `"dry_run":true`) {
		t.Fatalf("resume mutated preview: %+v %v", inst, err)
	}
}

func TestActionResumeRejectsInvalidInputsJSONWithoutAdvancing(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "action_invalid_resume_inputs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	engine := action.NewEngine(action.EngineDeps{Store: st, Config: &config.Config{}})
	engine.RegisterTemplate(action.ActionTemplate{
		Name: "resume_parse_test",
		Steps: []action.StepDefinition{{
			Name: "wait",
			Run: func(ctx context.Context, ec *action.ExecutionContext) (action.StepResult, error) {
				if ec.Decision == "continue" {
					return action.StepResult{Status: action.StepCompleted}, nil
				}
				return action.StepResult{
					Status:         action.StepWaitingDecision,
					WaitingReason:  "waiting for continue",
					WaitingOptions: []action.WaitingOption{{Decision: "continue", Description: "Continue"}},
				}, nil
			},
		}},
	})

	s := server.NewMCPServer("test", "0.0.0")
	registerActionTools(s, engine)

	runRes := callTool(t, s, "action_run", map[string]any{"action": "resume_parse_test"})
	runTxt := resultText(t, runRes)
	var run map[string]any
	if err := json.Unmarshal([]byte(runTxt), &run); err != nil {
		t.Fatal(err)
	}
	id, _ := run["id"].(string)
	if id == "" {
		t.Fatalf("missing action id: %s", runTxt)
	}

	for _, raw := range []string{`{bad`, `["foo"]`, `"foo"`} {
		res := callTool(t, s, "action_resume", map[string]any{
			"id":       id,
			"decision": "continue",
			"inputs":   raw,
		})
		txt := resultText(t, res)
		if !strings.Contains(txt, "invalid action_resume inputs") {
			t.Fatalf("action_resume should reject %q explicitly, got %s", raw, txt)
		}
		inst, err := st.GetActionInstance(id)
		if err != nil || inst == nil {
			t.Fatalf("reading waiting action: inst=%v err=%v", inst, err)
		}
		if inst.Status != action.StatusWaitingDecision {
			t.Fatalf("invalid resume inputs advanced workflow: status=%s", inst.Status)
		}
	}

	okRes := callTool(t, s, "action_resume", map[string]any{
		"id":       id,
		"decision": "continue",
		"inputs":   `{}`,
	})
	okTxt := resultText(t, okRes)
	if !strings.Contains(okTxt, `"status": "completed"`) {
		t.Fatalf("valid empty object should resume normally, got %s", okTxt)
	}
}

func TestCompactActionPhaseCostsRetainEvidenceAndUnknowns(t *testing.T) {
	cost := map[string]any{"duration_ms": float64(15), "attempts": float64(1), "active_compute_ms": nil, "nas_read_bytes": nil, "provenance": "coordinator_measured"}
	result := toCompactSummary(&action.ActionResult{State: map[string]any{"phase_costs": map[string]any{"coordinator_inventory": cost}}, Inputs: map[string]any{}})
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var projected map[string]any
	if err = json.Unmarshal(b, &projected); err != nil {
		t.Fatal(err)
	}
	costs := projected["phase_costs"].(map[string]any)
	measured := costs["coordinator_inventory"].(map[string]any)
	if measured["duration_ms"] != float64(15) || measured["active_compute_ms"] != nil || measured["nas_read_bytes"] != nil {
		t.Fatalf("compact response changed evidence: %s", b)
	}
	legacy := toCompactSummary(&action.ActionResult{State: map[string]any{}})
	if legacy.PhaseCosts != nil {
		t.Fatal("legacy action fabricated phase costs")
	}
}
