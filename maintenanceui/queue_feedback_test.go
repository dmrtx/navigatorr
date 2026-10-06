package maintenanceui

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func TestSavingsGateGroupsFilesButPreservesEachEstimate(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "batch", "transcode_batch", "completed", map[string]any{"promote_candidates": true}, map[string]any{"counts": map[string]int{"total": 8, "failed": 8}, "batch_promotion": map[string]int{"eligible": 0, "promoted": 0}}, nil)
	var firstRaw string
	for i := 0; i < 8; i++ {
		reason := fmt.Sprintf("benchmark winner predicts only -%.1f%% savings, below required minimum 15.0%% (min_savings_percent=15.0): full transcode not started", 33.2+float64(i))
		if i == 0 {
			reason = mustJSON(t, map[string]string{"step": "wait_benchmark", "error": reason})
			firstRaw = reason
		}
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: fmt.Sprint(i), FilePath: fmt.Sprintf("/media/%d.mkv", i), Status: "failed", Error: reason}); err != nil {
			t.Fatal(err)
		}
	}
	w := request(h, "GET", "/api/maintenance/operations?id=batch", "", true)
	var page operationsPage
	json.Unmarshal(w.Body.Bytes(), &page)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	feedback := operationMap(page.Jobs[0]["batch_files"])
	reasons := feedback["reasons"].([]any)
	if len(reasons) != 1 || operationMap(reasons[0])["count"] != float64(8) {
		t.Fatal("misleading failure count", feedback)
	}
	stages := page.Jobs[0]["stages"].([]any)
	if operationMap(stages[1])["status"] != "failed" || operationMap(stages[2])["status"] != "skip" {
		t.Fatal("failed files or skipped replacement marked complete", stages)
	}
	w = request(h, "GET", "/api/maintenance/batch-items?id=batch", "", true)
	if !strings.Contains(w.Body.String(), "33.2% larger") || strings.Contains(w.Body.String(), "wait_benchmark") || !strings.Contains(w.Body.String(), "15.0% savings required") {
		t.Fatal("opaque file error", w.Body.String())
	}
	items, _ := st.ListTranscodeBatchItems("batch")
	if items[0].Error != firstRaw {
		t.Fatal("projection rewrote history")
	}
	if got := batchReasonText("benchmark winner predicts only 10.5% savings, below required minimum 15.0%", true); !strings.Contains(got, "Estimated savings 10.5%") {
		t.Fatal(got)
	}
	if got := batchReasonText(`{"step":"copy","error":"Storage unavailable"}`, true); got != "Storage unavailable" {
		t.Fatal(got)
	}
}

func TestBatchStagesShowPartialAndUnexecutedWork(t *testing.T) {
	s, _ := testUI(t)
	for _, tc := range []struct {
		status                             string
		step                               int
		completed                          int
		expectedProcess, expectedPromotion string
	}{
		{"completed", 3, 2, "partial", "skip"},
		{"failed", 1, 0, "failed", "skip"},
		{"cancelled", 1, 0, "cancelled", "skip"},
	} {
		r := operationRecord{inst: store.ActionInstance{ActionName: "transcode_batch", Status: tc.status, CurrentStep: tc.step}, inputs: map[string]any{}, outputs: map[string]any{"counts": map[string]any{"completed": float64(tc.completed), "failed": float64(2)}}}
		stages := s.queueStages(r)
		if stages[1]["status"] != tc.expectedProcess || stages[2]["status"] != tc.expectedPromotion {
			t.Fatal(tc, stages)
		}
	}
}

