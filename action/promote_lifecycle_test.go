package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromotionFinalizeRejectsUnrelatedTemporaryPath(t *testing.T) {
	h := newPromotionHarness(t)
	h.adoptedPathOverride = filepath.Join(filepath.Dir(h.candidate), "unrelated.mkv")
	id, p := h.seedPostRenameFinalize(t, nil)
	r := h.resume(id, "")
	_, backupErr := os.Stat(p.BackupPath)
	if r.Status != StatusFailed || backupErr != nil {
		t.Fatalf("unrelated temporary path accepted: status=%s error=%s backup=%v", r.Status, r.Error, backupErr)
	}
}

func TestPromotionFinalizeRejectsStalePathWithoutDurableFileID(t *testing.T) {
	h := newPromotionHarness(t)
	h.permanentStaleFinalPath = true
	id, p := h.seedPostRenameFinalize(t, func(p *promotionState) { p.NewFileID = 0 })
	r := h.resume(id, "")
	_, backupErr := os.Stat(p.BackupPath)
	if r.Status != StatusFailed || backupErr != nil {
		t.Fatalf("missing durable identity accepted: status=%s error=%s backup=%v", r.Status, r.Error, backupErr)
	}
}

func TestPromotionFinalizeResumesAfterRecoveryRemoved(t *testing.T) {
	h := newPromotionHarness(t)
	id, p := h.seedPostRenameFinalize(t, func(p *promotionState) {
		p.RecoveryCleanupStarted = true
		p.BackupVerified = false
	})
	if err := os.RemoveAll(filepath.Dir(p.BackupPath)); err != nil {
		t.Fatal(err)
	}
	r := h.resume(id, "")
	if r.Status != StatusCompleted {
		t.Fatalf("cleanup resume: %s %s", r.Status, r.Error)
	}
}

func TestPromotionRenameHandlesStalePathAfterPhysicalMove(t *testing.T) {
	h := newPromotionHarness(t)
	id, p := h.seedPostRenameFinalize(t, nil)
	h.permanentStaleFinalPath = true
	inst, err := h.st.GetActionInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	inst.CurrentStep = 5
	if err := h.st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	r := h.resume(id, "")
	if r.Status == StatusFailed {
		_, backupErr := os.Stat(p.BackupPath)
		t.Fatalf("rename cannot reconcile completed physical move: status=%s error=%s backup=%v", r.Status, r.Error, backupErr)
	}
}

// Drive the actual workflow through a physical rename with a stale response,
// then restart while rename_candidate is still the current step.
func TestPromotionRenameStaleResponseSurvivesRestart(t *testing.T) {
	h := newPromotionHarness(t)
	h.stalePathAfterRename = true
	r := h.run()
	r = h.resume(r.ID, "approve")
	for i := 0; i < 12 && r.Status == StatusWaitingExternal && h.renames == 0; i++ {
		r = h.resume(r.ID, "")
	}
	if r.Status != StatusWaitingExternal {
		t.Fatalf("rename should wait: %s %s", r.Status, r.Error)
	}
	if _, err := os.Stat(h.candidate); !os.IsNotExist(err) {
		t.Fatalf("candidate should have moved: %v", err)
	}
	inst, err := h.st.GetActionInstance(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPromotion(parseExecutionContext(inst, h.engine))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.BackupPath); err != nil {
		t.Fatal(err)
	}
	h.restart()
	r = h.resume(r.ID, "")
	if r.Status != StatusWaitingExternal {
		t.Fatalf("stale rename after restart: %s %s", r.Status, r.Error)
	}
	h.mu.Lock()
	h.permanentStaleFinalPath = false
	h.mu.Unlock()
	r = h.resume(r.ID, "")
	if r.Status != StatusCompleted {
		t.Fatalf("reconcile rename: %s %s", r.Status, r.Error)
	}
	if h.imports != 1 || h.renames != 1 || h.deletes != 1 {
		t.Fatalf("mutations replayed: %d/%d/%d", h.imports, h.renames, h.deletes)
	}
	if _, err := os.Stat(filepath.Dir(h.candidate)); !os.IsNotExist(err) {
		t.Fatalf("empty candidate directory remains: %v", err)
	}
}

