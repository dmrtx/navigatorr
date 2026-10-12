package maintenanceui

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

func seedActionableFiles(t *testing.T, s *Server) {
	t.Helper()
	st := s.engine.Deps().Store
	seedOperation(t, st, "batch", "transcode_batch", "waiting_decision", map[string]any{"mode": "size"}, nil, nil)
	for _, status := range []string{"ready", "skip", "wait"} {
		outputs := map[string]any{}
		childStatus := "completed"
		if status == "ready" {
			outputs = map[string]any{"original_intact": true, "result": map[string]any{"size_bytes": 100}, "original": map[string]any{"size_bytes": 200}}
		}
		if status == "skip" {
			outputs = map[string]any{"skip_transcode": true, "reason_code": "quality_not_met"}
		}
		if status == "wait" {
			childStatus = "waiting_decision"
		}
		path := "/media/" + status
		seedOperation(t, st, status, "transcode_media", childStatus, map[string]any{"parent_action_id": "batch", "path": path}, outputs, nil)
		itemStatus := childStatus
		if status == "skip" {
			itemStatus = "skip"
		}
		if status == "wait" {
			child, _ := st.GetActionInstance(status)
			child.WaitingReason = "Automatic search exhausted; original preserved"
			child.WaitingOptionsJSON = `[{"decision":"cancel"}]`
			child.StateJSON = `{"reason_code":"search_budget_exhausted"}`
			if err := st.UpdateActionInstance(*child); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: status, FilePath: path, DisplayLabel: status, ChildActionID: status, Status: itemStatus, Decision: "transcode", Reasons: []string{"per-source bounded samples required before full encode"}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBatchFilesExposeCurrentReasonsAndCandidatesDuringDecision(t *testing.T) {
	s, h := testUI(t)
	seedActionableFiles(t, s)
	read := func() map[string]batchItemView {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/batch-items?id=batch", "", true)
		var page struct{ Items []batchItemView }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		views := map[string]batchItemView{}
		for _, item := range page.Items {
			views[item.ItemKey] = item
		}
		return views
	}
	views := read()
	if !views["ready"].CandidateReady || !views["skip"].CanReconfigure || views["skip"].ReasonCode != "quality_not_met" || views["wait"].WaitingReason != "Automatic search exhausted; original preserved" || views["wait"].Mode != "size" {
		t.Fatal(views)
	}
	w := request(h, "GET", "/api/maintenance/operations?id=wait", "", true)
	var page operationsPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 || page.Jobs[0]["parent_item_key"] != "wait" {
		t.Fatal(w.Code, w.Body.String())
	}
	// Current child outcomes supersede an old coordinator snapshot.
	st := s.engine.Deps().Store
	item, _ := st.GetTranscodeBatchItem("batch", "ready")
	item.Status = "waiting_decision"
	st.UpdateTranscodeBatchItem(*item)
	if read()["ready"].Status != "completed" {
		t.Fatal("old waiting status hides ready candidate")
	}
	// A queued new attempt must not offer its old candidate for replacement.
	parent, _ := st.GetActionInstance("batch")
	parent.StateJSON = `{"batch_retry_pending":{"ready":true}}`
	st.UpdateActionInstance(*parent)
	if read()["ready"].CandidateReady {
		t.Fatal("old candidate offered during settings change")
	}
	w = request(h, "GET", "/api/maintenance/operations?id=ready", "", true)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Jobs[0]["candidate_ready"] != false {
		t.Fatal(w.Code, w.Body.String())
	}
	parent.StateJSON = `{}`
	st.UpdateActionInstance(*parent)
	// A reserved replacement offers its existing job, never another replacement.
	seedOperation(t, st, "replacement", "promote_transcode_candidate", "waiting_decision", map[string]any{"transcode_action_id": "ready"}, nil, nil)
	prior, _ := st.GetActionInstance("replacement")
	prior.IdempotencyKey = "promote:filesystem:ready"
	st.UpdateActionInstance(*prior)
	views = read()
	if views["ready"].CandidateReady || views["ready"].CanReconfigure || views["ready"].ReplacementActionID != "replacement" || views["ready"].ReplacementStatus != "waiting_decision" {
		t.Fatal(views["ready"])
	}
	// Child data outside the durable file ownership is not exposed.
	child, _ := st.GetActionInstance("wait")
	child.InputsJSON = `{"parent_action_id":"other","path":"/private/file"}`
	child.WaitingReason = "PRIVATE_WAIT_REASON"
	st.UpdateActionInstance(*child)
	if view := read()["wait"]; view.WaitingReason != "" || view.ReasonCode != "" {
		t.Fatal("unrelated child exposed", view)
	}
}

func TestBatchFileSettingsCommandChangesOnlyReviewedFile(t *testing.T) {
	for _, key := range []string{"skip", "wait"} {
		t.Run(key, func(t *testing.T) {
			s, h := testUI(t)
			seedActionableFiles(t, s)
			w := request(h, "GET", "/api/maintenance/batch-settings?id=batch&scope=file&item_key="+key, "", true)
			plan := map[string]any{}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan["selected"] != float64(1) || plan["item_key"] != key {
				t.Fatal(w.Code, w.Body.String())
			}
			if key == "wait" && plan["decision_version"] == nil {
				t.Fatal("waiting decision not frozen", plan)
			}
			st := s.engine.Deps().Store
			before, _ := st.GetActionInstance("ready")
			body := map[string]any{"id": "batch", "scope": "file", "item_key": key, "mode": "quality", "selection_version": plan["selection_version"], "decision_version": plan["decision_version"], "key": "one-file"}
			w = request(h, "POST", "/api/maintenance/batch-settings", mustJSON(t, body), true)
			receipt := map[string]string{}
			if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
				t.Fatal(w.Code, w.Body.String())
			}
			result, err := s.engine.Resume(context.Background(), receipt["command_id"], "", nil)
			if err != nil || result.Status != action.StatusCompleted {
				t.Fatal(result, err)
			}
			items, _ := st.ListTranscodeBatchItems("batch")
			for _, item := range items {
				if item.ItemKey == key && (item.Status != "queued" || item.ChildActionID != "") {
					t.Fatal("selected file did not change", item)
				}
				if item.ItemKey == "ready" && (item.Status != "completed" || item.ChildActionID != "ready") {
					t.Fatal("ready candidate changed", item)
				}
			}
			after, _ := st.GetActionInstance("ready")
			if after.InputsJSON != before.InputsJSON || after.OutputsJSON != before.OutputsJSON {
				t.Fatal("completed candidate changed")
			}
			for _, query := range []string{"&scope=file", "&scope=file&item_key=missing"} {
				if w := request(h, "GET", "/api/maintenance/batch-settings?id=batch"+query, "", true); w.Code == 200 {
					t.Fatal("file selection expanded", w.Body.String())
				}
			}
		})
	}
}
