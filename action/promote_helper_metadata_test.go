package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// metadataPlanned is a promotion by metadata that plan has accepted and that
// waits for its decision, with the recovery directory in place for the choice.
func metadataPlanned(t *testing.T, h *promotionHarness) *promotionState {
	t.Helper()
	markSourceByMetadata(t, h)
	r := runLocalPromotion(t, h)
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %s %s", r.Status, r.Error)
	}
	p := movedAsideState(t, h, r.ID)
	if !p.originalByMetadata() {
		t.Fatalf("plan did not trust the recorded stat: %+v", p)
	}
	backup, err := promotionExpectedBackupPath(p)
	if err != nil {
		t.Fatal(err)
	}
	p.BackupPath = backup
	if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// The common case: a promotion that never reads its original moves it aside,
// and neither it nor the moved file is read over the mount or hashed by the
// helper.
func TestMetadataPromotionMovesTheOriginalAsideWithoutReadingIt(t *testing.T) {
	resetHelperProbes()
	h := newPromotionHarness(t)
	markSourceByMetadata(t, h)
	ph := attachProofHelper(t, h, true)
	failMountProbe(h)
	reads := countReads(h.engine)
	moves := 0
	h.engine.promotionMoveAsideHook = func(from, to string) error {
		moves++
		return renamePromotionNoReplace(from, to)
	}
	r := runLocalPromotion(t, h)
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted || moves != 1 {
		t.Fatalf("promotion: %s %s moves=%d", r.Status, r.Error, moves)
	}
	p := movedAsideState(t, h, r.ID)
	if p.RecoveryMode != promotionRecoveryMoveAside || p.RecoveryModeReason != recoveryReasonMoveAside || p.RecoveryProof != promotionProofHelper {
		t.Fatalf("recovery %q/%q proof %q", p.RecoveryMode, p.RecoveryModeReason, p.RecoveryProof)
	}
	if !p.MovedAside || p.RecoveryCopy != nil || p.BackupHelper == nil || p.RecoverySample == "" || p.OriginalHelper == nil || p.OriginalHelper.Sample != p.RecoverySample {
		t.Fatalf("move-aside evidence: %+v", p)
	}
	if got := reads.of(h.original) + reads.of(p.BackupPath); got != 0 {
		t.Fatalf("%d bytes were hashed or copied", got)
	}
	if n := ph.hashed(filepath.Base(h.original)) + ph.hashed(filepath.Base(p.BackupPath)); n != 0 {
		t.Fatalf("the helper hashed the original %d times in full", n)
	}
}

func TestMetadataPromotionChoiceNeedsEveryCheck(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, h *promotionHarness, ph *proofHelper, p *promotionState)
		reason string
		detail string
	}{
		{"helper_down", func(t *testing.T, h *promotionHarness, ph *proofHelper, p *promotionState) { ph.down.Store(true) }, recoveryReasonOriginalNotRead, ""},
		{"service_is_not_filesystem", func(t *testing.T, h *promotionHarness, ph *proofHelper, p *promotionState) { p.Service = "sonarr" }, recoveryReasonOriginalNotRead, ""},
		{"mtime_differs", func(t *testing.T, h *promotionHarness, ph *proofHelper, p *promotionState) {
			p.OriginalStatMod = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
		}, recoveryReasonHelperStat, "size_equal=true mtime_diff_ns="},
		{"size_differs", func(t *testing.T, h *promotionHarness, ph *proofHelper, p *promotionState) { p.OriginalBytes++ }, recoveryReasonHelperStat, "size_equal=false"},
		{"helper_and_mount_disagree", func(t *testing.T, h *promotionHarness, ph *proofHelper, p *promotionState) {
			other := strings.Repeat("ab", 32)
			ph.digest.Store(&other)
		}, recoveryReasonHelperDisagrees, ""},
		{"leftover_recovery_file", func(t *testing.T, h *promotionHarness, ph *proofHelper, p *promotionState) {
			if err := os.WriteFile(p.BackupPath, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, recoveryReasonLeftover, ""},
		{"sample_already_recorded", func(t *testing.T, h *promotionHarness, ph *proofHelper, p *promotionState) {
			p.RecoverySample = "earlier"
		}, recoveryReasonLeftover, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetHelperProbes()
			h := newPromotionHarness(t)
			ph := attachProofHelper(t, h, true)
			failMountProbe(h)
			p := metadataPlanned(t, h)
			c.setup(t, h, ph, p)
			mode, reason, detail := h.engine.promotionChooseByHelperSample(context.Background(), p)
			if mode != promotionRecoveryCopy || reason != c.reason || !strings.HasPrefix(detail, c.detail) {
				t.Fatalf("chose %q (%s) %q", mode, reason, detail)
			}
			if p.RecoveryProof != "" || p.OriginalHelper != nil || (p.RecoverySample != "" && c.name != "sample_already_recorded") {
				t.Fatalf("a refused choice left proof behind: %+v", p)
			}
		})
	}
}

func TestMetadataPromotionNeedsACompleteIdentity(t *testing.T) {
	resetHelperProbes()
	h := newPromotionHarness(t)
	attachProofHelper(t, h, false)
	p := metadataPlanned(t, h)
	mode, reason, _ := h.engine.promotionChooseByHelperSample(context.Background(), p)
	if mode != promotionRecoveryCopy || reason != recoveryReasonOriginalNotRead {
		t.Fatalf("chose %q (%s)", mode, reason)
	}
}

