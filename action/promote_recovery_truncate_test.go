//go:build linux || darwin

package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// emptyingHarness is a promotion past the library rename whose verified
// recovery copy is about to be removed, with cleanup.truncate_before_delete set.
func emptyingHarness(t *testing.T, truncate bool, mutate func(p *promotionState)) (*promotionHarness, string, *promotionState) {
	t.Helper()
	h := newPromotionHarness(t)
	h.engine.deps.Config.Cleanup.TruncateBeforeDelete = truncate
	id, p := h.seedPostRenameFinalize(t, mutate)
	h.engine.deps.Config.Cleanup.TruncateBeforeDelete = truncate
	return h, id, p
}

func persistedPromotion(t *testing.T, h *promotionHarness, id string) *promotionState {
	t.Helper()
	inst, err := h.st.GetActionInstance(id)
	if err != nil || inst == nil {
		t.Fatalf("instance: %v", err)
	}
	p, err := loadPromotion(parseExecutionContext(inst, h.engine))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFinalizeEmptiesTheRecoveryCopyBeforeUnlinkingIt(t *testing.T) {
	h, id, p := emptyingHarness(t, true, nil)
	held := holdOpen(t, p.BackupPath)
	r := h.resume(id, "")
	if r.Status != StatusCompleted {
		t.Fatalf("finalize: %s %s", r.Status, r.Error)
	}
	if got := sizeOf(t, held); got != 0 {
		t.Fatalf("the recovery copy kept %d bytes when it was unlinked", got)
	}
	if _, err := os.Lstat(p.BackupPath); !os.IsNotExist(err) {
		t.Fatal("recovery copy still present")
	}
	if r.Outputs["recovery_truncate"] != truncateEmptied {
		t.Fatalf("outcome not reported: %v", r.Outputs["recovery_truncate"])
	}
	saved := persistedPromotion(t, h, id)
	if saved.RecoveryTruncate != truncateEmptied || !saved.RecoveryCleanupCompleted {
		t.Fatalf("outcome not persisted: %+v", saved)
	}
	if err := h.engine.verifyPromotionHash(context.Background(), h.final, p.CandidateSHA); err != nil {
		t.Fatalf("the replacement was touched: %v", err)
	}
}

func TestFinalizeLeavesTheRecoveryCopyWholeWhenTheSettingIsOff(t *testing.T) {
	h, id, p := emptyingHarness(t, false, nil)
	held := holdOpen(t, p.BackupPath)
	r := h.resume(id, "")
	if r.Status != StatusCompleted {
		t.Fatalf("finalize: %s %s", r.Status, r.Error)
	}
	if got := sizeOf(t, held); got != p.OriginalBytes {
		t.Fatalf("setting off emptied the copy: %d of %d bytes", got, p.OriginalBytes)
	}
	if _, ok := r.Outputs["recovery_truncate"]; ok {
		t.Fatalf("setting off must report nothing: %v", r.Outputs["recovery_truncate"])
	}
	if saved := persistedPromotion(t, h, id); saved.RecoveryEmptyStarted || saved.RecoveryTruncate != "" {
		t.Fatalf("setting off left emptying state: %+v", saved)
	}
}

func TestFinalizeNeverEmptiesARecoveryCopyThatFailsVerification(t *testing.T) {
	cases := map[string]func(t *testing.T, h *promotionHarness, p *promotionState){
		"recovery bytes differ": func(t *testing.T, h *promotionHarness, p *promotionState) {
			bad := []byte(strings.Repeat("x", int(p.OriginalBytes)))
			if err := os.WriteFile(p.BackupPath, bad, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"replacement bytes differ": func(t *testing.T, h *promotionHarness, p *promotionState) {
			if err := os.WriteFile(h.final, []byte(strings.Repeat("y", int(p.CandidateBytes))), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"replacement is gone": func(t *testing.T, h *promotionHarness, p *promotionState) {
			if err := os.Remove(h.final); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			h, id, p := emptyingHarness(t, true, nil)
			damage(t, h, p)
			before, err := os.ReadFile(p.BackupPath)
			if err != nil {
				t.Fatal(err)
			}
			r := h.resume(id, "")
			if r.Status != StatusFailed {
				t.Fatalf("unverified replacement accepted: %s", r.Status)
			}
			after, err := os.ReadFile(p.BackupPath)
			if err != nil || string(after) != string(before) {
				t.Fatalf("the recovery copy was changed by a failed verification: %v", err)
			}
			if saved := persistedPromotion(t, h, id); saved.RecoveryEmptyStarted || saved.RecoveryTruncate != "" || saved.RecoveryCleanupCompleted {
				t.Fatalf("emptying was recorded before verification passed: %+v", saved)
			}
		})
	}
}

// Whatever recovery_mode says, a recovery copy another name still links is
// kept whole: it can be the original a torrent client seeds or the library file.
func TestFinalizeNeverEmptiesALinkedRecoveryCopyInAnyMode(t *testing.T) {
	modes := map[string]func(p *promotionState){
		"copy":       func(p *promotionState) { p.RecoveryMode = promotionRecoveryCopy },
		"legacy":     func(p *promotionState) { p.RecoveryMode = "" },
		"move_aside": func(p *promotionState) { p.RecoveryMode, p.MovedAside = promotionRecoveryMoveAside, true },
	}
	for name, setMode := range modes {
		t.Run(name, func(t *testing.T) {
			h, id, p := emptyingHarness(t, true, setMode)
			seeding := filepath.Join(h.root, "Downloads", "seeding-copy.mp4")
			if err := os.MkdirAll(filepath.Dir(seeding), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(p.BackupPath, seeding); err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(seeding)
			if err != nil {
				t.Fatal(err)
			}
			r := h.resume(id, "")
			if r.Status != StatusCompleted {
				t.Fatalf("finalize: %s %s", r.Status, r.Error)
			}
			got, err := os.ReadFile(seeding)
			if err != nil || string(got) != string(want) {
				t.Fatalf("the other name lost its bytes: %v", err)
			}
			if _, err := os.Lstat(p.BackupPath); !os.IsNotExist(err) {
				t.Fatal("recovery name still present")
			}
			if outcome, _ := r.Outputs["recovery_truncate"].(string); !strings.HasPrefix(outcome, truncateSkipped) {
				t.Fatalf("outcome: %q", outcome)
			}
		})
	}
}

func TestRemoveRecoveryFileNeverEmptiesTheLibraryFile(t *testing.T) {
	h, _, p := emptyingHarness(t, true, nil)
	// A recovery name that is the same inode as the new library file.
	if err := os.Remove(p.BackupPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(h.final, p.BackupPath); err != nil {
		t.Fatal(err)
	}
	truncate, err := h.engine.removeRecoveryFile(p.BackupPath)
	if err != nil || !strings.HasPrefix(truncate, truncateSkipped) {
		t.Fatalf("outcome %q, error %v", truncate, err)
	}
	if err := h.engine.verifyPromotionHash(context.Background(), h.final, p.CandidateSHA); err != nil {
		t.Fatalf("the library file was emptied: %v", err)
	}
}

// A stop after the copy is emptied and before it is unlinked must not leave a
// promotion that fails verifying an empty file.
func TestFinalizeResumesAfterTheRecoveryCopyWasEmptied(t *testing.T) {
	h, id, p := emptyingHarness(t, true, nil)
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop root")
	}
	keyDir := filepath.Dir(p.BackupPath)
	// The directory refuses the unlink, so the run stops right after emptying.
	if err := os.Chmod(keyDir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(keyDir, 0700) })
	r := h.resume(id, "")
	if r.Status != StatusFailed {
		t.Fatalf("the unlink should have failed: %s", r.Status)
	}
	info, err := os.Lstat(p.BackupPath)
	if err != nil || info.Size() != 0 {
		t.Fatalf("expected an emptied copy left behind: %v", err)
	}
	if saved := persistedPromotion(t, h, id); !saved.RecoveryEmptyStarted || !saved.RecoveryCleanupStarted || saved.RecoveryCleanupCompleted {
		t.Fatalf("interrupted state not persisted: %+v", saved)
	}
	if err := os.Chmod(keyDir, 0700); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.engine.deps.Config.Cleanup.TruncateBeforeDelete = true
	r, err = h.engine.Retry(context.Background(), id)
	if err != nil || r.Status != StatusCompleted {
		t.Fatalf("resume after emptying: %+v %v", r, err)
	}
	if _, err := os.Lstat(p.BackupPath); !os.IsNotExist(err) {
		t.Fatal("resumed run left the empty copy")
	}
	if r.Outputs["recovery_truncate"] != truncateEmptied {
		t.Fatalf("outcome after resume: %v", r.Outputs["recovery_truncate"])
	}
}

// Resume tolerates an empty copy only when this promotion recorded the intent
// to empty it; an empty file otherwise is a failed verification.
func TestFinalizeStillVerifiesAnEmptyRecoveryCopyWithoutTheRecordedIntent(t *testing.T) {
	h, id, p := emptyingHarness(t, true, func(p *promotionState) { p.RecoveryCleanupStarted = true })
	if err := os.Truncate(p.BackupPath, 0); err != nil {
		t.Fatal(err)
	}
	r := h.resume(id, "")
	if r.Status != StatusFailed {
		t.Fatalf("an unexplained empty recovery copy was accepted: %s", r.Status)
	}
}

func TestFinalizeFallsBackToThePlainUnlinkWhenEmptyingFails(t *testing.T) {
	h, id, p := emptyingHarness(t, true, nil)
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not stop root")
	}
	// Not writable, so it cannot be emptied; its directory still lets it go.
	if err := os.Chmod(p.BackupPath, 0400); err != nil {
		t.Fatal(err)
	}
	held := holdOpen(t, p.BackupPath)
	r := h.resume(id, "")
	if r.Status != StatusCompleted {
		t.Fatalf("a failed emptying blocked the promotion: %s %s", r.Status, r.Error)
	}
	if _, err := os.Lstat(p.BackupPath); !os.IsNotExist(err) {
		t.Fatal("recovery copy still present")
	}
	if got := sizeOf(t, held); got != p.OriginalBytes {
		t.Fatalf("unexpected size %d", got)
	}
	if outcome, _ := r.Outputs["recovery_truncate"].(string); !strings.HasPrefix(outcome, truncateSkipped) {
		t.Fatalf("outcome: %q", outcome)
	}
}

func TestFilesystemFinalizeEmptiesTheRecoveryCopyBeforeUnlinkingIt(t *testing.T) {
	for _, truncate := range []bool{true, false} {
		name := "setting on"
		if !truncate {
			name = "setting off"
		}
		t.Run(name, func(t *testing.T) {
			h := newPromotionHarness(t)
			h.engine.deps.Config.Cleanup.TruncateBeforeDelete = truncate
			tmpl, _ := h.engine.GetTemplate("promote_transcode_candidate")
			last := len(tmpl.Steps) - 1
			run := tmpl.Steps[last].Run
			var held *os.File
			var original int64
			tmpl.Steps[last].Run = func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
				// The recovery copy exists here and is removed by this step.
				if p, err := loadPromotion(ec); err == nil {
					if f, err := os.Open(p.BackupPath); err == nil {
						held = f
						original = p.OriginalBytes
					}
				}
				return run(ctx, ec)
			}
			h.engine.RegisterTemplate(tmpl)
			r := runLocalPromotion(t, h)
			r = h.resume(r.ID, "approve")
			if r.Status != StatusCompleted {
				t.Fatalf("local replacement: %s %s", r.Status, r.Error)
			}
			if held == nil {
				t.Fatal("the recovery copy never existed at finalize")
			}
			defer held.Close()
			want, outcome := original, ""
			if truncate {
				want, outcome = 0, truncateEmptied
			}
			if got := sizeOf(t, held); got != want {
				t.Fatalf("recovery copy kept %d bytes, want %d", got, want)
			}
			if got, _ := r.Outputs["recovery_truncate"].(string); got != outcome {
				t.Fatalf("outcome %q, want %q", got, outcome)
			}
			p, err := loadPromotion(&ExecutionContext{State: r.State})
			if err != nil || p.RecoveryTruncate != outcome {
				t.Fatalf("outcome not persisted: %+v %v", p, err)
			}
			if err := h.engine.verifyPromotionHash(context.Background(), h.final, p.CandidateSHA); err != nil {
				t.Fatalf("the replacement was touched: %v", err)
			}
		})
	}
}

func TestDiscardBackupsEmptyTheCopyBeforeUnlinkingIt(t *testing.T) {
	for _, mode := range []string{"discard", "discard_duplicate"} {
		t.Run(mode, func(t *testing.T) {
			h := newPromotionHarness(t)
			h.engine.deps.Config.Cleanup.TruncateBeforeDelete = true
			id, p := seedFailedBackup(t, h)
			h.engine.deps.Config.Cleanup.TruncateBeforeDelete = true
			data, err := os.ReadFile(p.BackupPath)
			if err != nil {
				t.Fatal(err)
			}
			partial := p.BackupPath + ".partial"
			if err := os.WriteFile(partial, data[:len(data)/2], 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "discard_duplicate" {
				// The duplicate guard wants the original still in the library.
				if err := os.WriteFile(p.OriginalPath, data, 0600); err != nil {
					t.Fatal(err)
				}
				p.OldRemoved = false
				p.Commands = map[string]*promotionCommand{"import": {Done: true}}
				inst, _ := h.st.GetActionInstance(id)
				inst.CurrentStep = 3
				inst.StateJSON = toJSON(map[string]any{"promotion": p})
				if err := h.st.UpdateActionInstance(*inst); err != nil {
					t.Fatal(err)
				}
			}
			backup, part := holdOpen(t, p.BackupPath), holdOpen(t, partial)
			if mode == "discard" {
				_, err = h.engine.DiscardTranscodeBackup(context.Background(), id)
			} else {
				_, err = h.engine.DiscardDuplicateTranscodeBackup(context.Background(), id)
			}
			if err != nil {
				t.Fatal(err)
			}
			if sizeOf(t, backup) != 0 || sizeOf(t, part) != 0 {
				t.Fatal("a discarded recovery file kept its bytes")
			}
			for _, path := range []string{p.BackupPath, partial} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("still present: %s", path)
				}
			}
			if saved := persistedPromotion(t, h, id); saved.RecoveryTruncate != truncateEmptied {
				t.Fatalf("outcome not persisted: %q", saved.RecoveryTruncate)
			}
			if mode == "discard_duplicate" {
				if got, err := os.ReadFile(p.OriginalPath); err != nil || string(got) != string(data) {
					t.Fatalf("the original was touched: %v", err)
				}
			}
		})
	}
}

func TestDiscardDuplicateKeepsAnUnverifiedCopyWhole(t *testing.T) {
	h := newPromotionHarness(t)
	h.engine.deps.Config.Cleanup.TruncateBeforeDelete = true
	id, p := seedFailedBackup(t, h)
	data, _ := os.ReadFile(p.BackupPath)
	if err := os.WriteFile(p.OriginalPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	data[0] ^= 1
	if err := os.WriteFile(p.BackupPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	p.OldRemoved = false
	p.Commands = map[string]*promotionCommand{"import": {Done: true}}
	inst, _ := h.st.GetActionInstance(id)
	inst.CurrentStep = 3
	inst.StateJSON = toJSON(map[string]any{"promotion": p})
	if err := h.st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	h.engine.deps.Config.Cleanup.TruncateBeforeDelete = true
	if _, err := h.engine.DiscardDuplicateTranscodeBackup(context.Background(), id); err == nil {
		t.Fatal("a copy that differs from the original was discarded")
	}
	got, err := os.ReadFile(p.BackupPath)
	if err != nil || string(got) != string(data) {
		t.Fatalf("the copy was changed: %v", err)
	}
}

func TestDiscardDuplicateFinishesAfterTheCopyWasEmptied(t *testing.T) {
	h := newPromotionHarness(t)
	id, p := seedFailedBackup(t, h)
	data, _ := os.ReadFile(p.BackupPath)
	if err := os.WriteFile(p.OriginalPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	// An earlier run recorded its intent, emptied the verified copy and stopped.
	if err := os.Truncate(p.BackupPath, 0); err != nil {
		t.Fatal(err)
	}
	p.OldRemoved, p.RecoveryEmptyStarted = false, true
	p.Commands = map[string]*promotionCommand{"import": {Done: true}}
	inst, _ := h.st.GetActionInstance(id)
	inst.CurrentStep = 3
	inst.StateJSON = toJSON(map[string]any{"promotion": p})
	if err := h.st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	h.engine.deps.Config.Cleanup.TruncateBeforeDelete = true
	if _, err := h.engine.DiscardDuplicateTranscodeBackup(context.Background(), id); err != nil {
		t.Fatalf("resume after emptying: %v", err)
	}
	if _, err := os.Lstat(p.BackupPath); !os.IsNotExist(err) {
		t.Fatal("the empty copy was left behind")
	}
	if got, err := os.ReadFile(p.OriginalPath); err != nil || string(got) != string(data) {
		t.Fatalf("the original was touched: %v", err)
	}
}
