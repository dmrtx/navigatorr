package transcodeworker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// Strong full-transcode idempotency must bind to source CONTENT identity when
// the coordinator supplies a SHA-256, so a changed source at the same path
// cannot silently reuse an existing job. Legacy empty-SHA behavior is
// unchanged.

func shaSourceReqFixture(t *testing.T) (*Worker, string, string) {
	t.Helper()
	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "src.mkv")
	if err := os.WriteFile(src, []byte("source-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &WorkerConfig{
		StateDir:        filepath.Join(tempDir, "jobs"),
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: 1,
	}
	w := NewWorker(cfg)
	stub := newStubSpawn()
	stub.install(w)
	return w, src, filepath.Join(tempDir, "out.mkv")
}

func TestSubmit_SourceSHA256StrongIdempotency(t *testing.T) {
	w, src, cand := shaSourceReqFixture(t)
	ctx := context.Background()
	shaA := strings.Repeat("a", 64)
	shaB := strings.Repeat("b", 64)
	req := SubmitRequest{ID: "job-sha", SourcePath: src, CandidatePath: cand, IdempotencyKey: "key-sha", SourceSHA256: shaA}

	first, err := w.Submit(ctx, req, "exe", "")
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if first.ExecutionSpecDigest == "" {
		t.Fatal("expected a canonical execution spec digest")
	}

	// Same key + same paths/profile/plan + same SourceSHA256 => reuse.
	reuse, err := w.Submit(ctx, req, "exe", "")
	if err != nil {
		t.Fatalf("idempotent resubmit: %v", err)
	}
	if !reuse.Reused || reuse.ExecutionSpecDigest != first.ExecutionSpecDigest {
		t.Fatalf("same content identity must reuse the existing job: %+v", reuse)
	}

	// Same key + same paths/profile/plan + DIFFERENT SourceSHA256 =>
	// deterministic conflict, never reuse.
	changed := req
	changed.SourceSHA256 = shaB
	_, err = w.Submit(ctx, changed, "exe", "")
	if !IsIdempotencyConflict(err) {
		t.Fatalf("changed source SHA-256 must be a deterministic idempotency conflict, got %v", err)
	}
}

func TestSubmit_LegacyEmptySourceSHAIdempotencyUnchanged(t *testing.T) {
	w, src, cand := shaSourceReqFixture(t)
	ctx := context.Background()
	req := SubmitRequest{ID: "job-legacy-sha", SourcePath: src, CandidatePath: cand, IdempotencyKey: "key-legacy"}

	first, err := w.Submit(ctx, req, "exe", "")
	if err != nil {
		t.Fatalf("first legacy submit: %v", err)
	}
	if first.ExecutionSpecDigest == "" {
		t.Fatal("expected a canonical execution spec digest")
	}

	// Legacy empty-SHA resubmit still reuses (unchanged behavior).
	reuse, err := w.Submit(ctx, req, "exe", "")
	if err != nil {
		t.Fatalf("legacy idempotent resubmit: %v", err)
	}
	if !reuse.Reused || reuse.ExecutionSpecDigest != first.ExecutionSpecDigest {
		t.Fatalf("legacy empty-SHA resubmit must reuse: %+v", reuse)
	}

	// A content-identified request for the SAME key must NOT reuse the legacy
	// job: the digest changed, so it is a deterministic conflict (fail closed).
	identified := req
	identified.SourceSHA256 = strings.Repeat("c", 64)
	if _, err := w.Submit(ctx, identified, "exe", ""); !IsIdempotencyConflict(err) {
		t.Fatalf("content-identified submit must conflict with a legacy empty-SHA job, got %v", err)
	}
}

// TestSubmit_ExplicitHashAwareDigestAccepted proves the coordinator/executor
// hash-aware digest is exactly what the worker recomputes, so an explicitly
// supplied digest is accepted rather than rejected as a mismatch.
func TestSubmit_ExplicitHashAwareDigestAccepted(t *testing.T) {
	w, src, cand := shaSourceReqFixture(t)
	ctx := context.Background()
	sha := strings.Repeat("d", 64)
	plan, err := ResolveWorkerPlan("hevc-vt", nil)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := transcode.DigestTranscodeExecutionSpecWithSourceSHA(filepath.Clean(src), filepath.Clean(cand), "hevc-vt", sha, plan)
	if err != nil {
		t.Fatal(err)
	}
	req := SubmitRequest{ID: "job-explicit-sha", SourcePath: src, CandidatePath: cand, IdempotencyKey: "key-explicit", SourceSHA256: sha, ExecutionSpecDigest: digest}
	resp, err := w.Submit(ctx, req, "exe", "")
	if err != nil {
		t.Fatalf("hash-aware explicit digest must be accepted: %v", err)
	}
	if resp.ExecutionSpecDigest != digest {
		t.Fatalf("stored digest %q != supplied %q", resp.ExecutionSpecDigest, digest)
	}
}
