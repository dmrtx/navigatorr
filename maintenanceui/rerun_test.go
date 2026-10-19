package maintenanceui

import (
	"encoding/json"
	"testing"
)

func TestRerunStartsANewActionWithTheSameInputs(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	inputs := map[string]any{"path": "/media/Show/S01E01.mkv", "mode": "size", "replace_original": false, "profile": "auto"}
	noWinner := map[string]any{"reason_code": "no_suitable_candidate"}
	seedOperation(t, st, "round-1", "transcode_media", "completed", inputs, nil, noWinner)
	seedOperation(t, st, "round-2", "transcode_media", "completed", inputs, noWinner, nil)
	seedOperation(t, st, "stopped", "transcode_media", "cancelled", inputs, nil, nil)
	seedOperation(t, st, "elsewhere", "transcode_media", "completed", map[string]any{"path": "/media/Show/S01E02.mkv", "mode": "size"}, nil, noWinner)

	read := func(id string) map[string]any {
		w := request(h, "GET", "/api/maintenance/operations?id="+id, "", true)
		var page struct {
			Jobs []map[string]any `json:"jobs"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
			t.Fatal(w.Code, w.Body.String())
		}
		return page.Jobs[0]
	}
	rerun := operationMap(read("stopped")["rerun"])
	if rerun["mode"] != "size" || rerun["replace_original"] != false || rerun["no_winner_rounds"] != float64(2) {
		t.Fatalf("rerun summary wrong: %v", rerun)
	}
	if got := operationMap(read("elsewhere")["rerun"])["no_winner_rounds"]; got != float64(1) {
		t.Fatalf("rounds leaked across files: %v", got)
	}

	before, _ := st.GetActionInstance("round-1")
	w := request(h, "POST", "/api/maintenance/rerun", `{"id":"round-1","key":"attempt-one"}`, true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var started struct{ ID string }
	json.Unmarshal(w.Body.Bytes(), &started)
	created, err := st.GetActionInstance(started.ID)
	if err != nil || created == nil || started.ID == "round-1" {
		t.Fatal("no new action", started, err)
	}
	if created.ActionName != "transcode_media" || created.IdempotencyKey != "attempt-one" {
		t.Fatal(created.ActionName, created.IdempotencyKey)
	}
	copied := decodeOperationJSON(created.InputsJSON)
	for k, v := range inputs {
		if copied[k] != v {
			t.Fatalf("input %s not copied: %v", k, copied)
		}
	}
	after, _ := st.GetActionInstance("round-1")
	if after.Status != before.Status || after.StateJSON != before.StateJSON || after.InputsJSON != before.InputsJSON {
		t.Fatal("the earlier action was changed")
	}

	// Another attempt needs its own key; reusing the earlier action's key is refused.
	if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"round-1","key":"attempt-two"}`, true); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	} else {
		var second struct{ ID string }
		json.Unmarshal(w.Body.Bytes(), &second)
		if second.ID == started.ID {
			t.Fatal("second attempt reused the first action")
		}
	}
	if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"round-1","key":"`+before.IdempotencyKey+`"}`, true); before.IdempotencyKey != "" && w.Code != 400 {
		t.Fatal("old key accepted", w.Code)
	}
	if request(h, "POST", "/api/maintenance/rerun", `{"id":"round-1","key":"bad key!"}`, true).Code != 400 {
		t.Fatal("invalid key accepted")
	}
	if request(h, "POST", "/api/maintenance/rerun", `{"id":"stopped","key":"attempt-three"}`, false).Code != 401 {
		t.Fatal("unauthenticated rerun")
	}
}

func TestRerunRefusesJobsThatAreReviewedOrOwnedByABatch(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	path := map[string]any{"path": "/media/Show/S01E03.mkv", "mode": "size"}
	seedOperation(t, st, "candidate", "transcode_media", "completed", path, map[string]any{"original_intact": true}, nil)
	seedOperation(t, st, "decision", "transcode_media", "waiting_decision", path, nil, map[string]any{"reason_code": "search_budget_exhausted"})
	seedOperation(t, st, "failed", "transcode_media", "failed", path, nil, nil)
	seedOperation(t, st, "skipped-gate", "transcode_media", "completed", path, nil, map[string]any{"reason_code": "unsupported_source"})
	seedOperation(t, st, "child", "transcode_media", "cancelled", map[string]any{"path": "/media/Show/S01E04.mkv", "mode": "size", "parent_action_id": "batch"}, nil, nil)
	seedOperation(t, st, "batch", "transcode_batch", "cancelled", map[string]any{"paths": []string{"/media/Show/S01E04.mkv"}}, nil, nil)
	for _, id := range []string{"candidate", "decision", "failed", "skipped-gate", "child", "batch", "missing"} {
		if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"`+id+`","key":"attempt-`+id+`"}`, true); w.Code != 409 && w.Code != 403 {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body.String())
		}
		w := request(h, "GET", "/api/maintenance/operations?id="+id, "", true)
		var page struct {
			Jobs []map[string]any `json:"jobs"`
		}
		if json.Unmarshal(w.Body.Bytes(), &page) == nil && len(page.Jobs) == 1 && page.Jobs[0]["rerun"] != nil {
			t.Fatalf("%s advertises rerun", id)
		}
	}
}
