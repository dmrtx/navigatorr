//go:build linux || darwin

package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/config"
)

// copyModeFilesystemPromotion plans a filesystem promotion that keeps the
// original in place until the replacement is verified and a recovery copy
// exists (recovery_mode copy), with cleanup.truncate_before_delete set. The
// original is held open, so its size is readable after its name is unlinked.
func copyModeFilesystemPromotion(t *testing.T, truncate bool) (*promotionHarness, *ActionResult, *os.File) {
	t.Helper()
	h := newPromotionHarness(t)
	h.engine.promotionAlwaysCopy = true
	h.engine.deps.Config.Cleanup.TruncateBeforeDelete = truncate
	r := runLocalPromotion(t, h)
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	return h, r, holdOpen(t, h.original)
}

func TestFilesystemPromotionEmptiesTheOriginalBeforeUnlinkingIt(t *testing.T) {
	h, r, held := copyModeFilesystemPromotion(t, true)
	want := sizeOf(t, held)
	r = h.resume(r.ID, "approve")
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	if _, err := os.Lstat(h.original); !os.IsNotExist(err) {
		t.Fatal("original still present")
	}
	if got := sizeOf(t, held); got != 0 {
		t.Fatalf("the original kept %d of %d bytes when it was unlinked", got, want)
	}
	if r.Outputs["original_truncate"] != truncateEmptied {
		t.Fatalf("outcome not reported: %v", r.Outputs["original_truncate"])
	}
	saved, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil || saved.OriginalTruncate != truncateEmptied || !saved.OriginalEmptyStarted || saved.RecoveryMode != promotionRecoveryCopy {
		t.Fatalf("outcome not persisted: %+v %v", saved, err)
	}
	if err := h.engine.verifyPromotionHash(context.Background(), h.final, saved.CandidateSHA); err != nil {
		t.Fatalf("the replacement was touched: %v", err)
	}
}

func TestFilesystemPromotionKeepsTheOriginalWholeWhenTheSettingIsOff(t *testing.T) {
	h, r, held := copyModeFilesystemPromotion(t, false)
	want := sizeOf(t, held)
	r = h.resume(r.ID, "approve")
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	if got := sizeOf(t, held); got != want {
		t.Fatalf("setting off emptied the original: %d of %d bytes", got, want)
	}
	if _, ok := r.Outputs["original_truncate"]; ok {
		t.Fatalf("setting off must report nothing: %v", r.Outputs["original_truncate"])
	}
	saved, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil || saved.OriginalEmptyStarted || saved.OriginalTruncate != "" {
		t.Fatalf("setting off left emptying state: %+v %v", saved, err)
	}
}

// move_aside renames the original into the recovery directory, so nothing
// unlinks it in place here; the recovery cleanup of the previous change
// empties it.
func TestFilesystemPromotionMoveAsideNeverTouchesTheOriginalHere(t *testing.T) {
	h := newPromotionHarness(t)
	h.engine.deps.Config.Cleanup.TruncateBeforeDelete = true
	r := runLocalPromotion(t, h)
	r = h.resume(r.ID, "approve")
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	saved, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil || !saved.movesAside() {
		t.Skipf("the harness filesystem did not choose move_aside: %+v %v", saved, err)
	}
	if saved.OriginalEmptyStarted || saved.OriginalTruncate != "" {
		t.Fatalf("a moved-aside original was handled as a deletion: %+v", saved)
	}
	if _, ok := r.Outputs["original_truncate"]; ok {
		t.Fatalf("move_aside must report no original outcome: %v", r.Outputs["original_truncate"])
	}
}

