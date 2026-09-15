//go:build darwin

package transcodeworker

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestRenameNoReplaceDarwin verifies that the Darwin renamex_np(RENAME_EXCL)
// primitive is actually available on the test filesystem: it must rename,
// consume the source, and fail with fs.ErrExist without clobbering an existing
// destination. A fallback to errNoReplaceUnsupported fails this test on
// purpose, so we know the native no-replace path is live.
func TestRenameNoReplaceDarwin(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")
	if err := os.WriteFile(src, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := renameNoReplace(src, dst); err != nil {
		t.Fatalf("renameNoReplace: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source should be gone after rename, stat err = %v", err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "data" {
		t.Fatalf("dst = %q (%v), want %q", got, err, "data")
	}

	src2 := filepath.Join(base, "src2")
	if err := os.WriteFile(src2, []byte("new bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(src2, dst); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("renameNoReplace over existing = %v, want fs.ErrExist", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "data" {
		t.Fatalf("existing dst was clobbered: %q", got)
	}
	if _, err := os.Stat(src2); err != nil {
		t.Fatalf("src2 must survive a failed no-replace rename: %v", err)
	}
}
