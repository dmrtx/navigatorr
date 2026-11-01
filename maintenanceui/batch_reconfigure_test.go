package maintenanceui

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

func seedFailedBatch(t *testing.T, s *Server, sonarr bool) map[string]any {
	t.Helper()
	inputs := map[string]any{"profile_config": testProfileConfig(), "min_savings_percent": 15, "max_size_increase_percent": 0, "preserve_source_bit_depth": true}
	if sonarr {
		inputs["service"], inputs["series_id"], inputs["season"], inputs["promote_candidates"] = "sonarr", 10, 1, true
		inputs["episode_file_ids"] = []int{101, 102, 103, 999}
		s.cfg.AllowDestructive = true
	} else {
		inputs["paths"] = []string{"/media/one.mkv", "/media/two.mkv", "/media/three.mkv", "/media/new.mkv"}
	}
	seedOperation(t, s.engine.Deps().Store, "failed-batch", "transcode_batch", "completed", inputs, map[string]any{"series_title": "Example", "counts": map[string]int{"total": 3, "failed": 1, "cancelled": 1, "completed": 1}}, nil)
	for i, status := range []string{"failed", "completed", "cancelled"} {
		item := store.TranscodeBatchItem{BatchID: "failed-batch", ItemKey: []string{"epfile-101", "epfile-102", "epfile-103"}[i], FilePath: []string{"/media/one.mkv", "/media/two.mkv", "/media/three.mkv"}[i], DisplayLabel: []string{"One", "Two", "Three"}[i], Status: status, Decision: "transcode"}
		if err := s.engine.Deps().Store.CreateTranscodeBatchItem(item); err != nil {
			t.Fatal(err)
		}
	}
	return inputs
}

func TestReconfigureFreezesFailuresPreservesCustomSettingsAndReceipt(t *testing.T) {
	for _, sonarr := range []bool{true, false} {
		t.Run(map[bool]string{true: "sonarr", false: "folder"}[sonarr], func(t *testing.T) {
			s, h := testUI(t)
			original := seedFailedBatch(t, s, sonarr)
			w := request(h, "GET", "/api/maintenance/batch-reconfigure?id=failed-batch", "", true)
			var plan batchReconfigurePlan
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.Selected != 2 || plan.Kept != 1 || strings.Contains(w.Body.String(), "profile_config") {
				t.Fatal(w.Code, w.Body.String())
			}
			rows, _ := s.engine.Deps().Store.ListActionInstances("", 100)
			if len(rows) != 1 {
				t.Fatal("review submitted work")
			}
			body := `{"id":"failed-batch","profile":"same","min_savings_percent":10,"max_size_increase_percent":0,"key":"attempt-1"}`
			w = request(h, "POST", "/api/maintenance/batch-reconfigure", body, true)
			var receipt map[string]string
			if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
				t.Fatal(w.Code, w.Body.String())
			}
			inst, _ := s.engine.Deps().Store.GetActionInstance(receipt["id"])
			actual := decodeOperationJSON(inst.InputsJSON)
			if !reflect.DeepEqual(actual["profile_config"], decodeOperationJSON(mustJSON(t, original))["profile_config"]) || actual["min_savings_percent"] != float64(10) {
				t.Fatal("lost original settings", actual)
			}
			if sonarr {
				if !reflect.DeepEqual(actual["episode_file_ids"], []any{float64(101), float64(103)}) || actual["promote_candidates"] != true {
					t.Fatal(actual)
				}
			} else if !reflect.DeepEqual(actual["paths"], []any{"/media/one.mkv", "/media/three.mkv"}) {
				t.Fatal(actual)
			}
			w = request(h, "GET", "/api/maintenance/batch-items?id="+receipt["id"], "", true)
			var pending struct {
				Total int
				Items []batchItemView
			}
			json.Unmarshal(w.Body.Bytes(), &pending)
			if pending.Total != 2 || pending.Items[0].Status != "queued" {
				t.Fatal("pending attempt lost its selection", w.Body.String())
			}
			deps := s.engine.Deps()
			deps.Transcode = unavailableWorker{}
			s.engine = action.NewEngine(deps)
			w = request(h, "POST", "/api/maintenance/batch-reconfigure", body, true)
			var repeated map[string]string
			json.Unmarshal(w.Body.Bytes(), &repeated)
			if w.Code != 200 || repeated["id"] != receipt["id"] {
				t.Fatal("duplicate attempt", w.Code, w.Body.String())
			}
			w = request(h, "POST", "/api/maintenance/batch-reconfigure", strings.Replace(body, ":10", ":12", 1), true)
			if w.Code != 409 {
				t.Fatal("changed settings reused receipt")
			}
			old, _ := deps.Store.GetActionInstance("failed-batch")
			if !reflect.DeepEqual(decodeOperationJSON(old.InputsJSON), decodeOperationJSON(mustJSON(t, original))) {
				t.Fatal("source history changed")
			}
			rows, _ = deps.Store.ListActionInstances("", 100)
			if len(rows) != 2 {
				t.Fatal("duplicate enqueue")
			}
		})
	}
}

