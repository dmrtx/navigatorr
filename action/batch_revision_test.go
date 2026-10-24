package action

import (
	"context"
	"fmt"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
)

func TestBatchRevisionFromUnsubmittedSearchReview(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%v", deferred), func(t *testing.T) {
			st := setupTestStore(t)
			e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}})
			state := map[string]any{}
			if deferred {
				state["batch_retry_pending"] = map[string]any{"one": true}
			}
			seedActionInstance(t, st, "batch-review", "transcode_batch", StatusWaitingDecision, 1, "", map[string]any{"mode": "x265_preserve"}, state)
			seedActionInstance(t, st, "search-review", "transcode_media", StatusWaitingDecision, 1, "", map[string]any{"profile": "old", "replace_original": false, "parent_action_id": "batch-review"}, map[string]any{"benchmark_search_review_required": true})
			child, _ := st.GetActionInstance("search-review")
			child.WaitingCondition = "benchmark_search_review"
			child.WaitingOptionsJSON = toJSON([]WaitingOption{{Decision: "cancel"}, {Decision: "run_once_after_review"}})
			st.UpdateActionInstance(*child)
			st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-review", ItemKey: "one", FilePath: "/media/one.mkv", Status: "waiting_decision", Decision: "transcode", ChildActionID: child.ID, Profile: "old"})
			root, _ := st.GetActionInstance("batch-review")
			items, _ := st.ListTranscodeBatchItems(root.ID)
			if deferred {
				ctx, release, err := e.claimExecution(context.Background(), root.ID, true)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				if err := e.applyPendingBatchAttempts(ctx, parseExecutionContext(root, e), items); err != nil {
					t.Fatal(err)
				}
			} else if _, err := e.ReconfigureBatch(context.Background(), root.ID, "unfinished", "", BatchSelectionVersion(root, items), "", "new-profile", map[string]any{"profile": "live-action-hevc"}); err != nil {
				t.Fatal(err)
			}
			items, _ = st.ListTranscodeBatchItems(root.ID)
			child, _ = st.GetActionInstance(child.ID)
			if child.Status != StatusCancelled || items[0].Status != "queued" || items[0].ChildActionID != "" {
				t.Fatal("search review blocked or admitted old work", child, items)
			}
			root, _ = st.GetActionInstance(root.ID)
			if stateObject(parseExecutionContext(root, e).State, "batch_attempt_history")["one"] == nil {
				t.Fatal("previous search review was lost")
			}
		})
	}
}

func TestModeBatchExplicitProfileKeepsTheOriginalIntent(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{Transcode: config.TranscodeConfig{MaxParallelJobs: 2}}})
	e.RegisterTemplate(ActionTemplate{Name: "transcode_media", ImmutableInputs: true, Steps: []StepDefinition{{Name: "encode", Run: func(ctx context.Context, child *ExecutionContext) (StepResult, error) {
		if getString(child.Inputs, "profile") != "live-action-hevc-vt" || getString(child.Inputs, "mode") != "" || getBool(child.Inputs, "replace_original") {
			t.Errorf("saved profile or original protection lost: %s", toJSON(child.Inputs))
		}
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "transcode_processing"}, nil
	}}}})
	ec := &ExecutionContext{InstanceID: "mode-parent", Inputs: map[string]any{"mode": "x265_preserve"}, State: map[string]any{}}
	if _, err := freezeModePolicy(ec); err != nil {
		t.Fatal(err)
	}
	seedActionInstance(t, st, ec.InstanceID, "transcode_batch", StatusWaitingDecision, 1, "", ec.Inputs, ec.State)
	for i := 0; i < 10; i++ {
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: ec.InstanceID, ItemKey: fmt.Sprintf("file-%d", i), FilePath: fmt.Sprintf("/media/%d.mkv", i), Status: "failed", Decision: "transcode", Profile: "mode-x265_preserve"}); err != nil {
			t.Fatal(err)
		}
	}
	root, _ := st.GetActionInstance(ec.InstanceID)
	items, _ := st.ListTranscodeBatchItems(ec.InstanceID)
	if _, err := e.ReconfigureBatch(context.Background(), ec.InstanceID, "unfinished", "", BatchSelectionVersion(root, items), "", "profile-choice", map[string]any{"profile": "live-action-hevc-vt"}); err != nil {
		t.Fatal(err)
	}
	res, err := e.Resume(context.Background(), ec.InstanceID, "", nil)
	if err != nil || res.Status != StatusWaitingExternal {
		t.Fatalf("explicit profile not admitted: %+v %v", res, err)
	}
	items, _ = st.ListTranscodeBatchItems(ec.InstanceID)
	children := 0
	for _, item := range items {
		if item.ChildActionID != "" {
			children++
		}
	}
	root, _ = st.GetActionInstance(ec.InstanceID)
	if children != 2 || root.InputsJSON != toJSON(ec.Inputs) {
		t.Fatal("bounded admission or immutable original intent lost", children, root)
	}
}

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

