package action

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runLocalPromotion(t *testing.T, h *promotionHarness) *ActionResult {
	t.Helper()
	h.engine.deps.Registry = nil
	r, err := h.engine.Run(context.Background(), "promote_transcode_candidate", map[string]any{"service": "filesystem", "transcode_action_id": "source-transcode"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFilesystemPromotionRequiresApprovalAndVerifiesCleanup(t *testing.T) {
	h := newPromotionHarness(t)
	r := runLocalPromotion(t, h)
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	if _, err := os.Stat(h.original); err != nil {
		t.Fatal("original removed before approval")
	}
	if _, err := os.Stat(h.final); !os.IsNotExist(err) {
		t.Fatal("candidate published before approval")
	}
	r = h.resume(r.ID, "approve")
	if r.Status != StatusCompleted || r.Outputs["promoted"] != true || r.Outputs["recovery_retained"] != false {
		t.Fatalf("local replacement: %+v", r)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil || !p.RecoveryCleanupCompleted || !p.RecoveryVerifiedBeforeReplacement {
		t.Fatalf("missing recovery evidence: %+v %v", p, err)
	}
	if err := h.engine.verifyPromotionHash(context.Background(), h.final, p.CandidateSHA); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{h.original, h.candidate, p.BackupPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("old/private file retained after completion: %s", path)
		}
	}
	if len(h.commands) != 0 {
		t.Fatal("filesystem replacement called a library service")
	}
}

func TestFilesystemPromotionFailsClosedOnChangedOriginalCandidateOrTarget(t *testing.T) {
	for _, changed := range []string{"original", "candidate", "target"} {
		t.Run(changed, func(t *testing.T) {
			h := newPromotionHarness(t)
			r := runLocalPromotion(t, h)
			if r.Status != StatusWaitingDecision {
				t.Fatalf("plan: %+v", r)
			}
			path := map[string]string{"original": h.original, "candidate": h.candidate, "target": h.final}[changed]
			if err := os.WriteFile(path, []byte("changed after approval screen"), 0600); err != nil {
				t.Fatal(err)
			}
			r = h.resume(r.ID, "approve")
			if r.Status != StatusFailed {
				t.Fatalf("drift accepted: %+v", r)
			}
			if _, err := os.Stat(h.original); err != nil {
				t.Fatal("original removed on integrity failure")
			}
			if changed == "target" {
				p, err := loadPromotion(&ExecutionContext{State: r.State})
				if err != nil || h.engine.verifyPromotionHash(context.Background(), p.BackupPath, p.OriginalSHA) != nil {
					t.Fatal("recovery was not retained")
				}
			}
		})
	}
}

func TestFilesystemPromotionReconcilesPublishedCandidateAfterRestart(t *testing.T) {
	h := newPromotionHarness(t)
	tmpl, _ := h.engine.GetTemplate("promote_transcode_candidate")
	run := tmpl.Steps[3].Run
	tmpl.Steps[3].Run = func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
		result, err := run(ctx, ec)
		if err != nil || result.Status != StepCompleted {
			return result, err
		}
		return StepResult{Status: StepFailed, Error: "simulated crash after atomic publication"}, nil
	}
	h.engine.RegisterTemplate(tmpl)
	r := runLocalPromotion(t, h)
	r = h.resume(r.ID, "approve")
	if r.Status != StatusFailed || r.CurrentStep != 3 {
		t.Fatalf("fixture did not stop after publication: %+v", r)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil || h.engine.verifyPromotionHash(context.Background(), p.BackupPath, p.OriginalSHA) != nil {
		t.Fatal("missing recovery checkpoint")
	}
	if _, err := os.Stat(h.candidate); !os.IsNotExist(err) {
		t.Fatal("atomic publication did not consume candidate")
	}
	h.restart()
	r, err = h.engine.Retry(context.Background(), r.ID)
	if err != nil || r.Status != StatusCompleted {
		t.Fatalf("restart failed to reconcile existing published bytes: %+v %v", r, err)
	}
}

func TestFilesystemPromotionRetainsRecoveryWhenFinalBytesDrift(t *testing.T) {
	h := newPromotionHarness(t)
	tmpl, _ := h.engine.GetTemplate("promote_transcode_candidate")
	run := tmpl.Steps[3].Run
	tmpl.Steps[3].Run = func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
		result, err := run(ctx, ec)
		if err != nil || result.Status != StepCompleted {
			return result, err
		}
		if err := os.WriteFile(h.final, []byte("unapproved final bytes"), 0600); err != nil {
			return StepResult{}, err
		}
		return result, nil
	}
	h.engine.RegisterTemplate(tmpl)
	r := runLocalPromotion(t, h)
	r = h.resume(r.ID, "approve")
	if r.Status != StatusFailed {
		t.Fatalf("drift accepted: %+v", r)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil || h.engine.verifyPromotionHash(context.Background(), p.BackupPath, p.OriginalSHA) != nil {
		t.Fatal("recovery disappeared on final drift")
	}
	if _, err := os.Stat(h.original); err != nil {
		t.Fatal("original removed before final digest was verified")
	}
}

func TestFilesystemPromotionRejectsExistingDestinationAndSymlinks(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprint(symlink), func(t *testing.T) {
			h := newPromotionHarness(t)
			if symlink {
				if err := os.Symlink(h.original, h.final); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(h.final, []byte("other movie"), 0600); err != nil {
				t.Fatal(err)
			}
			r := runLocalPromotion(t, h)
			if r.Status != StatusFailed {
				t.Fatalf("occupied target accepted: %+v", r)
			}
		})
	}
}

func TestFilesystemPromotionSameExtension(t *testing.T) {
	h := newPromotionHarness(t)
	old := h.original
	h.original = strings.TrimSuffix(old, filepath.Ext(old)) + ".mkv"
	if err := os.Rename(old, h.original); err != nil {
		t.Fatal(err)
	}
	inst, err := h.st.GetActionInstance("source-transcode")
	if err != nil {
		t.Fatal(err)
	}
	ec := parseExecutionContext(inst, h.engine)
	ec.State["resolved_path"] = h.original
	inst.StateJSON = toJSON(ec.State)
	if err := h.st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	r := runLocalPromotion(t, h)
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	r = h.resume(r.ID, "approve")
	if r.Status != StatusCompleted {
		t.Fatalf("same-extension replacement: %+v", r)
	}
}

func TestFilesystemPromotionRejectKeepsBothFiles(t *testing.T) {
	h := newPromotionHarness(t)
	r := runLocalPromotion(t, h)
	r = h.resume(r.ID, "reject")
	if r.Status != StatusFailed {
		t.Fatalf("rejection: %+v", r)
	}
	for _, path := range []string{h.original, h.candidate} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("rejection modified %s: %v", path, err)
		}
	}
}

