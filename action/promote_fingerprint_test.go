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

type passMode int

const (
	passMoveAside passMode = iota
	passCopyClone
	passCopyStream
)

// promotionPasses runs a whole approved promotion and returns how many times
// this process read each file in full (copy streaming included). The harness
// verifies recovery itself outside this accounting.
func promotionPasses(t *testing.T, mode passMode, local bool) (original, candidate float64, p *promotionState) {
	t.Helper()
	h := newPromotionHarness(t)
	reads := countReads(h.engine)
	h.engine.promotionAlwaysCopy = mode != passMoveAside
	if mode == passCopyClone {
		h.engine.promotionCloneHook = func(d, s *os.File) error { _, err := io.Copy(d, s); return err }
	}
	var r *ActionResult
	if local {
		r = runLocalPromotion(t, h)
	} else {
		r = h.run()
	}
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %s %s", r.Status, r.Error)
	}
	if r = h.finish(h.resume(r.ID, "approve")); r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	if testing.Verbose() {
		reads.mu.Lock()
		for path, n := range reads.bytes {
			t.Logf("read %s: %d", filepath.Base(path), n)
		}
		reads.mu.Unlock()
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
	// Before fingerprints and move-aside, a promotion read the original ten
	// times and the candidate nine, and wrote a full copy of the original.
	// Plan stats the original only. Approval is a wait, so preserve reads the
	// original and candidate in full; the original then moves aside by
	// rename, proved by its fingerprint. Finalization reads the final file.
	// Sonarr and Radarr add one candidate read before the old record is
	// deleted, after the import wait.
	for _, tc := range []struct {
		name                string
		mode                passMode
		local               bool
		original, candidate float64
		method              string
	}{
		{"library/move-aside", passMoveAside, false, 1, 3, ""},
		{"filesystem/move-aside", passMoveAside, true, 1, 2, ""},
		// The copy fallback adds the copy's read-back, plus the streamed
		// copy itself when the filesystem cannot clone and the kernel does
		// not copy it (Linux copy_file_range is not read by this process).
		{"library/copy-clone", passCopyClone, false, 2, 3, promotionCopyClone},
		{"library/copy-no-clone", passCopyStream, false, 3, 3, ""},
		{"filesystem/copy-no-clone", passCopyStream, true, 3, 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original, candidate, p := promotionPasses(t, tc.mode, tc.local)
			t.Logf("full passes: original-sized %.0f, candidate-sized %.0f, copy %+v", original, candidate, p.RecoveryCopy)
			if tc.mode == passCopyStream && p.RecoveryCopy != nil && p.RecoveryCopy.Method == promotionCopyKernel {
				tc.original--
			}
			if original != tc.original || candidate != tc.candidate {
				t.Fatalf("read original-sized %.2f times, candidate-sized %.2f; want %.0f and %.0f", original, candidate, tc.original, tc.candidate)
			}
			switch tc.mode {
			case passMoveAside:
				if p.RecoveryMode != promotionRecoveryMoveAside || !p.MovedAside || p.RecoveryCopy != nil {
					t.Fatalf("not moved aside: %+v", p)
				}
			default:
				if p.RecoveryMode != promotionRecoveryCopy || p.RecoveryCopy == nil || tc.mode == passCopyStream && p.RecoveryCopy.Method != promotionCopyStream && p.RecoveryCopy.Method != promotionCopyKernel {
					t.Fatalf("no recovery copy: %+v", p)
				}
				if tc.method != "" && p.RecoveryCopy.Method != tc.method {
					t.Fatalf("recovery copy method: %+v", p.RecoveryCopy)
				}
			}
		})
	}
}

func TestPromotionHashesFullyAfterApprovalAndRestart(t *testing.T) {
	h := newPromotionHarness(t)
	reads := countReads(h.engine)
	r := h.run()
	if reads.of(h.original) != 0 || reads.of(h.candidate) == 0 {
		t.Fatalf("plan read original %d, candidate %d bytes; want a stat and a full hash", reads.of(h.original), reads.of(h.candidate))
	}
	// Approval is a wait: preserve must read both files in full again.
	h.restart()
	reads = countReads(h.engine)
	if r = h.finish(h.resume(r.ID, "approve")); r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	if reads.of(h.original) < p.OriginalBytes || reads.of(h.candidate) < p.CandidateBytes {
		t.Fatalf("after approval read original %d of %d, candidate %d of %d", reads.of(h.original), p.OriginalBytes, reads.of(h.candidate), p.CandidateBytes)
	}
}
