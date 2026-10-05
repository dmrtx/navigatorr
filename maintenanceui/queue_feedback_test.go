package maintenanceui

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func TestQueueGroupsDurableWorkflowsBeforeFilteringAndPaging(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "batch", "transcode_batch", "completed", nil, nil, nil)
	seedOperation(t, st, "sample", "benchmark_transcode", "completed", map[string]any{"parent_action_id": "batch", "path": "/series/one.mkv"}, nil, nil)
	seedOperation(t, st, "source", "transcode_media", "completed", map[string]any{"path": "/series/one.mkv"}, nil, nil)
	seedOperation(t, st, "replace", "promote_transcode_candidate", "running", map[string]any{"transcode_action_id": "source"}, nil, nil)
	// An independent request sharing the same path must remain visible.
	seedOperation(t, st, "independent", "benchmark_transcode", "completed", map[string]any{"path": "/series/one.mkv"}, nil, nil)
	read := func(query string) operationsPage {
		w := request(h, "GET", "/api/maintenance/operations?"+query, "", true)
		var page operationsPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return page
	}
	page := read("group=workflow&limit=1")
	if page.Total != 3 || page.ActiveCount != 1 || !page.HasMore || len(page.Jobs) != 1 || page.Jobs[0]["id"] != "replace" || page.Jobs[0]["workflow_id"] != "source" {
		t.Fatalf("wrong grouped page: %+v", page)
	}
	number := page.Jobs[0]["number"]
	if number == nil || len(page.Jobs[0]["workflow_actions"].([]any)) != 2 {
		t.Fatal("lost original workflow identity/history", page.Jobs[0])
	}
	completed := read("group=workflow&status=completed")
	if completed.Total != 2 {
		t.Fatal("running replacement reappeared as completed conversion", completed)
	}
	detail := read("id=source")
	if len(detail.Jobs) != 1 || detail.Jobs[0]["number"] != number || detail.Jobs[0]["id"] != "source" {
		t.Fatal("source detail lost its identity", detail)
	}
	inst, _ := st.GetActionInstance("replace")
	inst.CurrentStep = 2
	if err := st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	replacement := read("id=replace").Jobs[0]
	stages := replacement["stages"].([]any)
	if stages[2].(map[string]any)["name"] != "preserve_original" || stages[2].(map[string]any)["status"] != "running" || stages[3].(map[string]any)["status"] != "pending" {
		t.Fatal("missing live replacement stage", stages)
	}
}

func TestBatchQueueNamesAndReasonsAreBoundedAndSurvivePaging(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "batch", "transcode_batch", "completed", nil, nil, nil)
	for i := 0; i < 125; i++ {
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: fmt.Sprint(i), FilePath: fmt.Sprintf("/media/Series/Season 1/E%03d.mkv", i), DisplayLabel: fmt.Sprintf("Episode %d", i), Status: "skip", Reasons: []string{"Unsupported format", "Unsupported format"}}); err != nil {
			t.Fatal(err)
		}
	}
	w := request(h, "GET", "/api/maintenance/operations?group=workflow", "", true)
	var page operationsPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	feedback := page.Jobs[0]["batch_files"].(map[string]any)
	if feedback["context"] != "Series · Season 1" || feedback["file_count"] != float64(125) || len(feedback["files"].([]any)) != 2 || feedback["reasons"].([]any)[0].(map[string]any)["count"] != float64(125) {
		t.Fatal("missing or unbounded batch feedback", feedback)
	}
	w = request(h, "GET", "/api/maintenance/batch-items?id=batch&offset=100", "", true)
	var items struct {
		Items []batchItemView `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &items) != nil || len(items.Items) != 25 || items.Items[0].DisplayLabel != "Episode 100" || len(items.Items[0].Reasons) == 0 {
		t.Fatal("full contents/reasons unavailable", w.Body.String())
	}
}

func TestQueueBatchTelemetryIdentifiesTheActualActiveFile(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "batch", "transcode_batch", "waiting_external", nil, nil, nil)
	seedOperation(t, st, "active", "transcode_media", "waiting_external", map[string]any{"parent_action_id": "batch", "path": "/media/active.mkv"}, nil, map[string]any{"transcode_phase": "encoding", "progress": 34})
	seedOperation(t, st, "queued", "transcode_media", "pending", map[string]any{"parent_action_id": "batch", "path": "/media/queued.mkv"}, nil, nil)
	w := request(h, "GET", "/api/maintenance/operations?group=workflow&status=active", "", true)
	var page operationsPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 || page.ActiveCount != 1 || page.Jobs[0]["activity_file"] != "/media/active.mkv" {
		t.Fatal("active child was hidden without a progress replacement", w.Body.String())
	}
	worker := page.Jobs[0]["worker"].(map[string]any)
	if worker["transcode_phase"] != "encoding" || worker["progress"] != float64(34) {
		t.Fatal("batch progress is not the child's measured progress", worker)
	}
}

func TestBatchQueueFailureReasonsDoNotClaimSelectionCriteriaAreErrors(t *testing.T) {
	feedback := batchQueueFeedback([]store.TranscodeBatchItem{
		{FilePath: "/media/one.mkv", Status: "failed", Reasons: []string{"h264_1080p", "oversized"}, Error: "Worker connection failed"},
		{FilePath: "/media/two.mkv", Status: "failed", Reasons: []string{"h264_1080p", "oversized"}},
		{FilePath: "/media/three.mkv", Status: "skip", Reasons: []string{"anime", "No suitable candidate"}},
	})
	reasons := feedback["reasons"].([]map[string]any)
	for _, reason := range reasons {
		if reason["reason"] == "h264_1080p" || reason["reason"] == "oversized" || reason["reason"] == "anime" {
			t.Fatal("selection criteria presented as outcome", reasons)
		}
	}
	if len(reasons) != 2 {
		t.Fatal("missing bounded failure feedback", reasons)
	}
	errorFeedback := batchQueueFeedback([]store.TranscodeBatchItem{{FilePath: "/media/one.mkv", Status: "failed", Reasons: []string{"oversized"}, Error: "Worker connection failed"}})
	if errorFeedback["reasons"].([]map[string]any)[0]["reason"] != "Worker connection failed" {
		t.Fatal("file error was lost", errorFeedback)
	}
	reasonFeedback := batchQueueFeedback([]store.TranscodeBatchItem{{FilePath: "/media/one.mkv", Status: "failed", Reasons: []string{"oversized", "Missing source"}}})
	if reasonFeedback["reasons"].([]map[string]any)[0]["reason"] != "Missing source" {
		t.Fatal("recorded failure reason was lost", reasonFeedback)
	}
}
