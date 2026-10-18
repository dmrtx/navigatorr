//go:build linux || darwin

package action

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A file with no change time has no fingerprint, so it is always read.
func TestPromotionFingerprintNeedsAChangeTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "media.mkv")
	if err := os.WriteFile(path, []byte("bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := promotionFileFingerprint(info); !ok {
		t.Fatal("no fingerprint for a local file")
	}
	st := *info.Sys().(*syscall.Stat_t)
	zeroCtime(&st)
	if _, ok := promotionFileFingerprint(statInfo{info, &st}); ok {
		t.Fatal("fingerprint without a change time")
	}
}

type statInfo struct {
	os.FileInfo
	st *syscall.Stat_t
}

func (s statInfo) Sys() any { return s.st }