func TestReconfigureRejectsUnsafeSelectionAndUnavailableWorker(t *testing.T) {
	for _, scenario := range []string{"active", "preview", "empty", "child-active", "offline", "bad-profile", "zero-minimum", "negative-growth", "bad-key", "auth"} {
		t.Run(scenario, func(t *testing.T) {
			s, h := testUI(t)
			seedFailedBatch(t, s, false)
			st := s.engine.Deps().Store
			body := `{"id":"failed-batch","profile":"auto","min_savings_percent":15,"max_size_increase_percent":0,"key":"attempt-1"}`
			inst, _ := st.GetActionInstance("failed-batch")
			switch scenario {
			case "active":
				inst.Status = "waiting_external"
				st.UpdateActionInstance(*inst)
			case "preview":
				inputs := decodeOperationJSON(inst.InputsJSON)
				inputs["dry_run"] = true
				inst.InputsJSON = mustJSON(t, inputs)
				st.UpdateActionInstance(*inst)
			case "empty":
				items, _ := st.ListTranscodeBatchItems(inst.ID)
				for _, item := range items {
					item.Status = "completed"
					st.UpdateTranscodeBatchItem(item)
				}
			case "child-active":
				seedOperation(t, st, "child", "transcode_media", "waiting_external", nil, nil, nil)
				items, _ := st.ListTranscodeBatchItems(inst.ID)
				items[0].ChildActionID = "child"
				st.UpdateTranscodeBatchItem(items[0])
			case "offline":
				deps := s.engine.Deps()
				deps.Transcode = unavailableWorker{}
				s.engine = action.NewEngine(deps)
			case "bad-profile":
				body = strings.Replace(body, `"auto"`, `"does-not-exist"`, 1)
			case "zero-minimum":
				body = strings.Replace(body, ":15", ":0", 1)
			case "negative-growth":
				body = strings.Replace(body, ":0", ":-1", 1)
			case "bad-key":
				body = strings.Replace(body, "attempt-1", "bad:key", 1)
			}
			before, _ := st.ListActionInstances("", 100)
			w := request(h, "POST", "/api/maintenance/batch-reconfigure", body, scenario != "auth")
			if w.Code < 400 {
				t.Fatal(scenario, w.Code, w.Body.String())
			}
			after, _ := st.ListActionInstances("", 100)
			if len(before) != len(after) {
				t.Fatal("rejected request enqueued work")
			}
		})
	}
}

func TestReconfigureAutoClearsCustomProfile(t *testing.T) {
	s, h := testUI(t)
	seedFailedBatch(t, s, false)
	w := request(h, "POST", "/api/maintenance/batch-reconfigure", `{"id":"failed-batch","profile":"auto","key":"attempt-1"}`, true)
	var receipt map[string]string
	json.Unmarshal(w.Body.Bytes(), &receipt)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	inst, _ := s.engine.Deps().Store.GetActionInstance(receipt["id"])
	inputs := decodeOperationJSON(inst.InputsJSON)
	if inputs["profile_config"] != nil || inputs["priority"] != nil || inputs["profile"] != "auto" || inputs["min_savings_percent"] != nil {
		t.Fatal(inputs)
	}
}

func TestRepeatedCancelledAdmissionsKeepFrozenSelection(t *testing.T) {
	s, h := testUI(t)
	seedFailedBatch(t, s, true)
	source := "failed-batch"
	for i := 0; i < 3; i++ {
		w := request(h, "POST", "/api/maintenance/batch-reconfigure", `{"id":"`+source+`","profile":"same","min_savings_percent":15,"key":"repeat"}`, true)
		var receipt map[string]string
		json.Unmarshal(w.Body.Bytes(), &receipt)
		if w.Code != 202 {
			t.Fatal(i, w.Code, w.Body.String())
		}
		inst, _ := s.engine.Deps().Store.GetActionInstance(receipt["id"])
		inst.Status = store.ActionStatusCancelled
		if err := s.engine.Deps().Store.UpdateActionInstance(*inst); err != nil {
			t.Fatal(err)
		}
		w = request(h, "GET", "/api/maintenance/batch-items?id="+inst.ID, "", true)
		var files struct {
			Total int
			Items []batchItemView
		}
		json.Unmarshal(w.Body.Bytes(), &files)
		if files.Total != 2 || files.Items[0].Status != "cancelled" {
			t.Fatal(i, "lost uninspected cancelled files", w.Body.String())
		}
		source = inst.ID
	}
}

