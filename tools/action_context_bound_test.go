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

func TestCompactTranscodeStatusPreservesWorkerEvidence(t *testing.T) {
	encodeMs, lagMs := int64(240000), int64(25000000)
	res := &action.ActionResult{
		ID: "transcode-observed", ActionName: "transcode_media", Status: action.StatusWaitingExternal,
		CurrentStep: 2, TotalSteps: 5, DurationMs: 25240000, WallDurationMs: 25240000,
		EncodeDurationMs: &encodeMs, ReconcileLagMs: &lagMs,
		Outputs: map[string]any{
			"transcode_status": "running", "transcode_phase": "encoding", "progress": 26.2,
			"progress_is_stale": true, "worker_heartbeat_at": "2026-01-01T00:00:00Z",
			"last_known_progress": map[string]any{"progress": 26.2, "speed": 15.5, "fps": 366.1},
			"source_report":       strings.Repeat("private-metadata", 10000),
		},
		State: map[string]any{"next_poll_at": "2026-01-01T00:00:15Z"},
	}
	compact := toCompactSummary(res)
	data, err := json.Marshal(compact)
	if err != nil {
		t.Fatal(err)
	}
	if compact.Worker["progress_is_stale"] != true || compact.Worker["last_known_progress"] == nil {
		t.Fatalf("compact status lost stale progress evidence: %s", data)
	}
	if compact.WallDurationMs == *compact.EncodeDurationMs || *compact.ReconcileLagMs != lagMs {
		t.Fatalf("compact status confused wall time and encoding: %s", data)
	}
	if compact.Reconciliation["next_poll_at"] == nil || len(data) > 4096 || strings.Contains(string(data), "private-metadata") {
		t.Fatalf("compact status must expose polling without raw media payload: %s", data)
	}
}

func TestCompactPromotionApprovalIncludesConcreteReplacement(t *testing.T) {
	res := &action.ActionResult{
		ID: "promotion-approval", ActionName: "promote_transcode_candidate", Status: action.StatusWaitingDecision,
		Outputs: map[string]any{"promotion": map[string]any{
			"transcode_action_id": "source-action", "series_id": 42, "service": "sonarr",
			"original_path": "/media/series/original.mp4", "candidate_path": "/media/series/.navigatorr-candidates/candidate.mkv",
			"episode_ids": []int{101, 102}, "original_bytes": 1800000000, "candidate_bytes": 530000000,
			"candidate_sha256": strings.Repeat("a", 64), "approved": false,
			"commands": map[string]any{"large_history": strings.Repeat("unrelated-command", 10000)},
		}},
	}
	summary := toCompactSummary(res)
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"transcode_action_id", "series_id", "original_path", "candidate_path", "episode_ids", "candidate_sha256", "original_bytes", "candidate_bytes"} {
		if summary.Promotion[key] == nil {
			t.Errorf("approval omitted %s: %s", key, encoded)
		}
	}
	if summary.Promotion["approved"] != false || len(encoded) > 4096 || strings.Contains(string(encoded), "unrelated-command") {
		t.Fatalf("approval must show its bounded, unapproved plan: %s", encoded)
	}
}

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

func TestActionList_OneHundredTranscodesRemainBounded(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "transcode_list.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine := action.NewEngine(action.EngineDeps{Store: st})
	s := server.NewMCPServer("navigatorr-test", "1.0.0", server.WithToolCapabilities(true))
	registerActionTools(s, engine)

	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339)
	telemetry := map[string]any{
		"transcode_status": "running", "transcode_phase": "encoding",
		"progress": 26.2, "speed": 15.5, "fps": 366.1,
		"last_progress_at": stamp, "worker_heartbeat_at": stamp,
		"progress_is_stale":   false,
		"last_known_progress": map[string]any{"progress": 26.2, "speed": 15.5, "fps": 366.1, "updated_at": stamp},
		"worker_slots_total":  3, "worker_slots_used": 3, "queue_position": 0,
		"storage_backend": "smb_direct",
		"next_poll_at":    now.Add(5 * time.Second).Format(time.RFC3339), "last_worker_poll_at": stamp,
		"queue_duration_ms": 5, "encode_duration_ms": 180000,
		"validation_duration_ms": 4000, "reconcile_lag_ms": 5000,
	}
	encoded, err := json.Marshal(telemetry)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := st.CreateActionInstance(store.ActionInstance{
			ID: fmt.Sprintf("act-transcode-media-%08d", i), ActionName: "transcode_media",
			Status: action.StatusWaitingExternal, CurrentStep: 2,
			WaitingReason:    "Transcoding media (running, progress: 26.2%, speed: 15.5x, fps: 366.1)",
			WaitingCondition: "transcode_complete",
			InputsJSON:       `{}`, OutputsJSON: string(encoded), StateJSON: string(encoded),
			IdempotencyKey: fmt.Sprintf("series-10-episode-%03d-q65", i),
			CreatedAt:      now.Add(-4 * time.Minute).Format(time.RFC3339), UpdatedAt: stamp,
		}); err != nil {
			t.Fatal(err)
		}
	}

	response := resultText(t, callTool(t, s, "action_list", map[string]any{"limit": "100", "offset": "0"}))
	if len(response) >= MaxActionResponseBytes {
		t.Fatalf("normal 100-action page exceeds response budget: %d bytes", len(response))
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(response), &rows); err != nil {
		t.Fatalf("expected an array of actions, not an overflow error: %v; response: %s", err, response)
	}
	if len(rows) != 100 {
		t.Fatalf("normal page was truncated: got %d actions, want 100", len(rows))
	}
	seen := make(map[string]bool, 100)
	for _, row := range rows {
		id, _ := row["id"].(string)
		if id == "" || seen[id] {
			t.Fatalf("missing or duplicate action ID: %q", id)
		}
		seen[id] = true
		if row["status"] != action.StatusWaitingExternal || row["current_step"] != float64(2) || row["wall_duration_ms"] == nil {
			t.Fatalf("list lost operational fields: %+v", row)
		}
		for _, field := range []string{"worker", "reconciliation", "queue_duration_ms", "encode_duration_ms", "validation_duration_ms", "reconcile_lag_ms"} {
			if _, exists := row[field]; exists {
				t.Fatalf("per-action detail %q should remain in action_status: %+v", field, row)
			}
		}
	}
	t.Logf("100 transcode actions fit in %d bytes", len(response))
}
