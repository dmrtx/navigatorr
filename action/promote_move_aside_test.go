package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func movedAsideState(t *testing.T, h *promotionHarness, id string) *promotionState {
	t.Helper()
	inst, err := h.st.GetActionInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPromotion(parseExecutionContext(inst, h.engine))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPromotionMovesOriginalAsideWithoutCopying(t *testing.T) {
	h := newPromotionHarness(t)
	moves := 0
	h.engine.promotionMoveAsideHook = func(from, to string) error {
		moves++
		// Every episode already uses the verified candidate when the
		// original leaves its library path.
		for _, ep := range h.episodes {
			if ep.EpisodeFileID != 202 {
				t.Errorf("original moved while episode %d still used it", ep.ID)
			}
		}
		if h.deletes != 0 {
			t.Error("original moved after DELETE")
		}
		return renamePromotionNoReplace(from, to)
	}
	r := h.run()
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted || moves != 1 || h.deletes != 1 {
		t.Fatalf("move-aside promotion: %s %s moves=%d deletes=%d", r.Status, r.Error, moves, h.deletes)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	if p.RecoveryMode != promotionRecoveryMoveAside || !p.MovedAside || p.RecoveryCopy != nil || p.BackupFingerprint == nil || !p.RecoveryCleanupCompleted {
		t.Fatalf("move-aside evidence: %+v", p)
	}
	for _, path := range []string{h.original, p.BackupPath, filepath.Dir(p.BackupPath)} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("left behind after success: %s", path)
		}
	}
	checks, _ := r.State[promotionIntegrityState].(map[string]any)
	if check, _ := checks[p.BackupPath].(map[string]any); check["kind"] != promotionCheckIdentity {
		t.Fatalf("recovery not proved by identity after the DELETE wait: %+v", check)
	}
}

// A crash leaves the move-aside intent durable on either side of the rename.
func TestPromotionMoveAsideResumesAfterCrashOnEitherSideOfRename(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_rename", true: "after_rename"}[renamed], func(t *testing.T) {
			h := newPromotionHarness(t)
			crashed := false
			h.engine.promotionMoveAsideHook = func(from, to string) error {
				if crashed {
					return renamePromotionNoReplace(from, to)
				}
				crashed = true
				if renamed {
					if err := renamePromotionNoReplace(from, to); err != nil {
						return err
					}
				}
				return errors.New("simulated crash")
			}
			r := h.run()
			r = h.finish(h.resume(r.ID, "approve"))
			if r.Status != StatusFailed || h.deletes != 0 {
				t.Fatalf("crash fixture: %s %s deletes=%d", r.Status, r.Error, h.deletes)
			}
			p := movedAsideState(t, h, r.ID)
			if p.MoveAsideStartedAt == "" || p.MovedAside {
				t.Fatalf("intent not durable before the rename: %+v", p)
			}
			if err := testFileSHA(map[bool]string{false: h.original, true: p.BackupPath}[renamed], p.OriginalSHA); err != nil {
				t.Fatalf("original lost across the crash: %v", err)
			}
			h.restart()
			reads := countReads(h.engine)
			r, err := h.engine.Retry(context.Background(), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if r = h.finish(r); r.Status != StatusCompleted || h.imports != 1 || h.deletes != 1 {
				t.Fatalf("resume after crash: %s %s imports=%d deletes=%d", r.Status, r.Error, h.imports, h.deletes)
			}
			// A restart is a wait: the original is read in full wherever it is.
			if got := reads.of(h.original) + reads.of(p.BackupPath); got < p.OriginalBytes {
				t.Fatalf("original read %d of %d bytes after the restart", got, p.OriginalBytes)
			}
		})
	}
}