// The original is emptied only when the replacement and the recovery copy are
// verified; any step that fails them leaves its bytes alone.
func TestFilesystemPromotionNeverEmptiesTheOriginalWhenTheReplacementOrRecoveryIsNotVerified(t *testing.T) {
	cases := map[string]func(t *testing.T, h *promotionHarness, p *promotionState){
		"recovery bytes differ": func(t *testing.T, h *promotionHarness, p *promotionState) {
			bad := []byte(strings.Repeat("x", int(p.OriginalBytes)))
			if err := os.WriteFile(p.BackupPath, bad, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"recovery is gone": func(t *testing.T, h *promotionHarness, p *promotionState) {
			if err := os.Remove(p.BackupPath); err != nil {
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
			h, r, held := copyModeFilesystemPromotion(t, true)
			want := sizeOf(t, held)
			tmpl, _ := h.engine.GetTemplate("promote_transcode_candidate")
			// Step 3 publishes the replacement; the damage lands right after it.
			run := tmpl.Steps[3].Run
			tmpl.Steps[3].Run = func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
				result, err := run(ctx, ec)
				if err != nil || result.Status != StepCompleted {
					return result, err
				}
				p, err := loadPromotion(ec)
				if err != nil {
					return StepResult{}, err
				}
				damage(t, h, p)
				return result, nil
			}
			h.engine.RegisterTemplate(tmpl)
			r = h.resume(r.ID, "approve")
			if r.Status != StatusFailed {
				t.Fatalf("an unverified replacement or recovery was accepted: %s", r.Status)
			}
			if _, err := os.Lstat(h.original); err != nil {
				t.Fatalf("the original was unlinked: %v", err)
			}
			if got := sizeOf(t, held); got != want {
				t.Fatalf("the original was emptied: %d of %d bytes", got, want)
			}
			if saved := persistedPromotion(t, h, r.ID); saved.OriginalEmptyStarted || saved.OriginalTruncate != "" || saved.OldRemoved {
				t.Fatalf("emptying was recorded before verification passed: %+v", saved)
			}
		})
	}
}

// Another name for the original (a torrent client's seeding link) keeps it
// whole: the name goes, the bytes stay.
func TestFilesystemPromotionNeverEmptiesALinkedOriginal(t *testing.T) {
	h, r, held := copyModeFilesystemPromotion(t, true)
	want := sizeOf(t, held)
	seeding := filepath.Join(h.root, "Downloads", "seeding-copy.mp4")
	if err := os.MkdirAll(filepath.Dir(seeding), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(h.original, seeding); err != nil {
		t.Fatal(err)
	}
	r = h.resume(r.ID, "approve")
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	if _, err := os.Lstat(h.original); !os.IsNotExist(err) {
		t.Fatal("original name still present")
	}
	if got := sizeOf(t, held); got != want {
		t.Fatalf("a linked original was emptied: %d of %d bytes", got, want)
	}
	if outcome, _ := r.Outputs["original_truncate"].(string); !strings.HasPrefix(outcome, truncateSkipped) {
		t.Fatalf("outcome: %q", outcome)
	}
}

// A replacement or recovery copy that is the original's own inode proves
// nothing about its bytes, so the original is never emptied for it.
func TestOriginalEmptyBlockerRefusesSharedInodes(t *testing.T) {
	for _, other := range []string{"recovery", "replacement"} {
		t.Run(other, func(t *testing.T) {
			e, p, replacement, original := originalEmptyingFixture(t)
			target := p.BackupPath
			if other == "replacement" {
				target = replacement
			}
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(original, target); err != nil {
				t.Fatal(err)
			}
			p.CandidateBytes = p.OriginalBytes
			info, err := os.Lstat(original)
			if err != nil {
				t.Fatal(err)
			}
			if why := p.originalEmptyBlocker(info, replacement); !strings.Contains(why, "is the original file") {
				t.Fatalf("blocker: %q", why)
			}
			held := holdOpen(t, original)
			empty, skip := e.planOriginalEmptying(p, info, replacement)
			if empty || !strings.HasPrefix(skip, truncateSkipped) || p.OriginalEmptyStarted {
				t.Fatalf("plan: %v %q %+v", empty, skip, p)
			}
			truncate, err := e.removeOriginal(p, original, replacement, info, empty, skip)
			if err != nil || truncate != skip {
				t.Fatalf("removal: %q %v", truncate, err)
			}
			if got := sizeOf(t, held); got != p.OriginalBytes {
				t.Fatalf("the shared file was emptied: %d bytes", got)
			}
			if _, err := os.Stat(target); err != nil {
				t.Fatalf("the other name lost its file: %v", err)
			}
		})
	}
}

// originalEmptyingFixture is an original of 10 bytes, a recovery copy of the
// same size and a replacement of 5, with the recorded evidence a copy-mode
// promotion has when it removes the original.
func originalEmptyingFixture(t *testing.T) (*Engine, *promotionState, string, string) {
	t.Helper()
	dir := t.TempDir()
	original := filepath.Join(dir, "sample-episode.mp4")
	backup := filepath.Join(dir, "recovery", "sample-episode.mp4")
	replacement := filepath.Join(dir, "sample-episode.mkv")
	for path, size := range map[string]int{original: 10, backup: 10, replacement: 5} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Repeat("b", size)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	e := NewEngine(EngineDeps{})
	e.deps.Config = &config.Config{Cleanup: config.CleanupConfig{TruncateBeforeDelete: true}}
	return e, &promotionState{
		RecoveryMode:                      promotionRecoveryCopy,
		BackupPath:                        backup,
		BackupVerified:                    true,
		RecoveryVerifiedBeforeReplacement: true,
		CandidateSHA:                      "candidate-digest",
		CandidateMediaVerified:            true,
		OriginalBytes:                     10,
		CandidateBytes:                    5,
	}, replacement, original
}

// Every mode and state that does not prove the original's bytes safe elsewhere
// keeps the plain unlink, and the original is left whole until then.
func TestOriginalIsKeptWholeUnlessItsBytesAreProvenSafeElsewhere(t *testing.T) {
	cases := map[string]func(p *promotionState, replacement, original string){
		"move_aside mode":         func(p *promotionState, _, _ string) { p.RecoveryMode = promotionRecoveryMoveAside },
		"legacy mode":             func(p *promotionState, _, _ string) { p.RecoveryMode = "" },
		"unknown mode":            func(p *promotionState, _, _ string) { p.RecoveryMode = "hardlink" },
		"original proved by stat": func(p *promotionState, _, _ string) { p.OriginalStatMod = "2026-01-01T00:00:00Z" },
		"recovery not verified":   func(p *promotionState, _, _ string) { p.BackupVerified = false },
		"recovery not verified before replacement": func(p *promotionState, _, _ string) {
			p.RecoveryVerifiedBeforeReplacement = false
		},
		"no recovery path":          func(p *promotionState, _, _ string) { p.BackupPath = "" },
		"replacement digest absent": func(p *promotionState, _, _ string) { p.CandidateSHA = "" },
		"replacement not inspected": func(p *promotionState, _, _ string) { p.CandidateMediaVerified = false },
		"no replacement path":       func(p *promotionState, _, _ string) {},
		"recovery missing": func(p *promotionState, _, _ string) {
			os.Remove(p.BackupPath)
		},
		"recovery size differs": func(p *promotionState, _, _ string) {
			os.WriteFile(p.BackupPath, []byte("short"), 0600)
		},
		"replacement missing": func(_ *promotionState, replacement, _ string) { os.Remove(replacement) },
		"replacement size differs": func(_ *promotionState, replacement, _ string) {
			os.WriteFile(replacement, []byte("much longer than recorded"), 0600)
		},
		"original size differs": func(p *promotionState, _, _ string) { p.OriginalBytes = 11 },
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			e, p, replacement, original := originalEmptyingFixture(t)
			info, err := os.Lstat(original)
			if err != nil {
				t.Fatal(err)
			}
			held := holdOpen(t, original)
			spoil(p, replacement, original)
			path := replacement
			if name == "no replacement path" {
				path = ""
			}
			empty, skip := e.planOriginalEmptying(p, info, path)
			if empty || !strings.HasPrefix(skip, truncateSkipped) || p.OriginalEmptyStarted {
				t.Fatalf("plan allowed emptying: %v %q %+v", empty, skip, p)
			}
			truncate, err := e.removeOriginal(p, original, path, info, empty, skip)
			if err != nil || truncate != skip {
				t.Fatalf("removal: %q %v", truncate, err)
			}
			if _, err := os.Lstat(original); !os.IsNotExist(err) {
				t.Fatal("original still present")
			}
			if got := sizeOf(t, held); got != 10 {
				t.Fatalf("the original was emptied: %d bytes", got)
			}
		})
	}
	t.Run("proven", func(t *testing.T) {
		e, p, replacement, original := originalEmptyingFixture(t)
		info, _ := os.Lstat(original)
		if why := p.originalEmptyBlocker(info, replacement); why != "" {
			t.Fatalf("a verified copy-mode original was refused: %s", why)
		}
		held := holdOpen(t, original)
		empty, skip := e.planOriginalEmptying(p, info, replacement)
		if !empty || skip != "" || !p.OriginalEmptyStarted {
			t.Fatalf("plan: %v %q %+v", empty, skip, p)
		}
		truncate, err := e.removeOriginal(p, original, replacement, info, empty, skip)
		if err != nil || truncate != truncateEmptied || sizeOf(t, held) != 0 {
			t.Fatalf("removal: %q %v size %d", truncate, err, sizeOf(t, held))
		}
	})
}

