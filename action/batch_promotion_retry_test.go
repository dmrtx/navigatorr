package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
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

// The guard refuses only a not-yet-approved member of a failed batch. A
// single-file replacement, a member whose batch is still going, a member
// already past approve_promotion and a member whose batch cannot be read all
// retry as before.
func TestBatchPromotionRetryGuardAllowsEveryOtherRetry(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := NewEngine(EngineDeps{Store: st})
	tmpl, ok := e.GetTemplate("promote_transcode_candidate")
	if !ok {
		t.Fatal("promote_transcode_candidate not registered")
	}
	approve := actionStepIndex(tmpl, "approve_promotion")
	if approve < 0 {
		t.Fatal("approve_promotion step missing")
	}
	for id, status := range map[string]string{"batch-failed": StatusFailed, "batch-waiting": StatusWaitingDecision, "batch-running": StatusRunning} {
		if err := st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: "transcode_batch", Status: status, InputsJSON: "{}", OutputsJSON: "{}", StateJSON: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, action, parent string
		step                 int
		refused              bool
	}{
		{"member of a failed batch, not yet approved", "promote_transcode_candidate", "batch-failed", 0, true},
		{"member of a failed batch, at approval", "promote_transcode_candidate", "batch-failed", approve, true},
		{"single-file replacement", "promote_transcode_candidate", "", 0, false},
		{"member of a batch waiting for a decision", "promote_transcode_candidate", "batch-waiting", 0, false},
		{"member of a running batch", "promote_transcode_candidate", "batch-running", 0, false},
		{"member past approve_promotion", "promote_transcode_candidate", "batch-failed", approve + 1, false},
		{"member whose batch cannot be read", "promote_transcode_candidate", "batch-missing", 0, false},
		{"another action", "transcode_media", "batch-failed", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := &store.ActionInstance{ID: "member", ActionName: tc.action, Status: StatusFailed, CurrentStep: tc.step}
			ec := &ExecutionContext{Inputs: map[string]any{}, State: map[string]any{}}
			if tc.parent != "" {
				ec.Inputs["batch_promote_parent_id"] = tc.parent
			}
			err := e.batchPromotionRetryOwner(inst, ec, tmpl)
			if tc.refused != (err != nil) {
				t.Fatalf("refused=%v, want %v: %v", err != nil, tc.refused, err)
			}
			if err != nil && !strings.Contains(err.Error(), "retry the batch instead") {
				t.Fatalf("error %v", err)
			}
		})
	}
}
