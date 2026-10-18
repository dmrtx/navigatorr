package action

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func copyFixture(t *testing.T, content []byte) (src, dst *os.File, partial string) {
	t.Helper()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "original.mkv")
	if err := os.WriteFile(srcPath, content, 0600); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	partial = filepath.Join(dir, "original.bak.partial")
	dst, err = os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close(); _ = dst.Close() })
	return src, dst, partial
}

func assertCopied(t *testing.T, partial string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(partial)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("copy has %d bytes, want the %d original bytes", len(got), len(want))
	}
}

// kernelCopy emulates copy_file_range: both offsets advance by what it copies.
func kernelCopy(calls *int) func(dst, src *os.File, n int) (int, error) {
	return func(dst, src *os.File, n int) (int, error) {
		*calls++
		copied, err := io.CopyN(dst, src, int64(min(n, 1000)))
		if err == io.EOF {
			err = nil
		}
		return int(copied), err
	}
}

func TestPromotionCopyPrefersCloneThenKernelThenStream(t *testing.T) {
	content := []byte(strings.Repeat("recovery-bytes-", 400))
	refuse := func(*os.File, *os.File) error { return syscall.EOPNOTSUPP }

	t.Run("clone", func(t *testing.T) {
		src, dst, partial := copyFixture(t, content)
		e := NewEngine(EngineDeps{})
		e.promotionCloneHook = func(d, s *os.File) error { _, err := io.Copy(d, s); return err }
		e.promotionCopyRangeHook = func(*os.File, *os.File, int) (int, error) {
			t.Fatal("a successful clone must not be followed by a copy")
			return 0, nil
		}
		var reported int64
		result, err := e.promotionCopyData(context.Background(), dst, src, partial, int64(len(content)), func(_ string, read, _ int64) error { reported = read; return nil })
		if err != nil || result.Method != promotionCopyClone || result.CloneError != "" || result.Streamed != 0 {
			t.Fatalf("clone: %+v %v", result, err)
		}
		if reported != int64(len(content)) {
			t.Fatalf("clone reported %d bytes, want %d", reported, len(content))
		}
		assertCopied(t, partial, content)
	})

	t.Run("kernel copy after a refused clone", func(t *testing.T) {
		src, dst, partial := copyFixture(t, content)
		e := NewEngine(EngineDeps{})
		e.promotionCloneHook = refuse
		calls := 0
		e.promotionCopyRangeHook = kernelCopy(&calls)
		var reports []int64
		result, err := e.promotionCopyData(context.Background(), dst, src, partial, int64(len(content)), func(_ string, read, _ int64) error { reports = append(reports, read); return nil })
		if err != nil || result.Method != promotionCopyKernel || result.Streamed != 0 || !strings.Contains(result.CloneError, "not supported") {
			t.Fatalf("kernel copy: %+v %v", result, err)
		}
		if calls < 2 || len(reports) < 2 || reports[len(reports)-1] != int64(len(content)) {
			t.Fatalf("kernel copy was not chunked with progress: calls=%d reports=%v", calls, reports)
		}
		assertCopied(t, partial, content)
	})

	for _, unsupported := range []error{syscall.EXDEV, syscall.ENOSYS, syscall.EOPNOTSUPP, syscall.EINVAL} {
		t.Run("stream after "+unsupported.Error(), func(t *testing.T) {
			src, dst, partial := copyFixture(t, content)
			e := NewEngine(EngineDeps{})
			e.promotionCloneHook = refuse
			e.promotionCopyRangeHook = func(*os.File, *os.File, int) (int, error) { return 0, unsupported }
			result, err := e.promotionCopyData(context.Background(), dst, src, partial, int64(len(content)), nil)
			if err != nil || result.Method != promotionCopyStream || result.Streamed != int64(len(content)) {
				t.Fatalf("fallback: %+v %v", result, err)
			}
			assertCopied(t, partial, content)
		})
	}

	t.Run("stream resumes a kernel copy that stops being supported", func(t *testing.T) {
		src, dst, partial := copyFixture(t, content)
		e := NewEngine(EngineDeps{})
		e.promotionCloneHook = refuse
		calls := 0
		inner := kernelCopy(&calls)
		e.promotionCopyRangeHook = func(d, s *os.File, n int) (int, error) {
			if calls == 2 {
				return 0, syscall.EXDEV
			}
			return inner(d, s, n)
		}
		result, err := e.promotionCopyData(context.Background(), dst, src, partial, int64(len(content)), nil)
		if err != nil || result.Method != promotionCopyStream || result.Streamed != int64(len(content))-2000 {
			t.Fatalf("mixed copy: %+v %v", result, err)
		}
		assertCopied(t, partial, content)
	})

	t.Run("a silent refusal streams", func(t *testing.T) {
		src, dst, partial := copyFixture(t, content)
		e := NewEngine(EngineDeps{})
		e.promotionCloneHook = refuse
		e.promotionCopyRangeHook = func(*os.File, *os.File, int) (int, error) { return 0, nil }
		result, err := e.promotionCopyData(context.Background(), dst, src, partial, int64(len(content)), nil)
		if err != nil || result.Method != promotionCopyStream {
			t.Fatalf("silent refusal: %+v %v", result, err)
		}
		assertCopied(t, partial, content)
	})

	t.Run("a real copy error fails instead of streaming", func(t *testing.T) {
		src, dst, partial := copyFixture(t, content)
		e := NewEngine(EngineDeps{})
		e.promotionCloneHook = refuse
		e.promotionCopyRangeHook = func(*os.File, *os.File, int) (int, error) { return 0, syscall.EIO }
		if result, err := e.promotionCopyData(context.Background(), dst, src, partial, int64(len(content)), nil); !errors.Is(err, syscall.EIO) {
			t.Fatalf("EIO was not reported: %+v %v", result, err)
		}
	})

	t.Run("a refused clone that wrote anything fails", func(t *testing.T) {
		src, dst, partial := copyFixture(t, content)
		e := NewEngine(EngineDeps{})
		e.promotionCloneHook = func(d, _ *os.File) error { _, _ = d.Write([]byte("x")); return syscall.EIO }
		if _, err := e.promotionCopyData(context.Background(), dst, src, partial, int64(len(content)), nil); err == nil || !strings.Contains(err.Error(), "not empty") {
			t.Fatalf("dirty partial was trusted: %v", err)
		}
	})
}