func TestPromotionMoveAsideStopsInUnexpectedStates(t *testing.T) {
	for _, state := range []string{"both_present", "neither_present", "partial_present", "foreign_recovery", "changed_inode"} {
		t.Run(state, func(t *testing.T) {
			h := newPromotionHarness(t)
			h.engine.promotionMoveAsideHook = func(from, to string) error {
				switch state {
				case "both_present":
					// The rename "happened", yet the original is still there.
					if err := os.WriteFile(to, []byte("other"), 0600); err != nil {
						t.Fatal(err)
					}
				case "neither_present":
					if err := os.Rename(from, filepath.Join(h.root, "elsewhere.mp4")); err != nil {
						t.Fatal(err)
					}
				case "partial_present", "foreign_recovery":
					return renamePromotionNoReplace(from, to)
				case "changed_inode":
					// Same bytes, different file: not the verified original.
					b, err := os.ReadFile(from)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(to, b, 0600); err != nil {
						t.Fatal(err)
					}
					return os.Remove(from)
				}
				return errors.New("simulated crash")
			}
			if state == "partial_present" || state == "foreign_recovery" {
				// Crash before the rename, then something appears at the
				// recovery location before the retry.
				h.engine.promotionMoveAsideHook = func(string, string) error { return errors.New("simulated crash") }
			}
			r := h.run()
			r = h.finish(h.resume(r.ID, "approve"))
			p := movedAsideState(t, h, r.ID)
			if state == "partial_present" || state == "foreign_recovery" {
				target := p.BackupPath
				if state == "partial_present" {
					target += ".partial"
				}
				if err := os.WriteFile(target, []byte("unexpected"), 0600); err != nil {
					t.Fatal(err)
				}
				h.engine.promotionMoveAsideHook = nil
				var err error
				if r, err = h.engine.Retry(context.Background(), r.ID); err == nil {
					r = h.finish(r)
				}
			}
			if r.Status != StatusFailed || h.deletes != 0 || h.renames != 0 {
				t.Fatalf("unexpected state accepted: %s %s deletes=%d renames=%d", r.Status, r.Error, h.deletes, h.renames)
			}
			if p = movedAsideState(t, h, r.ID); p.MovedAside {
				t.Fatalf("unexpected state recorded as moved: %+v", p)
			}
			switch state {
			case "neither_present":
				if err := testFileSHA(filepath.Join(h.root, "elsewhere.mp4"), p.OriginalSHA); err != nil {
					t.Fatal(err)
				}
			case "changed_inode", "foreign_recovery":
				if err := testFileSHA(map[bool]string{true: p.BackupPath, false: h.original}[state == "changed_inode"], p.OriginalSHA); err != nil {
					t.Fatal(err)
				}
			default:
				if err := testFileSHA(h.original, p.OriginalSHA); err != nil {
					t.Fatalf("original disturbed: %v", err)
				}
			}
		})
	}
}

// A refused rename (another device, or no atomic no-replace rename) falls
// back to a verified recovery copy, made only then.
func TestPromotionMoveAsideFallsBackToCopy(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "library", true: "filesystem"}[local], func(t *testing.T) {
			h := newPromotionHarness(t)
			h.engine.promotionMoveAsideHook = func(from, to string) error {
				return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EXDEV}
			}
			var r *ActionResult
			if local {
				r = runLocalPromotion(t, h)
			} else {
				r = h.run()
			}
			r = h.finish(h.resume(r.ID, "approve"))
			if r.Status != StatusCompleted {
				t.Fatalf("copy fallback: %s %s", r.Status, r.Error)
			}
			p, err := loadPromotion(&ExecutionContext{State: r.State})
			if err != nil {
				t.Fatal(err)
			}
			if p.RecoveryMode != promotionRecoveryCopy || p.MovedAside || p.MoveAsideStartedAt != "" || p.RecoveryCopy == nil || !p.RecoveryCleanupCompleted {
				t.Fatalf("fallback evidence: %+v", p)
			}
			if !local && (h.deletes != 1 || !h.backupSeen) {
				t.Fatalf("DELETE without a verified copy: deletes=%d", h.deletes)
			}
			if _, err := os.Lstat(h.original); !os.IsNotExist(err) {
				t.Fatal("original remains after copy fallback")
			}
		})
	}
}

