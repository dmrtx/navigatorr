package action

import (
	"context"
	"fmt"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
)

// seasonChildTemplate stands in for transcode_media: each search waits once,
// then finishes having spent slightly more than its per-source budget, as a
// worker that stops at its deadline reports.
func seasonChildTemplate(spent float64) ActionTemplate {
	return ActionTemplate{Name: "transcode_media", ImmutableInputs: true, Steps: []StepDefinition{{Name: "search", Run: func(_ context.Context, ec *ExecutionContext) (StepResult, error) {
		if !getBool(ec.State, "searching") {
			ec.State["searching"], ec.State["benchmark_submitted"] = true, true
			return StepResult{Status: StepWaitingExternal, WaitingCondition: "benchmark_processing"}, nil
		}
		ec.State["benchmark_search_seconds"] = spent
		return StepResult{Status: StepCompleted}, nil
	}}}}
}

func seedModeSeason(t *testing.T, st *store.Store, id, mode string, files int, status string) *ExecutionContext {
	t.Helper()
	ec := &ExecutionContext{InstanceID: id, Inputs: map[string]any{"mode": mode}, State: map[string]any{}}
	if _, err := freezeModePolicy(ec); err != nil {
		t.Fatal(err)
	}
	seedActionInstance(t, st, id, "transcode_batch", status, 1, "", ec.Inputs, ec.State)
	for i := 0; i < files; i++ {
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: id, ItemKey: fmt.Sprintf("file-%d", i), FilePath: fmt.Sprintf("/media/%d.mkv", i), Status: "queued", Decision: "transcode"}); err != nil {
			t.Fatal(err)
		}
	}
	return ec
}

func TestModeBatchAdmitsWholeSeasonWithTwoSlots(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{Transcode: config.TranscodeConfig{MaxParallelJobs: 2}}})
	e.RegisterTemplate(seasonChildTemplate(600.006))
	ec := seedModeSeason(t, st, "season", "size", 10, StatusWaitingExternal)
	for round := 0; round < 20; round++ {
		if _, err := e.Resume(context.Background(), ec.InstanceID, "", nil); err != nil {
			t.Fatal(err)
		}
		items, _ := st.ListTranscodeBatchItems(ec.InstanceID)
		running, completed := 0, 0
		for _, item := range items {
			switch item.Status {
			case "running":
				running++
			case "completed":
				completed++
			}
		}
		if running > 2 {
			t.Fatalf("round %d: %d searches active with 2 slots", round, running)
		}
		if completed == len(items) {
			root, _ := st.GetActionInstance(ec.InstanceID)
			entries := modeBudgetEntries(parseExecutionContext(root, e))
			if len(entries) != 10 {
				t.Fatalf("expected one reservation per file, got %d", len(entries))
			}
			for key, entry := range entries {
				if entry.Consumed == nil || *entry.Consumed != 600 || entry.Provisional {
					t.Fatalf("overshooting search not settled at its reservation: %s %+v", key, entry)
				}
			}
			return
		}
	}
	items, _ := st.ListTranscodeBatchItems(ec.InstanceID)
	t.Fatalf("season stalled: %+v", items)
}

func TestModeBatchWaitingDecisionHoldsNoBudget(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st})
	ec := &ExecutionContext{InstanceID: "parent", Inputs: map[string]any{"mode": "size"}, State: map[string]any{}}
	if _, err := freezeModePolicy(ec); err != nil {
		t.Fatal(err)
	}
	item := store.TranscodeBatchItem{ItemKey: "file-a", ChildActionID: "review-child"}
	key := batchChildKey(ec, item)
	if !reserveModeSearch(ec, key) {
		t.Fatal("initial reservation failed")
	}
	// A search review stops before submission: nothing is being spent.
	seedActionInstance(t, st, item.ChildActionID, "transcode_media", StatusWaitingDecision, 1, key, ec.Inputs, map[string]any{"benchmark_search_review_required": true})
	e.settleModeSearches(ec, []store.TranscodeBatchItem{item})
	if entry := modeBudgetEntries(ec)[key]; entry.Consumed == nil || *entry.Consumed != 0 || !entry.Provisional {
		t.Fatalf("waiting decision kept its reservation: %+v", entry)
	}
	// Run one more test resumes the same attempt: its reservation reopens.
	child, _ := st.GetActionInstance(item.ChildActionID)
	child.Status, child.StateJSON = StatusWaitingExternal, toJSON(map[string]any{"benchmark_submitted": true})
	if err := st.UpdateActionInstance(*child); err != nil {
		t.Fatal(err)
	}
	e.settleModeSearches(ec, []store.TranscodeBatchItem{item})
	if entry := modeBudgetEntries(ec)[key]; entry.Consumed != nil || entry.Reserved != 600 {
		t.Fatalf("resumed search lost its reservation: %+v", entry)
	}
	child.Status, child.StateJSON = StatusWaitingDecision, toJSON(map[string]any{"benchmark_submitted": true, "benchmark_search_seconds": 120.5, "reason_code": "search_budget_exhausted"})
	if err := st.UpdateActionInstance(*child); err != nil {
		t.Fatal(err)
	}
	e.settleModeSearches(ec, []store.TranscodeBatchItem{item})
	if entry := modeBudgetEntries(ec)[key]; entry.Consumed == nil || *entry.Consumed != 120.5 || entry.Provisional {
		t.Fatalf("measured search in a waiting decision not settled: %+v", entry)
	}
}

