package transcodeworker

// PR5 tests: terminal-marker primitives, cancellation-wins terminal
// persistence, and startup reconciliation. Deterministic: liveness is injected
// via SetAliveFunc and no real ffmpeg/process is spawned.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pr5Digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func pr5NewWorker(t *testing.T, alive func(*JobRecord) bool) (*Worker, string) {
	t.Helper()
	stateDir := t.TempDir()
	w := NewWorker(&WorkerConfig{StateDir: stateDir, MaxParallelJobs: 1})
	if alive != nil {
		w.SetAliveFunc(alive)
	}
	return w, stateDir
}

func pr5JobDir(stateDir, id string) string     { return filepath.Join(stateDir, id) }
func pr5JobFile(stateDir, id string) string    { return filepath.Join(stateDir, id, "job.json") }
func pr5MarkerPath(stateDir, id string) string { return filepath.Join(stateDir, id, "terminal.json") }

func pr5RunningJob(id, digest string) *JobRecord {
	return &JobRecord{
		ID:                  id,
		Status:              "running",
		ExecutionSpecDigest: digest,
		PID:                 1<<30 + 1,
		CreatedAt:           time.Now().UTC(),
		StartedAt:           time.Now().UTC(),
	}
}

func pr5Marker(id, digest, status string) *TerminalMarker {
	return &TerminalMarker{
		JobID:               id,
		ExecutionSpecDigest: digest,
		Status:              status,
		FinishedAt:          time.Now().UTC(),
	}
}

func pr5SaveJob(t *testing.T, stateDir string, job *JobRecord) {
	t.Helper()
	if err := SaveJobAtomic(pr5JobFile(stateDir, job.ID), job); err != nil {
		t.Fatalf("save job: %v", err)
	}
}

func pr5LoadJob(t *testing.T, stateDir, id string) *JobRecord {
	t.Helper()
	job, err := LoadJob(pr5JobFile(stateDir, id))
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	return job
}

func pr5LoadMarker(t *testing.T, stateDir, id string) *TerminalMarker {
	t.Helper()
	marker, err := LoadTerminalMarker(pr5MarkerPath(stateDir, id))
	if err != nil {
		t.Fatalf("load marker: %v", err)
	}
	return marker
}

func pr5NeverAlive(*JobRecord) bool  { return false }
func pr5AlwaysAlive(*JobRecord) bool { return true }

func TestPR5_ValidMarkerAppliesCompleted(t *testing.T) {
	w, stateDir := pr5NewWorker(t, pr5AlwaysAlive)
	const id = "job-pr5-1"
	pr5SaveJob(t, stateDir, pr5RunningJob(id, pr5Digest))
	if err := SaveTerminalMarkerAtomic(pr5MarkerPath(stateDir, id), pr5Marker(id, pr5Digest, "completed")); err != nil {
		t.Fatal(err)
	}
	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	job := pr5LoadJob(t, stateDir, id)
	if job.Status != "completed" {
		t.Fatalf("expected completed from valid marker, got %+v", job)
	}
	if job.FinishedAt.IsZero() {
		t.Fatalf("expected finished_at copied from marker, got zero")
	}
}

func TestPR5_CorruptMarkerDeadBecomesFailedRunnerKilled(t *testing.T) {
	w, stateDir := pr5NewWorker(t, pr5NeverAlive)
	const id = "job-pr5-2"
	pr5SaveJob(t, stateDir, pr5RunningJob(id, pr5Digest))
	if err := os.WriteFile(pr5MarkerPath(stateDir, id), []byte("{not-json"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("corrupt marker is invalid evidence, not fatal: %v", err)
	}
	job := pr5LoadJob(t, stateDir, id)
	if job.Status != "failed" || job.FailureClassification != "runner_killed" || job.Error != "process terminated unexpectedly" {
		t.Fatalf("expected failed/runner_killed, got %+v", job)
	}
	if job.FinishedAt.IsZero() {
		t.Fatalf("expected finished_at set")
	}
	marker := pr5LoadMarker(t, stateDir, id)
	if marker.Status != "failed" || marker.JobID != id || marker.ExecutionSpecDigest != pr5Digest {
		t.Fatalf("expected valid failed marker, got %+v", marker)
	}
}

func TestPR5_MismatchedMarkerJobIDDeadBecomesFailed(t *testing.T) {
	w, stateDir := pr5NewWorker(t, pr5NeverAlive)
	const id = "job-pr5-3"
	pr5SaveJob(t, stateDir, pr5RunningJob(id, pr5Digest))
	if err := SaveTerminalMarkerAtomic(pr5MarkerPath(stateDir, id), pr5Marker("other-job", pr5Digest, "completed")); err != nil {
		t.Fatal(err)
	}
	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("mismatched marker is invalid evidence, not fatal: %v", err)
	}
	job := pr5LoadJob(t, stateDir, id)
	if job.Status != "failed" || job.FailureClassification != "runner_killed" {
		t.Fatalf("expected failed/runner_killed, got %+v", job)
	}
}

