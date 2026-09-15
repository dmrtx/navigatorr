package transcodeworker

// PR5A tests: durable terminal markers + startup reconciliation. Deterministic
// via the injected SetAliveFunc from PR3 (no real ffmpeg, no real signals).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pr5Digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
const pr5OtherDigest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"

func pr5StateDir(tempDir string) string { return filepath.Join(tempDir, "jobs") }

func pr5SeedJob(t *testing.T, tempDir string, job *JobRecord) {
	t.Helper()
	if err := SaveJobAtomic(filepath.Join(pr5StateDir(tempDir), job.ID, "job.json"), job); err != nil {
		t.Fatal(err)
	}
}

func pr5RunningJob(id, digest string, pid int) *JobRecord {
	return &JobRecord{
		ID: id, Status: "running", PID: pid,
		Source: filepath.Join("src", "a.mkv"), Candidate: filepath.Join("out", id+".mkv"),
		ExecutionSpecDigest: digest, ProcessStartTime: "stub-start",
		CreatedAt: time.Now().UTC().Add(-time.Hour), StartedAt: time.Now().UTC().Add(-time.Minute),
		Attempt: 1,
	}
}

func pr5MarkerPath(tempDir, id string) string {
	return filepath.Join(pr5StateDir(tempDir), id, "terminal.json")
}

