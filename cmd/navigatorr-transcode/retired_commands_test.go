package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The deployed server no longer runs sample tests, but an older caller may
// still invoke the retired subcommands: each gets an "unknown command" error
// and a non-zero exit, never a panic.
func TestRetiredBenchmarkSubcommandsAreRejected(t *testing.T) {
	if sub := os.Getenv("NAVIGATORR_TEST_RETIRED_SUBCOMMAND"); sub != "" {
		os.Args = []string{"navigatorr-transcode", "--config", os.Getenv("NAVIGATORR_TEST_CONFIG"), sub, "bench-old-1"}
		main()
		return
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "worker.yml")
	body := "state_dir: " + filepath.Join(dir, "jobs") + "\nallowed_roots:\n  - " + dir + "\n"
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"benchmark_submit", "benchmark_status", "benchmark_cancel", "_internal_benchmark"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRetiredBenchmarkSubcommandsAreRejected$")
		cmd.Env = append(os.Environ(), "NAVIGATORR_TEST_RETIRED_SUBCOMMAND="+sub, "NAVIGATORR_TEST_CONFIG="+config)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("%s: exited zero:\n%s", sub, out)
		}
		if !strings.Contains(string(out), `unknown command \"`+sub+`\"`) {
			t.Errorf("%s: no clear error:\n%s", sub, out)
		}
		if strings.Contains(string(out), "panic") {
			t.Errorf("%s: panicked:\n%s", sub, out)
		}
	}
}