// The checks are made again when the file is opened: a recovery copy that
// vanished since the plan keeps the original whole.
func TestRemoveOriginalRechecksTheEvidenceBeforeEmptying(t *testing.T) {
	e, p, replacement, original := originalEmptyingFixture(t)
	info, _ := os.Lstat(original)
	empty, skip := e.planOriginalEmptying(p, info, replacement)
	if !empty {
		t.Fatalf("plan refused: %q", skip)
	}
	if err := os.Remove(p.BackupPath); err != nil {
		t.Fatal(err)
	}
	held := holdOpen(t, original)
	truncate, err := e.removeOriginal(p, original, replacement, info, empty, skip)
	if err != nil || !strings.HasPrefix(truncate, truncateSkipped) || sizeOf(t, held) != 10 {
		t.Fatalf("removal: %q %v size %d", truncate, err, sizeOf(t, held))
	}
}

// A file swapped in at the original's path after it was checked is removed
// as before, never emptied.
func TestRemoveOriginalNeverEmptiesAFileThatChangedSinceItWasChecked(t *testing.T) {
	e, p, replacement, original := originalEmptyingFixture(t)
	info, _ := os.Lstat(original)
	empty, skip := e.planOriginalEmptying(p, info, replacement)
	if !empty {
		t.Fatalf("plan refused: %q", skip)
	}
	swapped := original + ".new"
	if err := os.WriteFile(swapped, []byte(strings.Repeat("n", 10)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(swapped, original); err != nil {
		t.Fatal(err)
	}
	held := holdOpen(t, original)
	truncate, err := e.removeOriginal(p, original, replacement, info, empty, skip)
	if err != nil || !strings.HasPrefix(truncate, truncateSkipped) || sizeOf(t, held) != 10 {
		t.Fatalf("removal: %q %v size %d", truncate, err, sizeOf(t, held))
	}
}

func TestFilesystemPromotionFallsBackToThePlainUnlinkWhenEmptyingTheOriginalFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not stop root")
	}
	h, r, held := copyModeFilesystemPromotion(t, true)
	want := sizeOf(t, held)
	// Not writable, so it cannot be emptied; its directory still lets it go.
	if err := os.Chmod(h.original, 0400); err != nil {
		t.Fatal(err)
	}
	r = h.resume(r.ID, "approve")
	if r.Status != StatusCompleted {
		t.Fatalf("a failed emptying blocked the promotion: %s %s", r.Status, r.Error)
	}
	if _, err := os.Lstat(h.original); !os.IsNotExist(err) {
		t.Fatal("original still present")
	}
	if got := sizeOf(t, held); got != want {
		t.Fatalf("unexpected size %d of %d", got, want)
	}
	if outcome, _ := r.Outputs["original_truncate"].(string); !strings.HasPrefix(outcome, truncateSkipped) {
		t.Fatalf("outcome: %q", outcome)
	}
}

