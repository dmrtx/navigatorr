package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// A promotion approved ahead (reviewed by a person, or its job replaces each
// file when done) replaces the file without waiting for a decision; switched
// off by then, or with a regrab warning, it waits as before.
func TestPromotionApprovedAheadReplacesOnlyWhileAllowed(t *testing.T) {
	run := func(h *promotionHarness, approval string) *ActionResult {
		t.Helper()
		h.engine.deps.Registry = nil
		if approval != "" {
			if err := h.st.GrantReplacementApproval("source-transcode", approval); err != nil {
				t.Fatal(err)
			}
		}
		r, err := h.engine.Run(context.Background(), "promote_transcode_candidate", map[string]any{"service": "filesystem", "transcode_action_id": "source-transcode"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	t.Run("reviewed", func(t *testing.T) {
		h := newPromotionHarness(t)
		r := run(h, ApprovalReviewed)
		if r.Status != StatusCompleted || r.Outputs["promoted"] != true {
			t.Fatalf("reviewed replacement waited: %s %q", r.Status, r.WaitingReason)
		}
		if _, err := os.Stat(h.final); err != nil {
			t.Fatal("new file not in place", err)
		}
	})
	t.Run("automatic while on", func(t *testing.T) {
		h := newPromotionHarness(t)
		if err := h.st.SetReplaceWhenDone("source-transcode", true); err != nil {
			t.Fatal(err)
		}
		if r := run(h, ApprovalAutomatic); r.Status != StatusCompleted || r.Outputs["promoted"] != true {
			t.Fatalf("automatic replacement waited: %s %q", r.Status, r.WaitingReason)
		}
	})
	t.Run("automatic switched off", func(t *testing.T) {
		h := newPromotionHarness(t)
		if err := h.st.SetReplaceWhenDone("source-transcode", false); err != nil {
			t.Fatal(err)
		}
		r := run(h, ApprovalAutomatic)
		if r.Status != StatusWaitingDecision || !strings.Contains(r.WaitingReason, "switched off") {
			t.Fatalf("switched-off replacement went ahead: %s %q", r.Status, r.WaitingReason)
		}
		if _, err := os.Stat(h.original); err != nil {
			t.Fatal("original touched", err)
		}
	})
	t.Run("no approval waits", func(t *testing.T) {
		h := newPromotionHarness(t)
		if r := run(h, ""); r.Status != StatusWaitingDecision {
			t.Fatalf("unapproved replacement went ahead: %s", r.Status)
		}
	})
	t.Run("regrab warning still stops", func(t *testing.T) {
		h := newPromotionHarness(t)
		h.tracking = map[string]any{"monitored": true, "qualityProfileId": 4}
		markCompactSource(t, h)
		if err := h.st.GrantReplacementApproval("source-transcode", ApprovalReviewed); err != nil {
			t.Fatal(err)
		}
		r, err := h.engine.Run(context.Background(), "promote_transcode_candidate", map[string]any{"transcode_action_id": "source-transcode", "series_id": 1})
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != StatusWaitingDecision || !strings.Contains(r.WaitingReason, CompactRegrabWarning) {
			t.Fatalf("regrab warning skipped: %s %q", r.Status, r.WaitingReason)
		}
		if h.imports != 0 || len(h.mutationOrder) != 0 {
			t.Fatalf("library changed: %v", h.mutationOrder)
		}
	})
}

func finishingTranscode(t *testing.T, firstRunning bool) (*Engine, string) {
	t.Helper()
	var calls int32
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0o755)
			_ = os.WriteFile(req.CandidatePath, []byte("transcoded-media"), 0o644)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			if firstRunning && atomic.AddInt32(&calls, 1) == 1 {
				return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning, Progress: 40}, nil
			}
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted, Progress: 100}, nil
		},
	}
	dir := t.TempDir()
	media := filepath.Join(dir, "Episode.mkv")
	if err := os.WriteFile(media, []byte("synthetic-source-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, _ := setupTranscodeEngine(t, mock, createSmartFakeFFprobeScript(t, standard8BitProbeJSON), []string{dir}, []string{dir}, true)
	e.deps.Registry = nil
	return e, media
}

func autoPromotion(t *testing.T, e *Engine, id string) map[string]any {
	t.Helper()
	inst, err := e.deps.Store.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:filesystem:"+id)
	if err != nil {
		t.Fatal(err)
	}
	if inst == nil {
		return nil
	}
	inputs := decodeInputs(inst.InputsJSON)
	if a, _ := e.deps.Store.GetReplacementApproval(id); a != nil {
		inputs["server_approval"] = a.Approval
	}
	return inputs
}

func TestReplaceWhenDoneStartsTheReplacementOnlyWhenAsked(t *testing.T) {
	t.Run("on at submit", func(t *testing.T) {
		e, media := finishingTranscode(t, false)
		r, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": media, "profile": "hevc-vt", "replace_when_done": true})
		if err != nil || r.Status != StatusCompleted {
			t.Fatal(r, err)
		}
		inputs := autoPromotion(t, e, r.ID)
		if inputs == nil || inputs["server_approval"] != ApprovalAutomatic || inputs["service"] != "filesystem" {
			t.Fatalf("no automatic replacement: %v", inputs)
		}
	})
	t.Run("default is off", func(t *testing.T) {
		e, media := finishingTranscode(t, false)
		r, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": media, "profile": "hevc-vt"})
		if err != nil || r.Status != StatusCompleted {
			t.Fatal(r, err)
		}
		inst, _ := e.deps.Store.GetActionInstance(r.ID)
		if got := decodeInputs(inst.InputsJSON)["replace_when_done"]; got != false {
			t.Fatalf("default not frozen as false: %v", got)
		}
		if inputs := autoPromotion(t, e, r.ID); inputs != nil {
			t.Fatalf("replaced without being asked: %v", inputs)
		}
	})
	t.Run("server default on", func(t *testing.T) {
		e, media := finishingTranscode(t, false)
		if err := e.deps.Store.SetReplaceWhenDoneDefault(true); err != nil {
			t.Fatal(err)
		}
		r, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": media, "profile": "hevc-vt"})
		if err != nil || r.Status != StatusCompleted {
			t.Fatal(r, err)
		}
		if inputs := autoPromotion(t, e, r.ID); inputs == nil {
			t.Fatal("server default ignored")
		}
	})
	t.Run("switched on while encoding", func(t *testing.T) {
		e, media := finishingTranscode(t, true)
		r, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": media, "profile": "hevc-vt", "replace_when_done": false})
		if err != nil || r.Status != StatusWaitingExternal {
			t.Fatal(r, err)
		}
		if _, err := e.SetReplaceWhenDone(context.Background(), r.ID, true); err != nil {
			t.Fatal(err)
		}
		if r, err = e.Resume(context.Background(), r.ID, "", nil); err != nil || r.Status != StatusCompleted {
			t.Fatal(r, err)
		}
		if inputs := autoPromotion(t, e, r.ID); inputs == nil {
			t.Fatal("switching on was ignored")
		}
		if _, err := e.SetReplaceWhenDone(context.Background(), r.ID, true); err != ErrReplaceWhenDoneFinished {
			t.Fatalf("a finished job was switched on: %v", err)
		}
	})
	t.Run("switched off while encoding", func(t *testing.T) {
		e, media := finishingTranscode(t, true)
		r, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": media, "profile": "hevc-vt", "replace_when_done": true})
		if err != nil || r.Status != StatusWaitingExternal {
			t.Fatal(r, err)
		}
		if _, err := e.SetReplaceWhenDone(context.Background(), r.ID, false); err != nil {
			t.Fatal(err)
		}
		if r, err = e.Resume(context.Background(), r.ID, "", nil); err != nil || r.Status != StatusCompleted {
			t.Fatal(r, err)
		}
		if inputs := autoPromotion(t, e, r.ID); inputs != nil {
			t.Fatalf("switching off was ignored: %v", inputs)
		}
	})
	t.Run("not a boolean", func(t *testing.T) {
		e, media := finishingTranscode(t, false)
		if _, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": media, "profile": "hevc-vt", "replace_when_done": "yes"}); err == nil {
			t.Fatal("accepted a string")
		}
	})
}