// The mount reports a modification time in 100 ns units; the filesystem has the
// exact nanosecond. They are the same time.
func TestMetadataPromotionComparesMtimeAtTheMountsResolution(t *testing.T) {
	resetHelperProbes()
	h := newPromotionHarness(t)
	attachProofHelper(t, h, true)
	failMountProbe(h)
	p := metadataPlanned(t, h)
	fi, err := os.Stat(p.OriginalPath)
	if err != nil {
		t.Fatal(err)
	}
	truncated := fi.ModTime().UnixNano() / 100 * 100
	p.OriginalStatMod = time.Unix(0, truncated).UTC().Format(time.RFC3339Nano)
	if mode, reason, detail := h.engine.promotionChooseByHelperSample(context.Background(), p); mode != promotionRecoveryMoveAside {
		t.Fatalf("chose %q (%s) %s", mode, reason, detail)
	}
	if p.RecoveryProof != promotionProofHelper || p.OriginalHelper == nil || p.RecoverySample == "" || p.OriginalHelper.Sample != p.RecoverySample {
		t.Fatalf("proof not recorded: %+v", p)
	}
	if ok, _, _ := helperMtimeMatches(p.OriginalStatMod, truncated+100); ok {
		t.Fatal("a time 100 ns later was accepted")
	}
}

// A restart between the choice and the move aside costs nothing while the file
// is untouched, and one read when it was touched.
func TestMetadataPromotionReprovesTheOriginalAfterARestart(t *testing.T) {
	resetHelperProbes()
	h := newPromotionHarness(t)
	ph := attachProofHelper(t, h, true)
	failMountProbe(h)
	p := metadataPlanned(t, h)
	if mode, reason, detail := h.engine.promotionChooseByHelperSample(context.Background(), p); mode != promotionRecoveryMoveAside {
		t.Fatalf("chose %q (%s) %s", mode, reason, detail)
	}
	h.restart()
	proof, err := h.engine.promotionVerifyOriginalByHelper(context.Background(), p)
	if err != nil || proof == nil || proof.Process != promotionProcess || ph.hashed(filepath.Base(p.OriginalPath)) != 0 {
		t.Fatalf("untouched original after a restart: proof %+v err %v", proof, err)
	}
	p.OriginalHelper = proof
	// Touched (ctime moves, bytes do not): the file is read in full and accepted.
	if err := os.Chmod(p.OriginalPath, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.promotionVerifyOriginalByHelper(context.Background(), p); err != nil || ph.hashed(filepath.Base(p.OriginalPath)) == 0 {
		t.Fatalf("touched original: err %v", err)
	}
}

func TestMetadataPromotionReprovesNothingAfterTheIntent(t *testing.T) {
	resetHelperProbes()
	h := newPromotionHarness(t)
	ph := attachProofHelper(t, h, true)
	failMountProbe(h)
	p := metadataPlanned(t, h)
	if mode, _, _ := h.engine.promotionChooseByHelperSample(context.Background(), p); mode != promotionRecoveryMoveAside {
		t.Fatal("not moved aside")
	}
	p.MoveAsideStartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if h.engine.promotionReproveOriginalByHelper(context.Background(), p) != nil {
		t.Fatal("the persisted proof was trusted after an intent")
	}
	h.restart()
	if _, err := h.engine.promotionVerifyOriginalByHelper(context.Background(), p); err != nil || ph.hashed(filepath.Base(p.OriginalPath)) == 0 {
		t.Fatalf("an original after an intent is read in full: err %v", err)
	}
}

// An original that was rewritten after the choice is not the file the choice
// was made for, whatever its mtime says.
func TestMetadataPromotionRefusesARewrittenOriginal(t *testing.T) {
	resetHelperProbes()
	h := newPromotionHarness(t)
	attachProofHelper(t, h, true)
	failMountProbe(h)
	p := metadataPlanned(t, h)
	if mode, _, _ := h.engine.promotionChooseByHelperSample(context.Background(), p); mode != promotionRecoveryMoveAside {
		t.Fatal("not moved aside")
	}
	fi, err := os.Stat(p.OriginalPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.OriginalPath)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(p.OriginalPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p.OriginalPath, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if h.engine.promotionReproveOriginalByHelper(context.Background(), p) != nil {
		t.Fatal("the rewritten original was re-proved")
	}
	if _, err := h.engine.promotionVerifyOriginalByHelper(context.Background(), p); err == nil || !strings.Contains(err.Error(), "integrity violation") {
		t.Fatalf("a rewritten original was accepted: %v", err)
	}
}

func TestMetadataRecoveryWithoutASampleIsNotTakenFromTheLibraryPath(t *testing.T) {
	p := &promotionState{OriginalStatMod: "2026-01-01T00:00:00Z", RecoveryMode: promotionRecoveryMoveAside}
	e := &Engine{}
	if err := e.verifyPromotionRecovery(context.Background(), p, "/nowhere"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("a move-aside recovery proved without a sample: %v", err)
	}
}