func TestFilesystemPromotionSharesPhysicalClaimWithLibraryPromotion(t *testing.T) {
	h := newPromotionHarness(t)
	tmpl, _ := h.engine.GetTemplate("promote_transcode_candidate")
	tmpl.Steps[3].Run = func(context.Context, *ExecutionContext) (StepResult, error) {
		return StepResult{Status: StepFailed, Error: "stopped after preserve"}, nil
	}
	h.engine.RegisterTemplate(tmpl)
	arr := h.run()
	arr = h.resume(arr.ID, "approve")
	if arr.Status != StatusFailed || arr.CurrentStep != 3 {
		t.Fatalf("fixture did not retain an approved library reservation: %+v", arr)
	}
	local := runLocalPromotion(t, h)
	if local.Status != StatusWaitingDecision {
		t.Fatalf("local plan: %+v", local)
	}
	local = h.resume(local.ID, "approve")
	if local.Status != StatusFailed || !strings.Contains(local.Error, "reserved by another promotion") {
		t.Fatalf("filesystem promotion bypassed shared physical reservation: %+v", local)
	}
}

func TestPromotionAtomicPublicationNeverOverwritesForeignDestination(t *testing.T) {
	root := t.TempDir()
	from, to := filepath.Join(root, "candidate.mkv"), filepath.Join(root, "target.mkv")
	if err := os.WriteFile(from, []byte("candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renamePromotionNoReplace(from, to); err == nil {
		t.Fatal("non-overwriting publication replaced existing target")
	}
	if b, err := os.ReadFile(to); err != nil || string(b) != "foreign" {
		t.Fatalf("foreign destination modified: %s %v", b, err)
	}
	if b, err := os.ReadFile(from); err != nil || string(b) != "candidate" {
		t.Fatalf("candidate consumed on conflict: %s %v", b, err)
	}
}