func TestPR5_MismatchedMarkerDigestDeadBecomesFailed(t *testing.T) {
	w, stateDir := pr5NewWorker(t, pr5NeverAlive)
	const id = "job-pr5-4"
	const otherDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	pr5SaveJob(t, stateDir, pr5RunningJob(id, pr5Digest))
	if err := SaveTerminalMarkerAtomic(pr5MarkerPath(stateDir, id), pr5Marker(id, otherDigest, "completed")); err != nil {
		t.Fatal(err)
	}
	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("mismatched marker is invalid evidence, not fatal: %v", err)
	}
	job := pr5LoadJob(t, stateDir, id)
	if job.Status != "failed" || job.FailureClassification != "runner_killed" {
		t.Fatalf("expected failed/runner_killed, got %+v", job)
	}
}

func TestPR5_AliveRunningRemainsRunning(t *testing.T) {
	w, stateDir := pr5NewWorker(t, pr5AlwaysAlive)
	const id = "job-pr5-5"
	pr5SaveJob(t, stateDir, pr5RunningJob(id, pr5Digest))
	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	job := pr5LoadJob(t, stateDir, id)
	if job.Status != "running" {
		t.Fatalf("expected alive running to stay running, got %+v", job)
	}
	if _, err := os.Stat(pr5MarkerPath(stateDir, id)); !os.IsNotExist(err) {
		t.Fatalf("alive running job must not gain a terminal marker")
	}
}

func TestPR5_DeadNoMarkerBecomesFailedWithMarker(t *testing.T) {
	w, stateDir := pr5NewWorker(t, pr5NeverAlive)
	const id = "job-pr5-6"
	pr5SaveJob(t, stateDir, pr5RunningJob(id, pr5Digest))
	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	job := pr5LoadJob(t, stateDir, id)
	if job.Status != "failed" || job.FailureClassification != "runner_killed" || job.Error != "process terminated unexpectedly" {
		t.Fatalf("expected failed/runner_killed, got %+v", job)
	}
	if job.FinishedAt.IsZero() {
		t.Fatalf("expected finished_at set")
	}
	marker := pr5LoadMarker(t, stateDir, id)
	if marker.Status != "failed" || marker.JobID != id || marker.ExecutionSpecDigest != pr5Digest || marker.FailureClassification != "runner_killed" {
		t.Fatalf("expected valid failed marker, got %+v", marker)
	}
}

func TestPR5_CancelledRecordWithCompletedMarkerRemainsCancelled(t *testing.T) {
	w, stateDir := pr5NewWorker(t, pr5AlwaysAlive)
	const id = "job-pr5-7"
	job := pr5RunningJob(id, pr5Digest)
	job.Status = "cancelled"
	job.FinishedAt = time.Now().UTC()
	job.Error = "cancelled by user"
	pr5SaveJob(t, stateDir, job)
	if err := SaveTerminalMarkerAtomic(pr5MarkerPath(stateDir, id), pr5Marker(id, pr5Digest, "completed")); err != nil {
		t.Fatal(err)
	}
	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := pr5LoadJob(t, stateDir, id)
	if got.Status != "cancelled" || got.Error != "cancelled by user" {
		t.Fatalf("cancelled record must remain cancelled, got %+v", got)
	}
}

