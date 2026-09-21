package action

import (
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
