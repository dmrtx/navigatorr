package action

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// strandedOriginal leaves a failed same-name promotion with its original at
// the recovery path and nothing at the library path, as a crash between the
// move aside and the candidate taking the name does.
func strandedOriginal(t *testing.T, h *promotionHarness) (string, *promotionState) {
	t.Helper()
	sameNameFixture(t, h)
	stash := h.candidate + ".stash"
	h.engine.promotionMoveAsideHook = func(from, to string) error {
		if err := renamePromotionNoReplace(from, to); err != nil {
			return err
		}
		return os.Rename(h.candidate, stash)
	}
	r := runLocalPromotion(t, h)
	if r = h.resume(r.ID, "approve"); r.Status != StatusFailed {
		t.Fatalf("failed publish: %s %s", r.Status, r.Error)
	}
	p := movedAsideState(t, h, r.ID)
	if err := os.Rename(h.original, p.BackupPath); err != nil {
		t.Fatal(err)
	}
	rewritePromotion(t, h, r.ID, func(q *promotionState) {
		q.MovedAside, q.BackupVerified, q.BackupFingerprint = true, true, q.OriginalFingerprint
		q.MoveAsideStartedAt = "2026-01-01T00:00:00Z"
		q.MoveBackStartedAt = ""
	})
	return r.ID, movedAsideState(t, h, r.ID)
}

