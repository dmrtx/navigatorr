package action

import (
	"context"
	"os"
	"strings"
	"testing"
)

// One member of an approved batch fails promotion validation. Retrying the
// batch retries only that member's checks, against the approved plan and the
// candidate's SHA-256, and replaces it without encoding anything; retrying
// the member alone is refused while the batch has failed, since its approval
// comes only from a running batch.
func TestBatchRetryRetriesOnlyTheFailedPromotion(t *testing.T) {
	h, batchID := setupApprovedPromotionBatch(t)
	probe := h.engine.Deps().Ffprobe
	good, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	// A probe that disagrees with the plan, as the stale-tag bitrate did.
	if err := os.WriteFile(probe, []byte(strings.Replace(string(good), `"codec_name":"hevc"`, `"codec_name":"h264"`, 1)), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Resume(context.Background(), batchID, "", nil); err != nil {
		t.Fatal(err)
	}
	r, err := h.engine.Resume(context.Background(), batchID, "approve", nil)
	for i := 0; err == nil && i < 12 && r.Status == StatusWaitingExternal; i++ {
		r, err = h.engine.Resume(context.Background(), batchID, "", nil)
	}
	if err != nil || r.Status != StatusFailed || !strings.Contains(r.Error, "candidate failed promotion validation") || h.imports != 0 {
		t.Fatalf("batch: status=%s error=%s imports=%d err=%v", r.Status, r.Error, h.imports, err)
	}
	child, err := h.st.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:sonarr:source-transcode")
	if err != nil || child == nil || child.Status != StatusFailed || child.CurrentStep != 0 {
		t.Fatalf("child %+v %v", child, err)
	}
	if _, err := h.engine.Retry(context.Background(), child.ID); err == nil || !strings.Contains(err.Error(), "retry the batch instead") {
		t.Fatalf("direct member retry: %v", err)
	}
	if stored, _ := h.st.GetActionInstance(child.ID); stored.Status != StatusFailed || stored.CurrentStep != 0 {
		t.Fatalf("refused retry moved the member: %s at %d", stored.Status, stored.CurrentStep)
	}
	// The validation is fixed; the same candidate now passes.
	if err := os.WriteFile(probe, good, 0o700); err != nil {
		t.Fatal(err)
	}
	h.restart()
	r, err = h.engine.Retry(context.Background(), batchID)
	for i := 0; err == nil && i < 12 && r.Status == StatusWaitingExternal; i++ {
		r, err = h.engine.Resume(context.Background(), batchID, "", nil)
	}
	if err != nil || r.Status != StatusCompleted || h.imports != 1 || h.deletes != 1 {
		t.Fatalf("batch retry: status=%s error=%s imports=%d deletes=%d err=%v", r.Status, r.Error, h.imports, h.deletes, err)
	}
	if stored, _ := h.st.GetActionInstance(child.ID); stored.Status != StatusCompleted {
		t.Fatalf("member %s", stored.Status)
	}
}
