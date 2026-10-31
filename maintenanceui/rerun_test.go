package maintenanceui

import (
	"encoding/json"
	"net/http"
	"strings"
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

// A media engine size budget encode that fit but missed the quality floor
// offers one more try with the media engine off: a new action with the same
// inputs plus allow_hardware=false. Any other job refuses it.
func TestRerunWithoutHardwareAfterABelowFloorBudgetEncode(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	inputs := map[string]any{"path": "/media/Show/S01E01.mkv", "mode": "size", "size_budget_percent": float64(40), "allow_hardware": true, "hardware_margin_percent": float64(30)}
	budget := map[string]any{"budget_bytes": float64(4e9), "outcome": "below_quality_floor", "retry_without_hardware": true, "encoder_choice": map[string]any{"video_codec": "hevc_videotoolbox"}}
	seedOperation(t, st, "below-floor", "transcode_media", "completed", inputs, map[string]any{"reason_code": "quality_not_met"}, map[string]any{"mode": "size", "reason_code": "quality_not_met", "size_budget": budget})
	seedOperation(t, st, "no-offer", "transcode_media", "completed", map[string]any{"path": "/media/Show/S01E02.mkv", "mode": "size"}, map[string]any{"reason_code": "quality_not_met"}, map[string]any{"mode": "size", "reason_code": "quality_not_met", "size_budget": map[string]any{"budget_bytes": float64(4e9), "outcome": "below_quality_floor"}})

	w := request(h, "GET", "/api/maintenance/operations?id=below-floor", "", true)
	var page struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	size := operationMap(page.Jobs[0]["size_mode"])
	shown := operationMap(size["size_budget"])
	if page.Jobs[0]["rerun"] == nil || shown["retry_without_hardware"] != true || operationMap(shown["encoder_choice"])["video_codec"] != "hevc_videotoolbox" || size["encoder"] != "hevc_videotoolbox" || size["hardware"] != true {
		t.Fatalf("job view %v", page.Jobs[0])
	}

	if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"no-offer","key":"x265-refused","without_hardware":true}`, true); w.Code != 409 {
		t.Fatal("a job that offered no retry accepted it", w.Code, w.Body.String())
	}
	w = request(h, "POST", "/api/maintenance/rerun", `{"id":"below-floor","key":"x265-one","without_hardware":true}`, true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var started struct{ ID string }
	json.Unmarshal(w.Body.Bytes(), &started)
	created, _ := st.GetActionInstance(started.ID)
	if created == nil || created.ActionName != "transcode_media" || created.IdempotencyKey != "x265-one" {
		t.Fatalf("new action %+v", created)
	}
	copied := decodeOperationJSON(created.InputsJSON)
	if copied["allow_hardware"] != false || copied["hardware_margin_percent"] != nil || copied["mode"] != "size" || copied["size_budget_percent"] != float64(40) || copied["path"] != "/media/Show/S01E01.mkv" {
		t.Fatalf("inputs %v", copied)
	}
	// A ladder whose every step overshot the budget offers x265 too.
	seedOperation(t, st, "over-budget", "transcode_media", "completed", map[string]any{"path": "/media/Show/S01E03.mkv", "mode": "size", "size_budget_percent": float64(40)}, map[string]any{"reason_code": "size_budget_exceeded"}, map[string]any{"mode": "size", "reason_code": "size_budget_exceeded", "size_budget": map[string]any{"budget_bytes": float64(4e9), "outcome": "size_budget_exceeded", "retry_without_hardware": true}})
	if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"over-budget","key":"x265-two","without_hardware":true}`, true); w.Code != 202 {
		t.Fatal("over-budget ladder refused the x265 retry", w.Code, w.Body.String())
	}
	// An overshoot the record did not offer x265 for stays refused.
	seedOperation(t, st, "over-no-offer", "transcode_media", "completed", map[string]any{"path": "/media/Show/S01E04.mkv", "mode": "size", "size_budget_percent": float64(40)}, map[string]any{"reason_code": "size_budget_exceeded"}, map[string]any{"mode": "size", "reason_code": "size_budget_exceeded", "size_budget": map[string]any{"budget_bytes": float64(4e9), "outcome": "size_budget_exceeded"}})
	if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"over-no-offer","key":"x265-three","without_hardware":true}`, true); w.Code != 409 {
		t.Fatal("x265 retry accepted without the record's offer", w.Code, w.Body.String())
	}
	// The earlier action is untouched and a plain run again still keeps the engine on.
	if before, _ := st.GetActionInstance("below-floor"); decodeOperationJSON(before.InputsJSON)["allow_hardware"] != true {
		t.Fatal("the earlier action was changed")
	}
}

func rerunJobView(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	w := request(h, "GET", "/api/maintenance/operations?id="+id, "", true)
	var page struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	return page.Jobs[0]
}

// A standalone file skipped because the goal is wrong for its source offers the
// goal that handles it, and starts a NEW job under that goal from the
// goal-neutral inputs alone.
func TestRerunRemedyStartsTheRightGoalWithoutTheOldGoalsSettings(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	sizeInputs := map[string]any{"path": "/media/movie.mkv", "mode": "size", "replace_original": false, "is_anime": true, "media_type": "movie", "allow_hardware": true, "hardware_margin_percent": float64(30), "size_budget_percent": float64(40), "allow_slow_preset": true}
	seedOperation(t, st, "hdr", "transcode_media", "completed", sizeInputs, map[string]any{"skip_transcode": true, "reason_code": "hdr_requires_compact"}, map[string]any{"mode": "size", "mode_policy": map[string]any{"mode": "size", "size": map[string]any{"hardware_search": map[string]any{"margin_percent": float64(30)}, "size_budget": map[string]any{"percent": float64(33)}}}})
	seedOperation(t, st, "sdr", "transcode_media", "completed", map[string]any{"path": "/media/clip.mkv", "mode": "compact", "max_height": float64(1080)}, map[string]any{"skip_transcode": true, "reason_code": "compact_not_needed"}, nil)

	if got := operationMap(rerunJobView(t, h, "hdr")["rerun"])["remedy_mode"]; got != "compact" {
		t.Fatalf("hdr remedy %v", got)
	}
	if got := operationMap(rerunJobView(t, h, "sdr")["rerun"])["remedy_mode"]; got != "size" {
		t.Fatalf("sdr remedy %v", got)
	}

	w := request(h, "POST", "/api/maintenance/rerun", `{"id":"hdr","key":"remedy-one","mode":"compact"}`, true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var started struct{ ID string }
	json.Unmarshal(w.Body.Bytes(), &started)
	created, _ := st.GetActionInstance(started.ID)
	if created == nil || started.ID == "hdr" || created.IdempotencyKey != "remedy-one" {
		t.Fatalf("new action %+v", created)
	}
	copied := decodeOperationJSON(created.InputsJSON)
	want := map[string]any{"path": "/media/movie.mkv", "mode": "compact", "replace_original": false, "is_anime": true, "media_type": "movie"}
	if len(copied) != len(want) {
		t.Fatalf("inputs carried goal-specific keys: %v", copied)
	}
	for k, v := range want {
		if copied[k] != v {
			t.Fatalf("input %s = %v, want %v (%v)", k, copied[k], v, copied)
		}
	}
	// A lost response replays the same key and returns the same job.
	if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"hdr","key":"remedy-one","mode":"compact"}`, true); w.Code != 202 || !strings.Contains(w.Body.String(), started.ID) {
		t.Fatal("replay", w.Code, w.Body.String())
	}
	// The file now has an active job: a different key is refused.
	if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"hdr","key":"remedy-two","mode":"compact"}`, true); w.Code != 409 {
		t.Fatal("active job ignored", w.Code, w.Body.String())
	}
	// The remedy ran: the old skip no longer offers it.
	if got := rerunJobView(t, h, "hdr")["rerun"]; got != nil {
		t.Fatalf("remedy offered again after it ran: %v", got)
	}
	if before, _ := st.GetActionInstance("hdr"); decodeOperationJSON(before.InputsJSON)["allow_hardware"] != true {
		t.Fatal("the earlier action was changed")
	}

	w = request(h, "POST", "/api/maintenance/rerun", `{"id":"sdr","key":"remedy-three","mode":"size"}`, true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &started)
	created, _ = st.GetActionInstance(started.ID)
	if copied := decodeOperationJSON(created.InputsJSON); copied["mode"] != "size" || copied["path"] != "/media/clip.mkv" || copied["max_height"] != nil || len(copied) != 2 {
		t.Fatalf("size inputs %v", copied)
	}
}

func TestRerunRemedyRefusesTheWrongGoalAndTheSameGoal(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "hdr", "transcode_media", "completed", map[string]any{"path": "/media/movie.mkv", "mode": "size"}, map[string]any{"skip_transcode": true, "reason_code": "hdr_requires_compact"}, nil)
	seedOperation(t, st, "plain", "transcode_media", "cancelled", map[string]any{"path": "/media/other.mkv", "mode": "size"}, nil, nil)
	seedOperation(t, st, "child", "transcode_media", "completed", map[string]any{"path": "/media/child.mkv", "mode": "size", "parent_action_id": "batch"}, map[string]any{"reason_code": "hdr_requires_compact"}, nil)
	seedOperation(t, st, "gate", "transcode_media", "completed", map[string]any{"path": "/media/gate.mkv", "mode": "size"}, map[string]any{"reason_code": "unsupported_source"}, nil)
	for name, tc := range map[string]struct{ body string }{
		"another goal":              {`{"id":"hdr","key":"k-1","mode":"quality"}`},
		"the goal it already ran":   {`{"id":"hdr","key":"k-2","mode":"size"}`},
		"same goal without a mode":  {`{"id":"hdr","key":"k-3"}`},
		"a mode for a normal job":   {`{"id":"plain","key":"k-4","mode":"compact"}`},
		"a batch child":             {`{"id":"child","key":"k-5","mode":"compact"}`},
		"a source no goal can fix":  {`{"id":"gate","key":"k-6","mode":"compact"}`},
		"x265 combined with a goal": {`{"id":"hdr","key":"k-7","mode":"compact","without_hardware":true}`},
	} {
		if w := request(h, "POST", "/api/maintenance/rerun", tc.body, true); w.Code != 409 {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	for _, id := range []string{"child", "gate"} {
		if rerunJobView(t, h, id)["rerun"] != nil {
			t.Fatalf("%s advertises a remedy", id)
		}
	}
	if rerunJobView(t, h, "plain")["rerun"] == nil || operationMap(rerunJobView(t, h, "plain")["rerun"])["remedy_mode"] != nil {
		t.Fatal("a cancelled job must offer a plain rerun only")
	}
}

// Sample tests were removed, and a job whose own settings drove the sample
// search would be refused if started again: neither offers "Run again". The
// latter names why, so the goal form can be offered in its place.
func TestRerunIsNotOfferedForTheRemovedSearch(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "sample-test", "benchmark_transcode", "cancelled", map[string]any{"path": "/media/Show/S03E01.mkv"}, nil, nil)
	seedOperation(t, st, "searched", "transcode_media", "completed", map[string]any{"path": "/media/Show/S03E02.mkv", "profile": "general-hevc", "metric": "vmaf"}, nil, map[string]any{"reason_code": "no_suitable_candidate"})
	if view := rerunJobView(t, h, "sample-test"); view["rerun"] != nil || view["rerun_refused"] != nil {
		t.Fatalf("a sample test offers a rerun: %v", view)
	}
	if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"sample-test","key":"sample-again"}`, true); w.Code != 409 {
		t.Fatal("a sample test was started again", w.Code, w.Body.String())
	}
	view := rerunJobView(t, h, "searched")
	if view["rerun"] != nil || !strings.Contains(operationString(view["rerun_refused"]), "sample search") {
		t.Fatalf("a searched job offers Run again: %v", view)
	}
	if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"searched","key":"searched-again"}`, true); w.Code != 409 || !strings.Contains(w.Body.String(), "sample search") {
		t.Fatal("a searched job was started again", w.Code, w.Body.String())
	}
}

// A size goal whose every VideoToolbox quality step missed the limits offers
// the same x265 retry a below-floor budget encode does.
func TestRerunWithoutHardwareAfterAVideoToolboxLadderMissed(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	inputs := map[string]any{"path": "/media/Show/S04E01.mkv", "mode": "size", "allow_hardware": true, "hardware_margin_percent": float64(30)}
	ladder := map[string]any{"video_codec": "hevc_videotoolbox", "quality_values": []any{60, 65, 70}, "rung": 2, "outcome": "quality_not_met", "retry_without_hardware": true}
	seedOperation(t, st, "ladder-missed", "transcode_media", "completed", inputs, map[string]any{"reason_code": "quality_not_met"}, map[string]any{"mode": "size", "reason_code": "quality_not_met", "quality_ladder": ladder})
	rerun := operationMap(rerunJobView(t, h, "ladder-missed")["rerun"])
	if rerun["without_hardware"] != true {
		t.Fatalf("rerun summary %v", rerun)
	}
	w := request(h, "POST", "/api/maintenance/rerun", `{"id":"ladder-missed","key":"ladder-x265","without_hardware":true}`, true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var started struct{ ID string }
	json.Unmarshal(w.Body.Bytes(), &started)
	created, _ := st.GetActionInstance(started.ID)
	if copied := decodeOperationJSON(created.InputsJSON); copied["allow_hardware"] != false || copied["hardware_margin_percent"] != nil || copied["mode"] != "size" {
		t.Fatalf("inputs %v", copied)
	}
	// An x265 ladder that missed offers nothing more.
	ladder["video_codec"], ladder["retry_without_hardware"] = "libx265", false
	seedOperation(t, st, "x265-missed", "transcode_media", "completed", map[string]any{"path": "/media/Show/S04E02.mkv", "mode": "size", "allow_hardware": false}, map[string]any{"reason_code": "quality_not_met"}, map[string]any{"mode": "size", "reason_code": "quality_not_met", "quality_ladder": ladder})
	if operationMap(rerunJobView(t, h, "x265-missed")["rerun"])["without_hardware"] != nil {
		t.Fatal("an x265 ladder offers x265")
	}
	if w := request(h, "POST", "/api/maintenance/rerun", `{"id":"x265-missed","key":"x265-again","without_hardware":true}`, true); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
}

// A full encode that missed the goal's size limit is a finished round without a
// result: the file is offered again like any other.
func TestRerunIsOfferedAfterTheSizeLimitWasMissed(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	inputs := map[string]any{"path": "/media/Show/S01E01.mkv", "mode": "size", "replace_original": false}
	missed := map[string]any{"reason_code": "insufficient_savings", "original_intact": true, "skip_transcode": true}
	seedOperation(t, st, "missed", "transcode_media", "completed", inputs, missed, nil)
	w := request(h, "GET", "/api/maintenance/operations?id=missed", "", true)
	var page struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if operationMap(page.Jobs[0]["rerun"])["mode"] != "size" {
		t.Fatalf("no rerun offered: %v", page.Jobs[0])
	}
}

func TestRerunSummaryNamesTheAudioOnlyGoal(t *testing.T) {
	r := operationRecord{inputs: map[string]any{"path": "/media/a.mkv", "mode": "size", "audio_only": true}, state: map[string]any{}}
	if got := rerunSummary(r, nil); got["mode"] != "size" || got["audio_only"] != true {
		t.Fatalf("summary = %v", got)
	}
	r.inputs["audio_only"] = false
	if _, ok := rerunSummary(r, nil)["audio_only"]; ok {
		t.Fatal("a Save space job says audio_only")
	}
}
