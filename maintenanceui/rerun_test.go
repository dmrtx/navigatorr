package maintenanceui

import (
	"encoding/json"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func TestRerunStartsANewActionWithTheSameInputs(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	inputs := map[string]any{"path": "/media/Show/S01E01.mkv", "mode": "size", "replace_original": false}
	noWinner := map[string]any{"reason_code": "no_suitable_candidate"}
	// An earlier attempt's key must never be accepted for a new one.
	encode := func(v map[string]any) string { b, _ := json.Marshal(v); return string(b) }
	if err := st.CreateActionInstance(store.ActionInstance{ID: "round-1", ActionName: "transcode_media", Status: "completed", InputsJSON: encode(inputs), OutputsJSON: "{}", StateJSON: encode(noWinner), IdempotencyKey: "earlier-key"}); err != nil {
		t.Fatal(err)
	}
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

	// Another attempt needs its own key and a finished earlier attempt on the file.
	created.Status = "cancelled"
	if err := st.UpdateActionInstance(*created); err != nil {
		t.Fatal(err)
	}
	if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"round-1","key":"attempt-two"}`, true); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	} else {
		var second struct{ ID string }
		json.Unmarshal(w.Body.Bytes(), &second)
		if second.ID == started.ID {
			t.Fatal("second attempt reused the first action")
		}
	}
	if before.IdempotencyKey != "earlier-key" {
		t.Fatal("old key was not seeded", before.IdempotencyKey)
	}
	if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"round-1","key":"earlier-key"}`, true); w.Code != 400 {
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

func TestRerunReplaysTheSameKeyAndRefusesAnActiveJobOnTheSameFile(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	inputs := map[string]any{"path": "/media/Show/S02E01.mkv", "mode": "size"}
	seedOperation(t, st, "stopped", "transcode_media", "cancelled", inputs, nil, nil)
	post := func(key string) (int, string) {
		w := request(h, "POST", "/api/maintenance/rerun", `{"id":"stopped","key":"`+key+`"}`, true)
		var out struct{ ID string }
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.ID
	}
	code, first := post("click-one")
	if code != 202 || first == "" {
		t.Fatal(code, first)
	}
	// Same key after a lost response: the same action, not a second job or an active-job refusal.
	if code, again := post("click-one"); code != 202 || again != first {
		t.Fatal("replay created or refused", code, again, first)
	}
	// A different key while the first job is still active is refused.
	w := request(h, "POST", "/api/maintenance/rerun", `{"id":"stopped","key":"click-two"}`, true)
	if w.Code != 409 {
		t.Fatal("duplicate active job accepted", w.Code, w.Body.String())
	}
	active, _ := st.GetActionInstance(first)
	if active == nil || active.Status == "completed" {
		t.Fatal("first job should still be active", active)
	}
	// Another file is unaffected.
	seedOperation(t, st, "other", "transcode_media", "cancelled", map[string]any{"path": "/media/Show/S02E02.mkv", "mode": "size"}, nil, nil)
	if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"other","key":"click-three"}`, true); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Once the first job has finished, the file can be run again.
	active.Status = "cancelled"
	if err := st.UpdateActionInstance(*active); err != nil {
		t.Fatal(err)
	}
	if code, next := post("click-four"); code != 202 || next == first || next == "" {
		t.Fatal("finished file could not be run again", code, next)
	}
}

func TestRerunRefusesAJobWithAReplacement(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	inputs := map[string]any{"path": "/media/Show/S03E01.mkv", "mode": "size"}
	seedOperation(t, st, "with-replacement", "transcode_media", "cancelled", inputs, nil, nil)
	seedOperation(t, st, "promotion", "promote_transcode_candidate", "running", map[string]any{"transcode_action_id": "with-replacement"}, nil, nil)
	if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"with-replacement","key":"k-replacement"}`, true); w.Code != 409 {
		t.Fatal("replacement ignored", w.Code, w.Body.String())
	}
	got := request(h, "GET", "/api/maintenance/operations?id=with-replacement", "", true).Body.String()
	if json.Valid([]byte(got)) && containsRerun(got) {
		t.Fatal("replacement job advertises rerun", got)
	}
}

func containsRerun(body string) bool {
	var page struct {
		Jobs []map[string]any `json:"jobs"`
	}
	return json.Unmarshal([]byte(body), &page) == nil && len(page.Jobs) == 1 && page.Jobs[0]["rerun"] != nil
}