func TestPromotionCleanupPreservesOtherCandidates(t *testing.T) {
	h := newPromotionHarness(t)
	id, _ := h.seedPostRenameFinalize(t, nil)
	sibling := filepath.Join(filepath.Dir(h.candidate), "another-job.mkv")
	if err := os.WriteFile(sibling, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	r := h.resume(id, "")
	if r.Status != StatusCompleted {
		t.Fatalf("finalize: %s %s", r.Status, r.Error)
	}
	b, err := os.ReadFile(sibling)
	if err != nil || string(b) != "keep" {
		t.Fatalf("other candidate changed: %q %v", b, err)
	}
}

func TestPromotionFinalizeToleratesCandidateDisappearingBeforeHash(t *testing.T) {
	h := newPromotionHarness(t)
	id, p := h.seedPostRenameFinalize(t, nil)
	final, err := os.ReadFile(h.final)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.candidate, final, 0600); err != nil {
		t.Fatal(err)
	}
	// The optional leftover existed at cleanup discovery, but an external
	// rename/removal becomes visible before it can be opened for hashing.
	disappeared := false
	h.engine.promotionLstatHook = func(path string) (os.FileInfo, error) {
		if path == h.candidate && !disappeared {
			stale, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			disappeared = true
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			return stale, nil
		}
		return os.Lstat(path)
	}
	r := h.resume(id, "")
	if !disappeared || r.Status != StatusCompleted {
		t.Fatalf("optional candidate disappearance: %s %s", r.Status, r.Error)
	}
	if _, err := os.Stat(p.BackupPath); !os.IsNotExist(err) {
		t.Fatalf("recovery not cleaned after verified finalization: %v", err)
	}
	got, err := os.ReadFile(h.final)
	if err != nil || string(got) != string(final) {
		t.Fatalf("final file changed: %v", err)
	}
}

func TestPromotionFinalizeRetainsChangedLeftoverCandidate(t *testing.T) {
	h := newPromotionHarness(t)
	id, p := h.seedPostRenameFinalize(t, func(p *promotionState) {
		// E01's final filename is also the former original filename.
		p.OriginalPath = h.final
	})
	if err := os.WriteFile(h.candidate, []byte("unapproved replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	r := h.resume(id, "")
	if r.Status != StatusFailed || !strings.Contains(r.Error, "SHA-256 changed") {
		t.Fatalf("changed leftover accepted: %s %s", r.Status, r.Error)
	}
	if _, err := os.Stat(p.BackupPath); err != nil {
		t.Fatalf("required recovery removed: %v", err)
	}
	if got, err := os.ReadFile(h.candidate); err != nil || string(got) != "unapproved replacement" {
		t.Fatalf("changed leftover was removed: %q %v", got, err)
	}
}

func TestPromotionRejectsChangedWorkerAttestedCandidate(t *testing.T) {
	h := newPromotionHarness(t)
	source, err := h.st.GetActionInstance("source-transcode")
	if err != nil {
		t.Fatal(err)
	}
	ec := parseExecutionContext(source, h.engine)
	sha, _, err := h.engine.promotionHash(context.Background(), h.candidate)
	if err != nil {
		t.Fatal(err)
	}
	ec.State["candidate_sha256"] = sha
	source.StateJSON = toJSON(ec.State)
	if err := h.st.UpdateActionInstance(*source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.candidate, []byte("modified after publication"), 0600); err != nil {
		t.Fatal(err)
	}
	r := h.run()
	if r.Status != StatusFailed || !strings.Contains(r.Error, "worker validation") {
		t.Fatalf("changed NAS candidate accepted: %s %s", r.Status, r.Error)
	}
	if h.imports != 0 {
		t.Fatal("changed candidate imported")
	}
}

func TestPromotionWaitingRenameDoesNotRehashRecovery(t *testing.T) {
	h := newPromotionHarness(t)
	h.stalePathAfterRename = true
	r := h.run()
	r = h.resume(r.ID, "approve")
	for i := 0; i < 12 && r.Status == StatusWaitingExternal && h.renames == 0; i++ {
		r = h.resume(r.ID, "")
	}
	if h.renames != 1 || r.Status != StatusWaitingExternal {
		t.Fatalf("did not reach stale rename: %s %s", r.Status, r.Error)
	}
	h.engine.promotionLstatHook = func(path string) (os.FileInfo, error) {
		t.Errorf("read-only rename poll hashed %s", path)
		return os.Lstat(path)
	}
	r = h.resume(r.ID, "")
	if r.Status != StatusWaitingExternal {
		t.Fatalf("poll should wait: %s %s", r.Status, r.Error)
	}
}

func TestPromotionReusesMediaProofButRejectsChangedBytes(t *testing.T) {
	h := newPromotionHarness(t)
	sha, _, err := h.engine.promotionHash(context.Background(), h.candidate)
	if err != nil {
		t.Fatal(err)
	}
	p := &promotionState{CandidateSHA: sha}
	if err := h.engine.promotionInspectAdopted(context.Background(), p, h.candidate); err != nil {
		t.Fatal(err)
	}
	h.engine.deps.Ffprobe = filepath.Join(h.root, "missing-ffprobe")
	if err := h.engine.promotionInspectAdopted(context.Background(), p, h.candidate); err != nil {
		t.Fatalf("media inspection repeated for identical bytes: %v", err)
	}
	if err := os.WriteFile(h.candidate, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.promotionInspectAdopted(context.Background(), p, h.candidate); err == nil {
		t.Fatal("media proof bypassed content integrity")
	}
}
