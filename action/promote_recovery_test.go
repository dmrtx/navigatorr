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
	if filepath.Clean(p.BackupPath) != filepath.Clean(backupPath) {
		t.Fatalf("backup path drift: %s vs %s", p.BackupPath, backupPath)
	}
	backupInfo, err := os.Lstat(p.BackupPath)
	if err != nil {
		t.Fatalf("reused backup missing: %v", err)
	}
	if !os.SameFile(partialInfo, backupInfo) {
		t.Fatal("completed verified partial was recopied instead of atomically renamed (inode changed)")
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
	truncated := originalBytes[:len(originalBytes)/2]
	if err := os.WriteFile(partial, truncated, 0600); err != nil {
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
	if err := h.engine.verifyPromotionHash(context.Background(), p.BackupPath, p.OriginalSHA); err != nil {
		t.Fatal(err)
	}
	r = h.finish(r)
	if r.Status != StatusCompleted {
		t.Fatalf("finish after rebuild: %s %s", r.Status, r.Error)
	}
}

func TestPromotionRebuildsMismatchedPartial(t *testing.T) {
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
	bad := make([]byte, len(originalBytes))
	for i := range bad {
		bad[i] = 'x'
	}
	if string(bad) == string(originalBytes) {
		bad[0] = 'y'
	}
	if err := os.WriteFile(partial, bad, 0600); err != nil {
		t.Fatal(err)
	}
	r = h.resume(r.ID, "approve")
	if r.Status != StatusWaitingExternal {
		t.Fatalf("mismatched partial was not rebuilt, got %s %s", r.Status, r.Error)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.engine.verifyPromotionHash(context.Background(), p.BackupPath, p.OriginalSHA); err != nil {
		t.Fatalf("rebuilt backup invalid: %v", err)
	}
	backupBytes, err := os.ReadFile(p.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(backupBytes) != string(originalBytes) {
		t.Fatal("mismatched partial was reused instead of rebuilt")
	}
}

func TestPromotionPreserveReentryCleansVerifiedPartial(t *testing.T) {
	h := newPromotionHarness(t)
	r := h.run()
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %s %s", r.Status, r.Error)
	}
	// Approve in state and drive preserve directly so the original still
	// exists (resume would continue through import/delete and remove it).
	inst, err := h.st.GetActionInstance(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	ec := parseExecutionContext(inst, h.engine)
	ec.Decision = "approve"
	ar, err := h.engine.stepPromoteApprove(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	if ar.Status != StepCompleted {
		t.Fatalf("approve: %+v", ar)
	}
	inst, err = h.st.GetActionInstance(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	ec = parseExecutionContext(inst, h.engine)
	first, err := h.engine.stepPromotePreserve(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != StepCompleted {
		t.Fatalf("first preserve: %+v", first)
	}
	p, err := loadPromotion(ec)
	if err != nil {
		t.Fatal(err)
	}
	backupBytes, err := os.ReadFile(p.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate leftover verified partial from an interrupted older publish.
	if err := os.WriteFile(p.BackupPath+".partial", backupBytes, 0600); err != nil {
		t.Fatal(err)
	}
	inst, err = h.st.GetActionInstance(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	ec = parseExecutionContext(inst, h.engine)
	res, err := h.engine.stepPromotePreserve(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StepCompleted {
		t.Fatalf("re-entrant preserve failed: %+v", res)
	}
	if _, err := os.Lstat(p.BackupPath + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("verified partial retained after re-entry: %v", err)
	}
	if err := h.engine.verifyPromotionHash(context.Background(), p.BackupPath, p.OriginalSHA); err != nil {
		t.Fatal(err)
	}
	// Same-action second re-entry without any partial must stay idempotent.
	inst2, err := h.st.GetActionInstance(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	ec2 := parseExecutionContext(inst2, h.engine)
	res2, err := h.engine.stepPromotePreserve(context.Background(), ec2)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != StepCompleted {
		t.Fatalf("second re-entry failed: %+v", res2)
	}
}

func TestPromotionFinalizeUsesDurableNewPathAfterRename(t *testing.T) {
	h := newPromotionHarness(t)
	r := h.run()
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted {
		t.Fatalf("full workflow: %s %s waiting=%s", r.Status, r.Error, r.WaitingReason)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	if p.NewPath == "" || filepath.Clean(p.NewPath) != filepath.Clean(h.final) {
		t.Fatalf("durable new_path not finalized: %q vs %q", p.NewPath, h.final)
	}
	if getString(r.Outputs, "final_path") != h.final {
		t.Fatalf("final_path output not durable: %v", r.Outputs["final_path"])
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
	if _, err := os.Stat(h.original); !os.IsNotExist(err) {
		t.Fatalf("original remains: %v", err)
	}
	if _, err := os.Stat(p.BackupPath); !os.IsNotExist(err) {
		t.Fatalf("backup retained after success: %v", err)
	}
	if getBool(r.Outputs, "recovery_retained") {
		t.Fatalf("recovery_retained should be false after finalize: %v", r.Outputs)
	}
	if !getBool(r.Outputs, "promoted") {
		t.Fatalf("promoted missing: %v", r.Outputs)
	}
	if !strings.Contains(strings.Join(h.mutationOrder, ","), "rename") {
		t.Fatalf("rename not observed: %v", h.mutationOrder)
	}
}