// Run again keeps the media engine choice the earlier policy froze: a size job
// from before the option existed stays on x265 instead of taking the default.
func TestRerunCarriesTheFrozenMediaEngineChoice(t *testing.T) {
	for name, tc := range map[string]struct {
		policy map[string]any
		inputs map[string]any
		allow  any
		margin any
	}{
		"frozen without the engine": {map[string]any{"mode": "size", "size": map[string]any{"min_savings_percent": 15}}, map[string]any{}, false, nil},
		"frozen with the engine":    {map[string]any{"mode": "size", "size": map[string]any{"hardware_search": map[string]any{"margin_percent": float64(30)}}}, map[string]any{}, true, float64(30)},
		"said so explicitly":        {map[string]any{"mode": "size", "size": map[string]any{}}, map[string]any{"allow_hardware": true}, true, nil},
		"nothing frozen yet":        {nil, map[string]any{}, nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			s, h := testUI(t)
			st := s.engine.Deps().Store
			inputs := map[string]any{"path": "/media/Show/S01E01.mkv", "mode": "size", "replace_original": false}
			for k, v := range tc.inputs {
				inputs[k] = v
			}
			state := map[string]any{"reason_code": "no_suitable_candidate"}
			if tc.policy != nil {
				state["mode_policy"] = tc.policy
			}
			seedOperation(t, st, "earlier", "transcode_media", "completed", inputs, map[string]any{"reason_code": "no_suitable_candidate"}, state)
			w := request(h, "POST", "/api/maintenance/rerun", `{"id":"earlier","key":"again-one"}`, true)
			if w.Code != 202 {
				t.Fatal(w.Code, w.Body.String())
			}
			var started struct{ ID string }
			json.Unmarshal(w.Body.Bytes(), &started)
			created, _ := st.GetActionInstance(started.ID)
			copied := decodeOperationJSON(created.InputsJSON)
			if got, said := copied["allow_hardware"]; got != tc.allow || said != (tc.allow != nil) {
				t.Fatalf("allow_hardware %v (set %v), want %v", got, said, tc.allow)
			}
			if got := copied["hardware_margin_percent"]; got != tc.margin {
				t.Fatalf("hardware_margin_percent %v, want %v", got, tc.margin)
			}
		})
	}
}

// Run again keeps a size budget a goal change froze, and one the job named.
func TestRerunCarriesTheSizeBudget(t *testing.T) {
	for name, tc := range map[string]struct {
		policy  map[string]any
		inputs  map[string]any
		percent any
		bytes   any
	}{
		"frozen percent":  {map[string]any{"mode": "size", "size": map[string]any{"size_budget": map[string]any{"percent": float64(33)}}}, map[string]any{}, float64(33), nil},
		"frozen bytes":    {map[string]any{"mode": "size", "size": map[string]any{"size_budget": map[string]any{"bytes": float64(2e9)}}}, map[string]any{}, nil, float64(2e9)},
		"frozen siblings": {map[string]any{"mode": "size", "size": map[string]any{"size_budget": map[string]any{"siblings": map[string]any{"multiplier": float64(2), "minimum_siblings": float64(3), "max_probed": float64(60)}}}}, map[string]any{}, nil, nil},
		"named in input":  {map[string]any{"mode": "size", "size": map[string]any{}}, map[string]any{"size_budget_percent": float64(40)}, float64(40), nil},
		"no budget":       {map[string]any{"mode": "size", "size": map[string]any{}}, map[string]any{}, nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			s, h := testUI(t)
			st := s.engine.Deps().Store
			inputs := map[string]any{"path": "/media/Show/S01E01.mkv", "mode": "size", "replace_original": false}
			for k, v := range tc.inputs {
				inputs[k] = v
			}
			state := map[string]any{"reason_code": "no_suitable_candidate", "mode_policy": tc.policy}
			seedOperation(t, st, "earlier", "transcode_media", "completed", inputs, map[string]any{"reason_code": "no_suitable_candidate"}, state)
			w := request(h, "POST", "/api/maintenance/rerun", `{"id":"earlier","key":"again-one"}`, true)
			if w.Code != 202 {
				t.Fatal(w.Code, w.Body.String())
			}
			var started struct{ ID string }
			json.Unmarshal(w.Body.Bytes(), &started)
			created, _ := st.GetActionInstance(started.ID)
			copied := decodeOperationJSON(created.InputsJSON)
			if name == "frozen siblings" {
				if copied["size_budget_source"] != "siblings" || copied["size_budget_sibling_multiplier"] != float64(2) {
					t.Fatalf("sibling budget not carried: %v", copied)
				}
				return
			}
			if copied["size_budget_percent"] != tc.percent || copied["size_budget_bytes"] != tc.bytes {
				t.Fatalf("budget %v / %v, want %v / %v", copied["size_budget_percent"], copied["size_budget_bytes"], tc.percent, tc.bytes)
			}
		})
	}
}
