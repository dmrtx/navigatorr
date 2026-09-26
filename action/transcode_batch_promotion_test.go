package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func setupApprovedPromotionBatch(t *testing.T) (*promotionHarness, string) {
	t.Helper()
	h := newPromotionHarness(t)
	candidate, err := os.ReadFile(h.candidate)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(candidate)
	source, err := h.st.GetActionInstance("source-transcode")
	if err != nil {
		t.Fatal(err)
	}
	source.StateJSON = strings.TrimSuffix(source.StateJSON, "}") + `,"candidate_sha256":"` + hex.EncodeToString(sum[:]) + `"}`
	source.OutputsJSON = toJSON(map[string]any{"candidate_path": h.candidate})
	if err := h.st.UpdateActionInstance(*source); err != nil {
		t.Fatal(err)
	}
	batchID := "batch-promotion-test"
	if err := h.st.CreateActionInstance(store.ActionInstance{ID: batchID, ActionName: "transcode_batch", Status: StatusWaitingExternal, CurrentStep: 2, InputsJSON: `{"service":"sonarr","series_id":1,"promote_candidates":true}`, StateJSON: "{}", OutputsJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: batchID, ItemKey: "epfile-101", FilePath: h.original, EpisodeInfo: "S01E01-E02", Decision: "transcode", Status: "completed", ChildActionID: "source-transcode", CandidatePath: h.candidate}); err != nil {
		t.Fatal(err)
	}
	return h, batchID
}

func TestBatchPromotionOneApprovalAndAutomaticChild(t *testing.T) {
	h, batchID := setupApprovedPromotionBatch(t)
	r, err := h.engine.Resume(context.Background(), batchID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusWaitingDecision || h.imports != 0 {
		t.Fatalf("batch must plan before mutating Sonarr: status=%s imports=%d error=%s", r.Status, h.imports, r.Error)
	}
	r, err = h.engine.Resume(context.Background(), batchID, "approve", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12 && r.Status == StatusWaitingExternal; i++ {
		r, err = h.engine.Resume(context.Background(), batchID, "", nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if r.Status != StatusCompleted || h.imports != 1 || h.deletes != 1 {
		t.Fatalf("one batch approval should promote candidate: status=%s error=%s imports=%d deletes=%d", r.Status, r.Error, h.imports, h.deletes)
	}
	child, err := h.st.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:sonarr:source-transcode")
	if err != nil || child == nil || child.Status != StatusCompleted {
		t.Fatalf("automatic promotion child was not completed: child=%+v err=%v", child, err)
	}
	h.restart()
	r, err = h.engine.Resume(context.Background(), batchID, "", nil)
	if err != nil || r.Status != StatusCompleted || h.imports != 1 {
		t.Fatalf("restart must not duplicate the import: status=%s err=%v imports=%d", r.Status, err, h.imports)
	}
}

func TestBatchPromotionChildCannotApproveBeforeParent(t *testing.T) {
	h, batchID := setupApprovedPromotionBatch(t)
	if _, err := h.engine.Resume(context.Background(), batchID, "", nil); err != nil {
		t.Fatal(err)
	}
	parent, err := h.st.GetActionInstance(batchID)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(parent.StateJSON), &state); err != nil {
		t.Fatal(err)
	}
	plan := getBatchPromotionPlan(state["batch_promotion_plan"])
	r, err := h.engine.Run(context.Background(), "promote_transcode_candidate", map[string]any{"transcode_action_id": "source-transcode", "series_id": 1, "service": "sonarr", "batch_promote_parent_id": batchID, "batch_promote_item_key": "epfile-101", "batch_promote_digest": plan.Digest})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusFailed || h.imports != 0 {
		t.Fatalf("child accepted unapproved parent: status=%s imports=%d", r.Status, h.imports)
	}
}

func TestBatchPromotionRejectKeepsFiles(t *testing.T) {
	h, batchID := setupApprovedPromotionBatch(t)
	if _, err := h.engine.Resume(context.Background(), batchID, "", nil); err != nil {
		t.Fatal(err)
	}
	r, err := h.engine.Resume(context.Background(), batchID, "reject", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusCompleted || h.imports != 0 {
		t.Fatalf("rejection should keep files: status=%s imports=%d", r.Status, h.imports)
	}
	if _, err := os.Stat(h.original); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.candidate); err != nil {
		t.Fatal(err)
	}
}