// An approval granted ahead is used once: the first promotion to reach its
// approval keeps it, and another promotion of the file waits.
func TestAnApprovalAheadIsBoundToOnePromotion(t *testing.T) {
	h := newPromotionHarness(t)
	h.engine.deps.Registry = nil
	if err := h.st.GrantReplacementApproval("source-transcode", ApprovalReviewed); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.st.BindReplacementApproval("source-transcode", ApprovalReviewed, "someone-else"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	r, err := h.engine.Run(context.Background(), "promote_transcode_candidate", map[string]any{"service": "filesystem", "transcode_action_id": "source-transcode"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusWaitingDecision {
		t.Fatalf("an approval kept by another promotion was used again: %s", r.Status)
	}
}

// Review item 5: switching off a job that has already finished still stops
// its automatic replacement while that has not been approved.
func TestSwitchingOffAFinishedJobHoldsItsAutomaticReplacement(t *testing.T) {
	e, media := finishingTranscode(t, false)
	r, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": media, "profile": "hevc-vt", "replace_when_done": true})
	if err != nil || r.Status != StatusCompleted {
		t.Fatal(r, err)
	}
	promote, err := e.deps.Store.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:filesystem:"+r.ID)
	if err != nil || promote == nil {
		t.Fatal("no automatic replacement", err)
	}
	if _, err := e.SetReplaceWhenDone(context.Background(), r.ID, false); err != nil {
		t.Fatalf("a finished job whose replacement is not approved could not be switched off: %v", err)
	}
	inst, _ := e.deps.Store.GetActionInstance(r.ID)
	if e.ReplaceWhenDone(inst) {
		t.Fatal("still on")
	}
	// At its approval the promotion now waits for a decision (the harness
	// in TestPromotionApprovedAheadReplacesOnlyWhileAllowed runs that step).
	if ok, why := e.promotionApprovedAhead(&ExecutionContext{InstanceID: promote.ID}, &promotionState{SourceActionID: r.ID}); ok || !strings.Contains(why, "switched off") {
		t.Fatalf("the replacement would go ahead after the switch: %v %q", ok, why)
	}
	// Once nothing is outstanding, an ended job cannot be switched.
	if _, err := e.SetReplaceWhenDone(context.Background(), r.ID, false); err != nil {
		t.Fatalf("switching off again: %v", err)
	}
	if _, err := e.SetReplaceWhenDone(context.Background(), r.ID, true); err != ErrReplaceWhenDoneFinished {
		t.Fatalf("a finished job was switched on: %v", err)
	}
}

// Review item 6: an automatic replacement that could not be submitted when
// its job finished (a crash, an admission error) is said on the job and
// submitted by the reconciler.
func TestAnAutomaticReplacementLeftBehindIsSubmittedLater(t *testing.T) {
	t.Run("crash after the job was saved", func(t *testing.T) {
		e, media := finishingTranscode(t, false)
		r, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": media, "profile": "hevc-vt", "replace_when_done": false})
		if err != nil || r.Status != StatusCompleted {
			t.Fatal(r, err)
		}
		// As the completion leaves it just before submitting.
		if err := e.deps.Store.SetReplaceWhenDone(r.ID, true); err != nil {
			t.Fatal(err)
		}
		if err := e.deps.Store.AddAutoReplacePending(r.ID); err != nil {
			t.Fatal(err)
		}
		if err := e.ReconcileOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if inputs := autoPromotion(t, e, r.ID); inputs == nil || inputs["server_approval"] != ApprovalAutomatic {
			t.Fatalf("the reconciler did not submit it: %v", inputs)
		}
		if pending, _, _ := e.deps.Store.AutoReplacePending(r.ID); pending {
			t.Fatal("still pending")
		}
	})
	t.Run("admission error", func(t *testing.T) {
		e, media := finishingTranscode(t, false)
		e.beforeAutoEnqueue = func(string) error { return errors.New("admission is closed") }
		r, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": media, "profile": "hevc-vt", "replace_when_done": true})
		if err != nil || r.Status != StatusCompleted {
			t.Fatal(r, err)
		}
		inst, _ := e.deps.Store.GetActionInstance(r.ID)
		if skipped := getString(decodeInputs(inst.OutputsJSON), "replace_when_done_skipped"); !strings.Contains(skipped, "admission is closed") {
			t.Fatalf("the failure is not said on the job: %q", skipped)
		}
		if inputs := autoPromotion(t, e, r.ID); inputs != nil {
			t.Fatal("submitted anyway", inputs)
		}
		if a, _ := e.deps.Store.GetReplacementApproval(r.ID); a != nil {
			t.Fatal("an approval outlived the failed submission", a)
		}
		e.beforeAutoEnqueue = nil
		if err := e.ReconcileOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if inputs := autoPromotion(t, e, r.ID); inputs == nil {
			t.Fatal("not submitted once admission worked")
		}
	})
}
