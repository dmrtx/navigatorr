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
