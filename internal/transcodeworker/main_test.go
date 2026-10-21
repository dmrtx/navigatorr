package transcodeworker

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain keeps the tone map LUT cache out of the shared temp dir. Tests that
// build compact plans write LUTs through the default location unless they
// override it, and a package-level directory has no *testing.T to clean it.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "transcodeworker-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	compactLUTDir = func() string { return filepath.Join(dir, "compact-lut") }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
