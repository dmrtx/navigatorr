package transcodeworker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A request without a source digest is hashed while the worker stages the
// file: the digest is learned from the stream, recorded apart from the
// request's SourceSHA256, reported in the status, and used to fill the cache.
func learnWorker(t *testing.T) (*Worker, *WorkerConfig, string, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	ext := filepath.Join(dir, "nas")
	src := pr6b1WriteFile(t, filepath.Join(ext, "src.mkv"))
	content, _ := os.ReadFile(src)
	cfg := pr6b2Config(dir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{ext}
		c.StagingPolicy = StagingPolicyAlways
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	(&pr6b2Recorder{createOut: true}).install(w)
	return w, cfg, src, filepath.Join(ext, "out.mkv"), content
}

func TestDigestlessRequestLearnsTheDigestWhileStaging(t *testing.T) {
	w, cfg, src, cand, content := learnWorker(t)
	hashes := countLocalHashes(t)
	req := SubmitRequest{ID: "job-learn", SourcePath: src, CandidatePath: cand, SourceSizeBytes: int64(len(content))}
	if _, err := w.Submit(context.Background(), req, "exe", ""); err != nil {
		t.Fatal(err)
	}
	if err := w.InternalRun(context.Background(), req.ID); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, req.ID)
	if got.Status != "completed" {
		t.Fatalf("status=%q err=%s", got.Status, got.Error)
	}
	want := sha256Hex(content)
	if got.LearnedSourceSHA256 != want {
		t.Fatalf("learned %q, want %q", got.LearnedSourceSHA256, want)
	}
	if got.SourceSHA256 != "" {
		t.Fatalf("the request's digest field was overwritten with %q", got.SourceSHA256)
	}
	staged, _ := StagedInputPath(w.localWorkDir(), req.ID, src)
	if n := hashes(staged); n != 0 {
		t.Fatalf("staged input hashed %d times, want 0: the digest comes from the copy", n)
	}
	st, err := w.Status(context.Background(), req.ID)
	if err != nil || st.SourceSHA256 != want {
		t.Fatalf("status digest %q err=%v, want %q", st.SourceSHA256, err, want)
	}
	// An identical resubmit still equals the stored spec: the learned digest is
	// not part of the idempotency digest.
	resp, err := w.Submit(context.Background(), req, "exe", "")
	if err != nil || !resp.Reused {
		t.Fatalf("identical resubmit after learning: reused=%v err=%v", resp.Reused, err)
	}
	// The cache entry is content-addressed by the learned digest.
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupSourceCache(w.sourceCacheDir(), filepath.Clean(src), fi, want); !ok {
		t.Fatal("cache was not populated with the learned digest")
	}
	if _, ok := lookupSourceCache(w.sourceCacheDir(), filepath.Clean(src), fi, ""); ok {
		t.Fatal("a digest-less lookup must never hit a content-addressed entry")
	}
}