// Changing the goal to smaller files carries the media engine choice with it,
// and the other goals refuse it.
func TestGoalChangeCarriesTheHardwareChoice(t *testing.T) {
	s, h := testUI(t)
	seedFailedBatch(t, s, false)
	st := s.engine.Deps().Store
	batch, _ := st.GetActionInstance("failed-batch")
	inputs := decodeOperationJSON(batch.InputsJSON)
	delete(inputs, "profile_config")
	inputs["mode"] = "quality"
	batch.InputsJSON = mustJSON(t, inputs)
	if err := st.UpdateActionInstance(*batch); err != nil {
		t.Fatal(err)
	}
	w := request(h, "GET", "/api/maintenance/batch-settings?id=failed-batch&scope=all", "", true)
	var plan map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	post := func(key string, extra map[string]any) *httptest.ResponseRecorder {
		body := map[string]any{"id": "failed-batch", "scope": "all", "selection_version": plan["selection_version"], "key": key}
		for k, v := range extra {
			body[k] = v
		}
		return request(h, "POST", "/api/maintenance/batch-settings", mustJSON(t, body), true)
	}
	cases := []struct {
		name  string
		extra map[string]any
		code  int
	}{
		{"size, hardware off", map[string]any{"mode": "size", "allow_hardware": false}, 202},
		{"size, hardware margin", map[string]any{"mode": "size", "allow_hardware": true, "hardware_margin_percent": 25}, 202},
		{"size, nothing said", map[string]any{"mode": "size"}, 202},
		{"size, margin too large", map[string]any{"mode": "size", "allow_hardware": true, "hardware_margin_percent": 60}, 400},
		{"quality with hardware", map[string]any{"mode": "quality", "allow_hardware": true}, 400},
		{"quality, hardware off", map[string]any{"mode": "quality", "allow_hardware": false}, 202},
		{"size, budget percent", map[string]any{"mode": "size", "size_budget_percent": 33}, 202},
		{"size, budget bytes", map[string]any{"mode": "size", "size_budget_bytes": 2000000000}, 202},
		{"size, budget both", map[string]any{"mode": "size", "size_budget_percent": 33, "size_budget_bytes": 2000000000}, 400},
		{"size, budget percent too large", map[string]any{"mode": "size", "size_budget_percent": 90}, 400},
		{"size, budget bytes too small", map[string]any{"mode": "size", "size_budget_bytes": 1000}, 400},
		{"quality with a budget", map[string]any{"mode": "quality", "size_budget_percent": 33}, 400},
	}
	var receipt map[string]string
	for i, tc := range cases {
		w := post("goal-"+strconv.Itoa(i), tc.extra)
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
		if i == 0 {
			json.Unmarshal(w.Body.Bytes(), &receipt)
		}
	}
	command, _ := st.GetActionInstance(receipt["command_id"])
	if command == nil || !strings.Contains(command.InputsJSON, `"allow_hardware":false`) {
		t.Fatalf("hardware choice not carried: %+v", command)
	}
	// Once applied, reopening the dialog shows the choice that was frozen,
	// not the default.
	if result, err := s.engine.Resume(context.Background(), command.ID, "", nil); err != nil || result.Status != store.ActionStatusCompleted {
		t.Fatal(result, err)
	}
	w = request(h, "GET", "/api/maintenance/batch-settings?id=failed-batch&scope=all", "", true)
	var reopened struct {
		Settings map[string]any `json:"settings"`
	}
	if json.Unmarshal(w.Body.Bytes(), &reopened) != nil || reopened.Settings["allow_hardware"] != false {
		t.Fatal("frozen hardware choice not shown", w.Body.String())
	}
}

