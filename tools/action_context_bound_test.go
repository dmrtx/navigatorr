package tools

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

func TestActionResponses_ContextBounded(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "action_bounded.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	engine := action.NewEngine(action.EngineDeps{Store: st})
	s := server.NewMCPServer("navigatorr-test", "1.0.0", server.WithToolCapabilities(true))
	registerActionTools(s, engine)

	// Build ~500-600 KiB synthetic action payload
	largeString200K := strings.Repeat("A", 200*1024) // 200 KiB
	largeInputs := map[string]any{"data": largeString200K, "service": "radarr", "media_id": "123"}
	largeOutputs := map[string]any{"result": largeString200K, "download_id": "dl-999"}
	largeState := map[string]any{"working_cache": largeString200K, "step_count": 2}

	inputsJSON, _ := json.Marshal(largeInputs)
	outputsJSON, _ := json.Marshal(largeOutputs)
	stateJSON, _ := json.Marshal(largeState)

	now := time.Now().UTC().Format(time.RFC3339)
	actionID := "act-test-large-payload-001"

	// Create action instance with ~600 KiB of total payload
	err = st.CreateActionInstance(store.ActionInstance{
		ID:          actionID,
		ActionName:  "validate_torrent",
		Status:      action.StatusRunning,
		CurrentStep: 1,
		InputsJSON:  string(inputsJSON),
		OutputsJSON: string(outputsJSON),
		StateJSON:   string(stateJSON),
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		t.Fatalf("failed to insert action instance: %v", err)
	}

	// Insert step logs with large payloads
	err = st.LogActionStep(store.ActionStepLog{
		InstanceID:  actionID,
		StepIndex:   0,
		StepName:    "inspect_torrent",
		Primitive:   "qbit_torrent_properties",
		InputsJSON:  string(inputsJSON),
		OutputsJSON: string(outputsJSON),
		Status:      "success",
		DurationMs:  450,
		CreatedAt:   now,
	})
	if err != nil {
		t.Fatalf("failed to insert step log: %v", err)
	}

	err = st.LogActionStep(store.ActionStepLog{
		InstanceID:  actionID,
		StepIndex:   1,
		StepName:    "verify_audio",
		Primitive:   "inspect_media",
		InputsJSON:  `{"stream":"audio"}`,
		OutputsJSON: `{"codec":"aac","channels":6}`,
		Status:      "running",
		DurationMs:  120,
		CreatedAt:   now,
	})
	if err != nil {
		t.Fatalf("failed to insert step log 2: %v", err)
	}

	// Also insert a second action for pagination testing
	actionID2 := "act-test-second-002"
	_ = st.CreateActionInstance(store.ActionInstance{
		ID:          actionID2,
		ActionName:  "safe_media_replacement",
		Status:      action.StatusCompleted,
		CurrentStep: 3,
		InputsJSON:  `{"service":"sonarr"}`,
		OutputsJSON: `{"final":"ok"}`,
		StateJSON:   `{}`,
		CreatedAt:   now,
		UpdatedAt:   now,
	})

	// 1. Test default action_status (compact by default)
	t.Run("action_status_default_is_compact", func(t *testing.T) {
		res := callTool(t, s, "action_status", map[string]any{"id": actionID})
		txt := resultText(t, res)

		// Size should be a few hundred bytes, strictly < 4 KiB (far below 64 KiB limit)
		if len(txt) > 4096 {
			t.Errorf("action_status response too large: got %d bytes, expected < 4 KiB", len(txt))
		}
		if strings.Contains(txt, largeString200K) {
			t.Errorf("action_status leaked raw JSON payload")
		}

		var parsed map[string]any
		if err := json.Unmarshal([]byte(txt), &parsed); err != nil {
			t.Fatalf("failed to parse action_status JSON: %v", err)
		}

		act, ok := parsed["action"].(map[string]any)
		if !ok {
			t.Fatalf("missing action summary object")
		}
		if act["id"] != actionID {
			t.Errorf("expected id %s, got %v", actionID, act["id"])
		}
		if act["progress"] == nil || act["progress"] == "" {
			t.Errorf("expected progress string in compact summary")
		}
		if act["inputs"] != nil || act["outputs"] != nil || act["state"] != nil {
			t.Errorf("compact action summary must omit raw inputs, outputs, and state")
		}

		steps, ok := parsed["steps"].([]any)
		if !ok || len(steps) != 2 {
			t.Fatalf("expected 2 compact step summaries, got %v", steps)
		}
		s0 := steps[0].(map[string]any)
		if s0["step_name"] != "inspect_torrent" {
			t.Errorf("expected step_name inspect_torrent, got %v", s0["step_name"])
		}
		if s0["inputs_json"] != nil || s0["outputs_json"] != nil {
			t.Errorf("step summaries must omit raw inputs_json / outputs_json")
		}
	})

	// 2. Test action_status verbose mode exceeding 64 KiB guard
	t.Run("action_status_verbose_triggers_guard", func(t *testing.T) {
		res := callTool(t, s, "action_status", map[string]any{"id": actionID, "verbose": true})
		txt := resultText(t, res)

		// Hard response size guard must bound the response around or well under 64 KiB
		if len(txt) > MaxActionResponseBytes {
			t.Errorf("verbose response exceeded hard 64 KiB guard: got %d bytes", len(txt))
		}

		var parsed map[string]any
		if err := json.Unmarshal([]byte(txt), &parsed); err != nil {
			t.Fatalf("failed to parse verbose response JSON: %v", err)
		}

		if parsed["error"] != "response_too_large" {
			t.Errorf("expected error=response_too_large, got %v", parsed["error"])
		}
		if parsed["hint"] == nil || !strings.Contains(fmt.Sprint(parsed["hint"]), "action_detail") {
			t.Errorf("expected hint to use action_detail, got %v", parsed["hint"])
		}
		if parsed["action"] == nil {
			t.Errorf("expected bounded action summary in guard response")
		}
	})

	// 3. Test action_status verbose mode on small action (< 64 KiB)
	t.Run("action_status_verbose_small_action_returns_full", func(t *testing.T) {
		res := callTool(t, s, "action_status", map[string]any{"id": actionID2, "verbose": true})
		txt := resultText(t, res)

		if len(txt) > MaxActionResponseBytes {
			t.Errorf("response exceeded 64 KiB: got %d bytes", len(txt))
		}

		var parsed map[string]any
		if err := json.Unmarshal([]byte(txt), &parsed); err != nil {
			t.Fatalf("failed to parse verbose JSON: %v", err)
		}
		if parsed["error"] != nil {
			t.Errorf("small action should not trigger response_too_large guard")
		}
		act, ok := parsed["action"].(map[string]any)
		if !ok {
			t.Fatalf("expected full action object")
		}
		// Full action includes inputs map
		if act["inputs"] == nil {
			t.Errorf("verbose mode on small action should include inputs")
		}
	})

	// 4. Test action_detail section retrieval
	t.Run("action_detail_sections", func(t *testing.T) {
		// Section inputs (inputs is ~200 KiB: chunked retrieval with chunk=0)
		resInputs := callTool(t, s, "action_detail", map[string]any{
			"id":      actionID,
			"section": "inputs",
		})
		txtInputs := resultText(t, resInputs)
		if len(txtInputs) > MaxActionResponseBytes {
			t.Errorf("action_detail inputs exceeded 64 KiB limit: got %d bytes", len(txtInputs))
		}
		var parsedInputs map[string]any
		_ = json.Unmarshal([]byte(txtInputs), &parsedInputs)
		if parsedInputs["chunk"] != float64(0) {
			t.Errorf("expected chunk=0, got %v", parsedInputs["chunk"])
		}
		if parsedInputs["has_more"] != true {
			t.Errorf("expected has_more=true for 200 KiB inputs")
		}
		if parsedInputs["next_chunk"] != float64(1) {
			t.Errorf("expected next_chunk=1, got %v", parsedInputs["next_chunk"])
		}
		if parsedInputs["content"] == nil || len(fmt.Sprint(parsedInputs["content"])) == 0 {
			t.Errorf("expected non-empty chunk content")
		}
		availKeys, ok := parsedInputs["available_keys"].([]any)
		if !ok || len(availKeys) == 0 {
			t.Errorf("expected available_keys in chunked response")
		}

		// Retrieve chunk 1
		resInputsChunk1 := callTool(t, s, "action_detail", map[string]any{
			"id":      actionID,
			"section": "inputs",
			"chunk":   "1",
		})
		txtChunk1 := resultText(t, resInputsChunk1)
		var parsedChunk1 map[string]any
		_ = json.Unmarshal([]byte(txtChunk1), &parsedChunk1)
		if parsedChunk1["chunk"] != float64(1) {
			t.Errorf("expected chunk=1, got %v", parsedChunk1["chunk"])
		}

		// Query specific key from inputs (service="radarr")
		resKey := callTool(t, s, "action_detail", map[string]any{
			"id":      actionID,
			"section": "inputs",
			"key":     "service",
		})
		txtKey := resultText(t, resKey)
		var parsedKey map[string]any
		_ = json.Unmarshal([]byte(txtKey), &parsedKey)
		if parsedKey["data"] != "radarr" {
			t.Errorf("expected key 'service' to be 'radarr', got %v", parsedKey["data"])
		}

		// Query missing key
		resMissingKey := callTool(t, s, "action_detail", map[string]any{
			"id":      actionID,
			"section": "inputs",
			"key":     "non_existent",
		})
		txtMissing := resultText(t, resMissingKey)
		if !strings.Contains(txtMissing, "key \"non_existent\" not found") {
			t.Errorf("expected missing key error, got: %s", txtMissing)
		}

		// Section state on actionID2 (small, should succeed with full object)
		resState := callTool(t, s, "action_detail", map[string]any{
			"id":      actionID2,
			"section": "state",
		})
		txtState := resultText(t, resState)
		var parsedState map[string]any
		if err := json.Unmarshal([]byte(txtState), &parsedState); err != nil {
			t.Fatalf("failed to parse state detail: %v", err)
		}
		if parsedState["section"] != "state" {
			t.Errorf("expected section=state, got %v", parsedState["section"])
		}

		// Section steps with step_index=1 on actionID (small step, should return individual step)
		resStep1 := callTool(t, s, "action_detail", map[string]any{
			"id":         actionID,
			"section":    "steps",
			"step_index": "1",
		})
		txtStep1 := resultText(t, resStep1)
		var parsedStep1 map[string]any
		if err := json.Unmarshal([]byte(txtStep1), &parsedStep1); err != nil {
			t.Fatalf("failed to parse step detail: %v", err)
		}
		stepObj, ok := parsedStep1["step"].(map[string]any)
		if !ok {
			t.Fatalf("expected step object in response")
		}
		if stepObj["step_name"] != "verify_audio" {
			t.Errorf("expected step_name=verify_audio, got %v", stepObj["step_name"])
		}

		// Section steps with pagination:
		// Step 0 has 400 KiB payloads, so step_offset=0, step_limit=1 triggers chunking safely
		resStepsPagedChunked := callTool(t, s, "action_detail", map[string]any{
			"id":          actionID,
			"section":     "steps",
			"step_offset": "0",
			"step_limit":  "1",
		})
		txtStepsChunked := resultText(t, resStepsPagedChunked)
		if len(txtStepsChunked) > MaxActionResponseBytes {
			t.Errorf("steps chunked response exceeded 64 KiB: %d bytes", len(txtStepsChunked))
		}
		var parsedStepsChunked map[string]any
		_ = json.Unmarshal([]byte(txtStepsChunked), &parsedStepsChunked)
		if parsedStepsChunked["chunk"] != float64(0) {
			t.Errorf("expected chunk=0 for 400 KiB step, got %v", parsedStepsChunked["chunk"])
		}

		// Step 1 is small (~100 bytes), so step_offset=1, step_limit=1 returns direct steps list
		resStepsPaged := callTool(t, s, "action_detail", map[string]any{
			"id":          actionID,
			"section":     "steps",
			"step_offset": "1",
			"step_limit":  "1",
		})
		txtStepsPaged := resultText(t, resStepsPaged)
		var parsedStepsPaged map[string]any
		_ = json.Unmarshal([]byte(txtStepsPaged), &parsedStepsPaged)
		pagedList, ok := parsedStepsPaged["steps"].([]any)
		if !ok || len(pagedList) != 1 {
			t.Fatalf("expected 1 step in paged response, got %v (raw: %s)", parsedStepsPaged["steps"], txtStepsPaged)
		}
		s1 := pagedList[0].(map[string]any)
		if s1["step_name"] != "verify_audio" {
			t.Errorf("expected step_name=verify_audio, got %v", s1["step_name"])
		}

		// Invalid section
		resInvalid := callTool(t, s, "action_detail", map[string]any{
			"id":      actionID,
			"section": "unknown_section",
		})
		txtInvalid := resultText(t, resInvalid)
		if !strings.Contains(txtInvalid, "invalid section") {
			t.Errorf("expected error for invalid section, got: %s", txtInvalid)
		}
	})

	// 5. Test action_list compact pagination
	t.Run("action_list_compact_and_paged", func(t *testing.T) {
		resList := callTool(t, s, "action_list", map[string]any{"limit": "1", "offset": "0"})
		txtList := resultText(t, resList)

		if strings.Contains(txtList, largeString200K) {
			t.Errorf("action_list leaked raw JSON payloads")
		}
		if len(txtList) > 4096 {
			t.Errorf("action_list item too large: got %d bytes", len(txtList))
		}

		var list0 []map[string]any
		if err := json.Unmarshal([]byte(txtList), &list0); err != nil {
			t.Fatalf("failed to parse action_list: %v", err)
		}
		if len(list0) != 1 {
			t.Fatalf("expected 1 action with limit=1, got %d", len(list0))
		}
		if list0[0]["id"] == "" || list0[0]["progress"] == "" {
			t.Errorf("expected operational fields id and progress in list output")
		}

		// Second page
		resList2 := callTool(t, s, "action_list", map[string]any{"limit": "1", "offset": "1"})
		txtList2 := resultText(t, resList2)
		var list1 []map[string]any
		if err := json.Unmarshal([]byte(txtList2), &list1); err != nil {
			t.Fatalf("failed to parse action_list offset 1: %v", err)
		}
		if len(list1) != 1 {
			t.Fatalf("expected 1 action with offset=1, got %d", len(list1))
		}
		if list1[0]["id"] == list0[0]["id"] {
			t.Errorf("expected different action on page 2")
		}
	})

	// 6. Test formatProgress
	t.Run("formatProgress_logic", func(t *testing.T) {
		p1 := formatProgress(0, 5, action.StatusRunning)
		if p1 != "0/5 steps (0%)" {
			t.Errorf("expected '0/5 steps (0%%)', got %q", p1)
		}
		p2 := formatProgress(2, 4, action.StatusRunning)
		if p2 != "2/4 steps (50%)" {
			t.Errorf("expected '2/4 steps (50%%)', got %q", p2)
		}
		p3 := formatProgress(3, 3, action.StatusCompleted)
		if p3 != "3/3 steps (100%)" {
			t.Errorf("expected '3/3 steps (100%%)', got %q", p3)
		}
	})

	// 7. Test action_resume returns compact summary without leaking raw JSON
	t.Run("action_resume_compact_response", func(t *testing.T) {
		waitID := "act-test-waiting-003"
		_ = st.CreateActionInstance(store.ActionInstance{
			ID:               waitID,
			ActionName:       "safe_media_replacement",
			Status:           action.StatusWaitingDecision,
			CurrentStep:      2,
			InputsJSON:       string(inputsJSON),
			OutputsJSON:      string(outputsJSON),
			StateJSON:        string(stateJSON),
			WaitingReason:    "Awaiting replacement confirmation",
			WaitingCondition: "user_approval",
			CreatedAt:        now,
			UpdatedAt:        now,
		})

		resResume := callTool(t, s, "action_resume", map[string]any{
			"id":       waitID,
			"decision": "reject",
		})
		txtResume := resultText(t, resResume)
		if strings.Contains(txtResume, largeString200K) {
			t.Errorf("action_resume leaked raw 200K payload")
		}
		if len(txtResume) > 4096 {
			t.Errorf("action_resume response exceeded 4 KiB: got %d bytes", len(txtResume))
		}
		var parsedResume map[string]any
		if err := json.Unmarshal([]byte(txtResume), &parsedResume); err != nil {
			t.Fatalf("failed to parse action_resume response: %v", err)
		}
		if parsedResume["id"] != waitID {
			t.Errorf("expected id %s, got %v", waitID, parsedResume["id"])
		}
		if parsedResume["inputs"] != nil || parsedResume["outputs"] != nil || parsedResume["state"] != nil {
			t.Errorf("action_resume must not include raw inputs/outputs/state")
		}
	})

	// 8. Test action_status step capping on large number of steps (>25 steps)
	t.Run("action_status_step_capping", func(t *testing.T) {
		actManyID := "act-test-many-steps-004"
		_ = st.CreateActionInstance(store.ActionInstance{
			ID:          actManyID,
			ActionName:  "batch_transcode",
			Status:      action.StatusRunning,
			CurrentStep: 30,
			InputsJSON:  `{}`,
			OutputsJSON: `{}`,
			StateJSON:   `{}`,
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		for i := 0; i < 30; i++ {
			_ = st.LogActionStep(store.ActionStepLog{
				InstanceID:  actManyID,
				StepIndex:   i,
				StepName:    fmt.Sprintf("step_%d", i),
				Primitive:   "fs_noop",
				InputsJSON:  `{}`,
				OutputsJSON: `{}`,
				Status:      "success",
				DurationMs:  10,
				CreatedAt:   now,
			})
		}

		resMany := callTool(t, s, "action_status", map[string]any{"id": actManyID})
		txtMany := resultText(t, resMany)
		var parsedMany map[string]any
		_ = json.Unmarshal([]byte(txtMany), &parsedMany)

		steps, ok := parsedMany["steps"].([]any)
		if !ok || len(steps) != 25 {
			t.Errorf("expected steps to be capped at 25, got %d", len(steps))
		}
		if parsedMany["total_steps_logged"] != float64(30) {
			t.Errorf("expected total_steps_logged=30, got %v", parsedMany["total_steps_logged"])
		}
	})

	// 9. Test action_list clamping on negative and huge values
	t.Run("action_list_clamping", func(t *testing.T) {
		resClamped := callTool(t, s, "action_list", map[string]any{"limit": "-5", "offset": "-10"})
		txtClamped := resultText(t, resClamped)
		var listClamped []map[string]any
		if err := json.Unmarshal([]byte(txtClamped), &listClamped); err != nil {
			t.Fatalf("failed to parse clamped action_list: %v", err)
		}
		if len(listClamped) == 0 {
			t.Errorf("expected items with clamped parameters")
		}

		resHuge := callTool(t, s, "action_list", map[string]any{"limit": "9999", "offset": "0"})
		txtHuge := resultText(t, resHuge)
		var listHuge []map[string]any
		if err := json.Unmarshal([]byte(txtHuge), &listHuge); err != nil {
			t.Fatalf("failed to parse clamped huge action_list: %v", err)
		}
		if len(listHuge) > 100 {
			t.Errorf("expected at most 100 items with limit clamped")
		}
	})
}