// The coordinator pins the learned digest and sends it with the next rung: that
// job is served from the cache, with no second read of the source.
func TestPinnedDigestAfterLearningHitsTheCache(t *testing.T) {
	w, cfg, src, cand, content := learnWorker(t)
	first := SubmitRequest{ID: "job-learn-a", SourcePath: src, CandidatePath: cand, SourceSizeBytes: int64(len(content))}
	if _, err := w.Submit(context.Background(), first, "exe", ""); err != nil {
		t.Fatal(err)
	}
	if err := w.InternalRun(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	learned := pr6b1LoadJob(t, cfg.StateDir, first.ID).LearnedSourceSHA256
	if learned == "" {
		t.Fatal("nothing learned")
	}
	second := SubmitRequest{ID: "job-learn-b", SourcePath: src, CandidatePath: filepath.Join(filepath.Dir(cand), "out-b.mkv"), SourceSHA256: learned}
	if _, err := w.Submit(context.Background(), second, "exe", ""); err != nil {
		t.Fatal(err)
	}
	if err := w.InternalRun(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, second.ID)
	if got.Status != "completed" {
		t.Fatalf("status=%q err=%s", got.Status, got.Error)
	}
	if c := got.PhaseCosts["staging"]; c.CacheHits == nil || *c.CacheHits != 1 || c.NASReadBytes == nil || *c.NASReadBytes != 0 {
		t.Fatalf("second rung staging cost = %+v, want a cache hit and no NAS read", c)
	}
	if got.LearnedSourceSHA256 != "" || got.effectiveSourceSHA() != learned {
		t.Fatalf("a request with a digest must not learn another: learned=%q", got.LearnedSourceSHA256)
	}
}

// A source that is not the size the coordinator probed fails closed before the
// long read, and nothing is learned.
func TestDigestlessRequestFailsClosedWhenTheSourceChangedSizeSinceTheProbe(t *testing.T) {
	w, cfg, src, cand, content := learnWorker(t)
	req := SubmitRequest{ID: "job-learn-size", SourcePath: src, CandidatePath: cand, SourceSizeBytes: int64(len(content)) + 7}
	if _, err := w.Submit(context.Background(), req, "exe", ""); err != nil {
		t.Fatal(err)
	}
	_ = w.InternalRun(context.Background(), req.ID)
	got := pr6b1LoadJob(t, cfg.StateDir, req.ID)
	if got.Status != "failed" || !strings.Contains(got.Error, "probed") {
		t.Fatalf("status=%q err=%q, want a size mismatch failure", got.Status, got.Error)
	}
	if got.LearnedSourceSHA256 != "" {
		t.Fatalf("learned %q from a source that did not match", got.LearnedSourceSHA256)
	}
	staged, _ := StagedInputPath(w.localWorkDir(), req.ID, src)
	if _, err := os.Stat(staged); err == nil {
		t.Fatal("a source of the wrong size was staged")
	}
}

// Staging that this run did not do (a staged copy left by an interrupted run,
// or an input read in place) still ends with a learned digest, taken from the
// bytes the job will encode.
func TestVerifyOrLearnHashesInputWhenStagingRecordedNothing(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.mkv")
	content := []byte("bytes left by an interrupted run")
	if err := os.WriteFile(input, content, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{dir}, LocalWorkDir: filepath.Join(dir, "work"), MaxParallelJobs: 1}
	w := NewWorker(cfg)
	job, _ := stagedJob(t, w, cfg.StateDir, "job-resume-learn", input, "")
	job.ExpectedSourceSizeBytes = int64(len(content))
	jobDir := filepath.Join(cfg.StateDir, job.ID)
	if err := w.verifyOrLearnSourceDigest(context.Background(), jobDir, filepath.Join(jobDir, "job.json"), job, input); err != nil {
		t.Fatal(err)
	}
	if job.LearnedSourceSHA256 != sha256Hex(content) || job.effectiveSourceSHA() != sha256Hex(content) {
		t.Fatalf("learned %q", job.LearnedSourceSHA256)
	}
	if got := pr6b1LoadJob(t, cfg.StateDir, job.ID); got.LearnedSourceSHA256 != job.LearnedSourceSHA256 {
		t.Fatal("learned digest was not persisted")
	}
	// A digest the job already has is only verified, never replaced.
	job.LearnedSourceSHA256 = ""
	job.SourceSHA256 = sha256Hex([]byte("other"))
	if err := w.verifyOrLearnSourceDigest(context.Background(), jobDir, filepath.Join(jobDir, "job.json"), job, input); err == nil {
		t.Fatal("a wrong digest must fail closed")
	}
	if job.LearnedSourceSHA256 != "" {
		t.Fatal("verification of a request digest learned a second one")
	}
}

// A crash after the staged copy landed but before the learned digest was saved
// leaves a complete staged input and no digest. The next run learns it by
// hashing that local copy; the NAS is not read again.
func TestRunAfterACrashBetweenStagingAndPersistingLearnsFromTheStagedCopy(t *testing.T) {
	w, cfg, src, cand, content := learnWorker(t)
	req := SubmitRequest{ID: "job-learn-crash", SourcePath: src, CandidatePath: cand, SourceSizeBytes: int64(len(content))}
	if _, err := w.Submit(context.Background(), req, "exe", ""); err != nil {
		t.Fatal(err)
	}
	staged, _ := StagedInputPath(w.localWorkDir(), req.ID, src)
	if err := os.MkdirAll(filepath.Dir(staged), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, content, 0600); err != nil {
		t.Fatal(err)
	}
	hashes := countLocalHashes(t)
	if err := w.InternalRun(context.Background(), req.ID); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, req.ID)
	if got.Status != "completed" || got.LearnedSourceSHA256 != sha256Hex(content) || got.SourceSHA256 != "" {
		t.Fatalf("status=%q err=%q learned=%q", got.Status, got.Error, got.LearnedSourceSHA256)
	}
	if n := hashes(staged); n != 1 {
		t.Fatalf("staged copy hashed %d times, want exactly 1", n)
	}
	if c := got.PhaseCosts["staging"]; c.NASReadBytes != nil && *c.NASReadBytes != 0 {
		t.Fatalf("the source was read again: %+v", c)
	}
}

// A staged copy left by a crashed run that is not the size the coordinator
// probed fails closed instead of being learned as the source's digest.
func TestRunAfterACrashWithAWrongSizedStagedCopyFailsClosed(t *testing.T) {
	w, cfg, src, cand, content := learnWorker(t)
	req := SubmitRequest{ID: "job-learn-crash-size", SourcePath: src, CandidatePath: cand, SourceSizeBytes: int64(len(content))}
	if _, err := w.Submit(context.Background(), req, "exe", ""); err != nil {
		t.Fatal(err)
	}
	staged, _ := StagedInputPath(w.localWorkDir(), req.ID, src)
	_ = os.MkdirAll(filepath.Dir(staged), 0755)
	if err := os.WriteFile(staged, append(append([]byte{}, content...), '!'), 0600); err != nil {
		t.Fatal(err)
	}
	_ = w.InternalRun(context.Background(), req.ID)
	got := pr6b1LoadJob(t, cfg.StateDir, req.ID)
	if got.Status != "failed" || got.LearnedSourceSHA256 != "" {
		t.Fatalf("status=%q learned=%q err=%q", got.Status, got.LearnedSourceSHA256, got.Error)
	}
}
