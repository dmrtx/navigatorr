package action

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func expectedPromotionBackup(candidatePath string) string {
	key := sha256.Sum256([]byte("sonarr\x00source-transcode"))
	return filepath.Join(filepath.Dir(candidatePath), ".promotion-recovery", hex.EncodeToString(key[:]), "original.bak")
}

func TestPromotionReusesCompletedVerifiedPartialWithoutRecopy(t *testing.T) {
	h := newPromotionHarness(t)
	r := h.run()
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %s %s", r.Status, r.Error)
	}
	backupPath := expectedPromotionBackup(h.candidate)
	partial := backupPath + ".partial"
	if err := os.MkdirAll(filepath.Dir(backupPath), 0700); err != nil {
		t.Fatal(err)
	}
	originalBytes, err := os.ReadFile(h.original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, originalBytes, 0600); err != nil {
		t.Fatal(err)
	}
	partialInfo, err := os.Lstat(partial)
	if err != nil {
		t.Fatal(err)
	}

	r = h.resume(r.ID, "approve")
	if r.Status != StatusWaitingExternal {
		t.Fatalf("expected deletion checkpoint after preserve reuse, got %s %s", r.Status, r.Error)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	backupInfo, err := os.Lstat(p.BackupPath)
	if err != nil {
		t.Fatalf("reused backup missing: %v", err)
	}
	if !os.SameFile(partialInfo, backupInfo) {
		t.Fatal("completed verified partial was recopied instead of atomically renamed")
	}
	if _, err := os.Lstat(partial); !os.IsNotExist(err) {
		t.Fatalf("partial retained after reuse: %v", err)
	}
	if err := h.engine.verifyPromotionHash(context.Background(), p.BackupPath, p.OriginalSHA); err != nil {
		t.Fatal(err)
	}
	r = h.finish(r)
	if r.Status != StatusCompleted {
		t.Fatalf("finish after reuse: %s %s", r.Status, r.Error)
	}
}

func TestPromotionRebuildsIncompletePartial(t *testing.T) {
	h := newPromotionHarness(t)
	r := h.run()
	backupPath := expectedPromotionBackup(h.candidate)
	partial := backupPath + ".partial"
	if err := os.MkdirAll(filepath.Dir(backupPath), 0700); err != nil {
		t.Fatal(err)
	}
	originalBytes, err := os.ReadFile(h.original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, originalBytes[:len(originalBytes)/2], 0600); err != nil {
		t.Fatal(err)
	}

	r = h.resume(r.ID, "approve")
	if r.Status != StatusWaitingExternal {
		t.Fatalf("incomplete partial was not rebuilt, got %s %s", r.Status, r.Error)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	backupBytes, err := os.ReadFile(p.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(backupBytes) != string(originalBytes) {
		t.Fatal("backup does not match original after incomplete partial rebuild")
	}
}

func TestPromotionNeverTrustsMismatchedPartialAtExactSize(t *testing.T) {
	h := newPromotionHarness(t)
	r := h.run()
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %s %s", r.Status, r.Error)
	}
	backupPath := expectedPromotionBackup(h.candidate)
	partial := backupPath + ".partial"
	if err := os.MkdirAll(filepath.Dir(backupPath), 0700); err != nil {
		t.Fatal(err)
	}
	originalBytes, err := os.ReadFile(h.original)
	if err != nil {
		t.Fatal(err)
	}
	// Same size as the original but different content: a size-only retry check
	// would publish this corrupt partial as the recovery copy.
	mismatched := append([]byte{}, originalBytes...)
	mismatched[len(mismatched)/2] ^= 0xFF
	if err := os.WriteFile(partial, mismatched, 0600); err != nil {
		t.Fatal(err)
	}

	r = h.resume(r.ID, "approve")
	if r.Status != StatusWaitingExternal {
		t.Fatalf("mismatched partial was not rebuilt: %s %s", r.Status, r.Error)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	backupBytes, err := os.ReadFile(p.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backupBytes, originalBytes) {
		t.Fatal("mismatched partial was trusted as the recovery copy")
	}
	if _, err := os.Lstat(partial); !os.IsNotExist(err) {
		t.Fatalf("mismatched partial retained: %v", err)
	}
}

func TestPromotionVerifiesRecoveryOnlyAfterCopyIsClosedAndSynced(t *testing.T) {
	h := newPromotionHarness(t)
	originalBytes, err := os.ReadFile(h.original)
	if err != nil {
		t.Fatal(err)
	}
	boundaryObserved := false
	// The hook runs after the copy destination has been synced and closed. If
	// verification ran before that boundary, corrupting the finished copy here
	// could not be detected; the step must fail instead of publishing it.
	h.engine.promotionCopyBoundaryHook = func(partial string) {
		boundaryObserved = true
		got, err := os.ReadFile(partial)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, originalBytes) {
			t.Fatalf("copy boundary hook saw an incomplete copy: %d of %d bytes", len(got), len(originalBytes))
		}
		if err := os.WriteFile(partial, bytes.Repeat([]byte("x"), len(originalBytes)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := h.run()
	r = h.resume(r.ID, "approve")
	if !boundaryObserved {
		t.Fatal("copy boundary hook was never observed")
	}
	if r.Status != StatusFailed || !strings.Contains(r.Error, "SHA-256") {
		t.Fatalf("verification did not run after the finished copy boundary: %s %s", r.Status, r.Error)
	}
	if h.imports != 0 {
		t.Fatalf("Sonarr was mutated despite a corrupted recovery partial: imports=%d", h.imports)
	}
}

func TestPromotionFinalizeWaitsForStaleSonarrPathThenUsesDurableNewPath(t *testing.T) {
	h := newPromotionHarness(t)
	h.stalePathAfterRescan = true
	r := h.run()
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted {
		t.Fatalf("promotion did not recover from stale post-rename path: %s %s waiting=%s", r.Status, r.Error, r.WaitingReason)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(p.NewPath) != filepath.Clean(h.final) {
		t.Fatalf("durable new_path drifted: %q vs %q", p.NewPath, h.final)
	}
	if getString(r.Outputs, "final_path") != h.final {
		t.Fatalf("final_path output = %v, want %s", r.Outputs["final_path"], h.final)
	}
	finalBytes, err := os.ReadFile(h.final)
	if err != nil {
		t.Fatalf("final file missing: %v", err)
	}
	actual := sha256.Sum256(finalBytes)
	if hex.EncodeToString(actual[:]) != p.CandidateSHA {
		t.Fatal("final file hash does not match validated candidate SHA")
	}
	if _, err := os.Stat(h.candidate); !os.IsNotExist(err) {
		t.Fatalf("temporary candidate remains: %v", err)
	}
	if _, err := os.Stat(p.BackupPath); !os.IsNotExist(err) {
		t.Fatalf("backup retained after successful retry: %v", err)
	}
	if !strings.Contains(strings.Join(h.mutationOrder, ","), "rename,rescan") {
		t.Fatalf("rename/rescan not observed: %v", h.mutationOrder)
	}
}