// A stop after the original is emptied and before it is unlinked must not
// leave a promotion that fails verifying an empty file.
func TestFilesystemPromotionResumesAfterTheOriginalWasEmptied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop root")
	}
	h, r, _ := copyModeFilesystemPromotion(t, true)
	dir := filepath.Dir(h.original)
	tmpl, _ := h.engine.GetTemplate("promote_transcode_candidate")
	// Step 4 removes the original. The directory refuses the unlink, so the
	// run stops right after emptying.
	run := tmpl.Steps[4].Run
	tmpl.Steps[4].Run = func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
		if err := os.Chmod(dir, 0500); err != nil {
			return StepResult{}, err
		}
		defer os.Chmod(dir, 0700)
		return run(ctx, ec)
	}
	h.engine.RegisterTemplate(tmpl)
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	r = h.resume(r.ID, "approve")
	if r.Status != StatusFailed {
		t.Fatalf("the unlink should have failed: %s", r.Status)
	}
	info, err := os.Lstat(h.original)
	if err != nil || info.Size() != 0 {
		t.Fatalf("expected an emptied original left behind: %v", err)
	}
	if saved := persistedPromotion(t, h, r.ID); !saved.OriginalEmptyStarted || saved.OldRemoved {
		t.Fatalf("interrupted state not persisted: %+v", saved)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.engine.deps.Config.Cleanup.TruncateBeforeDelete = true
	r, err = h.engine.Retry(context.Background(), r.ID)
	if err != nil || r.Status != StatusCompleted {
		t.Fatalf("resume after emptying: %+v %v", r, err)
	}
	if _, err := os.Lstat(h.original); !os.IsNotExist(err) {
		t.Fatal("resumed run left the empty original")
	}
	if r.Outputs["original_truncate"] != truncateEmptied {
		t.Fatalf("outcome after resume: %v", r.Outputs["original_truncate"])
	}
	if saved, err := loadPromotion(&ExecutionContext{State: r.State}); err != nil || saved.OriginalTruncate != truncateEmptied {
		t.Fatalf("outcome not persisted: %+v %v", saved, err)
	}
}