func TestPR5_ValidCompletedMarkerApplies(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	job := pr5RunningJob("job-pr5-completed", pr5Digest, 1<<30+1)
	pr5SeedJob(t, tempDir, job)

	finished := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	marker := &TerminalMarker{JobID: job.ID, ExecutionSpecDigest: pr5Digest, Status: "completed", FinishedAt: finished, ExitCode: 0}
	if err := SaveTerminalMarkerAtomic(pr5MarkerPath(tempDir, job.ID), marker); err != nil {
		t.Fatal(err)
	}

	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := LoadJob(filepath.Join(pr5StateDir(tempDir), job.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" || !got.FinishedAt.Equal(finished) || got.ExitCode != 0 {
		t.Fatalf("valid completed marker must apply terminal fields, got %+v", got)
	}
}

func TestPR5_CorruptMarkerDeadBecomesFailedAndMarkerReplaced(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	job := pr5RunningJob("job-pr5-corrupt", pr5Digest, 1<<30+2)
	pr5SeedJob(t, tempDir, job)
	markerPath := pr5MarkerPath(tempDir, job.ID)
	if err := os.WriteFile(markerPath, []byte("{not valid json"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := LoadJob(filepath.Join(pr5StateDir(tempDir), job.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.FailureClassification != "runner_killed" || got.Error != "process terminated unexpectedly" {
		t.Fatalf("corrupt marker + dead must fail runner_killed, got %+v", got)
	}
	marker, err := LoadTerminalMarker(markerPath)
	if err != nil {
		t.Fatalf("corrupt marker must be replaced by a valid failed marker: %v", err)
	}
	if marker.Status != "failed" || !terminalMarkerMatchesJob(got, marker) {
		t.Fatalf("replaced marker must match the failed job, got %+v", marker)
	}
}

func TestPR5_MismatchedMarkerJobIDDeadBecomesFailed(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	job := pr5RunningJob("job-pr5-idmismatch", pr5Digest, 1<<30+3)
	pr5SeedJob(t, tempDir, job)

	marker := &TerminalMarker{JobID: "job-pr5-intruder", ExecutionSpecDigest: pr5Digest, Status: "completed", FinishedAt: time.Now().UTC(), ExitCode: 0}
	if err := SaveTerminalMarkerAtomic(pr5MarkerPath(tempDir, job.ID), marker); err != nil {
		t.Fatal(err)
	}

	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := LoadJob(filepath.Join(pr5StateDir(tempDir), job.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.FailureClassification != "runner_killed" {
		t.Fatalf("mismatched marker job ID + dead must fail, got %+v", got)
	}
}

func TestPR5_MismatchedMarkerDigestDeadBecomesFailed(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	job := pr5RunningJob("job-pr5-digestmismatch", pr5Digest, 1<<30+4)
	pr5SeedJob(t, tempDir, job)

	marker := &TerminalMarker{JobID: job.ID, ExecutionSpecDigest: pr5OtherDigest, Status: "completed", FinishedAt: time.Now().UTC(), ExitCode: 0}
	if err := SaveTerminalMarkerAtomic(pr5MarkerPath(tempDir, job.ID), marker); err != nil {
		t.Fatal(err)
	}

	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := LoadJob(filepath.Join(pr5StateDir(tempDir), job.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.FailureClassification != "runner_killed" {
		t.Fatalf("mismatched marker digest + dead must fail, got %+v", got)
	}
}

func TestPR5_AliveRunningNoMarkerStaysRunningNoMarker(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	job := pr5RunningJob("job-pr5-alive", pr5Digest, stub.occupy("job-pr5-alive"))
	pr5SeedJob(t, tempDir, job)

	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := LoadJob(filepath.Join(pr5StateDir(tempDir), job.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" {
		t.Fatalf("alive running job must stay running, got %+v", got)
	}
	if _, err := os.Stat(pr5MarkerPath(tempDir, job.ID)); !os.IsNotExist(err) {
		t.Fatalf("alive running job must get no terminal marker")
	}
}

func TestPR5_DeadNoMarkerBecomesFailedWithMarker(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	job := pr5RunningJob("job-pr5-dead", pr5Digest, 1<<30+5)
	pr5SeedJob(t, tempDir, job)

	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := LoadJob(filepath.Join(pr5StateDir(tempDir), job.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.FailureClassification != "runner_killed" || got.Error != "process terminated unexpectedly" || got.FinishedAt.IsZero() {
		t.Fatalf("dead running job must fail runner_killed, got %+v", got)
	}
	marker, err := LoadTerminalMarker(pr5MarkerPath(tempDir, job.ID))
	if err != nil {
		t.Fatalf("dead running job must get a valid failed marker: %v", err)
	}
	if marker.Status != "failed" || !terminalMarkerMatchesJob(got, marker) {
		t.Fatalf("failed marker must match the job, got %+v", marker)
	}
}

func TestPR5_CancelledWithMisleadingCompletedMarkerStaysCancelled(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	job := &JobRecord{
		ID: "job-pr5-cancelled", Status: "cancelled", ExecutionSpecDigest: pr5Digest,
		FinishedAt: time.Now().UTC(), CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	pr5SeedJob(t, tempDir, job)

	marker := &TerminalMarker{JobID: job.ID, ExecutionSpecDigest: pr5Digest, Status: "completed", FinishedAt: time.Now().UTC(), ExitCode: 0}
	if err := SaveTerminalMarkerAtomic(pr5MarkerPath(tempDir, job.ID), marker); err != nil {
		t.Fatal(err)
	}

	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := LoadJob(filepath.Join(pr5StateDir(tempDir), job.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "cancelled" {
		t.Fatalf("cancelled job must never be revived, got %+v", got)
	}
	after, err := LoadTerminalMarker(pr5MarkerPath(tempDir, job.ID))
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "completed" {
		t.Fatalf("cancelled job's marker must be left untouched, got %+v", after)
	}
}

func TestPR5_TerminalMarkerValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "terminal.json")
	now := time.Now().UTC()

	if err := SaveTerminalMarkerAtomic(path, nil); err == nil {
		t.Fatal("nil marker must be rejected")
	}
	if err := SaveTerminalMarkerAtomic(path, &TerminalMarker{ExecutionSpecDigest: pr5Digest, Status: "completed", FinishedAt: now}); err == nil {
		t.Fatal("blank job_id must be rejected")
	}
	if err := SaveTerminalMarkerAtomic(path, &TerminalMarker{JobID: "job-x", Status: "completed", FinishedAt: now}); err == nil {
		t.Fatal("blank execution_spec_digest must be rejected")
	}
	if err := SaveTerminalMarkerAtomic(path, &TerminalMarker{JobID: "job-x", ExecutionSpecDigest: pr5Digest, Status: "running", FinishedAt: now}); err == nil {
		t.Fatal("nonterminal status must be rejected")
	}
	if err := SaveTerminalMarkerAtomic(path, &TerminalMarker{JobID: "job-x", ExecutionSpecDigest: pr5Digest, Status: "completed"}); err == nil {
		t.Fatal("zero finished_at must be rejected")
	}

	valid := &TerminalMarker{JobID: "job-x", ExecutionSpecDigest: pr5Digest, Status: "completed", FinishedAt: now, ExitCode: 0}
	if err := SaveTerminalMarkerAtomic(path, valid); err != nil {
		t.Fatalf("valid marker must save: %v", err)
	}
	loaded, err := LoadTerminalMarker(path)
	if err != nil {
		t.Fatalf("valid marker must load: %v", err)
	}
	if loaded.JobID != "job-x" || loaded.ExecutionSpecDigest != pr5Digest || loaded.Status != "completed" {
		t.Fatalf("loaded marker mismatch: %+v", loaded)
	}

	missingFinished := `{"job_id":"job-x","execution_spec_digest":"` + pr5Digest + `","status":"completed"}`
	if err := os.WriteFile(path, []byte(missingFinished), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTerminalMarker(path); err == nil {
		t.Fatal("marker with missing finished_at must be rejected on load")
	}

	zeroFinished := `{"job_id":"job-x","execution_spec_digest":"` + pr5Digest + `","status":"completed","finished_at":"0001-01-01T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(zeroFinished), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTerminalMarker(path); err == nil {
		t.Fatal("marker with zero finished_at must be rejected on load")
	}

	nonterminal := `{"job_id":"job-x","execution_spec_digest":"` + pr5Digest + `","status":"running","finished_at":"` + now.Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(path, []byte(nonterminal), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTerminalMarker(path); err == nil {
		t.Fatal("nonterminal marker must be rejected on load")
	}
}

func TestPR5_JobIDDirectoryMismatchFailsClosedWithoutMutation(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	job := pr5RunningJob("job-pr5-other-id", pr5Digest, 1<<30+9)
	jobDir := filepath.Join(pr5StateDir(tempDir), "job-pr5-dirname")
	if err := SaveJobAtomic(filepath.Join(jobDir, "job.json"), job); err != nil {
		t.Fatal(err)
	}

	if err := w.ReconcileStartup(context.Background()); err == nil {
		t.Fatal("job ID/directory mismatch must fail closed")
	}
	got, err := LoadJob(filepath.Join(jobDir, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" {
		t.Fatalf("mismatched job must be unmutated, got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(jobDir, "terminal.json")); !os.IsNotExist(err) {
		t.Fatalf("mismatched job must not get a terminal marker")
	}
}

func TestPR5_PersistTerminalCompletedOverCancelledCancellationWins(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	id := "job-pr5-cancel-wins"
	jobDir := filepath.Join(pr5StateDir(tempDir), id)
	jobFile := filepath.Join(jobDir, "job.json")
	cancelled := &JobRecord{
		ID: id, Status: "cancelled", ExecutionSpecDigest: pr5Digest,
		FinishedAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Second),
		ExitCode:   1, Error: "cancelled", FailureClassification: "cancelled",
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	if err := SaveJobAtomic(jobFile, cancelled); err != nil {
		t.Fatal(err)
	}

	completed := &JobRecord{ID: id, Status: "completed", ExecutionSpecDigest: pr5Digest, FinishedAt: time.Now().UTC(), ExitCode: 0}
	if err := w.persistTerminalJob(jobDir, jobFile, completed); err != nil {
		t.Fatalf("persistTerminalJob: %v", err)
	}

	got, err := LoadJob(jobFile)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "cancelled" {
		t.Fatalf("cancellation must win over attempted completion, got %+v", got)
	}
	marker, err := LoadTerminalMarker(filepath.Join(jobDir, "terminal.json"))
	if err != nil {
		t.Fatalf("cancelled marker must be written for normal digest/timestamp: %v", err)
	}
	if marker.Status != "cancelled" || marker.JobID != id || marker.ExecutionSpecDigest != pr5Digest {
		t.Fatalf("expected matching cancelled marker, got %+v", marker)
	}
	if !marker.FinishedAt.Equal(cancelled.FinishedAt) {
		t.Fatalf("cancelled marker must carry the cancelled finished_at: got %v want %v", marker.FinishedAt, cancelled.FinishedAt)
	}
}

func TestPR5_PersistTerminalLegacyCancelledUntouchedNoMarker(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	id := "job-pr5-legacy-cancelled"
	jobDir := filepath.Join(pr5StateDir(tempDir), id)
	jobFile := filepath.Join(jobDir, "job.json")
	cancelled := &JobRecord{ID: id, Status: "cancelled", CreatedAt: time.Now().UTC().Add(-time.Hour)}
	if err := SaveJobAtomic(jobFile, cancelled); err != nil {
		t.Fatal(err)
	}

	completed := &JobRecord{ID: id, Status: "completed", ExecutionSpecDigest: pr5Digest, FinishedAt: time.Now().UTC(), ExitCode: 0}
	if err := w.persistTerminalJob(jobDir, jobFile, completed); err != nil {
		t.Fatalf("persistTerminalJob: %v", err)
	}

	got, err := LoadJob(jobFile)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "cancelled" || strings.TrimSpace(got.ExecutionSpecDigest) != "" || !got.FinishedAt.IsZero() {
		t.Fatalf("legacy cancelled record must remain untouched, got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(jobDir, "terminal.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy cancelled record must get no terminal marker")
	}
}

func TestPR5_PersistTerminalNormalWritesMarkerAfterJob(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	id := "job-pr5-normal"
	jobDir := filepath.Join(pr5StateDir(tempDir), id)
	jobFile := filepath.Join(jobDir, "job.json")
	running := pr5RunningJob(id, pr5Digest, 1<<30+10)
	if err := SaveJobAtomic(jobFile, running); err != nil {
		t.Fatal(err)
	}

	finished := time.Now().UTC().Truncate(time.Second)
	completed := &JobRecord{ID: id, Status: "completed", ExecutionSpecDigest: pr5Digest, FinishedAt: finished, ExitCode: 0}
	if err := w.persistTerminalJob(jobDir, jobFile, completed); err != nil {
		t.Fatalf("persistTerminalJob: %v", err)
	}

	got, err := LoadJob(jobFile)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" {
		t.Fatalf("normal terminal write must persist completed, got %+v", got)
	}
	marker, err := LoadTerminalMarker(filepath.Join(jobDir, "terminal.json"))
	if err != nil {
		t.Fatalf("normal terminal write must persist a marker: %v", err)
	}
	if marker.Status != "completed" || !terminalMarkerMatchesJob(got, marker) {
		t.Fatalf("marker must match the completed job, got %+v", marker)
	}
}

func TestPR5_BenchmarkOnlyAndEmptyDirsSkipped(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	stateDir := pr5StateDir(tempDir)

	benchDir := filepath.Join(stateDir, "bench-pr5-1")
	if err := os.MkdirAll(benchDir, 0755); err != nil {
		t.Fatal(err)
	}
	benchJSON := `{"id":"bench-pr5-1","status":"completed"}`
	if err := os.WriteFile(filepath.Join(benchDir, "benchmark.json"), []byte(benchJSON), 0644); err != nil {
		t.Fatal(err)
	}
	emptyDir := filepath.Join(stateDir, "job-pr5-empty")
	if err := os.MkdirAll(emptyDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("benchmark-only/empty dirs must be skipped, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(benchDir, "terminal.json")); !os.IsNotExist(err) {
		t.Fatalf("benchmark-only dir must get no terminal marker")
	}
	raw, err := os.ReadFile(filepath.Join(benchDir, "benchmark.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != benchJSON {
		t.Fatalf("benchmark.json must be untouched, got %s", raw)
	}
	if _, err := os.Stat(filepath.Join(emptyDir, "terminal.json")); !os.IsNotExist(err) {
		t.Fatalf("empty dir must get no job or marker mutation")
	}
}
