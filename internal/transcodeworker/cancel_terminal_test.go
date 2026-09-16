package transcodeworker

// Focused regression tests for MINIMAL FIX A: Cancel terminal immutability,
// durable-before-marker cancellation, legacy blank-digest compatibility, and
// safe Phase 6 artifact cleanup. Deterministic and ffmpeg-free.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cancelWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestCancelTerminalCompletedImmutable(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 4, stub)
	const id = "job-cancel-completed"
	cand := filepath.Join(tempDir, "completed-out.mkv")
	cancelWriteFile(t, cand, "finalized output")

	finished := time.Now().UTC().Truncate(time.Second)
	pr5SeedJob(t, tempDir, &JobRecord{
		ID: id, Status: "completed", Source: sourceFile, Candidate: cand,
		ExecutionSpecDigest: pr5Digest, FinishedAt: finished, ExitCode: 0,
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	})

	resp, err := w.Cancel(context.Background(), id)
	if err != nil {
		t.Fatalf("cancel on completed must not error: %v", err)
	}
	if resp.Status != "completed" {
		t.Fatalf("completed must stay completed, got %q", resp.Status)
	}

	got, err := LoadJob(filepath.Join(pr5StateDir(tempDir), id, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" || !got.FinishedAt.Equal(finished) || got.FailureClassification != "" {
		t.Fatalf("durable completed record must be unchanged, got %+v", got)
	}
	if _, err := os.Stat(cand); err != nil {
		t.Fatalf("terminal cancel must never delete the finalized artifact: %v", err)
	}
	if _, err := os.Stat(pr5MarkerPath(tempDir, id)); !os.IsNotExist(err) {
		t.Fatalf("cancel on terminal completed must not create a terminal marker")
	}
}