// A running batch admits a file and creates its child action every few
// seconds. A settings plan opened before that must still be accepted; only a
// change in which files the batch holds makes it stale.
func TestBatchRevisionSurvivesFilesBeingAdmittedWhileTheSettingsAreOpen(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}})
	seedActionInstance(t, st, "batch", "transcode_batch", StatusRunning, 2, "", map[string]any{"paths": []string{"/media/one", "/media/two"}}, map[string]any{"series_title": "Season"})
	st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "one", FilePath: "/media/one", Status: "queued", Profile: "old"})
	st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "two", FilePath: "/media/two", Status: "queued", Profile: "old"})
	root, _ := st.GetActionInstance("batch")
	items, _ := st.ListTranscodeBatchItems("batch")
	version := BatchSelectionVersion(root, items)

	seedActionInstance(t, st, "child-two", "transcode_media", StatusRunning, 7, "", map[string]any{"path": "/media/two"}, nil)
	admitted := items[1]
	admitted.Status, admitted.ChildActionID = "running", "child-two"
	if err := st.UpdateTranscodeBatchItem(admitted); err != nil {
		t.Fatal(err)
	}
	moved, _ := st.ListTranscodeBatchItems("batch")
	if BatchSelectionVersion(root, moved) != version {
		t.Fatal("admitting a file must not change the selection version")
	}
	if _, err := e.ReconfigureBatch(context.Background(), "batch", "unfinished", "", version, "", "r", map[string]any{"profile": "new"}); err != nil {
		t.Fatalf("a plan opened before a file was admitted was refused: %v", err)
	}

	root, _ = st.GetActionInstance("batch")
	st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "three", FilePath: "/media/three", Status: "queued", Profile: "old"})
	grown, _ := st.ListTranscodeBatchItems("batch")
	if BatchSelectionVersion(root, grown) == version {
		t.Fatal("a file added to the batch must make an open plan stale")
	}
}

func TestBatchFileRevisionPreservesOtherCandidatesAndRejectsChangedDecision(t *testing.T) {
	for _, status := range []string{"skip", "waiting_decision"} {
		t.Run(status, func(t *testing.T) {
			st := setupTestStore(t)
			e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}})
			seedActionInstance(t, st, "batch", "transcode_batch", StatusWaitingDecision, 1, "", map[string]any{"mode": "size"}, nil)
			seedActionInstance(t, st, "child", "transcode_media", StatusCompleted, 7, "", map[string]any{"parent_action_id": "batch", "path": "/media/one"}, nil)
			child, _ := st.GetActionInstance("child")
			if status == "waiting_decision" {
				child.Status = StatusWaitingDecision
				child.WaitingReason = "Search exhausted"
				child.WaitingOptionsJSON = toJSON([]WaitingOption{{Decision: "cancel"}})
				st.UpdateActionInstance(*child)
			}
			st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "one", FilePath: "/media/one", Status: status, ChildActionID: "child", Decision: "transcode"})
			st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "ready", FilePath: "/media/ready", Status: "completed", CandidatePath: "/candidate/keep", Decision: "transcode"})
			root, _ := st.GetActionInstance("batch")
			items, _ := st.ListTranscodeBatchItems("batch")
			decision := ""
			if status == "waiting_decision" {
				decision = CandidateDecisionVersion(child)
				if _, err := e.ReconfigureBatch(context.Background(), "batch", "file", "one", BatchSelectionVersion(root, items), "stale", "stale-receipt", map[string]any{"mode": "quality"}); err == nil {
					t.Fatal("changed file decision accepted")
				}
			}
			if _, err := e.ReconfigureBatch(context.Background(), "batch", "file", "one", BatchSelectionVersion(root, items), decision, "receipt", map[string]any{"mode": "quality"}); err != nil {
				t.Fatal(err)
			}
			items, _ = st.ListTranscodeBatchItems("batch")
			for _, item := range items {
				if item.ItemKey == "one" && (item.Status != "queued" || item.ChildActionID != "") {
					t.Fatal("selected file not rescheduled", item)
				}
				if item.ItemKey == "ready" && (item.Status != "completed" || item.CandidatePath != "/candidate/keep") {
					t.Fatal("unselected candidate changed", item)
				}
			}
			root, _ = st.GetActionInstance("batch")
			if stateObject(parseExecutionContext(root, e).State, "batch_attempt_history")["one"] == nil {
				t.Fatal("lost prior attempt")
			}
			child, _ = st.GetActionInstance("child")
			if status == "waiting_decision" && child.Status != StatusCancelled {
				t.Fatal("search review not retired", child)
			}
		})
	}
}