// A size budget given with the goal change is frozen with the policy and shown
// again when the dialog reopens.
func TestGoalChangeFreezesAndShowsTheSizeBudget(t *testing.T) {
	s, h := testUI(t)
	seedFailedBatch(t, s, false)
	st := s.engine.Deps().Store
	batch, _ := st.GetActionInstance("failed-batch")
	inputs := decodeOperationJSON(batch.InputsJSON)
	delete(inputs, "profile_config")
	inputs["mode"] = "quality"
	batch.InputsJSON = mustJSON(t, inputs)
	if err := st.UpdateActionInstance(*batch); err != nil {
		t.Fatal(err)
	}
	read := func() (map[string]any, map[string]any) {
		w := request(h, "GET", "/api/maintenance/batch-settings?id=failed-batch&scope=all", "", true)
		var plan struct {
			Settings map[string]any `json:"settings"`
			Version  string         `json:"selection_version"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return plan.Settings, map[string]any{"selection_version": plan.Version}
	}
	before, version := read()
	if before["size_budget_percent"] != nil || before["size_budget_bytes"] != nil || before["size_budget_mixed"] != false {
		t.Fatalf("a budget shown before one was set: %v", before)
	}
	body := mustJSON(t, map[string]any{"id": "failed-batch", "scope": "all", "selection_version": version["selection_version"], "key": "goal-budget", "mode": "size", "size_budget_percent": 33})
	w := request(h, "POST", "/api/maintenance/batch-settings", body, true)
	var receipt map[string]string
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	command, _ := st.GetActionInstance(receipt["command_id"])
	if command == nil || !strings.Contains(command.InputsJSON, `"size_budget_percent":33`) {
		t.Fatalf("budget not carried: %+v", command)
	}
	if result, err := s.engine.Resume(context.Background(), command.ID, "", nil); err != nil || result.Status != store.ActionStatusCompleted {
		t.Fatal(result, err)
	}
	if after, _ := read(); after["size_budget_percent"] != float64(33) || after["size_budget_mixed"] != false {
		t.Fatalf("frozen size budget not shown: %v", after)
	}
}

// The Change goal dialog shows the media engine choice the files were frozen
// with: an earlier batch is x265 only, not today's default, and files frozen
// differently say so.
func TestGoalChangeShowsTheFrozenMediaEngineChoice(t *testing.T) {
	s, h := testUI(t)
	seedFailedBatch(t, s, false)
	st := s.engine.Deps().Store
	batch, _ := st.GetActionInstance("failed-batch")
	inputs := decodeOperationJSON(batch.InputsJSON)
	delete(inputs, "profile_config")
	inputs["mode"] = "size"
	batch.InputsJSON = mustJSON(t, inputs)
	read := func(state map[string]any) map[string]any {
		batch.StateJSON = mustJSON(t, state)
		if err := st.UpdateActionInstance(*batch); err != nil {
			t.Fatal(err)
		}
		w := request(h, "GET", "/api/maintenance/batch-settings?id=failed-batch&scope=all", "", true)
		var plan struct {
			Settings map[string]any `json:"settings"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return plan.Settings
	}
	earlier := map[string]any{"mode": "size", "size": map[string]any{"min_savings_percent": 15}}
	withEngine := map[string]any{"mode": "size", "size": map[string]any{"hardware_search": map[string]any{"margin_percent": float64(30)}}}
	if got := read(map[string]any{"mode_policy": earlier}); got["allow_hardware"] != false || got["hardware_mixed"] != false {
		t.Fatalf("an earlier batch should show the engine off: %v", got)
	}
	if got := read(map[string]any{"mode_policy": withEngine}); got["allow_hardware"] != true || got["hardware_margin_percent"] != float64(30) || got["hardware_mixed"] != false {
		t.Fatalf("a frozen margin was not shown: %v", got)
	}
	// One file already changed to a different choice makes the selection mixed.
	mixed := map[string]any{"mode_policy": earlier, "batch_item_settings": map[string]any{"epfile-101": map[string]any{"mode": "size", "mode_policy": withEngine}}}
	if got := read(mixed); got["hardware_mixed"] != true {
		t.Fatalf("different choices were not reported: %v", got)
	}
	// Nothing frozen yet: the dialog falls back to the default (no stored choice).
	if got := read(map[string]any{}); got["allow_hardware"] != nil || got["hardware_mixed"] != nil {
		t.Fatalf("an unfrozen batch invented a choice: %v", got)
	}
}

// Shrink's height given with a goal change reaches the command, and the dialog
// shows the height the files were frozen with.
func TestGoalChangeCarriesAndShowsTheShrinkHeight(t *testing.T) {
	s, h := testUI(t)
	seedFailedBatch(t, s, false)
	st := s.engine.Deps().Store
	batch, _ := st.GetActionInstance("failed-batch")
	inputs := decodeOperationJSON(batch.InputsJSON)
	delete(inputs, "profile_config")
	inputs["mode"] = "compact"
	batch.InputsJSON = mustJSON(t, inputs)
	read := func(state map[string]any) (map[string]any, string) {
		batch.StateJSON = mustJSON(t, state)
		if err := st.UpdateActionInstance(*batch); err != nil {
			t.Fatal(err)
		}
		w := request(h, "GET", "/api/maintenance/batch-settings?id=failed-batch&scope=all", "", true)
		var plan struct {
			Settings map[string]any `json:"settings"`
			Version  string         `json:"selection_version"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return plan.Settings, plan.Version
	}
	at4K := map[string]any{"mode": "compact", "compact": map[string]any{"max_height": float64(2160)}}
	if got, _ := read(map[string]any{"mode_policy": at4K}); got["max_height"] != float64(2160) {
		t.Fatalf("frozen height not shown: %v", got)
	}
	// Files frozen at different heights show none.
	mixed := map[string]any{"mode_policy": at4K, "batch_item_settings": map[string]any{"epfile-101": map[string]any{"mode": "compact", "mode_policy": map[string]any{"mode": "compact", "compact": map[string]any{"max_height": float64(1080)}}}}}
	if got, _ := read(mixed); got["max_height"] != nil {
		t.Fatalf("mixed heights shown as one: %v", got)
	}
	_, version := read(map[string]any{})
	// A height with another goal, or one Shrink does not offer, is refused now.
	for _, bad := range []map[string]any{{"mode": "size", "max_height": 2160}, {"mode": "compact", "max_height": 900}} {
		bad["id"], bad["scope"], bad["selection_version"], bad["key"] = "failed-batch", "all", version, "goal-bad"
		if w := request(h, "POST", "/api/maintenance/batch-settings", mustJSON(t, bad), true); w.Code != 400 {
			t.Fatalf("%v: %d %s", bad, w.Code, w.Body.String())
		}
	}
	body := mustJSON(t, map[string]any{"id": "failed-batch", "scope": "all", "selection_version": version, "key": "goal-4k", "mode": "compact", "max_height": 2160})
	w := request(h, "POST", "/api/maintenance/batch-settings", body, true)
	var receipt map[string]string
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if command, _ := st.GetActionInstance(receipt["command_id"]); command == nil || !strings.Contains(command.InputsJSON, `"max_height":2160`) {
		t.Fatalf("height not carried: %+v", command)
	}
}

// Keep HDR given with a Shrink goal change reaches the command, and the dialog
// shows it only when every selected file was frozen with it.
func TestGoalChangeCarriesAndShowsKeepHDR(t *testing.T) {
	s, h := testUI(t)
	seedFailedBatch(t, s, false)
	st := s.engine.Deps().Store
	batch, _ := st.GetActionInstance("failed-batch")
	inputs := decodeOperationJSON(batch.InputsJSON)
	delete(inputs, "profile_config")
	inputs["mode"] = "compact"
	batch.InputsJSON = mustJSON(t, inputs)
	read := func(state map[string]any) (map[string]any, string) {
		batch.StateJSON = mustJSON(t, state)
		if err := st.UpdateActionInstance(*batch); err != nil {
			t.Fatal(err)
		}
		w := request(h, "GET", "/api/maintenance/batch-settings?id=failed-batch&scope=all", "", true)
		var plan struct {
			Settings map[string]any `json:"settings"`
			Version  string         `json:"selection_version"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return plan.Settings, plan.Version
	}
	keep := map[string]any{"mode": "compact", "compact": map[string]any{"keep_hdr": true}}
	if got, _ := read(map[string]any{"mode_policy": keep}); got["keep_hdr"] != true {
		t.Fatalf("frozen keep-HDR not shown: %v", got)
	}
	mixed := map[string]any{"mode_policy": keep, "batch_item_settings": map[string]any{"epfile-101": map[string]any{"mode": "compact", "mode_policy": map[string]any{"mode": "compact", "compact": map[string]any{}}}}}
	if got, _ := read(mixed); got["keep_hdr"] != false {
		t.Fatalf("mixed keep-HDR shown as kept: %v", got)
	}
	_, version := read(map[string]any{})
	bad := map[string]any{"id": "failed-batch", "scope": "all", "selection_version": version, "key": "goal-bad", "mode": "size", "keep_hdr": true}
	if w := request(h, "POST", "/api/maintenance/batch-settings", mustJSON(t, bad), true); w.Code != 400 {
		t.Fatalf("keep_hdr with Save space: %d %s", w.Code, w.Body.String())
	}
	body := mustJSON(t, map[string]any{"id": "failed-batch", "scope": "all", "selection_version": version, "key": "goal-hdr", "mode": "compact", "keep_hdr": true})
	w := request(h, "POST", "/api/maintenance/batch-settings", body, true)
	var receipt map[string]string
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if command, _ := st.GetActionInstance(receipt["command_id"]); command == nil || !strings.Contains(command.InputsJSON, `"keep_hdr":true`) {
		t.Fatalf("keep_hdr not carried: %+v", command)
	}
}