// Resume tolerates an empty original only when this promotion recorded the
// intent to empty it; a short file otherwise is a failed verification.
func TestFilesystemPromotionStillVerifiesAnEmptyOriginalWithoutTheRecordedIntent(t *testing.T) {
	h, r, _ := copyModeFilesystemPromotion(t, true)
	tmpl, _ := h.engine.GetTemplate("promote_transcode_candidate")
	run := tmpl.Steps[4].Run
	tmpl.Steps[4].Run = func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
		if err := os.Truncate(h.original, 0); err != nil {
			return StepResult{}, err
		}
		return run(ctx, ec)
	}
	h.engine.RegisterTemplate(tmpl)
	r = h.resume(r.ID, "approve")
	if r.Status != StatusFailed {
		t.Fatalf("an unexplained empty original was accepted: %s", r.Status)
	}
	if _, err := os.Lstat(h.original); err != nil {
		t.Fatalf("the unexplained file was removed: %v", err)
	}
}

// The Sonarr/Radarr flow removes an original the library left behind as an
// orphan; it is emptied there the same way.
func TestLibraryPromotionEmptiesAnOrphanedOriginalBeforeUnlinkingIt(t *testing.T) {
	for _, truncate := range []bool{true, false} {
		name := "setting on"
		if !truncate {
			name = "setting off"
		}
		t.Run(name, func(t *testing.T) {
			h, id, held, want := orphanedOriginal(t, truncate)
			r := h.finish(h.resume(id, ""))
			if r.Status != StatusCompleted {
				t.Fatalf("promotion: %s %s", r.Status, r.Error)
			}
			if _, err := os.Lstat(h.original); !os.IsNotExist(err) {
				t.Fatal("original still present")
			}
			saved, err := loadPromotion(&ExecutionContext{State: r.State})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.engine.verifyPromotionHash(context.Background(), h.final, saved.CandidateSHA); err != nil {
				t.Fatalf("the replacement was touched: %v", err)
			}
			gotSize, outcome := want, ""
			if truncate {
				gotSize, outcome = 0, truncateEmptied
			}
			if got := sizeOf(t, held); got != gotSize {
				t.Fatalf("the original kept %d bytes, want %d", got, gotSize)
			}
			if got, _ := r.Outputs["original_truncate"].(string); got != outcome || saved.OriginalTruncate != outcome {
				t.Fatalf("outcome %q / %q, want %q", got, saved.OriginalTruncate, outcome)
			}
			if truncate != saved.OriginalEmptyStarted {
				t.Fatalf("intent %v with the setting %v", saved.OriginalEmptyStarted, truncate)
			}
		})
	}
}