func TestSkippedBatchAndFailedFileDoNotClaimUnperformedStages(t *testing.T) {
	s, _ := testUI(t)
	r := operationRecord{inst: store.ActionInstance{ActionName: "transcode_batch", Status: "completed", CurrentStep: 3}, inputs: map[string]any{"promote_candidates": true}, outputs: map[string]any{"counts": map[string]any{"total": float64(1), "skip": float64(1)}}}
	stages := s.queueStages(r)
	if stages[1]["status"] != "skip" || stages[2]["status"] != "skip" {
		t.Fatal("skipped work marked complete", stages)
	}
	r.inst = store.ActionInstance{ActionName: "transcode_media", Status: "failed", CurrentStep: 2}
	stages = s.queueStages(r)
	for i := 3; i < len(stages); i++ {
		if stages[i]["status"] != "skip" || stages[i]["note"] != "Not run." {
			t.Fatal("future work labelled next after failure", stages)
		}
	}
}

func TestWorkflowFiltersMatchFailuresAndTreatRejectionsAsFinishedResults(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	for _, tc := range []struct{ id, reason string }{
		{"blocked", "benchmark winner predicts only -33.2% savings, below required minimum 15.0%"},
		{"rejected", "transcode candidate rejected by user decision; original file remains untouched"},
	} {
		seedOperation(t, st, tc.id, "transcode_batch", "completed", nil, nil, map[string]any{"counts": map[string]int{"total": 1, "failed": 1}})
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: tc.id, ItemKey: "one", Status: "failed", Error: tc.reason}); err != nil {
			t.Fatal(err)
		}
	}
	seedOperation(t, st, "cancelled", "transcode_media", "cancelled", nil, nil, nil)
	read := func(query string) operationsPage {
		w := request(h, "GET", "/api/maintenance/operations?"+query, "", true)
		var page operationsPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return page
	}
	failed := read("group=workflow&status=failed")
	if failed.Total != 1 || failed.Jobs[0]["id"] != "blocked" {
		t.Fatal("failed season disappeared or rejection became failure", failed)
	}
	if finished := read("group=workflow&status=completed"); finished.Total != 3 {
		t.Fatal("finished outcomes missing", finished)
	}
	if legacy := read("status=completed"); legacy.Total != 2 {
		t.Fatal("ungrouped durable status changed", legacy)
	}
}

func TestBatchSavingsExcludeExplicitAttemptHistoryWithoutDependingOnChildKeyFormat(t *testing.T) {
	oldSource, oldEstimate, newSource := int64(1000), int64(400), int64(900)
	records := []operationRecord{
		{inst: store.ActionInstance{ID: "batch", ActionName: "transcode_batch"}, state: map[string]any{"batch_attempt_history": map[string]any{"file": []any{map[string]any{"child_action_id": "old"}}}}},
		{inst: store.ActionInstance{ID: "old", ActionName: "transcode_media"}, inputs: map[string]any{"parent_action_id": "batch", "path": "/media/one.mkv"}, savings: operationSavings{SourceBytes: &oldSource, EstimatedSavedBytes: &oldEstimate}},
		{inst: store.ActionInstance{ID: "current", ActionName: "transcode_media"}, inputs: map[string]any{"parent_action_id": "batch", "path": "/media/one.mkv"}, savings: operationSavings{SourceBytes: &newSource}},
	}
	aggregateBatchSavings(records)
	if records[0].savings.SourceBytes == nil || *records[0].savings.SourceBytes != newSource || records[0].savings.EstimatedSavedBytes != nil {
		t.Fatal("old estimate leaked into the new attempt", records[0].savings)
	}
}

