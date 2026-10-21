package action

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points the process temp dir at a private directory removed after
// the run. Tests here drive the real worker, which keeps its tone map LUT cache
// under os.TempDir() with no per-test owner to clean it.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "action-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("TMPDIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
