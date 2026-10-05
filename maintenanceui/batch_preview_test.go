package maintenanceui

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

func seedPreview(t *testing.T, s *Server, inputs map[string]any) {
	t.Helper()
	inputs["dry_run"] = true
	seedOperation(t, s.engine.Deps().Store, "preview", "transcode_batch", "completed", inputs,
		map[string]any{"dry_run": true, "series_title": "Example season", "counts": map[string]int{"total": 3, "queued": 2, "skip": 1}}, nil)
	for i, item := range []store.TranscodeBatchItem{
		{ItemKey: "epfile-101", FilePath: "/media/one.mkv", Status: "queued", Decision: "transcode"},
		{ItemKey: "epfile-102", FilePath: "/media/two.mkv", Status: "skip", Decision: "skip"},
		{ItemKey: "epfile-103", FilePath: "/media/three.mkv", Status: "queued", Decision: "transcode"},
	} {
		item.BatchID = "preview"
		item.DisplayLabel = []string{"One", "Two", "Three"}[i]
		if err := s.engine.Deps().Store.CreateTranscodeBatchItem(item); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPreviewStartFreezesEligibilityAndPreservesSettings(t *testing.T) {
	for _, sonarr := range []bool{false, true} {
		t.Run(map[bool]string{true: "sonarr", false: "folder"}[sonarr], func(t *testing.T) {
			s, h := testUI(t)
			inputs := map[string]any{"profile_config": map[string]any{"container": "mkv", "video": map[string]any{"codec": "hevc_videotoolbox", "quality": 80}}, "metric": "vmaf", "max_items": 3, "preserve_source_bit_depth": true, "min_savings_percent": 20, "max_size_increase_percent": 0}
			if sonarr {
				inputs["service"], inputs["series_id"], inputs["season"] = "sonarr", 10, 1
				inputs["episode_file_ids"] = []int{101, 102, 103, 999}
				inputs["promote_candidates"] = true
			} else {
				inputs["paths"] = []string{"/media/one.mkv", "/media/two.mkv", "/media/three.mkv", "/media/uninspected.mkv"}
				inputs["media_type"] = "movie"
			}
			seedPreview(t, s, inputs)
			w := request(h, "GET", "/api/maintenance/batch-preview?id=preview", "", true)
			var plan batchPreviewPlan
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.Eligible != 2 || plan.Other != 1 || len(plan.Files) != 2 {
				t.Fatal("incorrect preview plan", w.Code, w.Body.String())
			}
			rows, _ := s.engine.Deps().Store.ListActionInstances("", 100)
			if len(rows) != 1 {
				t.Fatal("read-only review enqueued work")
			}
			w = request(h, "POST", "/api/maintenance/batch-preview", `{"id":"preview"}`, true)
			var receipt map[string]string
			if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt["id"] == "" {
				t.Fatal(w.Code, w.Body.String())
			}
			job, _ := s.engine.Deps().Store.GetActionInstance(receipt["id"])
			w = request(h, "GET", "/api/maintenance/operations?id="+job.ID, "", true)
			var pending operationsPage
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &pending) != nil || operationMap(pending.Jobs[0]["batch"])["total"] != float64(2) {
				t.Fatal("pending batch lost frozen selection", w.Body.String())
			}
			w = request(h, "GET", "/api/maintenance/batch-items?id="+job.ID, "", true)
			var pendingFiles struct {
				Total int
				Items []batchItemView
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &pendingFiles) != nil || pendingFiles.Total != 2 || pendingFiles.Items[0].Status != "queued" {
				t.Fatal("pending detail lost frozen files", w.Body.String())
			}
			actual := decodeOperationJSON(job.InputsJSON)
			if actual["dry_run"] != false || actual["metric"] != "vmaf" || actual["min_savings_percent"] != float64(20) || !reflect.DeepEqual(actual["profile_config"], decodeOperationJSON(mustJSON(t, inputs))["profile_config"]) {
				t.Fatal("preview settings changed", actual)
			}
			if sonarr {
				if !reflect.DeepEqual(actual["episode_file_ids"], []any{float64(101), float64(103)}) || actual["season"] != float64(1) || actual["promote_candidates"] != true {
					t.Fatal("selection or approval intent changed", actual)
				}
			} else if !reflect.DeepEqual(actual["paths"], []any{"/media/one.mkv", "/media/three.mkv"}) {
				t.Fatal("expanded frozen preview selection", actual)
			}
			// Retry the same start after the encode is terminal and the worker is
			// offline: it must return its receipt rather than enqueue another batch.
			job.Status = store.ActionStatusCancelled
			if err := s.engine.Deps().Store.UpdateActionInstance(*job); err != nil {
				t.Fatal(err)
			}
			w = request(h, "GET", "/api/maintenance/operations?id="+job.ID, "", true)
			json.Unmarshal(w.Body.Bytes(), &pending)
			if w.Code != 200 || operationMap(pending.Jobs[0]["batch"])["cancelled"] != float64(2) {
				t.Fatal("cancelled admission lost file count", w.Body.String())
			}
			deps := s.engine.Deps()
			deps.Transcode = unavailableWorker{}
			s.engine = action.NewEngine(deps)
			w = request(h, "POST", "/api/maintenance/batch-preview", `{"id":"preview"}`, true)
			var repeated map[string]string
			json.Unmarshal(w.Body.Bytes(), &repeated)
			if w.Code != 200 || repeated["id"] != receipt["id"] {
				t.Fatal("duplicate preview start", w.Code, w.Body.String())
			}
			rows, _ = deps.Store.ListActionInstances("", 100)
			original, _ := deps.Store.GetActionInstance("preview")
			if len(rows) != 2 || original.Status != store.ActionStatusCompleted || decodeOperationJSON(original.InputsJSON)["dry_run"] != true {
				t.Fatal("start mutated the preview or duplicated its encode")
			}
			w = request(h, "GET", "/api/maintenance/operations?id=preview", "", true)
			var page operationsPage
			json.Unmarshal(w.Body.Bytes(), &page)
			if w.Code != 200 || page.Jobs[0]["preview_execution_action_id"] != receipt["id"] {
				t.Fatal("missing durable next action", w.Body.String())
			}
		})
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPreviewStartRejectsOfflineInvalidAndEmptyPreviews(t *testing.T) {
	for _, reason := range []string{"offline", "storage", "empty", "not-preview", "unfinished", "missing", "unsupported-setting"} {
		t.Run(reason, func(t *testing.T) {
			s, h := testUI(t)
			seedPreview(t, s, map[string]any{"service": "sonarr", "series_id": 10, "profile": "general-hevc"})
			st := s.engine.Deps().Store
			inst, _ := st.GetActionInstance("preview")
			id := "preview"
			switch reason {
			case "offline", "storage":
				deps := s.engine.Deps()
				if reason == "offline" {
					deps.Transcode = unavailableWorker{}
				} else {
					deps.Transcode = inaccessibleStorageWorker{}
				}
				s.engine = action.NewEngine(deps)
			case "empty":
				items, _ := st.ListTranscodeBatchItems("preview")
				for _, item := range items {
					item.Status, item.Decision = "skip", "skip"
					if err := st.UpdateTranscodeBatchItem(item); err != nil {
						t.Fatal(err)
					}
				}
			case "not-preview":
				inst.InputsJSON = `{"service":"sonarr","series_id":10,"dry_run":false}`
			case "unfinished":
				inst.Status = "running"
			case "missing":
				id = "missing"
			case "unsupported-setting":
				inst.InputsJSON = `{"service":"sonarr","series_id":10,"dry_run":true,"unknown":1}`
			}
			if err := st.UpdateActionInstance(*inst); err != nil {
				t.Fatal(err)
			}
			w := request(h, "POST", "/api/maintenance/batch-preview", mustJSON(t, map[string]string{"id": id}), true)
			rows, _ := st.ListActionInstances("", 100)
			if w.Code != 409 || len(rows) != 1 {
				t.Fatal("invalid preview was submitted", w.Code, w.Body.String(), len(rows))
			}
		})
	}
}

func TestPreviewAccessRequiresAuthentication(t *testing.T) {
	s, h := testUI(t)
	seedPreview(t, s, map[string]any{"paths": []string{"/media/one.mkv"}})
	for _, method := range []string{"GET", "POST"} {
		if w := request(h, method, "/api/maintenance/batch-preview?id=preview", `{"id":"preview"}`, false); w.Code != 401 {
			t.Fatal("unprotected preview", method, w.Code)
		}
	}
}
