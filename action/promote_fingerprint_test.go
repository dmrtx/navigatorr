package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/fsop"
)

type readCounter struct {
	mu    sync.Mutex
	bytes map[string]int64
}

func countReads(e *Engine) *readCounter {
	c := &readCounter{bytes: map[string]int64{}}
	e.promotionReadHook = func(path string, n int64) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.bytes[path] += n
	}
	return c
}

func (c *readCounter) of(path string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes[path]
}

func fingerprintEngine(t *testing.T) (*Engine, string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := fsop.NewResolver([]string{root}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "media.mkv")
	content := []byte(strings.Repeat("media-bytes-", 1000))
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return NewEngine(EngineDeps{Fs: resolver}), path, hex.EncodeToString(sum[:])
}

func TestPromotionFingerprintSkipsOnlyAnUnchangedRead(t *testing.T) {
	e, path, sum := fingerprintEngine(t)
	reads := countReads(e)
	ec := &ExecutionContext{State: map[string]any{}}
	ctx := withPromotionVerifiedFiles(context.Background(), ec)
	if err := e.verifyPromotionHash(ctx, path, sum); err != nil {
		t.Fatal(err)
	}
	full := reads.of(path)
	if err := e.verifyPromotionHash(ctx, path, sum); err != nil {
		t.Fatal(err)
	}
	if err := e.verifyPromotionHashStable(ctx, path, sum); err != nil {
		t.Fatal(err)
	}
	if reads.of(path) != full {
		t.Fatalf("an unchanged file was read again: %d bytes after %d", reads.of(path), full)
	}
	check := ec.State[promotionIntegrityState].(map[string]any)[path].(map[string]any)
	if check["kind"] != promotionCheckStat || check["sha256"] != sum {
		t.Fatalf("verification kind not recorded: %+v", check)
	}
	// A different expected digest is never answered from the fingerprint.
	if err := e.verifyPromotionHash(ctx, path, strings.Repeat("0", 64)); err == nil || reads.of(path) != 2*full {
		t.Fatalf("mismatched digest trusted: %v", err)
	}

	// Same size, and mtime restored to the nanosecond: ctime still changes.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte(strings.Repeat("MEDIA-BYTES-", 1000)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := e.verifyPromotionHash(ctx, path, sum); err == nil || !strings.Contains(err.Error(), "SHA-256 changed") {
		t.Fatalf("tampered file passed: %v", err)
	}
	if reads.of(path) != 3*full {
		t.Fatalf("tampered file was not read in full: %d", reads.of(path))
	}
}

func TestPromotionFingerprintNeedsTheSameExecution(t *testing.T) {
	e, path, sum := fingerprintEngine(t)
	reads := countReads(e)
	first := withPromotionVerifiedFiles(context.Background(), &ExecutionContext{State: map[string]any{}})
	if err := e.verifyPromotionHash(first, path, sum); err != nil {
		t.Fatal(err)
	}
	full := reads.of(path)
	for name, ctx := range map[string]context.Context{
		"later execution": withPromotionVerifiedFiles(context.Background(), &ExecutionContext{State: map[string]any{}}),
		"no execution":    context.Background(),
		"cleanup":         withoutPromotionVerifiedFiles(first),
	} {
		before := reads.of(path)
		if err := e.verifyPromotionHash(ctx, path, sum); err != nil {
			t.Fatal(err)
		}
		if reads.of(path)-before != full {
			t.Fatalf("%s did not read the whole file", name)
		}
	}
}