// A stop after the orphan is emptied must not make the promotion fail on the
// empty file when it resumes.
func TestLibraryPromotionResumesAfterTheOrphanWasEmptied(t *testing.T) {
	h, id, held, _ := orphanedOriginal(t, true)
	inst, err := h.st.GetActionInstance(id)
	if err != nil || inst == nil {
		t.Fatalf("instance: %v", err)
	}
	ec := parseExecutionContext(inst, h.engine)
	p, err := loadPromotion(ec)
	if err != nil {
		t.Fatal(err)
	}
	// The state an earlier run left: intent recorded, original emptied.
	p.OriginalEmptyStarted = true
	if err := h.engine.savePromotion(context.Background(), ec, p); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(h.original, 0); err != nil {
		t.Fatal(err)
	}
	if sizeOf(t, held) != 0 {
		t.Fatal("fixture did not empty the original")
	}
	r := h.finish(h.resume(id, ""))
	if r.Status != StatusCompleted {
		t.Fatalf("resume after emptying: %s %s", r.Status, r.Error)
	}
	if _, err := os.Lstat(h.original); !os.IsNotExist(err) {
		t.Fatal("resumed run left the empty original")
	}
	if r.Outputs["original_truncate"] != truncateEmptied {
		t.Fatalf("outcome after resume: %v", r.Outputs["original_truncate"])
	}
}

func TestLibraryPromotionStillVerifiesAnEmptyOrphanWithoutTheRecordedIntent(t *testing.T) {
	h, id, _, _ := orphanedOriginal(t, true)
	if err := os.Truncate(h.original, 0); err != nil {
		t.Fatal(err)
	}
	r := h.finish(h.resume(id, ""))
	if r.Status != StatusFailed {
		t.Fatalf("an unexplained empty original was accepted: %s", r.Status)
	}
	if _, err := os.Lstat(h.original); err != nil {
		t.Fatalf("the unexplained file was removed: %v", err)
	}
}

// orphanedOriginal suspends a copy-mode Sonarr promotion right after the
// import, with the library having dropped the old record but left the file.
func orphanedOriginal(t *testing.T, truncate bool) (*promotionHarness, string, *os.File, int64) {
	t.Helper()
	h := newPromotionHarness(t)
	h.engine.promotionAlwaysCopy = true
	h.engine.deps.Config.Cleanup.TruncateBeforeDelete = truncate
	h.dropImportResponse = true // Suspend after adoption, before old-file cleanup.
	r := h.resume(h.run().ID, "approve")
	h.mu.Lock()
	imports := h.imports
	delete(h.files, 101)
	h.mu.Unlock()
	if r.Status != StatusWaitingExternal || imports != 1 {
		t.Fatalf("import fixture: %s %s", r.Status, r.Error)
	}
	held := holdOpen(t, h.original)
	return h, r.ID, held, sizeOf(t, held)
}