func TestPreviewExecutionIsOneStableWorkflowWithReadableHistory(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "preview", "transcode_batch", "completed", map[string]any{"dry_run": true}, map[string]any{"dry_run": true}, nil)
	seedOperation(t, st, "independent", "transcode_batch", "completed", nil, nil, nil)
	read := func(query string) operationsPage {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/operations?"+query, "", true)
		var page operationsPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return page
	}
	before := read("group=workflow")
	numbers := map[string]any{}
	for _, job := range before.Jobs {
		numbers[job["id"].(string)] = job["number"]
	}
	seedOperation(t, st, "execution", "transcode_batch", "waiting_external", nil, nil, nil)
	execution, _ := st.GetActionInstance("execution")
	execution.IdempotencyKey = previewExecutionPrefix + "preview"
	if err := st.UpdateActionInstance(*execution); err != nil {
		t.Fatal(err)
	}
	seedOperation(t, st, "child", "transcode_media", "running", map[string]any{"parent_action_id": "execution"}, nil, nil)
	page := read("group=workflow")
	if page.Total != 2 || page.ActiveCount != 1 {
		t.Fatal("duplicate preview or child row", page)
	}
	for _, job := range page.Jobs {
		if job["id"] == "execution" {
			if job["workflow_id"] != "preview" || job["preview_action_id"] != "preview" || job["number"] != numbers["preview"] || job["can_archive"] != false {
				t.Fatal(job)
			}
		} else if job["id"] != "independent" || job["number"] != numbers["independent"] {
			t.Fatal(job)
		}
	}
	for _, id := range []string{"preview", "execution", "child"} {
		if detail := read("id=" + id); len(detail.Jobs) != 1 || detail.Jobs[0]["id"] != id {
			t.Fatal("history inaccessible", id, detail)
		}
		if w := request(h, "POST", "/api/maintenance/archive", `{"id":"`+id+`","archived":true}`, true); w.Code != 409 {
			t.Fatal("active workflow archived", w.Code)
		}
	}
	for _, id := range []string{"execution", "child"} {
		inst, _ := st.GetActionInstance(id)
		inst.Status = "completed"
		st.UpdateActionInstance(*inst)
	}
	if w := request(h, "POST", "/api/maintenance/archive", `{"id":"execution","archived":true}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if page := read("group=workflow"); page.Total != 1 {
		t.Fatal(page)
	}
	if page := read("group=workflow&status=archived"); page.Total != 1 || page.Jobs[0]["id"] != "execution" {
		t.Fatal(page)
	}
	if w := request(h, "POST", "/api/maintenance/archive", `{"id":"preview","archived":false}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if page := read("group=workflow"); page.Total != 2 {
		t.Fatal(page)
	}
	// A malformed receipt cannot attach a batch to an unrelated job.
	execution.IdempotencyKey = previewExecutionPrefix + "independent"
	execution.Status = "completed"
	st.UpdateActionInstance(*execution)
	if page := read("group=workflow"); page.Total != 3 {
		t.Fatal("invalid preview relation hid an independent job", page)
	}
}