func TestPromotionRenameKeepsOnlyAStrictIdentity(t *testing.T) {
	e, path, sum := fingerprintEngine(t)
	reads := countReads(e)
	ctx := withPromotionVerifiedFiles(context.Background(), &ExecutionContext{State: map[string]any{}})
	if err := e.verifyPromotionHash(ctx, path, sum); err != nil {
		t.Fatal(err)
	}
	renamed := path + ".bak"
	if err := os.Rename(path, renamed); err != nil {
		t.Fatal(err)
	}
	e.promotionCarryRenamed(ctx, path, renamed)
	full := reads.of(path)
	if err := e.verifyPromotionHash(ctx, renamed, sum); err != nil || reads.of(renamed) != 0 {
		t.Fatalf("renamed file re-read: %d %v", reads.of(renamed), err)
	}
	// A rename does not carry a digest onto a different file.
	other := path + ".other"
	if err := os.WriteFile(other, []byte(strings.Repeat("media-bytes-", 1000)), 0600); err != nil {
		t.Fatal(err)
	}
	e.promotionCarryRenamed(ctx, renamed, other)
	if err := e.verifyPromotionHash(ctx, other, sum); err != nil || reads.of(other) != full {
		t.Fatalf("a different inode inherited a digest: %d %v", reads.of(other), err)
	}
}

// promotionPasses runs a whole approved promotion and returns how many times
// this process read each file in full (copy streaming included).
func promotionPasses(t *testing.T, clone bool) (original, candidate float64, p *promotionState) {
	t.Helper()
	h := newPromotionHarness(t)
	reads := countReads(h.engine)
	if clone {
		h.engine.promotionCloneHook = func(d, s *os.File) error { _, err := io.Copy(d, s); return err }
	}
	r := h.run()
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %s %s", r.Status, r.Error)
	}
	if r = h.finish(h.resume(r.ID, "approve")); r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	inst, err := h.st.GetActionInstance(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err = loadPromotion(parseExecutionContext(inst, h.engine))
	if err != nil {
		t.Fatal(err)
	}
	reads.mu.Lock()
	defer reads.mu.Unlock()
	var originalBytes, candidateBytes int64
	for path, n := range reads.bytes {
		switch {
		case path == p.OriginalPath || strings.HasPrefix(path, p.BackupPath):
			originalBytes += n
		default:
			candidateBytes += n
		}
	}
	return float64(originalBytes) / float64(p.OriginalBytes), float64(candidateBytes) / float64(p.CandidateBytes), p
}

func TestPromotionReadsEachFileFewerTimes(t *testing.T) {
	original, candidate, p := promotionPasses(t, true)
	t.Logf("full passes with a clone: original-sized %.0f, candidate-sized %.0f", original, candidate)
	// Plan hashes the original; approval is a wait, so preserve hashes it
	// again and reads the recovery copy back once. The harness checks the copy
	// itself before it accepts the import (not a production read). The wait for
	// the DELETE command forces a full hash of the copy before the rename, and
	// finalization reads it once more before removing it. Before fingerprints
	// this was ten production passes plus the streamed copy.
	if original > 6 {
		t.Fatalf("original-sized files read %.0f times", original)
	}
	if p.RecoveryCopy == nil || p.RecoveryCopy.Method != promotionCopyClone {
		t.Fatalf("recovery copy method: %+v", p.RecoveryCopy)
	}
	// Without a clone, Linux copies in the kernel (no read by this process)
	// and other platforms stream the copy through it (one more read).
	streamed, _, p := promotionPasses(t, false)
	t.Logf("full passes without a clone: original-sized %.0f, %+v", streamed, p.RecoveryCopy)
	want := original + 1
	if p.RecoveryCopy.Method == promotionCopyKernel {
		want = original
	}
	if streamed != want || p.RecoveryCopy.Method != promotionCopyStream && p.RecoveryCopy.Method != promotionCopyKernel {
		t.Fatalf("copy without a clone: %.0f passes, want %.0f, %+v", streamed, want, p.RecoveryCopy)
	}
}

func TestPromotionHashesFullyAfterApprovalAndRestart(t *testing.T) {
	h := newPromotionHarness(t)
	reads := countReads(h.engine)
	r := h.run()
	planned := reads.of(h.original)
	if planned == 0 {
		t.Fatal("plan did not hash the original")
	}
	// Approval is a wait: preserve must read the whole original again.
	h.restart()
	reads = countReads(h.engine)
	if r = h.finish(h.resume(r.ID, "approve")); r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	if reads.of(h.original) < planned {
		t.Fatalf("original read %d bytes after approval, want a full %d", reads.of(h.original), planned)
	}
}