func TestModeBatchRetryReservesAfterSeasonSpentItsBudget(t *testing.T) {
	ec := &ExecutionContext{InstanceID: "parent", Inputs: map[string]any{"mode": "size"}, State: map[string]any{}}
	if _, err := freezeModePolicy(ec); err != nil {
		t.Fatal(err)
	}
	entries := map[string]SearchBudgetEntry{}
	for i := 0; i < 10; i++ {
		spent := 600.0
		entries[fmt.Sprintf("batch-parent-file-%d", i)] = SearchBudgetEntry{Reserved: 600, Consumed: &spent}
	}
	ec.State["search_budget_entries"] = entries
	item := store.TranscodeBatchItem{ItemKey: "file-3", ChildActionID: "old"}
	recordBatchAttempt(ec, &item)
	key := batchChildKey(ec, item)
	if key != "batch-parent-file-3-attempt-1" || !reserveModeSearch(ec, key) {
		t.Fatalf("changed goal could not reserve a new attempt %q", key)
	}
	ec.State["search_budget_seconds"] = 0
	if reserveModeSearch(ec, "unbudgeted") {
		t.Fatal("a policy without a per-source budget must not admit searches")
	}
}

// A batch stopped by the retired flat cap before deploy resumes by itself.
func TestLegacyBudgetStoppedBatchResumesAutomatically(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{Transcode: config.TranscodeConfig{MaxParallelJobs: 2}}})
	e.RegisterTemplate(seasonChildTemplate(300))
	ec := seedModeSeason(t, st, "legacy", "size", 4, StatusWaitingDecision)
	for i := 0; i < 6; i++ {
		reserveModeSearch(ec, fmt.Sprintf("earlier-%d", i))
	}
	root, _ := st.GetActionInstance(ec.InstanceID)
	ec.State["reason_code"] = "search_budget_exhausted"
	root.WaitingCondition, root.WaitingReason, root.StateJSON = "search_budget_exhausted", "Operation search budget is reserved or exhausted", toJSON(ec.State)
	if err := st.UpdateActionInstance(*root); err != nil {
		t.Fatal(err)
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	root, _ = st.GetActionInstance(ec.InstanceID)
	items, _ := st.ListTranscodeBatchItems(ec.InstanceID)
	admitted := 0
	for _, item := range items {
		if item.ChildActionID != "" {
			admitted++
		}
	}
	if root.Status == StatusWaitingDecision || admitted != len(items) {
		t.Fatalf("legacy stopped batch not resumed: %s %s admitted=%d", root.Status, root.WaitingCondition, admitted)
	}
	if reason := getString(parseExecutionContext(root, e).State, "reason_code"); reason != "" {
		t.Fatalf("resumed batch still reports %q", reason)
	}
	// Other waiting decisions still need the person.
	seedActionInstance(t, st, "other", "transcode_batch", StatusWaitingDecision, 1, "", ec.Inputs, ec.State)
	if e.shouldReconcile(mustAction(t, st, "other"), ActionTemplate{AutoReconcile: true}, ec) {
		t.Fatal("ordinary waiting decision resumed without a decision")
	}
}

func mustAction(t *testing.T, st *store.Store, id string) *store.ActionInstance {
	t.Helper()
	inst, err := st.GetActionInstance(id)
	if err != nil || inst == nil {
		t.Fatalf("action %s: %v", id, err)
	}
	return inst
}