func TestQueueOrderAndNumbersSurviveStateChangesAndChildPromotion(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	for _, record := range []struct {
		id, name, status string
		inputs           map[string]any
	}{
		{"batch", "transcode_batch", "waiting_external", nil},
		{"child", "transcode_media", "running", map[string]any{"parent_action_id": "batch", "path": "/child.mkv"}},
		{"newer", "benchmark_transcode", "completed", map[string]any{"path": "/newer.mkv"}},
	} {
		seedOperation(t, st, record.id, record.name, record.status, record.inputs, nil, nil)
	}
	read := func() []map[string]any {
		w := request(h, "GET", "/api/maintenance/operations?group=workflow", "", true)
		var page operationsPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Body.String())
		}
		return page.Jobs
	}
	before := read()
	if before[0]["id"] != "newer" || before[1]["id"] != "batch" {
		t.Fatal("active jobs displaced creation order", before)
	}
	old, _ := st.GetActionInstance("batch")
	old.Status = "completed"
	st.UpdateActionInstance(*old)
	old, _ = st.GetActionInstance("newer")
	old.Status = "running"
	st.UpdateActionInstance(*old)
	after := read()
	for i := range before {
		if !reflect.DeepEqual([]any{before[i]["workflow_id"], before[i]["number"]}, []any{after[i]["workflow_id"], after[i]["number"]}) {
			t.Fatal("state change moved or renumbered row", before, after)
		}
	}
	seedOperation(t, st, "promote", "promote_transcode_candidate", "waiting_decision", map[string]any{"transcode_action_id": "child"}, nil, nil)
	after = read()
	for _, job := range after {
		for _, previous := range before {
			if job["workflow_id"] == previous["workflow_id"] && job["number"] != previous["number"] {
				t.Fatal("child promotion renumbered existing rows", after)
			}
		}
	}
}

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
	if page.Total != 3 || page.ActiveCount != 1 || !page.HasMore || len(page.Jobs) != 1 || page.Jobs[0]["id"] != "independent" {
		t.Fatalf("wrong grouped page: %+v", page)
	}
	linked := read("group=workflow&offset=1&limit=1").Jobs[0]
	number := linked["number"]
	if linked["id"] != "replace" || linked["workflow_id"] != "source" || number == nil || len(linked["workflow_actions"].([]any)) != 2 {
		t.Fatal("lost original workflow identity/history", linked)
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

func TestQueueBatchIncludesEveryActiveFile(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "batch", "transcode_batch", "waiting_external", nil, nil, nil)
	for _, id := range []string{"one", "two"} {
		seedOperation(t, st, id, "transcode_media", "waiting_external", map[string]any{"parent_action_id": "batch", "path": "/media/" + id + ".mkv"}, nil, map[string]any{"phase": "evaluating_metrics", "progress": 84.4})
	}
	w := request(h, "GET", "/api/maintenance/operations?group=workflow&status=active", "", true)
	var page operationsPage
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 || len(page.Jobs[0]["activities"].([]any)) != 2 {
		t.Fatal(w.Body.String())
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

func TestQueueNumbersAppendWithinOneSecondRegardlessOfRandomID(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "z-first", "transcode_media", "completed", nil, nil, nil)
	read := func() operationsPage {
		w := request(h, "GET", "/api/maintenance/operations?group=workflow", "", true)
		var page operationsPage
		if json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Body.String())
		}
		return page
	}
	before := read().Jobs[0]["number"]
	seedOperation(t, st, "a-second", "transcode_media", "pending", nil, nil, nil)
	after := read()
	if after.Jobs[0]["id"] != "a-second" || after.Jobs[1]["number"] != before {
		t.Fatal("new random ID renumbered earlier job", after)
	}
}

func TestReplacementHasFileContextBeforePlanningCheckpoint(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "original", "transcode_media", "completed", map[string]any{"path": "/media/movie.mkv"}, map[string]any{"original": map[string]any{"size_bytes": 1000}, "result": map[string]any{"size_bytes": 400}}, nil)
	seedOperation(t, st, "replacement", "promote_transcode_candidate", "running", map[string]any{"transcode_action_id": "original"}, nil, nil)
	w := request(h, "GET", "/api/maintenance/operations?group=workflow", "", true)
	var page operationsPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
		t.Fatal(w.Body.String())
	}
	job := page.Jobs[0]
	if job["workflow_id"] != "original" || job["source_path"] != "/media/movie.mkv" || operationMap(job["savings"])["candidate_bytes"] != float64(400) {
		t.Fatal("planning lost durable file context", job)
	}
}

func TestRejectionCountIsIndependentOfTruncatedQueueReasons(t *testing.T) {
	items := []store.TranscodeBatchItem{}
	for i := 0; i < 6; i++ {
		message := "A storage error"
		if i > 2 {
			message = "Another worker error"
		}
		items = append(items, store.TranscodeBatchItem{Status: "failed", Error: message})
	}
	items = append(items, store.TranscodeBatchItem{Status: "failed", Error: `{"step":"validate","error":"transcode candidate rejected by user decision; original file remains untouched"}`})
	feedback := batchQueueFeedback(items)
	if feedback["rejected_count"] != 1 {
		t.Fatal(feedback)
	}
	for _, reason := range feedback["reasons"].([]map[string]any) {
		if strings.Contains(reason["reason"].(string), "rejected") {
			t.Fatal("fixture did not truncate rejection reason", feedback)
		}
	}
	if items[6].Status != "failed" {
		t.Fatal("presentation changed stored execution status")
	}
}
