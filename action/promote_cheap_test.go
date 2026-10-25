package action

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// markSourceByMetadata makes the harness's completed transcode one that was
// preflighted by metadata: the original's stat recorded, accept having checked
// it unchanged.
func markSourceByMetadata(t *testing.T, h *promotionHarness) {
	t.Helper()
	fi, err := os.Stat(h.original)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := h.st.GetActionInstance("source-transcode")
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{}
	if err := json.Unmarshal([]byte(inst.StateJSON), &state); err != nil {
		t.Fatal(err)
	}
	recordOriginalStat(state, fi)
	state[originalIntegrityCheckKey] = "metadata"
	updated := *inst
	updated.StateJSON = toJSON(state)
	if err := h.st.UpdateActionInstance(updated); err != nil {
		t.Fatal(err)
	}
}

func cheapPromotion(t *testing.T, h *promotionHarness) (*ActionResult, *promotionState) {
	t.Helper()
	r := h.run()
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %s %s", r.Status, r.Error)
	}
	r = h.finish(h.resume(r.ID, "approve"))
	inst, err := h.st.GetActionInstance(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPromotion(parseExecutionContext(inst, h.engine))
	if err != nil {
		t.Fatal(err)
	}
	return r, p
}

func TestPromotionByMetadataNeverReadsTheOriginal(t *testing.T) {
	h := newPromotionHarness(t)
	markSourceByMetadata(t, h)
	h.engine.promotionCloneHook = func(d, s *os.File) error { _, err := io.Copy(d, s); return err }
	reads := countReads(h.engine)
	r, p := cheapPromotion(t, h)
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	if !p.originalByMetadata() {
		t.Fatalf("plan did not trust the recorded stat: %+v", p)
	}
	if p.RecoveryMode != promotionRecoveryCopy || p.RecoveryModeReason != recoveryReasonOriginalNotRead {
		t.Fatalf("recovery mode %q (%s)", p.RecoveryMode, p.RecoveryModeReason)
	}
	if p.RecoverySample == "" {
		t.Fatal("the recovery copy was not tied to a sampled digest")
	}
	reads.mu.Lock()
	defer reads.mu.Unlock()
	for path, n := range reads.bytes {
		if path == h.original || strings.HasPrefix(path, p.BackupPath) {
			t.Fatalf("read %d bytes of %s; the original and its recovery copy are never hashed", n, filepath.Base(path))
		}
	}
	if h.deletes != 1 {
		t.Fatalf("old record deletes: %d", h.deletes)
	}
}

func TestPromotionByMetadataStopsWhenTheOriginalChangedSincePlan(t *testing.T) {
	h := newPromotionHarness(t)
	markSourceByMetadata(t, h)
	r := h.run()
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %s %s", r.Status, r.Error)
	}
	// Same size, a later modification time: the edit a re-encode by something
	// else, or a replaced download, leaves.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(h.original, later, later); err != nil {
		t.Fatal(err)
	}
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusFailed || !strings.Contains(r.Error, "integrity violation") {
		t.Fatalf("a changed original was accepted: %s %s", r.Status, r.Error)
	}
	if h.imports != 0 || h.deletes != 0 {
		t.Fatalf("library touched: imports=%d deletes=%d", h.imports, h.deletes)
	}
}

func TestPromotionPlanRefusesAnOriginalChangedSinceTheTranscode(t *testing.T) {
	h := newPromotionHarness(t)
	markSourceByMetadata(t, h)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(h.original, later, later); err != nil {
		t.Fatal(err)
	}
	reads := countReads(h.engine)
	r := h.run()
	if r.Status != StatusFailed || !strings.Contains(r.Error, "changed since it was transcoded") {
		t.Fatalf("plan: %s %s", r.Status, r.Error)
	}
	if n := reads.of(h.original); n != 0 {
		t.Fatalf("read %d bytes of the original to find it changed", n)
	}
}

func TestPromotionWithoutARecordedStatKeepsFullReads(t *testing.T) {
	h := newPromotionHarness(t)
	h.engine.promotionAlwaysCopy = true
	r, p := cheapPromotion(t, h)
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	if p.originalByMetadata() || p.RecoverySample != "" {
		t.Fatalf("an action admitted before hash-once changed behaviour: %+v", p)
	}
}