// When the candidate takes the original's exact name, the original moves
// aside first and the candidate is renamed into the empty name.
func TestFilesystemPromotionMovesAsideBeforeTakingTheSameName(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "move_aside", true: "copy_fallback"}[refused], func(t *testing.T) {
			h := newPromotionHarness(t)
			sameName := filepath.Join(filepath.Dir(h.original), "Series S01E01-E02.mkv")
			if err := os.Rename(h.original, sameName); err != nil {
				t.Fatal(err)
			}
			h.original = sameName
			inst, _ := h.st.GetActionInstance("source-transcode")
			inst.StateJSON = strings.ReplaceAll(inst.StateJSON, "Series S01E01-E02.mp4", "Series S01E01-E02.mkv")
			if err := h.st.UpdateActionInstance(*inst); err != nil {
				t.Fatal(err)
			}
			if refused {
				h.engine.promotionMoveAsideHook = func(from, to string) error {
					return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EXDEV}
				}
			}
			r := runLocalPromotion(t, h)
			r = h.resume(r.ID, "approve")
			if r.Status != StatusCompleted {
				t.Fatalf("same-name replacement: %s %s", r.Status, r.Error)
			}
			p, err := loadPromotion(&ExecutionContext{State: r.State})
			if err != nil || p.NewPath != p.OriginalPath {
				t.Fatalf("fixture is not a same-name replacement: %+v %v", p, err)
			}
			if p.MovedAside == refused {
				t.Fatalf("recovery mode: %+v", p)
			}
			if err := testFileSHA(h.original, p.CandidateSHA); err != nil {
				t.Fatalf("candidate not published at the original name: %v", err)
			}
			if _, err := os.Lstat(p.BackupPath); !os.IsNotExist(err) {
				t.Fatal("recovery retained after verified success")
			}
		})
	}
}

func TestPromotionMovedAsideBlocksRestoredOriginalReimport(t *testing.T) {
	h, id := restoredOriginalFixture(t, 4)
	inst, _ := h.st.GetActionInstance(id)
	ec := parseExecutionContext(inst, h.engine)
	p, _ := loadPromotion(ec)
	// The original was moved aside before the library lost the adoption.
	if err := os.Rename(p.OriginalPath, p.BackupPath); err != nil {
		t.Fatal(err)
	}
	fp, err := h.engine.promotionCurrentFingerprint(p.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	p.MovedAside, p.BackupVerified, p.BackupFingerprint, p.MoveAsideStartedAt = true, true, &fp, "2026-09-30T18:00:00Z"
	ec.State["promotion"] = p
	inst.StateJSON = toJSON(ec.State)
	if err := h.st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	r, err := h.engine.Retry(context.Background(), id)
	if err == nil && r.Status != StatusFailed {
		t.Fatalf("reimport allowed after move-aside: %+v", r)
	}
	if h.imports != 1 {
		t.Fatal("restored-original retry reimported a moved original")
	}
	if err := testFileSHA(p.BackupPath, p.OriginalSHA); err != nil {
		t.Fatalf("moved original lost: %v", err)
	}
}

// A recovery that changed while the promotion waited is read in full and
// stops it before the rename.
func TestPromotionRereadsChangedRecoveryAfterWait(t *testing.T) {
	for _, change := range []string{"touched", "rewritten"} {
		t.Run(change, func(t *testing.T) {
			h := newPromotionHarness(t)
			tmpl, _ := h.engine.GetTemplate("promote_transcode_candidate")
			step := actionStepIndex(tmpl, "remove_old_file")
			run := tmpl.Steps[step].Run
			tampered := false
			tmpl.Steps[step].Run = func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
				result, err := run(ctx, ec)
				p, _ := loadPromotion(ec)
				if err == nil && result.Status == StepWaitingExternal && p != nil && p.MovedAside && !tampered {
					tampered = true
					if change == "touched" {
						touched := time.Now().Add(-time.Hour)
						if err := os.Chtimes(p.BackupPath, touched, touched); err != nil {
							t.Fatal(err)
						}
					} else if err := os.WriteFile(p.BackupPath, []byte(strings.Repeat("x", int(p.OriginalBytes))), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return result, err
			}
			h.engine.RegisterTemplate(tmpl)
			reads := countReads(h.engine)
			r := h.run()
			r = h.finish(h.resume(r.ID, "approve"))
			p := movedAsideState(t, h, r.ID)
			if !tampered {
				t.Fatal("fixture never reached the DELETE wait")
			}
			if reads.of(p.BackupPath) < p.OriginalBytes {
				t.Fatalf("changed recovery not read in full: %d bytes", reads.of(p.BackupPath))
			}
			if change == "touched" {
				if r.Status != StatusCompleted {
					t.Fatalf("unchanged bytes rejected: %s %s", r.Status, r.Error)
				}
				return
			}
			if r.Status != StatusFailed || !strings.Contains(r.Error, "SHA-256") || h.renames != 0 {
				t.Fatalf("rewritten recovery accepted: %s %s renames=%d", r.Status, r.Error, h.renames)
			}
		})
	}
}