func TestPromotionCopyStopsBetweenChunksWhenCancelled(t *testing.T) {
	content := []byte(strings.Repeat("recovery-bytes-", 400))
	src, dst, partial := copyFixture(t, content)
	e := NewEngine(EngineDeps{})
	e.promotionCloneHook = func(*os.File, *os.File) error { return syscall.EOPNOTSUPP }
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	inner := kernelCopy(&calls)
	e.promotionCopyRangeHook = func(d, s *os.File, n int) (int, error) {
		defer cancel()
		return inner(d, s, n)
	}
	if _, err := e.promotionCopyData(ctx, dst, src, partial, int64(len(content)), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled copy returned %v", err)
	}
	if calls != 1 {
		t.Fatalf("copy continued after cancellation: %d chunks", calls)
	}
	if info, err := os.Stat(partial); err != nil || info.Size() >= int64(len(content)) {
		t.Fatalf("cancelled partial: %v %v", info, err)
	}
}

// The real platform path: FICLONE or copy_file_range on Linux, whichever the
// test filesystem supports, and streaming elsewhere. The bytes always match.
func TestPromotionCopyPlatformPath(t *testing.T) {
	content := []byte(strings.Repeat("recovery-bytes-", 100000))
	src, dst, partial := copyFixture(t, content)
	result, err := NewEngine(EngineDeps{}).promotionCopyData(context.Background(), dst, src, partial, int64(len(content)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" && result.Method != promotionCopyStream {
		t.Fatalf("%s copy used %s", runtime.GOOS, result.Method)
	}
	t.Logf("platform copy: %+v", result)
	assertCopied(t, partial, content)
}

func TestPromotionPreserveRecordsCopyMethod(t *testing.T) {
	h := newPromotionHarness(t)
	ec := approvedPreserve(t, h)
	h.engine.promotionCloneHook = func(d, s *os.File) error { _, err := io.Copy(d, s); return err }
	step, err := h.engine.stepPromotePreserve(context.Background(), ec)
	if err != nil || step.Status != StepCompleted {
		t.Fatalf("preserve: %+v %v", step, err)
	}
	p, err := loadPromotion(ec)
	if err != nil {
		t.Fatal(err)
	}
	if p.RecoveryCopy == nil || p.RecoveryCopy.Method != promotionCopyClone {
		t.Fatalf("copy method not recorded: %+v", p.RecoveryCopy)
	}
	if err := h.engine.verifyPromotionHash(context.Background(), p.BackupPath, p.OriginalSHA); err != nil {
		t.Fatal(err)
	}
}