func TestCancelTerminalFailedImmutable(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 4, stub)
	const id = "job-cancel-failed"
	cand := filepath.Join(tempDir, "failed-out.mkv")
	cancelWriteFile(t, cand, "leftover output")

	finished := time.Now().UTC().Truncate(time.Second)
	pr5SeedJob(t, tempDir, &JobRecord{
		ID: id, Status: "failed", Source: sourceFile, Candidate: cand,
		ExecutionSpecDigest: pr5Digest, FinishedAt: finished, ExitCode: 1,
		Error: "boom", FailureClassification: "runner_killed",
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	})

	resp, err := w.Cancel(context.Background(), id)
	if err != nil {
		t.Fatalf("cancel on failed must not error: %v", err)
	}
	if resp.Status != "failed" {
		t.Fatalf("failed must stay failed, got %q", resp.Status)
	}

	got, err := LoadJob(filepath.Join(pr5StateDir(tempDir), id, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.FailureClassification != "runner_killed" || got.Error != "boom" || !got.FinishedAt.Equal(finished) {
		t.Fatalf("durable failed record must be unchanged, got %+v", got)
	}
	if _, err := os.Stat(cand); err != nil {
		t.Fatalf("terminal cancel must never delete a terminal artifact: %v", err)
	}
	if _, err := os.Stat(pr5MarkerPath(tempDir, id)); !os.IsNotExist(err) {
		t.Fatalf("cancel on terminal failed must not create a terminal marker")
	}
}

func TestCancelQueuedWritesJobThenMarkerModernDigest(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 4, stub)
	const id = "job-cancel-modern"
	cand := filepath.Join(tempDir, "modern-out.mkv")
	cancelWriteFile(t, cand, "incomplete encode")

	pr5SeedJob(t, tempDir, &JobRecord{
		ID: id, Status: "queued", Source: sourceFile, Candidate: cand,
		ExecutionSpecDigest: pr5Digest, CreatedAt: time.Now().UTC(),
	})

	resp, err := w.Cancel(context.Background(), id)
	if err != nil || resp.Status != "cancelled" {
		t.Fatalf("queued cancel failed: %+v err %v", resp, err)
	}

	got, err := LoadJob(filepath.Join(pr5StateDir(tempDir), id, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "cancelled" || got.FinishedAt.IsZero() {
		t.Fatalf("job.json must be durably cancelled with finished_at, got %+v", got)
	}
	marker, err := LoadTerminalMarker(pr5MarkerPath(tempDir, id))
	if err != nil {
		t.Fatalf("modern cancelled record must write a terminal marker: %v", err)
	}
	if marker.Status != "cancelled" || marker.JobID != id || marker.ExecutionSpecDigest != pr5Digest {
		t.Fatalf("marker must match the cancelled record, got %+v", marker)
	}
	if !marker.FinishedAt.Equal(got.FinishedAt) {
		t.Fatalf("marker finished_at must mirror job.json: marker=%v job=%v", marker.FinishedAt, got.FinishedAt)
	}
	if _, err := os.Stat(cand); !os.IsNotExist(err) {
		t.Fatalf("incomplete local candidate must be removed on cancel")
	}
}

func TestCancelQueuedLegacyBlankDigestNoMarker(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 4, stub)
	const id = "job-cancel-legacy"
	cand := filepath.Join(tempDir, "legacy-out.mkv")
	cancelWriteFile(t, cand, "incomplete encode")

	pr5SeedJob(t, tempDir, &JobRecord{
		ID: id, Status: "queued", Source: sourceFile, Candidate: cand,
		CreatedAt: time.Now().UTC(),
	})

	resp, err := w.Cancel(context.Background(), id)
	if err != nil || resp.Status != "cancelled" {
		t.Fatalf("legacy queued cancel failed: %+v err %v", resp, err)
	}

	got, err := LoadJob(filepath.Join(pr5StateDir(tempDir), id, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "cancelled" || got.FinishedAt.IsZero() || strings.TrimSpace(got.ExecutionSpecDigest) != "" {
		t.Fatalf("legacy record must be cancelled without adopting a digest, got %+v", got)
	}
	if _, err := os.Stat(pr5MarkerPath(tempDir, id)); !os.IsNotExist(err) {
		t.Fatalf("legacy blank-digest record must get no terminal marker")
	}
	if _, err := os.Stat(cand); !os.IsNotExist(err) {
		t.Fatalf("incomplete local candidate must be removed on cancel")
	}
}

func TestCancelAlreadyCancelledModernEnsuresMarkerUntouched(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 4, stub)
	const id = "job-cancel-already"
	cand := filepath.Join(tempDir, "already-out.mkv")
	cancelWriteFile(t, cand, "preserved artifact")

	finished := time.Now().UTC().Truncate(time.Second)
	pr5SeedJob(t, tempDir, &JobRecord{
		ID: id, Status: "cancelled", Source: sourceFile, Candidate: cand,
		ExecutionSpecDigest: pr5Digest, FinishedAt: finished, Error: "cancelled by user",
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	})
	// A misleading completed marker must be replaced by the cancelled marker.
	if err := SaveTerminalMarkerAtomic(pr5MarkerPath(tempDir, id), &TerminalMarker{
		JobID: id, ExecutionSpecDigest: pr5Digest, Status: "completed", FinishedAt: finished, ExitCode: 0,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := w.Cancel(context.Background(), id)
	if err != nil || resp.Status != "cancelled" {
		t.Fatalf("already-cancelled cancel failed: %+v err %v", resp, err)
	}

	got, err := LoadJob(filepath.Join(pr5StateDir(tempDir), id, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "cancelled" || got.Error != "cancelled by user" || !got.FinishedAt.Equal(finished) {
		t.Fatalf("already-cancelled job.json must be untouched, got %+v", got)
	}
	marker, err := LoadTerminalMarker(pr5MarkerPath(tempDir, id))
	if err != nil {
		t.Fatalf("cancelled marker must be ensured: %v", err)
	}
	if marker.Status != "cancelled" || marker.ExecutionSpecDigest != pr5Digest {
		t.Fatalf("misleading marker must be replaced with cancelled, got %+v", marker)
	}
	if _, err := os.Stat(cand); err != nil {
		t.Fatalf("already-cancelled resume must not delete artifacts: %v", err)
	}
}

func TestCancelAlreadyCancelledLegacyUntouchedNoMarker(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 4, stub)
	const id = "job-cancel-already-legacy"
	cand := filepath.Join(tempDir, "already-legacy-out.mkv")
	cancelWriteFile(t, cand, "preserved artifact")

	pr5SeedJob(t, tempDir, &JobRecord{
		ID: id, Status: "cancelled", Source: sourceFile, Candidate: cand,
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	})

	resp, err := w.Cancel(context.Background(), id)
	if err != nil || resp.Status != "cancelled" {
		t.Fatalf("legacy already-cancelled cancel failed: %+v err %v", resp, err)
	}

	got, err := LoadJob(filepath.Join(pr5StateDir(tempDir), id, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "cancelled" || !got.FinishedAt.IsZero() || strings.TrimSpace(got.ExecutionSpecDigest) != "" {
		t.Fatalf("legacy already-cancelled record must remain untouched, got %+v", got)
	}
	if _, err := os.Stat(pr5MarkerPath(tempDir, id)); !os.IsNotExist(err) {
		t.Fatalf("legacy already-cancelled record must get no terminal marker")
	}
}

func TestCancelSaveFailureFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping permission-based cancel failure test as root (permissions bypassed)")
	}
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 4, stub)
	const id = "job-cancel-savefail"
	jobDir := filepath.Join(pr5StateDir(tempDir), id)
	cand := filepath.Join(tempDir, "savefail-out.mkv")
	cancelWriteFile(t, cand, "incomplete encode")

	pr5SeedJob(t, tempDir, &JobRecord{
		ID: id, Status: "queued", Source: sourceFile, Candidate: cand,
		ExecutionSpecDigest: pr5Digest, CreatedAt: time.Now().UTC(),
	})
	// Pre-create the lock file so acquireJobLock succeeds on a read-only dir
	// while temp-file creation for job.json fails.
	if err := os.WriteFile(filepath.Join(jobDir, ".lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(jobDir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(jobDir, 0755) })

	if _, err := w.Cancel(context.Background(), id); err == nil {
		t.Fatal("SaveJobAtomic failure must fail closed")
	}
	_ = os.Chmod(jobDir, 0755)

	got, err := LoadJob(filepath.Join(jobDir, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "queued" {
		t.Fatalf("failed cancel must not durably mark cancelled, got %+v", got)
	}
	if _, err := os.Stat(cand); err != nil {
		t.Fatalf("failed cancel must not clean artifacts: %v", err)
	}
	if _, err := os.Stat(filepath.Join(jobDir, "terminal.json")); !os.IsNotExist(err) {
		t.Fatalf("failed cancel must not write a terminal marker")
	}
}

func TestCancelExternalFinalizedDestinationPreserved(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 4, stub)
	const id = "job-cancel-external"
	jobDir := filepath.Join(pr5StateDir(tempDir), id)

	extRoot := filepath.Join(tempDir, "external")
	destination := filepath.Join(extRoot, "published.mkv")
	localCandidate := filepath.Join(pr5StateDir(tempDir), "_work", id, "candidate.mkv")
	partial := PartialPathFor(destination, id)

	cancelWriteFile(t, destination, "finalized destination bytes")
	cancelWriteFile(t, localCandidate, "intermediate local encode")
	cancelWriteFile(t, partial, "in-progress finalization")

	pr5SeedJob(t, tempDir, &JobRecord{
		ID: id, Status: "queued", Source: sourceFile, Candidate: destination,
		Profile: "hevc-vt", ExecutionSpecDigest: pr5Digest,
		StagingPolicy: string(StagingPolicyAuto), StagingState: string(StagingStateNotRequired),
		EffectiveInputPath: sourceFile, LocalCandidatePath: localCandidate,
		IntendedDestination: destination, FinalizationState: string(FinalizationStateCompleted),
		PartialPath: partial, CreatedAt: time.Now().UTC(),
	})

	resp, err := w.Cancel(context.Background(), id)
	if err != nil || resp.Status != "cancelled" {
		t.Fatalf("external queued cancel failed: %+v err %v", resp, err)
	}

	if data, err := os.ReadFile(destination); err != nil || string(data) != "finalized destination bytes" {
		t.Fatalf("finalized Phase 6 destination must be preserved, got data=%q err=%v", data, err)
	}
	if _, err := os.Stat(localCandidate); !os.IsNotExist(err) {
		t.Fatalf("incomplete local candidate must be removed on cancel")
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("own finalization partial must be removed on cancel")
	}
	got, err := LoadJob(filepath.Join(jobDir, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "cancelled" || got.Candidate != destination {
		t.Fatalf("semantic destination must be unchanged, got %+v", got)
	}
}

func TestCancelExternalLegacyNoPartialPathPreservedDestination(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 4, stub)
	const id = "job-cancel-external-legacy"
	extRoot := filepath.Join(tempDir, "external-legacy")
	destination := filepath.Join(extRoot, "legacy-published.mkv")
	localCandidate := filepath.Join(pr5StateDir(tempDir), "_work", id, "candidate.mkv")
	partial := PartialPathFor(destination, id)

	cancelWriteFile(t, destination, "finalized destination bytes")
	cancelWriteFile(t, localCandidate, "intermediate local encode")
	cancelWriteFile(t, partial, "in-progress finalization")

	// PartialPath intentionally omitted: cleanup must derive the exact own
	// partial from the destination without ever touching the destination.
	pr5SeedJob(t, tempDir, &JobRecord{
		ID: id, Status: "queued", Source: sourceFile, Candidate: destination,
		Profile: "hevc-vt", ExecutionSpecDigest: pr5Digest,
		EffectiveInputPath: sourceFile, LocalCandidatePath: localCandidate,
		IntendedDestination: destination, FinalizationState: string(FinalizationStatePending),
		CreatedAt: time.Now().UTC(),
	})

	if _, err := w.Cancel(context.Background(), id); err != nil {
		t.Fatalf("external legacy queued cancel failed: %v", err)
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("finalized destination must be preserved: %v", err)
	}
	if _, err := os.Stat(localCandidate); !os.IsNotExist(err) {
		t.Fatalf("incomplete local candidate must be removed on cancel")
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("derived own partial must be removed on cancel")
	}
}