func TestBatchFileRevisionSelectsOmittedFileWithoutChild(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}})
	seedActionInstance(t, st, "batch", "transcode_batch", StatusCompleted, 2, "", map[string]any{"paths": []string{"/media/one", "/media/two"}}, nil)
	st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "one", FilePath: "/media/one", Status: "skip"})
	st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "two", FilePath: "/media/two", Status: "skip"})
	root, _ := st.GetActionInstance("batch")
	items, _ := st.ListTranscodeBatchItems("batch")
	if _, err := e.ReconfigureBatch(context.Background(), "batch", "file", "missing", BatchSelectionVersion(root, items), "", "bad", map[string]any{"profile": "new"}); err == nil {
		t.Fatal("missing file broadened selection")
	}
	if _, err := e.ReconfigureBatch(context.Background(), "batch", "file", "one", BatchSelectionVersion(root, items), "", "one", map[string]any{"profile": "new"}); err != nil {
		t.Fatal(err)
	}
	items, _ = st.ListTranscodeBatchItems("batch")
	for _, item := range items {
		if item.ItemKey == "one" && item.Status != "queued" || item.ItemKey == "two" && item.Status != "skip" {
			t.Fatal(items)
		}
	}
}

// A compact skip for missing frame evidence may be a transient probe timeout:
// resubmitting the same goal for that file is the retry.
func TestBatchFileRevisionRetriesFrameEvidenceSkipWithSameGoal(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}})
	seedActionInstance(t, st, "batch", "transcode_batch", StatusCompleted, 2, "", map[string]any{"mode": "compact"}, nil)
	seedActionInstance(t, st, "child", "transcode_media", StatusCompleted, 7, "", map[string]any{"parent_action_id": "batch", "path": "/media/one"}, nil)
	st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "one", FilePath: "/media/one", Status: "skip", ChildActionID: "child", Decision: "transcode", Reasons: []string{"compact_frame_evidence_unavailable"}})
	root, _ := st.GetActionInstance("batch")
	items, _ := st.ListTranscodeBatchItems("batch")
	if _, err := e.ReconfigureBatch(context.Background(), "batch", "file", "one", BatchSelectionVersion(root, items), "", "retry", map[string]any{"mode": "compact"}); err != nil {
		t.Fatal(err)
	}
	items, _ = st.ListTranscodeBatchItems("batch")
	if len(items) != 1 || items[0].Status != "queued" || items[0].ChildActionID != "" {
		t.Fatal("frame-evidence skip not requeued", items)
	}
	root, _ = st.GetActionInstance("batch")
	ec := parseExecutionContext(root, e)
	if reviewedItemMode(ec, items[0]) != "compact" || batchChildKey(ec, items[0]) == "batch-batch-one" {
		t.Fatal("retry did not keep the goal under a new attempt key", reviewedItemMode(ec, items[0]), batchChildKey(ec, items[0]))
	}
}

// reviewedItemMode is the goal a batch item's next attempt runs under: the one
// its review saved, else the batch's own.
func reviewedItemMode(ec *ExecutionContext, item store.TranscodeBatchItem) string {
	settings, _ := stateObject(ec.State, "batch_item_settings")[item.ItemKey].(map[string]any)
	if mode := getString(settings, "mode"); mode != "" {
		return mode
	}
	return getString(ec.Inputs, "mode")
}
