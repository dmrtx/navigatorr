package action

import (
	"context"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
)

func TestBatchRevisionRetainsIdentityAndDefersActiveFilesWithoutDuplicateAdmission(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{Transcode: config.TranscodeConfig{MaxParallelJobs: 2}}})
	finishOld := false
	e.RegisterTemplate(ActionTemplate{Name: "transcode_media", ImmutableInputs: true, AutoReconcile: true, Steps: []StepDefinition{{Name: "encode", Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
		if getString(ec.Inputs, "profile") == "old" && !finishOld {
			return StepResult{Status: StepWaitingExternal, WaitingCondition: "transcode_processing", Outputs: map[string]any{"job_id": "original-worker-job"}}, nil
		}
		return StepResult{Status: StepCompleted, Outputs: map[string]any{"candidate_path": "/candidates/" + ec.InstanceID}}, nil
	}}}})
	seedActionInstance(t, st, "season", "transcode_batch", StatusWaitingExternal, 1, "", map[string]any{"paths": []string{"/media/a.mkv", "/media/b.mkv"}, "profile": "anime-x265-calibrated"}, map[string]any{"series_title": "Season 1"})
	original, _ := st.GetActionInstance("season")
	old, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": "/media/a.mkv", "profile": "old", "parent_action_id": "season"}, "batch-season-file-a")
	if err != nil {
		t.Fatal(err)
	}
	st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "season", ItemKey: "file-a", FilePath: "/media/a.mkv", Status: "running", Decision: "transcode", Profile: "old", ChildActionID: old.ID, JobID: "original-worker-job"})
	st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "season", ItemKey: "file-b", FilePath: "/media/b.mkv", Status: "failed", Decision: "transcode", Profile: "old"})
	items, _ := st.ListTranscodeBatchItems("season")
	r, err := e.ReconfigureBatch(context.Background(), "season", "all", "", BatchSelectionVersion(original, items), "", "receipt-1", map[string]any{"profile": "new", "min_savings_percent": 10})
	if err != nil || r.ID != "season" {
		t.Fatal(r, err)
	}
	root, _ := st.GetActionInstance("season")
	items, _ = st.ListTranscodeBatchItems("season")
	if root.InputsJSON != original.InputsJSON || items[0].ChildActionID != old.ID || items[0].Status != "running" || items[1].Status != "queued" {
		t.Fatal(root, items)
	}
	// A lost command response returns the same revision, without resetting work.
	r, err = e.ReconfigureBatch(context.Background(), "season", "all", "", "stale-version", "", "receipt-1", map[string]any{"profile": "new"})
	if err != nil || r.ID != "season" {
		t.Fatal("receipt not idempotent", r, err)
	}
	finishOld = true
	if _, err := e.Resume(context.Background(), old.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(context.Background(), "season", "", nil); err != nil {
		t.Fatal(err)
	}
	items, _ = st.ListTranscodeBatchItems("season")
	if items[0].ChildActionID == old.ID || items[0].ChildActionID == "" || items[1].ChildActionID == "" {
		t.Fatal("new attempts missing", items)
	}
	for _, item := range items {
		child, _ := st.GetActionInstance(item.ChildActionID)
		if getString(parseExecutionContext(child, e).Inputs, "profile") != "new" || child.IdempotencyKey != "batch-season-"+item.ItemKey+"-attempt-1" {
			t.Fatal(child)
		}
	}
	before := items[0].ChildActionID
	// Restart with the same durable DB: completed attempts must not be recreated.
	restarted := NewEngine(e.Deps())
	tmpl, _ := e.GetTemplate("transcode_media")
	restarted.RegisterTemplate(tmpl)
	if err := restarted.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, _ = st.ListTranscodeBatchItems("season")
	if items[0].ChildActionID != before {
		t.Fatal("restart duplicated child")
	}
	root, _ = st.GetActionInstance("season")
	state := parseExecutionContext(root, e).State
	if len(stateObject(state, "batch_retry_pending")) != 0 || stateObject(state, "batch_attempt_history")["file-a"] == nil {
		t.Fatal("deferred history missing", state)
	}
	oldInst, _ := st.GetActionInstance(old.ID)
	if getString(parseExecutionContext(oldInst, e).Inputs, "profile") != "old" || oldInst.Status != StatusCompleted {
		t.Fatal("old attempt rewritten", oldInst)
	}
}

func TestBatchRevisionRejectsStaleSelectionAndPreservesCompletedSubset(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}})
	seedActionInstance(t, st, "batch", "transcode_batch", StatusCompleted, 2, "", map[string]any{"paths": []string{"/media/one", "/media/two"}}, map[string]any{"series_title": "Saved season"})
	st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "one", FilePath: "/media/one", Status: "failed", Profile: "old"})
	st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "two", FilePath: "/media/two", Status: "completed", Profile: "old", CandidatePath: "/candidate/keep"})
	root, _ := st.GetActionInstance("batch")
	items, _ := st.ListTranscodeBatchItems("batch")
	if _, err := e.ReconfigureBatch(context.Background(), "batch", "unfinished", "", "stale", "", "r", map[string]any{"profile": "new"}); err == nil {
		t.Fatal("stale version accepted")
	}
	r, err := e.ReconfigureBatch(context.Background(), "batch", "unfinished", "", BatchSelectionVersion(root, items), "", "r", map[string]any{"profile": "new"})
	if err != nil || r.ID != "batch" {
		t.Fatal(r, err)
	}
	items, _ = st.ListTranscodeBatchItems("batch")
	if items[0].Status != "queued" || items[1].Status != "completed" || items[1].CandidatePath != "/candidate/keep" {
		t.Fatal(items)
	}
	root, _ = st.GetActionInstance("batch")
	ec := parseExecutionContext(root, e)
	ec.State["batch_promotion_plan"] = map[string]any{"approved": true}
	root.StateJSON = toJSON(ec.State)
	st.UpdateActionInstance(*root)
	if _, err := e.ReconfigureBatch(context.Background(), "batch", "all", "", BatchSelectionVersion(root, items), "", "r2", map[string]any{"profile": "new"}); err == nil {
		t.Fatal("replacement plan superseded")
	}
}