// Cancelling between a crash and the automatic resume leaves the original at
// its recovery path with an empty library path; discard is refused, and the
// explicit move back puts it home without overwriting anything.
func TestRestoreMovesACancelledPromotionsOriginalBack(t *testing.T) {
	h := newPromotionHarness(t)
	id, p := strandedOriginal(t, h)
	if _, err := h.engine.Cancel(context.Background(), id, "stop"); err != nil {
		t.Fatal(err)
	}
	page, err := h.engine.ListTranscodeBackups(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || !page.Items[0].RestoreAvailable || page.Items[0].DeleteAvailable || !strings.Contains(page.Items[0].Reason, "moved back") {
		t.Fatalf("inventory: %+v", page.Items)
	}
	if _, err := h.engine.DiscardTranscodeBackup(context.Background(), id); err == nil {
		t.Fatal("discard accepted for the only copy")
	}

	// A file at the library path is never replaced.
	if err := os.WriteFile(h.original, []byte("stray"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.RestoreTranscodeBackup(context.Background(), id); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("restore over a stray file: %v", err)
	}
	if got, _ := os.ReadFile(h.original); string(got) != "stray" {
		t.Fatalf("stray file replaced: %q", got)
	}
	if err := testFileSHA(p.BackupPath, p.OriginalSHA); err != nil {
		t.Fatalf("recovery changed: %v", err)
	}
	if err := os.Remove(h.original); err != nil {
		t.Fatal(err)
	}

	// A recovery whose bytes are not the recorded original is not moved.
	good, err := os.ReadFile(p.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), good...)
	bad[0] ^= 0xff
	if err := os.WriteFile(p.BackupPath, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.RestoreTranscodeBackup(context.Background(), id); err == nil {
		t.Fatal("restore moved a recovery that is not the original")
	}
	if _, err := os.Lstat(h.original); !os.IsNotExist(err) {
		t.Fatalf("library path not left empty: %v", err)
	}
	if err := os.WriteFile(p.BackupPath, good, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p.BackupPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	res, err := h.engine.RestoreTranscodeBackup(context.Background(), id)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Status != StatusCancelled {
		t.Fatalf("restore changed the job status: %s", res.Status)
	}
	if err := testFileSHA(h.original, p.OriginalSHA); err != nil {
		t.Fatalf("original not back at its library path: %v", err)
	}
	if _, err := os.Lstat(p.BackupPath); !os.IsNotExist(err) {
		t.Fatalf("recovery path still occupied: %v", err)
	}
	q := movedAsideState(t, h, id)
	if q.MovedAside || q.MoveAsideStartedAt != "" || q.MoveBackStartedAt != "" || q.BackupVerified {
		t.Fatalf("state after restore: %+v", q)
	}
	if _, err := h.engine.RestoreTranscodeBackup(context.Background(), id); err == nil {
		t.Fatal("a second restore had something to move")
	}
}

// Only a finished same-name move-aside promotion qualifies.
func TestRestoreRefusesWhatIsNotAStrandedOriginal(t *testing.T) {
	h := newPromotionHarness(t)
	id, _ := strandedOriginal(t, h)
	set := func(status string) {
		t.Helper()
		inst, err := h.st.GetActionInstance(id)
		if err != nil {
			t.Fatal(err)
		}
		inst.Status = status
		if err := h.st.UpdateActionInstance(*inst); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{StatusRunning, StatusCompleted, StatusWaitingDecision} {
		set(status)
		if _, err := h.engine.RestoreTranscodeBackup(context.Background(), id); err == nil {
			t.Fatalf("restore accepted for a %s job", status)
		}
	}
	set(StatusCancelled)
	rewritePromotion(t, h, id, func(q *promotionState) { q.RecoveryMode = promotionRecoveryCopy })
	if _, err := h.engine.RestoreTranscodeBackup(context.Background(), id); err == nil {
		t.Fatal("restore accepted for a copy-mode promotion")
	}
	rewritePromotion(t, h, id, func(q *promotionState) { q.RecoveryMode = promotionRecoveryMoveAside })
	// A library-managed (Sonarr/Radarr) promotion never qualifies, whatever else
	// its state says: the library may rename onto the path and import meanwhile.
	managed := *movedAsideState(t, h, id)
	if err := h.engine.promotionRestorableOriginal(&managed); err != nil {
		t.Fatalf("fixture should qualify: %v", err)
	}
	managed.Service = "sonarr"
	if err := h.engine.promotionRestorableOriginal(&managed); err == nil || !strings.Contains(err.Error(), "filesystem") {
		t.Fatalf("restore guard accepted a library-managed promotion: %v", err)
	}
	rewritePromotion(t, h, id, func(q *promotionState) { q.NewPath = q.OriginalPath + ".other" })
	if _, err := h.engine.RestoreTranscodeBackup(context.Background(), id); err == nil {
		t.Fatal("restore accepted for a different-name replacement")
	}
}

// A move back a crash interrupted finishes through the same explicit path.
func TestRestoreFinishesAnInterruptedMoveBack(t *testing.T) {
	h := newPromotionHarness(t)
	id, p := strandedOriginal(t, h)
	if err := os.Rename(p.BackupPath, h.original); err != nil {
		t.Fatal(err)
	}
	rewritePromotion(t, h, id, func(q *promotionState) { q.MoveBackStartedAt = "2026-01-01T00:00:00Z" })
	set, err := h.st.GetActionInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	set.Status = StatusCancelled
	if err := h.st.UpdateActionInstance(*set); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.RestoreTranscodeBackup(context.Background(), id); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := testFileSHA(h.original, p.OriginalSHA); err != nil {
		t.Fatal(err)
	}
	if q := movedAsideState(t, h, id); q.MovedAside || q.MoveBackStartedAt != "" || q.MoveAsideStartedAt != "" {
		t.Fatalf("state: %+v", q)
	}
}

// After a restart the saved fingerprint comes from an earlier process, and a
// network mount often reports another device number. The resume read the file
// in full against its digest, so the device number alone no longer stops it.
func TestSameFileAfterReadIgnoresTheDeviceOnlyForAnEarlierProcess(t *testing.T) {
	fp := promotionFingerprint{Size: 10, MtimeNs: 20, CtimeNs: 30, Dev: 1, Ino: 2}
	earlier := fp
	earlier.Process = "an-earlier-process"
	other := fp
	other.Dev = 99
	if !other.sameFileAfterRead(&earlier) {
		t.Fatal("a new device number stopped a resume after a restart")
	}
	if other.sameFileAfterRead(fp.persisted()) {
		t.Fatal("a new device number was ignored within one process")
	}
	if !fp.sameFileAfterRead(fp.persisted()) {
		t.Fatal("the same file was rejected within one process")
	}
	for name, mutate := range map[string]func(*promotionFingerprint){
		"inode": func(f *promotionFingerprint) { f.Ino++ },
		"size":  func(f *promotionFingerprint) { f.Size++ },
		"mtime": func(f *promotionFingerprint) { f.MtimeNs++ },
	} {
		changed := other
		mutate(&changed)
		if changed.sameFileAfterRead(&earlier) {
			t.Fatalf("a different %s was accepted", name)
		}
	}
	if fp.sameFileAfterRead(nil) {
		t.Fatal("no fingerprint matched")
	}
}

// A host reboot between the rename and its checkpoint changes the saved
// device number; the retry still completes instead of failing for good.
func TestPromotionMoveAsideResumesWhenTheDeviceChangedAfterRestart(t *testing.T) {
	h := newPromotionHarness(t)
	crashed := false
	h.engine.promotionMoveAsideHook = func(from, to string) error {
		if crashed {
			return renamePromotionNoReplace(from, to)
		}
		crashed = true
		if err := renamePromotionNoReplace(from, to); err != nil {
			return err
		}
		return errors.New("simulated crash")
	}
	r := h.run()
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusFailed || h.deletes != 0 {
		t.Fatalf("crash fixture: %s %s deletes=%d", r.Status, r.Error, h.deletes)
	}
	rewritePromotion(t, h, r.ID, func(q *promotionState) {
		fp := *q.OriginalFingerprint
		fp.Dev += 7
		fp.Process = "a-process-before-the-reboot"
		q.OriginalFingerprint = &fp
	})
	h.restart()
	r, err := h.engine.Retry(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r = h.finish(r); r.Status != StatusCompleted || h.deletes != 1 {
		t.Fatalf("resume after a device change: %s %s deletes=%d", r.Status, r.Error, h.deletes)
	}
}

// A file that takes the library path while the recovery is being read makes
// the no-replace rename refuse. Nothing moves, the intent is cleared, and the
// inventory stops offering a move back onto a path that is no longer empty.
func TestRestoreRecordsWhenAnotherFileTookTheLibraryPath(t *testing.T) {
	h := newPromotionHarness(t)
	id, p := strandedOriginal(t, h)
	if _, err := h.engine.Cancel(context.Background(), id, "stop"); err != nil {
		t.Fatal(err)
	}
	placed := false
	h.engine.promotionReadHook = func(path string, _ int64) {
		if path == p.BackupPath && !placed {
			placed = true
			if err := os.WriteFile(h.original, []byte("appeared during the read"), 0600); err != nil {
				t.Error(err)
			}
		}
	}
	// Force a full read: a fingerprint from an earlier process proves nothing.
	rewritePromotion(t, h, id, func(q *promotionState) {
		fp := *q.BackupFingerprint
		fp.Process = "an-earlier-process"
		q.BackupFingerprint = &fp
	})
	if _, err := h.engine.RestoreTranscodeBackup(context.Background(), id); err == nil || !strings.Contains(err.Error(), "no longer empty") {
		t.Fatalf("restore: %v", err)
	}
	if !placed {
		t.Fatal("the recovery was not read, so the race was not exercised")
	}
	if got, _ := os.ReadFile(h.original); string(got) != "appeared during the read" {
		t.Fatalf("the other file was replaced: %q", got)
	}
	if err := testFileSHA(p.BackupPath, p.OriginalSHA); err != nil {
		t.Fatalf("recovery changed: %v", err)
	}
	if q := movedAsideState(t, h, id); q.MoveBackStartedAt != "" || !q.MovedAside {
		t.Fatalf("state after the lost race: %+v", q)
	}
	page, err := h.engine.ListTranscodeBackups(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].RestoreAvailable || strings.Contains(page.Items[0].Reason, "moved back") {
		t.Fatalf("inventory still offers the move back: %+v", page.Items)
	}
	if _, err := h.engine.RestoreTranscodeBackup(context.Background(), id); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("a second restore: %v", err)
	}
}
