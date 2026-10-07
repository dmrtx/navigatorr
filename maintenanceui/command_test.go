package maintenanceui

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

func TestBackgroundControlRespondsWhileTargetLeaseIsBusyAndSurvivesRestart(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "batch", "transcode_batch", "waiting_decision", map[string]any{"paths": []string{"/media/one.mkv"}}, nil, map[string]any{"paused": true})
	ok, err := st.ClaimActionExecution("batch", "slow-step", time.Now(), time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	body := `{"name":"action_cancel","background":true,"key":"cancel-once","arguments":{"id":"batch","reason":"test"}}`
	start := time.Now()
	w := request(h, "POST", "/api/maintenance/tool", body, true)
	if w.Code != 202 || time.Since(start) > 500*time.Millisecond {
		t.Fatal("admission waited for execution", w.Code, w.Body.String(), time.Since(start))
	}
	t.Logf("busy target: command admitted in %s", time.Since(start))
	var receipt map[string]string
	json.Unmarshal(w.Body.Bytes(), &receipt)
	commandID := receipt["command_id"]
	w = request(h, "POST", "/api/maintenance/tool", body, true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var again map[string]string
	json.Unmarshal(w.Body.Bytes(), &again)
	if again["command_id"] != commandID {
		t.Fatal("double click duplicated command")
	}
	batch, _ := st.GetActionInstance("batch")
	if batch.Status != "waiting_decision" {
		t.Fatal("request executed inline")
	}
	st.ReleaseActionExecution("batch", "slow-step")
	restarted := action.NewEngine(s.engine.Deps())
	if err := restarted.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	command, _ := st.GetActionInstance(commandID)
	batch, _ = st.GetActionInstance("batch")
	if command.Status != "completed" || batch.Status != "cancelled" {
		t.Fatal(command, batch)
	}
	w = request(h, "GET", "/api/maintenance/commands?id="+commandID, "", true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(h, "POST", "/api/maintenance/tool", body, true)
	json.Unmarshal(w.Body.Bytes(), &again)
	if again["command_id"] != commandID {
		t.Fatal("lost response created new command")
	}
	w = request(h, "GET", "/api/maintenance/operations?group=workflow", "", true)
	var page operationsPage
	json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Jobs) != 1 {
		t.Fatal("command leaked into media queue", w.Body.String())
	}
	if request(h, "GET", "/api/maintenance/commands?id="+commandID, "", false).Code != 401 {
		t.Fatal("unauthenticated receipt read")
	}
}

func TestBatchSettingsUsesSameCoordinatorAndFrozenSelection(t *testing.T) {
	s, h := testUI(t)
	original := seedFailedBatch(t, s, false)
	w := request(h, "GET", "/api/maintenance/batch-settings?id=failed-batch&scope=all", "", true)
	var plan map[string]any
	json.Unmarshal(w.Body.Bytes(), &plan)
	if w.Code != 200 || plan["selected"] != float64(3) {
		t.Fatal(w.Code, w.Body.String())
	}
	body := map[string]any{"id": "failed-batch", "scope": "all", "profile": "same", "selection_version": plan["selection_version"], "min_savings_percent": 10, "key": "same-batch"}
	w = request(h, "POST", "/api/maintenance/batch-settings", mustJSON(t, body), true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var receipt map[string]string
	json.Unmarshal(w.Body.Bytes(), &receipt)
	command, _ := s.engine.Deps().Store.GetActionInstance(receipt["command_id"])
	// Drive the receipt alone: the next media step has no real media fixture.
	result, err := s.engine.Resume(context.Background(), command.ID, "", nil)
	if err != nil || result.Status != "completed" {
		t.Fatal(result, err)
	}
	st := s.engine.Deps().Store
	root, _ := st.GetActionInstance("failed-batch")
	if root.ID != receipt["id"] || root.InputsJSON != mustJSON(t, original) || root.CurrentStep != 1 {
		t.Fatal("coordinator was replaced or inputs rewritten", root)
	}
	items, _ := st.ListTranscodeBatchItems(root.ID)
	if len(items) != 3 {
		t.Fatal("live catalog expanded selection", items)
	}
	for _, it := range items {
		if it.Status != "queued" || it.BatchID != root.ID {
			t.Fatal(it)
		}
	}
	instances, _ := st.ListActionInstances("", 100)
	batches := 0
	for _, inst := range instances {
		if inst.ActionName == "transcode_batch" {
			batches++
		}
	}
	if batches != 1 {
		t.Fatal("new season task created")
	}
	state := decodeOperationJSON(root.StateJSON)
	if operationMap(state["batch_item_settings"])[items[0].ItemKey] == nil {
		t.Fatal(state)
	}
	if result, err := s.engine.Resume(context.Background(), command.ID, "", nil); err != nil || result.Status != store.ActionStatusCompleted {
		t.Fatal("repeat command", err)
	}
}

func TestSkippedCalibrationSettingsRequireExplicitProfile(t *testing.T) {
	s, h := testUI(t)
	seedFailedBatch(t, s, false)
	st := s.engine.Deps().Store
	items, err := st.ListTranscodeBatchItems("failed-batch")
	if err != nil {
		t.Fatal(err)
	}
	items[0].Status = "skip"
	items[0].Reasons = []string{"shared VMAF/CAMBI calibration supports 8-bit SDR video below 45 fps; original preserved"}
	if err := st.UpdateTranscodeBatchItem(items[0]); err != nil {
		t.Fatal(err)
	}
	read := func(scope string) map[string]any {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/batch-settings?id=failed-batch&scope="+scope, "", true)
		var plan map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return plan
	}
	unfinished := read("unfinished")
	if unfinished["requires_explicit_profile"] != false || unfinished["selected"] != float64(1) {
		t.Fatal(unfinished)
	}
	all := read("all")
	if all["requires_explicit_profile"] != true {
		t.Fatal(all)
	}
	body := map[string]any{"id": "failed-batch", "scope": "all", "selection_version": all["selection_version"], "profile": "same", "key": "skip-retry"}
	w := request(h, "POST", "/api/maintenance/batch-settings", mustJSON(t, body), true)
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	rows, _ := st.ListActionInstances("", 100)
	if len(rows) != 1 {
		t.Fatal("invalid retry submitted work", len(rows))
	}
	body["profile"] = "general-hevc"
	w = request(h, "POST", "/api/maintenance/batch-settings", mustJSON(t, body), true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestBatchSettingsReadsCurrentLimitsAndCanPreserveMixedValues(t *testing.T) {
	s, h := testUI(t)
	seedFailedBatch(t, s, false)
	st := s.engine.Deps().Store
	inst, _ := st.GetActionInstance("failed-batch")
	inst.StateJSON = mustJSON(t, map[string]any{"batch_item_settings": map[string]any{"epfile-101": map[string]any{"min_savings_percent": 10, "max_size_increase_percent": 2}, "epfile-103": map[string]any{"min_savings_percent": 10, "max_size_increase_percent": 2}}})
	if err := st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	read := func(scope string) map[string]any {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/batch-settings?id=failed-batch&scope="+scope, "", true)
		var p map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return p
	}
	unfinished := read("unfinished")
	settings := unfinished["settings"].(map[string]any)
	if settings["min_savings_percent"] != float64(10) || settings["max_size_increase_percent"] != float64(2) || settings["mixed_limits"] != false {
		t.Fatal(settings)
	}
	all := read("all")
	settings = all["settings"].(map[string]any)
	if settings["mixed_limits"] != true || settings["min_savings_percent"] != nil || settings["max_size_increase_percent"] != nil {
		t.Fatal(settings)
	}
	w := request(h, "POST", "/api/maintenance/batch-settings", mustJSON(t, map[string]any{"id": "failed-batch", "scope": "all", "selection_version": all["selection_version"], "profile": "general-hevc", "preserve_limits": true, "key": "preserve-mixed"}), true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var receipt map[string]string
	json.Unmarshal(w.Body.Bytes(), &receipt)
	command, _ := st.GetActionInstance(receipt["command_id"])
	commandSettings := decodeOperationJSON(command.InputsJSON)["settings"].(map[string]any)
	if _, exists := commandSettings["min_savings_percent"]; exists {
		t.Fatal("limit override leaked into request", commandSettings)
	}
	res, err := s.engine.Resume(context.Background(), command.ID, "", nil)
	if err != nil || res.Status != "completed" {
		t.Fatal(res, err)
	}
	current := read("all")["settings"].(map[string]any)
	profiles := current["current_profiles"].([]any)
	if len(profiles) != 1 || operationMap(profiles[0])["name"] != "general-hevc" || operationMap(profiles[0])["files"] != float64(3) {
		t.Fatal("saved profile not visible", current)
	}
	w = request(h, "GET", "/api/maintenance/batch-items?id=failed-batch", "", true)
	var fileView struct {
		Items []batchItemView `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &fileView) != nil || len(fileView.Items) != 3 {
		t.Fatal("file settings unavailable", w.Body.String())
	}
	for _, item := range fileView.Items {
		if item.RequestedProfile != "general-hevc" {
			t.Fatal("new settings hidden from file details", item)
		}
	}
	if current["mixed_limits"] != true {
		t.Fatal("per-file limits were replaced", current)
	}
	next := read("all")
	w = request(h, "POST", "/api/maintenance/batch-settings", mustJSON(t, map[string]any{"id": "failed-batch", "scope": "all", "selection_version": next["selection_version"], "profile": "same", "min_savings_percent": 12, "preserve_growth": true, "key": "edit-savings-only"}), true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &receipt)
	res, err = s.engine.Resume(context.Background(), receipt["command_id"], "", nil)
	if err != nil || res.Status != "completed" {
		t.Fatal(res, err)
	}
	current = read("all")["settings"].(map[string]any)
	if current["min_savings_percent"] != float64(12) || current["max_size_increase_percent"] != nil || current["mixed_limits"] != true {
		t.Fatal("editing savings changed per-file growth limits", current)
	}
}

func TestBatchCandidateReviewHTTPAdmissionIsAuthenticatedFastAndIdempotent(t *testing.T) {
	s, h := testUI(t)
	s.cfg.AllowDestructive = true
	st := s.engine.Deps().Store
	seedOperation(t, st, "finished", "transcode_batch", "completed", map[string]any{"paths": []string{"/media/file.mkv"}}, nil, nil)
	w := request(h, "GET", "/api/maintenance/batch-candidates?id=finished", "", true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if request(h, "GET", "/api/maintenance/batch-candidates?id=finished", "", false).Code != 401 {
		t.Fatal("unauthenticated review")
	}
	if request(h, "POST", "/api/maintenance/batch-candidates", `{"id":"finished","decision":"approve","key":"review-once"}`, true).Code != 400 {
		t.Fatal("approval without digest admitted")
	}
	ok, err := st.ClaimActionExecution("finished", "slow-check", time.Now(), time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer st.ReleaseActionExecution("finished", "slow-check")
	body := `{"id":"finished","version":"frozen-review","item_keys":["file"],"key":"review-once"}`
	start := time.Now()
	w = request(h, "POST", "/api/maintenance/batch-candidates", body, true)
	if w.Code != 202 || time.Since(start) > 500*time.Millisecond {
		t.Fatal("review waited for target execution", w.Code, w.Body.String())
	}
	var receipt, again map[string]string
	json.Unmarshal(w.Body.Bytes(), &receipt)
	w = request(h, "POST", "/api/maintenance/batch-candidates", body, true)
	json.Unmarshal(w.Body.Bytes(), &again)
	if w.Code != 202 || receipt["command_id"] != again["command_id"] {
		t.Fatal("repeated review duplicated command", w.Body.String())
	}
	parent, _ := st.GetActionInstance("finished")
	if parent.Status != "completed" {
		t.Fatal("HTTP request executed replacement inline")
	}
	command, _ := st.GetActionInstance(receipt["command_id"])
	inputs := decodeOperationJSON(command.InputsJSON)
	if inputs["kind"] != "prepare_batch_promotion" || inputs["version"] != "frozen-review" {
		t.Fatal(inputs)
	}
	if request(h, "POST", "/api/maintenance/batch-candidates", body, false).Code != 401 {
		t.Fatal("unauthenticated review mutation")
	}
	s.cfg.AllowDestructive = false
	if request(h, "POST", "/api/maintenance/batch-candidates", body, true).Code != 403 {
		t.Fatal("disabled replacement admitted")
	}
}