func TestPromotionByMetadataRejectsATruncatedRecoveryCopy(t *testing.T) {
	h := newPromotionHarness(t)
	markSourceByMetadata(t, h)
	ec := approvedPreserve(t, h)
	step, err := h.engine.stepPromotePreserve(context.Background(), ec)
	if err != nil || step.Status != StepCompleted {
		t.Fatalf("preserve: %+v %v", step, err)
	}
	p, err := loadPromotion(ec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p.BackupPath, p.OriginalBytes-1); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.promotionVerifyRecovered(context.Background(), p); err == nil || !strings.Contains(err.Error(), "integrity violation") {
		t.Fatalf("a truncated recovery copy passed: %v", err)
	}
}

func TestPromotionSampleCoversEndsAndSpreadsTheRest(t *testing.T) {
	const size = 3 << 30
	offsets := promotionSampleOffsets(size)
	if len(offsets) != promotionSampleRanges+2 || offsets[0] != 0 || offsets[len(offsets)-1] != size-promotionSampleChunk {
		t.Fatalf("offsets: %v", offsets)
	}
	for i, off := range offsets {
		if off < 0 || off+promotionSampleChunk > size || (i > 0 && off <= offsets[i-1]) {
			t.Fatalf("offset %d out of order or range: %v", i, offsets)
		}
		if i > 0 && i < len(offsets)-1 && off%4096 != 0 {
			t.Fatalf("interior offset %d not aligned", off)
		}
	}
	// A short file is read whole.
	short := promotionSampleOffsets(3*promotionSampleChunk + 5)
	if len(short) != 4 {
		t.Fatalf("short file offsets: %v", short)
	}
	if len(promotionSampleOffsets(0)) != 0 {
		t.Fatal("an empty file has nothing to sample")
	}
}

func TestPromotionSampleDigestSeesSampledBytesAndSize(t *testing.T) {
	e, path, _ := fingerprintEngine(t)
	// Past the whole-file threshold, so only the sampled ranges are read.
	size := int64(promotionSampleChunk * 40)
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	base, err := e.promotionSampleDigest(ctx, path, size)
	if err != nil {
		t.Fatal(err)
	}
	flip := func(off int64) string {
		t.Helper()
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		b := []byte{0}
		if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
			t.Fatal(err)
		}
		b[0] ^= 0xff
		if _, err := f.WriteAt(b, off); err != nil {
			t.Fatal(err)
		}
		got, err := e.promotionSampleDigest(ctx, path, size)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	offsets := promotionSampleOffsets(size)
	for _, off := range []int64{0, offsets[3] + 17, size - 1} {
		if flip(off) == base {
			t.Fatalf("a changed byte at %d went unseen", off)
		}
		flip(off) // restore
	}
	if got := flip(offsets[3] + promotionSampleChunk + 4096); got != base {
		t.Fatalf("a byte outside every sampled range changed the digest")
	}
	if _, err := e.promotionSampleDigest(ctx, path, size-1); err == nil {
		t.Fatal("a file of another size matched")
	}
}

func TestPromotionPlanReportsTheConvertedFileCheckAndLeavesNothingBehind(t *testing.T) {
	h := newPromotionHarness(t)
	markSourceByMetadata(t, h)
	var phases []string
	observe := func() {
		instances, err := h.st.ListActionInstances("", 100)
		if err != nil {
			return
		}
		for _, inst := range instances {
			if inst.ActionName != "promote_transcode_candidate" {
				continue
			}
			var state map[string]any
			if json.Unmarshal([]byte(inst.StateJSON), &state) != nil {
				continue
			}
			if progress, _ := state["work_progress"].(map[string]any); progress != nil {
				phase, _ := progress["phase"].(string)
				if len(phases) == 0 || phases[len(phases)-1] != phase {
					phases = append(phases, phase)
				}
			}
		}
	}
	h.engine.promotionLstatHook = func(path string) (os.FileInfo, error) {
		observe()
		return os.Lstat(path)
	}
	r := h.run()
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %s %s", r.Status, r.Error)
	}
	seen := false
	for _, phase := range phases {
		if phase == "verifying_original" {
			t.Fatalf("plan reported reading the original: %v", phases)
		}
		seen = seen || phase == "verifying_candidate"
	}
	if !seen {
		t.Fatalf("plan never reported the converted file's check: %v", phases)
	}
	if got := persistedWorkProgress(t, h, r.ID); got != nil {
		t.Fatalf("the finished plan left work looking current: %+v", got)
	}
}
