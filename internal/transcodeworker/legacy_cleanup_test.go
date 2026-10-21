package transcodeworker

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func legacyCleanupWorker(t *testing.T) (*Worker, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := pr6b2Config(dir, nil)
	return NewWorker(cfg), dir
}

func legacyWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	return &buf
}

// A state dir written by a build that still ran sample tests starts cleanly:
// the retired job dirs and the sample cache are removed, a transcode job and
// the source cache beside them are not, and the removal is logged once.
func TestStartupRemovesRetiredSampleTestState(t *testing.T) {
	w, _ := legacyCleanupWorker(t)
	state, work := w.cfg.StateDir, w.localWorkDir()
	legacyWrite(t, filepath.Join(state, "bench-done-1", "benchmark.json"), `{"id":"bench-done-1","status":"completed","pid":0}`)
	legacyWrite(t, filepath.Join(state, "bench-done-1", "samples", "ref_sample_0.mkv"), "x")
	legacyWrite(t, filepath.Join(state, "bench-corrupt-1", "benchmark.json"), `{not json`)
	legacyWrite(t, filepath.Join(state, "bench-empty-1", "placeholder"), "")
	legacyWrite(t, filepath.Join(work, "_sample-cache", "entry.json"), "{}")
	legacyWrite(t, filepath.Join(work, "_source-cache", "keep.bin"), "source bytes")
	pr6b2Seed(t, state, &JobRecord{ID: "job-keep", Status: "completed", Source: "/media/a.mkv", Candidate: "/media/b.mkv", ExecutionSpecDigest: pr5Digest, FinishedAt: time.Now().UTC()})
	buf := captureLog(t)

	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("startup failed on retired state: %v", err)
	}
	for _, gone := range []string{
		filepath.Join(state, "bench-done-1"),
		filepath.Join(state, "bench-corrupt-1"),
		filepath.Join(state, "bench-empty-1"),
		filepath.Join(work, "_sample-cache"),
	} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Errorf("%s was not removed: %v", filepath.Base(gone), err)
		}
	}
	for _, kept := range []string{
		filepath.Join(work, "_source-cache", "keep.bin"),
		filepath.Join(state, "job-keep", "job.json"),
	} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(kept), err)
		}
	}
	if n := strings.Count(buf.String(), "retired sample test state"); n != 1 {
		t.Errorf("want one log line, got %d:\n%s", n, buf.String())
	}

	// A second start finds nothing and stays quiet.
	buf.Reset()
	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("second start logged: %s", buf.String())
	}
}

// A sample test whose runner is still alive is left alone, a symlink named like
// a retired dir is never followed, and a dir holding a transcode job is never
// treated as sample test state.
func TestStartupLeavesLiveAndForeignDirsAlone(t *testing.T) {
	w, dir := legacyCleanupWorker(t)
	state := w.cfg.StateDir
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Skip("cannot start a helper process")
	}
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
	legacyWrite(t, filepath.Join(state, "bench-live-1", "benchmark.json"), `{"id":"bench-live-1","status":"running","pid":`+strconv.Itoa(sleeper.Process.Pid)+`}`)
	outside := filepath.Join(dir, "outside")
	legacyWrite(t, filepath.Join(outside, "precious.txt"), "do not touch")
	if err := os.Symlink(outside, filepath.Join(state, "bench-link-1")); err != nil {
		t.Fatal(err)
	}
	legacyWrite(t, filepath.Join(state, "bench-has-job", "job.json"), `{}`)
	captureLog(t)

	w.removeLegacySampleTestState()
	for _, kept := range []string{
		filepath.Join(state, "bench-live-1", "benchmark.json"),
		filepath.Join(outside, "precious.txt"),
		filepath.Join(state, "bench-has-job", "job.json"),
	} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(kept), err)
		}
	}
}

func TestStartupWithoutRetiredStateIsSilent(t *testing.T) {
	w, _ := legacyCleanupWorker(t)
	buf := captureLog(t)
	w.removeLegacySampleTestState()
	if buf.Len() != 0 {
		t.Fatalf("logged without anything to remove: %s", buf.String())
	}
	blank := NewWorker(&WorkerConfig{})
	blank.removeLegacySampleTestState()
}

// A worker config written while the sample cache existed still loads: the
// retired keys are ignored, so an upgrade needs no config change.
func TestLoadWorkerConfigIgnoresRetiredSampleCacheKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.yml")
	legacyWrite(t, path, "disable_sample_cache: false\nsample_cache_max_bytes: 67108864\nsample_cache_ttl_hours: 720\nmax_parallel_jobs: 2\n")
	cfg, err := LoadWorkerConfig(path)
	if err != nil {
		t.Fatalf("config with retired keys rejected: %v", err)
	}
	if cfg.MaxParallelJobs != 2 {
		t.Fatalf("other keys must still apply: %+v", cfg)
	}
}
