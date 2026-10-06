package maintenanceui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

func seedFailedBatch(t *testing.T, s *Server, sonarr bool) map[string]any {
	t.Helper()
	inputs := map[string]any{"profile_config": map[string]any{"container": "mkv", "video": map[string]any{"codec": "hevc_videotoolbox", "quality": 80}}, "min_savings_percent": 15, "max_size_increase_percent": 0, "preserve_source_bit_depth": true, "metric": "vmaf"}
	if sonarr {
		inputs["service"], inputs["series_id"], inputs["season"], inputs["promote_candidates"] = "sonarr", 10, 1, true
		inputs["episode_file_ids"] = []int{101, 102, 103, 999}
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
			if !reflect.DeepEqual(actual["profile_config"], decodeOperationJSON(mustJSON(t, original))["profile_config"]) || actual["min_savings_percent"] != float64(10) || actual["metric"] != "vmaf" {
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
			body := `{"id":"failed-batch","profile":"auto","priority":"savings","min_savings_percent":15,"max_size_increase_percent":0,"key":"attempt-1"}`
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
	w := request(h, "POST", "/api/maintenance/batch-reconfigure", `{"id":"failed-batch","profile":"auto","priority":"savings","key":"attempt-1"}`, true)
	var receipt map[string]string
	json.Unmarshal(w.Body.Bytes(), &receipt)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	inst, _ := s.engine.Deps().Store.GetActionInstance(receipt["id"])
	inputs := decodeOperationJSON(inst.InputsJSON)
	if inputs["profile_config"] != nil || inputs["priority"] != "savings" || inputs["profile"] != "auto" || inputs["min_savings_percent"] != nil {
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