func TestPR5_BenchmarkOnlyNoJobDirSkipped(t *testing.T) {
	w, stateDir := pr5NewWorker(t, pr5NeverAlive)
	benchDir := filepath.Join(stateDir, "bench-only")
	if err := os.MkdirAll(benchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(benchDir, "benchmark.json"), []byte(`{"id":"bench-only"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("benchmark-only directory must be skipped, got %v", err)
	}
}

func TestPR5_TerminalMarkerRejectsZeroFinishedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "terminal.json")

	saveMarker := &TerminalMarker{JobID: "job-z", ExecutionSpecDigest: pr5Digest, Status: "completed"}
	if err := SaveTerminalMarkerAtomic(path, saveMarker); err == nil {
		t.Fatal("save must reject zero finished_at")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("rejected marker must not create a file")
	}

	raw := `{"job_id":"job-z","execution_spec_digest":"` + pr5Digest + `","status":"completed"}`
	if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTerminalMarker(path); err == nil {
		t.Fatal("load must reject zero finished_at")
	}
}

func TestPR5_JobIDDirectoryMismatchFailsClosed(t *testing.T) {
	w, stateDir := pr5NewWorker(t, pr5AlwaysAlive)
	job := pr5RunningJob("job-real", pr5Digest)
	if err := SaveJobAtomic(filepath.Join(stateDir, "job-dir", "job.json"), job); err != nil {
		t.Fatal(err)
	}
	if err := w.ReconcileStartup(context.Background()); err == nil {
		t.Fatal("job id/directory mismatch must fail closed")
	}
}

func TestPR5_CancelQueuedPID0WritesCancelledJobThenMarker(t *testing.T) {
	w, stateDir := pr5NewWorker(t, pr5NeverAlive)
	const id = "job-pr5-11"
	job := &JobRecord{
		ID: id, Status: "queued", ExecutionSpecDigest: pr5Digest,
		Candidate: filepath.Join(stateDir, "out.mkv"), CreatedAt: time.Now().UTC(),
	}
	pr5SaveJob(t, stateDir, job)

	resp, err := w.Cancel(context.Background(), id)
	if err != nil || resp.Status != "cancelled" {
		t.Fatalf("queued cancel failed: %+v err %v", resp, err)
	}
	got := pr5LoadJob(t, stateDir, id)
	if got.Status != "cancelled" || got.FinishedAt.IsZero() {
		t.Fatalf("expected cancelled job with finished_at, got %+v", got)
	}
	marker := pr5LoadMarker(t, stateDir, id)
	if marker.Status != "cancelled" || marker.JobID != id || marker.ExecutionSpecDigest != pr5Digest || marker.FinishedAt.IsZero() {
		t.Fatalf("expected valid cancelled marker, got %+v", marker)
	}
}

func TestPR5_ReconcileStartupHonorsCancelledContext(t *testing.T) {
	w, stateDir := pr5NewWorker(t, pr5NeverAlive)
	const id = "job-pr5-ctx"
	pr5SaveJob(t, stateDir, pr5RunningJob(id, pr5Digest))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := w.ReconcileStartup(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// A cancelled reconciliation must not mutate the persisted record.
	job := pr5LoadJob(t, stateDir, id)
	if job.Status != "running" {
		t.Fatalf("cancelled reconcile must not mutate job, got %+v", job)
	}
}

func TestPR5_ReadFileTailReturnsBoundedSuffix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.log")
	content := []byte("0123456789abcdefghij")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	got, err := readFileTail(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fghij" {
		t.Fatalf("expected bounded suffix %q, got %q", "fghij", got)
	}
	all, err := readFileTail(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(all) != string(content) {
		t.Fatalf("expected whole file, got %q", all)
	}
	if _, err := readFileTail(path, 0); err == nil {
		t.Fatal("maxBytes=0 must be rejected")
	}
	if _, err := readFileTail(filepath.Join(t.TempDir(), "missing.log"), 5); err == nil {
		t.Fatal("missing file must error")
	}
}

func TestPR5_SummarizeFFmpegErrorIgnoresHistoricalLogPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ffmpeg.log")
	// A historical SpatialAQ failure sits well outside the final 8192-byte
	// tail; a durable append log must not let it classify the current run.
	historical := "historical error: VideoToolbox spatial_aq unsupported by device\n"
	pad := strings.Repeat("x", 9000) + "\n"
	morePad := strings.Repeat("y", 9000) + "\n"
	current := "current-run error: encoder initialization failed\n"
	if err := os.WriteFile(path, []byte(historical+pad+morePad+current), 0644); err != nil {
		t.Fatal(err)
	}

	summary := SummarizeFFmpegError(path, errors.New("exit status 1"))
	if !strings.Contains(summary, "current-run") {
		t.Fatalf("summary must come from the current tail, got %q", summary)
	}
	if strings.Contains(summary, "historical") {
		t.Fatalf("historical log prefix must not leak into the summary, got %q", summary)
	}
}

func TestPR5_PersistedJobLogSurvivesNewWorkerAndTailBounded(t *testing.T) {
	w1, stateDir := pr5NewWorker(t, nil)
	const id = "job-pr5-log"
	payload := []byte("0123456789abcdefghij")
	f, err := w1.openJobLog(pr5JobDir(stateDir, id), "runner.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh Worker over the same StateDir must see the same persisted log.
	w2 := NewWorker(&WorkerConfig{StateDir: stateDir, MaxParallelJobs: 1})
	tail, err := w2.ReadJobLogTail(id, "runner.log", 8)
	if err != nil {
		t.Fatal(err)
	}
	if string(tail) != "cdefghij" {
		t.Fatalf("expected bounded suffix %q, got %q", "cdefghij", tail)
	}
	all, err := w2.ReadJobLogTail(id, "runner.log", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(all) != string(payload) {
		t.Fatalf("expected full persisted log, got %q", all)
	}
}

func TestPR5_ReadJobLogTailRejectsInvalid(t *testing.T) {
	w, stateDir := pr5NewWorker(t, nil)
	const id = "job-pr5-log-reject"

	// ffmpeg.log is an allowed durable log.
	f, err := w.openJobLog(pr5JobDir(stateDir, id), "ffmpeg.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("ff")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if got, err := w.ReadJobLogTail(id, "ffmpeg.log", 4); err != nil || string(got) != "ff" {
		t.Fatalf("ffmpeg.log must be readable, got %q err %v", got, err)
	}

	if _, err := w.ReadJobLogTail(id, "secret.log", 4); err == nil {
		t.Fatal("invalid log name must be rejected")
	}
	if _, err := w.ReadJobLogTail("../evil", "runner.log", 4); err == nil {
		t.Fatal("traversal job id must be rejected")
	}
	if _, err := w.ReadJobLogTail("a/b", "runner.log", 4); err == nil {
		t.Fatal("job id containing a separator must be rejected")
	}
	if _, err := w.ReadJobLogTail(id, "runner.log", 0); err == nil {
		t.Fatal("maxBytes=0 must be rejected")
	}
	if _, err := w.ReadJobLogTail(id, "runner.log", -1); err == nil {
		t.Fatal("negative maxBytes must be rejected")
	}
	if _, err := w.ReadJobLogTail(id, "runner.log", 4); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing log must yield a wrapped os-not-exist error, got %v", err)
	}
}

func TestPR5_ReadJobLogTailCapsMaxBytes(t *testing.T) {
	w, stateDir := pr5NewWorker(t, nil)
	const id = "job-pr5-log-cap"
	big := make([]byte, maxJobLogTailBytes+1000)
	for i := range big {
		big[i] = byte('a' + (i % 26))
	}
	f, err := w.openJobLog(pr5JobDir(stateDir, id), "runner.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(big); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	tail, err := w.ReadJobLogTail(id, "runner.log", maxJobLogTailBytes*4)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(tail)) != maxJobLogTailBytes {
		t.Fatalf("expected hard cap %d bytes, got %d", maxJobLogTailBytes, len(tail))
	}
	offset := len(big) - int(maxJobLogTailBytes)
	if tail[0] != big[offset] {
		t.Fatalf("expected the tail of the log, got head byte %q", tail[0])
	}
}

func TestPR5_RunnerLogAppendPreservesChunks(t *testing.T) {
	w, stateDir := pr5NewWorker(t, nil)
	const id = "job-pr5-append"

	// Two separate opens/writes simulate a runner restart: O_APPEND must keep
	// both chunks, in order, without truncation.
	f1, err := w.openJobLog(pr5JobDir(stateDir, id), "runner.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f1.Write([]byte("chunk-1\n")); err != nil {
		t.Fatal(err)
	}
	_ = f1.Close()

	f2, err := w.openJobLog(pr5JobDir(stateDir, id), "runner.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.Write([]byte("chunk-2\n")); err != nil {
		t.Fatal(err)
	}
	_ = f2.Close()

	got, err := w.ReadJobLogTail(id, "runner.log", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "chunk-1\nchunk-2\n" {
		t.Fatalf("append must preserve both chunks in order, got %q", got)
	}
}

func TestPR5_StartupOrderReconcileBeforeSchedule(t *testing.T) {
	stub := newStubSpawn()
	stateDir := t.TempDir()
	w := NewWorker(&WorkerConfig{StateDir: stateDir, MaxParallelJobs: 8})
	stub.install(w)

	// A running job with a valid completed marker must be recovered as
	// completed and never (re)spawned by the startup sweep.
	const recoveredID = "job-pr5-recovered"
	const seededPID = 4242
	seed := pr5RunningJob(recoveredID, pr5Digest)
	seed.PID = seededPID
	pr5SaveJob(t, stateDir, seed)
	if err := SaveTerminalMarkerAtomic(pr5MarkerPath(stateDir, recoveredID), pr5Marker(recoveredID, pr5Digest, "completed")); err != nil {
		t.Fatal(err)
	}

	const queuedID = "job-pr5-queued"
	pr5SaveJob(t, stateDir, &JobRecord{
		ID: queuedID, Status: "queued", ExecutionSpecDigest: pr5Digest,
		CreatedAt: time.Now().UTC(),
	})

	srv := NewServer(w, "test-exe", "", "")
	stop, err := srv.StartSchedulerAfterReconcile(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("startup sequence failed: %v", err)
	}
	defer stop()

	recovered := pr5LoadJob(t, stateDir, recoveredID)
	if recovered.Status != "completed" {
		t.Fatalf("recovered job must become completed, got %+v", recovered)
	}
	if recovered.PID != seededPID {
		t.Fatalf("recovered completed job must not be re-spawned, PID=%d", recovered.PID)
	}
	queued := pr5LoadJob(t, stateDir, queuedID)
	if queued.PID <= 1 {
		t.Fatalf("queued job must be spawned, got %+v", queued)
	}
	if stub.n() != 1 {
		t.Fatalf("expected exactly one spawn (the queued job), got %d", stub.n())
	}
}

func TestPR5_ReconcileErrorPreventsScheduling(t *testing.T) {
	stub := newStubSpawn()
	stateDir := t.TempDir()
	w := NewWorker(&WorkerConfig{StateDir: stateDir, MaxParallelJobs: 8})
	stub.install(w)

	badDir := filepath.Join(stateDir, "job-pr5-bad")
	if err := os.MkdirAll(badDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "job.json"), []byte("{invalid json"), 0644); err != nil {
		t.Fatal(err)
	}
	const queuedID = "job-pr5-queued-err"
	pr5SaveJob(t, stateDir, &JobRecord{
		ID: queuedID, Status: "queued", ExecutionSpecDigest: pr5Digest,
		CreatedAt: time.Now().UTC(),
	})

	srv := NewServer(w, "test-exe", "", "")
	stop, err := srv.StartSchedulerAfterReconcile(context.Background(), time.Hour)
	if err == nil {
		if stop != nil {
			stop()
		}
		t.Fatal("reconciliation error must be returned")
	}
	if stop != nil {
		t.Fatal("scheduler must not start on reconciliation failure")
	}
	if stub.n() != 0 {
		t.Fatalf("no spawn allowed after failed reconciliation, got %d", stub.n())
	}
	queued := pr5LoadJob(t, stateDir, queuedID)
	if queued.Status != "queued" || queued.PID != 0 {
		t.Fatalf("queued job must remain unscheduled, got %+v", queued)
	}
}

func TestPR5_PersistTerminalCannotOverwriteCancelled(t *testing.T) {
	w, stateDir := pr5NewWorker(t, pr5NeverAlive)
	const id = "job-pr5-12"
	job := &JobRecord{
		ID: id, Status: "queued", ExecutionSpecDigest: pr5Digest,
		Candidate: filepath.Join(stateDir, "out.mkv"), CreatedAt: time.Now().UTC(),
	}
	pr5SaveJob(t, stateDir, job)
	if _, err := w.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}

	completed := &JobRecord{
		ID: id, Status: "completed", ExecutionSpecDigest: pr5Digest,
		FinishedAt: time.Now().UTC(), ExitCode: 0,
	}
	if err := w.persistTerminalJob(pr5JobDir(stateDir, id), pr5JobFile(stateDir, id), completed); err != nil {
		t.Fatalf("persistTerminalJob: %v", err)
	}
	got := pr5LoadJob(t, stateDir, id)
	if got.Status != "cancelled" {
		t.Fatalf("job must remain cancelled, got %+v", got)
	}
	marker := pr5LoadMarker(t, stateDir, id)
	if marker.Status != "cancelled" {
		t.Fatalf("marker must remain cancelled, got %+v", marker)
	}
}
