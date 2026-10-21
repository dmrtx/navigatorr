package action

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// oversizedCandidateExecutor produces a candidate larger than its source, which
// parks the child on a decision and the batch on the child's behalf.
func oversizedCandidateExecutor() *mockTranscodeExecutor {
	var candidate string
	return &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			candidate = req.CandidatePath
			if candidate != "" {
				_ = os.MkdirAll(filepath.Dir(candidate), 0755)
				f, _ := os.Create(candidate)
				_ = f.Truncate(2000000000)
				_ = f.Close()
			}
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted, CandidatePath: candidate}, nil
		},
	}
}

func startBatchWaitingOnChild(t *testing.T) (*Engine, *store.Store, string, string) {
	t.Helper()
	engine, st, _, srv, _ := setupBatchTestEnv(t, oversizedCandidateExecutor(), 1)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = st.Close() })
	res, err := engine.Run(context.Background(), "transcode_batch", map[string]any{"service": "sonarr", "series_id": 10, "season": 1, "dry_run": false})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != StatusWaitingDecision {
		t.Fatalf("batch status %s, want waiting_decision", res.Status)
	}
	items, _ := st.ListTranscodeBatchItems(res.ID)
	if len(items) != 1 || items[0].Status != "waiting_decision" || items[0].ChildActionID == "" {
		t.Fatalf("items %+v, want one waiting_decision item with a child", items)
	}
	return engine, st, res.ID, items[0].ChildActionID
}

func storedBatchCounts(t *testing.T, st *store.Store, id string) map[string]int {
	t.Helper()
	inst, err := st.GetActionInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	var outputs struct {
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal([]byte(inst.OutputsJSON), &outputs); err != nil {
		t.Fatal(err)
	}
	return outputs.Counts
}

// Rejecting the waiting child by its own ID, as action_resume does, must move
// the batch on: it leaves waiting_decision and its counters follow the child.
func TestRejectingWaitingChildDirectlyReevaluatesBatch(t *testing.T) {
	engine, st, batchID, childID := startBatchWaitingOnChild(t)

	if _, err := engine.Resume(context.Background(), childID, "reject", nil); err != nil {
		t.Fatalf("reject child: %v", err)
	}

	batch, _ := st.GetActionInstance(batchID)
	if batch.Status != StatusCompleted {
		t.Fatalf("batch status %s, want completed once its only child is rejected", batch.Status)
	}
	if batch.WaitingReason != "" || batch.WaitingOptionsJSON != "[]" {
		t.Fatalf("batch kept the child's decision: reason %q options %s", batch.WaitingReason, batch.WaitingOptionsJSON)
	}
	items, _ := st.ListTranscodeBatchItems(batchID)
	if items[0].Status != "failed" {
		t.Fatalf("item status %s, want failed", items[0].Status)
	}
	counts := storedBatchCounts(t, st, batchID)
	if counts["waiting_decision"] != 0 || counts["failed"] != 1 {
		t.Fatalf("counts %v, want waiting_decision 0 and failed 1", counts)
	}
}

// A child resolved without any refresh (for example the process stopped in
// between) is picked up by the reconciler instead of waiting forever.
func TestReconcilerRetiresBatchDecisionNoChildNeeds(t *testing.T) {
	engine, st, batchID, childID := startBatchWaitingOnChild(t)

	// Resolving the child below the engine leaves the batch row untouched.
	child, _ := st.GetActionInstance(childID)
	child.Status, child.WaitingReason, child.WaitingOptionsJSON = StatusFailed, "", "[]"
	child.ErrorJSON = "transcode candidate rejected by user decision; original file remains untouched"
	if err := st.UpdateActionInstance(*child); err != nil {
		t.Fatal(err)
	}
	if batch, _ := st.GetActionInstance(batchID); batch.Status != StatusWaitingDecision {
		t.Fatalf("setup: batch status %s, want waiting_decision", batch.Status)
	}

	if err := engine.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	batch, _ := st.GetActionInstance(batchID)
	if batch.Status != StatusCompleted {
		t.Fatalf("batch status %s, want completed", batch.Status)
	}
	if counts := storedBatchCounts(t, st, batchID); counts["waiting_decision"] != 0 || counts["failed"] != 1 {
		t.Fatalf("counts %v, want waiting_decision 0 and failed 1", counts)
	}
}

// While a child still needs its decision, the reconciler leaves the batch alone.
func TestReconcilerKeepsBatchWhileChildStillWaits(t *testing.T) {
	engine, st, batchID, _ := startBatchWaitingOnChild(t)

	if err := engine.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if batch, _ := st.GetActionInstance(batchID); batch.Status != StatusWaitingDecision {
		t.Fatalf("batch status %s, want waiting_decision", batch.Status)
	}
	if counts := storedBatchCounts(t, st, batchID); counts["waiting_decision"] != 1 {
		t.Fatalf("counts %v, want waiting_decision 1", counts)
	}
}

// Cancelling the batch stops its waiting child; the stored counters must show
// that file as cancelled, not still waiting for a decision.
func TestCancellingWaitingBatchSettlesCounters(t *testing.T) {
	engine, st, batchID, childID := startBatchWaitingOnChild(t)

	if _, err := engine.Cancel(context.Background(), batchID, "test"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	if child, _ := st.GetActionInstance(childID); child.Status != StatusCancelled {
		t.Fatalf("child status %s, want cancelled", child.Status)
	}
	items, _ := st.ListTranscodeBatchItems(batchID)
	if items[0].Status != "cancelled" {
		t.Fatalf("item status %s, want cancelled", items[0].Status)
	}
	if counts := storedBatchCounts(t, st, batchID); counts["waiting_decision"] != 0 || counts["cancelled"] != 1 {
		t.Fatalf("counts %v, want waiting_decision 0 and cancelled 1", counts)
	}
}
