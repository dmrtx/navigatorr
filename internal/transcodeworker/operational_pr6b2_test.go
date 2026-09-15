package transcodeworker

// Phase 6B2 tests: the operational execution state machine (staging, encode to
// LocalCandidate, the EncodeComplete checkpoint, and destination finalization).
// Deterministic without real ffmpeg/ffprobe via the injected probe/encoder
// seams. These tests also prove the semantic Source/Candidate/PlanDigest and
// execution-spec digest are never rewritten by operational execution.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// pr6b2Recorder records probe/encode invocations and materializes a synthetic
// encoder output so the EncodeComplete checkpoint and finalization can run.
type pr6b2Recorder struct {
	probePaths   []string
	probeErr     error
	probeDur     float64
	probeStreams []SourceStream
	encodeCalls  int
	encodeIns    []string
	encodeOuts   []string
	ffmpegErr    error
	createOut    bool
}

func (r *pr6b2Recorder) streams() []SourceStream {
	if r.probeStreams != nil {
		return r.probeStreams
	}
	return []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264"}}
}

func (r *pr6b2Recorder) install(w *Worker) {
	w.SetProbeSource(func(ctx context.Context, path string) ([]SourceStream, float64, error) {
		r.probePaths = append(r.probePaths, path)
		return r.streams(), r.probeDur, r.probeErr
	})
	w.SetRunFFmpeg(func(ctx context.Context, execPlan *ExecutionPlan, job *JobRecord, inputPath, outputPath, progressPath, logPath string) error {
		r.encodeCalls++
		r.encodeIns = append(r.encodeIns, inputPath)
		r.encodeOuts = append(r.encodeOuts, outputPath)
		if r.ffmpegErr != nil {
			return r.ffmpegErr
		}
		if r.createOut {
			if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(outputPath, []byte("encoded-media"), 0644); err != nil {
				return err
			}
		}
		return nil
	})
}

func pr6b2Config(tempDir string, mutate func(*WorkerConfig)) *WorkerConfig {
	cfg := &WorkerConfig{
		StateDir:        filepath.Join(tempDir, "jobs"),
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: 1,
	}
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

func pr6b2Seed(t *testing.T, stateDir string, job *JobRecord) {
	t.Helper()
	if err := SaveJobAtomic(filepath.Join(stateDir, job.ID, "job.json"), job); err != nil {
		t.Fatal(err)
	}
}

func pr6b2Plan(t *testing.T) *transcode.Plan {
	t.Helper()
	plan, err := ResolveWorkerPlan("hevc-vt", nil)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func pr6b2Run(t *testing.T, w *Worker, id string) error {
	t.Helper()
	return w.InternalRun(context.Background(), id)
}

func TestPR6B2_LegacyLocalToLocalUsesSourceCandidateAndCompletes(t *testing.T) {
	tempDir := t.TempDir()
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "src.mkv"))
	cand := filepath.Join(tempDir, "out.mkv")
	cfg := pr6b2Config(tempDir, nil)
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-legacy-local"
	pr6b2Seed(t, cfg.StateDir, &JobRecord{
		ID: id, Status: "queued", Source: src, Candidate: cand, Profile: "hevc-vt",
		Plan: pr6b2Plan(t), ExecutionSpecDigest: pr5Digest, CreatedAt: time.Now().UTC(),
	})

	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	if rec.encodeCalls != 1 {
		t.Fatalf("encode calls = %d, want 1", rec.encodeCalls)
	}
	if rec.encodeIns[0] != src || rec.encodeOuts[0] != cand {
		t.Fatalf("legacy encode paths = %q -> %q, want source -> candidate", rec.encodeIns[0], rec.encodeOuts[0])
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "completed" || !got.EncodeComplete {
		t.Fatalf("job = %+v, want completed with encode_complete", got)
	}
	if got.Source != src || got.Candidate != cand {
		t.Fatalf("semantic paths changed: %q/%q", got.Source, got.Candidate)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, id, "terminal.json")); err != nil {
		t.Fatalf("terminal marker missing: %v", err)
	}
	if fi, err := os.Stat(cand); err != nil || fi.Size() == 0 {
		t.Fatalf("local destination must hold the encoded output: %v", err)
	}
}

func TestPR6B2_ExternalSourceStagesAndReusesStagedInput(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	src := pr6b1WriteFile(t, filepath.Join(extRoot, "movie.mkv"))
	cand := filepath.Join(tempDir, "local", "out.mkv")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
		c.StagingPolicy = StagingPolicyAlways
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-staged"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	wantStaged := filepath.Join(cfg.StateDir, "_work", id, "input.mkv")

	var checkpoint *JobRecord
	var stagedExistsAtCheckpoint bool
	w.SetAfterEncodeCheckpoint(func(jobDir string, job *JobRecord) {
		checkpoint, _ = LoadJob(filepath.Join(jobDir, "job.json"))
		if fi, err := os.Stat(wantStaged); err == nil && fi.Size() > 0 {
			stagedExistsAtCheckpoint = true
		}
	})

	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	if rec.encodeCalls != 1 {
		t.Fatalf("encode calls = %d, want 1", rec.encodeCalls)
	}
	if len(rec.probePaths) != 1 || rec.probePaths[0] != wantStaged {
		t.Fatalf("probe paths = %v, want staged input %q", rec.probePaths, wantStaged)
	}
	if rec.encodeIns[0] != wantStaged {
		t.Fatalf("encode input = %q, want staged input %q", rec.encodeIns[0], wantStaged)
	}
	if rec.encodeOuts[0] != cand {
		t.Fatalf("encode output = %q, want candidate %q", rec.encodeOuts[0], cand)
	}
	if checkpoint == nil || checkpoint.StagingState != string(StagingStateReady) {
		t.Fatalf("checkpoint staging state = %+v, want ready", checkpoint)
	}
	if !stagedExistsAtCheckpoint {
		t.Fatalf("staged input must exist by the EncodeComplete checkpoint")
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Source != src {
		t.Fatalf("semantic Source changed: %q", got.Source)
	}
	if got.Status != "completed" {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	// Source content must be byte-identical after staging+encode.
	if b, err := os.ReadFile(src); err != nil || string(b) != "dummy-media" {
		t.Fatalf("source content changed: %q err=%v", b, err)
	}
}

func TestPR6B2_StagingNotRequiredUsesSourceWithoutCopy(t *testing.T) {
	tempDir := t.TempDir()
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "src.mkv"))
	cand := filepath.Join(tempDir, "out.mkv")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.StagingPolicy = StagingPolicyNever
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-no-stage"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	if rec.encodeIns[0] != src {
		t.Fatalf("encode input = %q, want source %q", rec.encodeIns[0], src)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.StagingState != string(StagingStateNotRequired) {
		t.Fatalf("StagingState = %q, want not_required", got.StagingState)
	}
	unwanted := filepath.Join(cfg.StateDir, "_work", id, "input.mkv")
	if _, err := os.Stat(unwanted); !os.IsNotExist(err) {
		t.Fatalf("no staged copy may be created when staging is not required: %s", unwanted)
	}
}

func TestPR6B2_ExternalDestinationEncodesToLocalCandidate(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "local", "src.mkv"))
	cand := filepath.Join(extRoot, "newsub", "out.mkv")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
		c.StagingPolicy = StagingPolicyNever
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-ext-dest"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	seeded := pr6b1LoadJob(t, cfg.StateDir, id)
	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	if rec.encodeOuts[0] != seeded.LocalCandidatePath {
		t.Fatalf("encode output = %q, want local candidate %q", rec.encodeOuts[0], seeded.LocalCandidatePath)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Candidate != filepath.Clean(cand) {
		t.Fatalf("semantic Candidate = %q, want intended destination %q", got.Candidate, filepath.Clean(cand))
	}
	if got.FinalizationState != string(FinalizationStateCompleted) {
		t.Fatalf("FinalizationState = %q, want completed", got.FinalizationState)
	}
	if b, err := os.ReadFile(cand); err != nil || string(b) != "encoded-media" {
		t.Fatalf("destination content = %q err=%v, want encoded output", b, err)
	}
}

func TestPR6B2_EncodeCompletePersistedBeforeFinalization(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "local", "src.mkv"))
	cand := filepath.Join(extRoot, "out.mkv")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
		c.StagingPolicy = StagingPolicyNever
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-checkpoint"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	var atCheckpoint *JobRecord
	w.SetAfterEncodeCheckpoint(func(jobDir string, job *JobRecord) {
		atCheckpoint, _ = LoadJob(filepath.Join(jobDir, "job.json"))
	})
	var stateAtFinalize FinalizationState
	var encodeCompleteAtFinalize bool
	w.SetFinalizeOutput(func(ctx context.Context, localCandidate, destination, jobID string) error {
		loaded, _ := LoadJob(filepath.Join(cfg.StateDir, jobID, "job.json"))
		if loaded != nil {
			stateAtFinalize = FinalizationState(loaded.FinalizationState)
			encodeCompleteAtFinalize = loaded.EncodeComplete
		}
		return FinalizeOutputAtomic(ctx, localCandidate, destination, jobID)
	})

	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	if atCheckpoint == nil || !atCheckpoint.EncodeComplete {
		t.Fatalf("EncodeComplete must be persisted at the checkpoint: %+v", atCheckpoint)
	}
	if atCheckpoint.FinalizationState == string(FinalizationStateCompleted) {
		t.Fatalf("finalization must not be completed at the encode checkpoint")
	}
	if !encodeCompleteAtFinalize {
		t.Fatalf("EncodeComplete must be durable before finalization runs")
	}
	if stateAtFinalize != FinalizationStateFinalizing {
		t.Fatalf("state at finalize = %q, want finalizing", stateAtFinalize)
	}
}

func TestPR6B2_FinalizationPartialSemantics(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "local", "src.mkv"))
	cand := filepath.Join(extRoot, "out.mkv")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
		c.StagingPolicy = StagingPolicyNever
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-partial"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	ownPartial := PartialPathFor(filepath.Clean(cand), id)
	unrelatedPartial := filepath.Clean(cand) + ".partial.somebody-else"
	if err := os.MkdirAll(filepath.Dir(cand), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownPartial, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unrelatedPartial, []byte("unrelated"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	if b, err := os.ReadFile(cand); err != nil || string(b) != "encoded-media" {
		t.Fatalf("destination = %q err=%v, want encoded output", b, err)
	}
	if _, err := os.Stat(ownPartial); !os.IsNotExist(err) {
		t.Fatalf("own partial must be consumed by the no-clobber commit")
	}
	if b, err := os.ReadFile(unrelatedPartial); err != nil || string(b) != "unrelated" {
		t.Fatalf("unrelated partial must be preserved: %q err=%v", b, err)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.FinalizationState != string(FinalizationStateCompleted) {
		t.Fatalf("FinalizationState = %q, want completed", got.FinalizationState)
	}
}

func pr6b2SeedPostEncode(t *testing.T, cfg *WorkerConfig, id, localCandidate, destination string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(localCandidate), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localCandidate, []byte("encoded-media"), 0644); err != nil {
		t.Fatal(err)
	}
	pr6b2Seed(t, cfg.StateDir, &JobRecord{
		ID: id, Status: "running", PID: 0,
		Source: filepath.Join(cfg.StateDir, "src.mkv"), Candidate: destination,
		Profile: "hevc-vt", Plan: pr6b2Plan(t), ExecutionSpecDigest: pr5Digest,
		CreatedAt: time.Now().UTC(), Attempt: 1,
		EncodeComplete: true, FinalizationState: string(FinalizationStatePending),
		LocalCandidatePath: localCandidate, IntendedDestination: destination,
		PartialPath: PartialPathFor(destination, id),
	})
}

func TestPR6B2_RestartResumeFinalizesWithoutReencode(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-resume"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, destination)

	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("resume InternalRun: %v", err)
	}
	if rec.encodeCalls != 0 || len(rec.probePaths) != 0 {
		t.Fatalf("resume must not probe/encode: encodes=%d probes=%v", rec.encodeCalls, rec.probePaths)
	}
	if b, err := os.ReadFile(destination); err != nil || string(b) != "encoded-media" {
		t.Fatalf("destination = %q err=%v, want finalized output", b, err)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "completed" || got.FinalizationState != string(FinalizationStateCompleted) {
		t.Fatalf("resumed job = %+v, want completed/finalization completed", got)
	}
}

func TestPR6B2_ResumeScanSpawnsOnlyPostEncode(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	stub := newStubSpawn()
	stub.install(w)

	const postID = "job-6b2-resume-scan"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", postID, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, postID, localCandidate, destination)

	const queuedID = "job-6b2-queued-idle"
	pr6b2Seed(t, cfg.StateDir, &JobRecord{
		ID: queuedID, Status: "queued", Source: filepath.Join(tempDir, "src.mkv"),
		Candidate: filepath.Join(tempDir, "q.mkv"), Profile: "hevc-vt",
		ExecutionSpecDigest: pr5Digest, CreatedAt: time.Now().UTC(),
	})

	started, err := w.ResumePostEncode(context.Background(), "test-exe", "")
	if err != nil {
		t.Fatalf("ResumePostEncode: %v", err)
	}
	if started != 1 {
		t.Fatalf("resume started %d runners, want exactly 1", started)
	}
	if stub.n() != 1 {
		t.Fatalf("spawn count = %d, want 1", stub.n())
	}
	post := pr6b1LoadJob(t, cfg.StateDir, postID)
	if post.Status != "running" || post.PID == 0 {
		t.Fatalf("post-encode job must be claimed for finalization resume: %+v", post)
	}
	queued := pr6b1LoadJob(t, cfg.StateDir, queuedID)
	if queued.Status != "queued" || queued.PID != 0 {
		t.Fatalf("queued job must be untouched by the resume scan: %+v", queued)
	}
}

func TestPR6B2_FinalizationFailureResumesWithoutReencode(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-fin-fail"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, destination)

	finalizeCalls := 0
	w.SetFinalizeOutput(func(ctx context.Context, local, dest, jobID string) error {
		finalizeCalls++
		if finalizeCalls == 1 {
			return fmt.Errorf("%w: injected destination failure", ErrStorageIO)
		}
		return FinalizeOutputAtomic(ctx, local, dest, jobID)
	})

	if err := pr6b2Run(t, w, id); err == nil {
		t.Fatalf("first finalization failure must surface an error")
	}
	failed := pr6b1LoadJob(t, cfg.StateDir, id)
	if failed.Status != "running" || failed.PID != 0 || !failed.EncodeComplete {
		t.Fatalf("failed finalization must stay nonterminal/resumable: %+v", failed)
	}
	if failed.FailureClassification != FailureStorageFinalization {
		t.Fatalf("classification = %q, want %q", failed.FailureClassification, FailureStorageFinalization)
	}
	if _, err := os.Stat(localCandidate); err != nil {
		t.Fatalf("local candidate must be preserved on finalization failure: %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("destination must not exist after a finalization failure")
	}
	if rec.encodeCalls != 0 {
		t.Fatalf("encode must never run for a post-encode resume, got %d", rec.encodeCalls)
	}

	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("second resume must succeed: %v", err)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "completed" || got.FinalizationState != string(FinalizationStateCompleted) {
		t.Fatalf("second resume job = %+v, want completed", got)
	}
	if b, err := os.ReadFile(destination); err != nil || string(b) != "encoded-media" {
		t.Fatalf("destination = %q err=%v, want finalized output", b, err)
	}
	if rec.encodeCalls != 0 {
		t.Fatalf("second resume must not encode, got %d", rec.encodeCalls)
	}
}

func TestPR6B2_CancelledPostEncodeDoesNotPublish(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "local", "src.mkv"))
	cand := filepath.Join(extRoot, "out.mkv")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
		c.StagingPolicy = StagingPolicyNever
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-cancel-postencode"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	jobFile := filepath.Join(cfg.StateDir, id, "job.json")
	w.SetAfterEncodeCheckpoint(func(jobDir string, job *JobRecord) {
		// Simulate a concurrent Cancel between the encode checkpoint and
		// finalization without signalling the in-process test binary.
		latest, err := LoadJob(jobFile)
		if err != nil || latest == nil {
			t.Fatalf("reloading job in cancel hook: %v", err)
		}
		latest.Status = "cancelled"
		latest.FinishedAt = time.Now().UTC()
		if err := SaveJobAtomic(jobFile, latest); err != nil {
			t.Fatalf("persisting simulated cancel: %v", err)
		}
	})

	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	if _, err := os.Stat(cand); !os.IsNotExist(err) {
		t.Fatalf("cancelled post-encode job must not publish the destination")
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
	if rec.encodeCalls != 1 {
		t.Fatalf("encode calls = %d, want 1 (cancel happened after encode)", rec.encodeCalls)
	}
}

func TestPR6B2_MalformedStateFailsBeforeMutations(t *testing.T) {
	extRoot := func(tempDir string) string { return filepath.Join(tempDir, "external") }

	t.Run("unknown staging", func(t *testing.T) {
		tempDir := t.TempDir()
		cfg := pr6b2Config(tempDir, func(c *WorkerConfig) { c.ExternalRoots = []string{extRoot(tempDir)} })
		w := NewWorker(cfg)
		newStubSpawn().install(w)
		rec := &pr6b2Recorder{createOut: true}
		rec.install(w)

		const id = "job-6b2-bad-staging"
		src := pr6b1WriteFile(t, filepath.Join(tempDir, "src.mkv"))
		pr6b2Seed(t, cfg.StateDir, &JobRecord{
			ID: id, Status: "queued", Source: src, Candidate: filepath.Join(tempDir, "out.mkv"),
			Profile: "hevc-vt", Plan: pr6b2Plan(t), ExecutionSpecDigest: pr5Digest,
			CreatedAt: time.Now().UTC(), StagingState: "bogus",
		})
		err := pr6b2Run(t, w, id)
		if !errors.Is(err, ErrInvalidStagingState) {
			t.Fatalf("error = %v, want ErrInvalidStagingState", err)
		}
		got := pr6b1LoadJob(t, cfg.StateDir, id)
		if got.Status != "queued" || got.PID != 0 {
			t.Fatalf("malformed job must be unmutated: %+v", got)
		}
		if rec.encodeCalls != 0 || len(rec.probePaths) != 0 {
			t.Fatalf("no probe/encode may run on malformed state")
		}
	})

	t.Run("unknown finalization", func(t *testing.T) {
		tempDir := t.TempDir()
		extRootDir := extRoot(tempDir)
		cfg := pr6b2Config(tempDir, func(c *WorkerConfig) { c.ExternalRoots = []string{extRootDir} })
		w := NewWorker(cfg)
		newStubSpawn().install(w)
		rec := &pr6b2Recorder{createOut: true}
		rec.install(w)

		const id = "job-6b2-bad-finalization"
		localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
		pr6b2SeedPostEncode(t, cfg, id, localCandidate, filepath.Join(extRootDir, "out.mkv"))
		// Corrupt the finalization state after seeding a valid record.
		seed := pr6b1LoadJob(t, cfg.StateDir, id)
		seed.FinalizationState = "bogus"
		pr6b2Seed(t, cfg.StateDir, seed)

		err := pr6b2Run(t, w, id)
		if !errors.Is(err, ErrInvalidFinalizationState) {
			t.Fatalf("error = %v, want ErrInvalidFinalizationState", err)
		}
		if _, err := os.Stat(filepath.Join(extRootDir, "out.mkv")); !os.IsNotExist(err) {
			t.Fatalf("malformed finalization must not publish")
		}
		if rec.encodeCalls != 0 {
			t.Fatalf("no encode may run on malformed state")
		}
	})

	t.Run("staging missing path", func(t *testing.T) {
		tempDir := t.TempDir()
		cfg := pr6b2Config(tempDir, func(c *WorkerConfig) { c.ExternalRoots = []string{extRoot(tempDir)} })
		w := NewWorker(cfg)
		newStubSpawn().install(w)
		rec := &pr6b2Recorder{createOut: true}
		rec.install(w)

		const id = "job-6b2-missing-staged-path"
		src := pr6b1WriteFile(t, filepath.Join(tempDir, "src.mkv"))
		pr6b2Seed(t, cfg.StateDir, &JobRecord{
			ID: id, Status: "queued", Source: src, Candidate: filepath.Join(tempDir, "out.mkv"),
			Profile: "hevc-vt", Plan: pr6b2Plan(t), ExecutionSpecDigest: pr5Digest,
			CreatedAt: time.Now().UTC(), StagingState: string(StagingStatePending),
		})
		if err := pr6b2Run(t, w, id); err == nil {
			t.Fatalf("pending staging without a staged path must fail closed")
		}
		if rec.encodeCalls != 0 {
			t.Fatalf("no encode may run on malformed state")
		}
	})
}

func TestPR6B2_SchedulerNeverReencodesPostEncode(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) { c.ExternalRoots = []string{extRoot} })
	w := NewWorker(cfg)
	stub := newStubSpawn()
	stub.install(w)

	const id = "job-6b2-scheduler-postencode"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, destination)

	if _, err := w.ScheduleQueued(context.Background(), "test-exe", ""); err != nil {
		t.Fatalf("ScheduleQueued: %v", err)
	}
	if stub.n() != 0 {
		t.Fatalf("scheduler must not spawn a running post-encode job, spawned %d", stub.n())
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "running" || got.PID != 0 || got.FailureClassification == "runner_killed" {
		t.Fatalf("scheduler must leave post-encode job resumable: %+v", got)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("scheduler must never finalize")
	}
}

func TestPR6B2_SemanticIdentityUnchanged(t *testing.T) {
	tempDir := t.TempDir()
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "src.mkv"))
	cand := filepath.Join(tempDir, "out.mkv")
	cfg := pr6b2Config(tempDir, nil)
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-semantics"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	before := pr6b1LoadJob(t, cfg.StateDir, id)
	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	after := pr6b1LoadJob(t, cfg.StateDir, id)

	if after.Source != before.Source || after.Candidate != before.Candidate {
		t.Fatalf("semantic paths changed: %q/%q -> %q/%q", before.Source, before.Candidate, after.Source, after.Candidate)
	}
	if after.ExecutionSpecDigest != before.ExecutionSpecDigest {
		t.Fatalf("execution-spec digest changed: %q -> %q", before.ExecutionSpecDigest, after.ExecutionSpecDigest)
	}
	if after.Plan == nil || before.Plan == nil || after.Plan.PlanDigest != before.Plan.PlanDigest {
		t.Fatalf("PlanDigest changed")
	}
	want, err := transcode.DigestTranscodeExecutionSpec(after.Source, after.Candidate, after.Profile, after.Plan)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if after.ExecutionSpecDigest != want {
		t.Fatalf("execution-spec digest = %q, want %q", after.ExecutionSpecDigest, want)
	}
}

func TestPR6B2_ResumeScanSurfacesLoadAndIDErrors(t *testing.T) {
	t.Run("load error", func(t *testing.T) {
		tempDir := t.TempDir()
		cfg := pr6b2Config(tempDir, nil)
		w := NewWorker(cfg)
		newStubSpawn().install(w)

		const id = "job-6b2-bad-json"
		jobDir := filepath.Join(cfg.StateDir, id)
		if err := os.MkdirAll(jobDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(jobDir, "job.json"), []byte("{not-json"), 0644); err != nil {
			t.Fatal(err)
		}
		started, err := w.ResumePostEncode(context.Background(), "test-exe", "")
		if err == nil {
			t.Fatal("a malformed candidate job record must surface an error")
		}
		if started != 0 {
			t.Fatalf("started = %d, want 0", started)
		}
		if !strings.Contains(err.Error(), id) {
			t.Fatalf("error must be contextual, got %v", err)
		}
	})

	t.Run("id mismatch", func(t *testing.T) {
		tempDir := t.TempDir()
		cfg := pr6b2Config(tempDir, nil)
		w := NewWorker(cfg)
		newStubSpawn().install(w)

		const dirName = "job-6b2-id-mismatch"
		jobDir := filepath.Join(cfg.StateDir, dirName)
		if err := os.MkdirAll(jobDir, 0755); err != nil {
			t.Fatal(err)
		}
		raw := `{"id":"some-other-id","status":"running","source":"/x","candidate":"/y"}`
		if err := os.WriteFile(filepath.Join(jobDir, "job.json"), []byte(raw), 0644); err != nil {
			t.Fatal(err)
		}
		started, err := w.ResumePostEncode(context.Background(), "test-exe", "")
		if err == nil {
			t.Fatal("an id/directory mismatch must surface an error")
		}
		if started != 0 {
			t.Fatalf("started = %d, want 0", started)
		}
	})
}

func TestPR6B2_ResumeScanSurfacesSpawnError(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) { c.ExternalRoots = []string{extRoot} })
	w := NewWorker(cfg)
	w.SetTranscodeSpawner(func(selfExe, configPath, jobID string) (int, string, error) {
		return 0, "", fmt.Errorf("spawn boom")
	})

	const id = "job-6b2-spawn-fail"
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, filepath.Join(extRoot, "out.mkv"))

	started, err := w.ResumePostEncode(context.Background(), "test-exe", "")
	if err == nil || !strings.Contains(err.Error(), "spawn boom") {
		t.Fatalf("spawn failure must surface, got %v", err)
	}
	if started != 0 {
		t.Fatalf("started = %d, want 0", started)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.PID != 0 || got.Status != "running" {
		t.Fatalf("record must be untouched on spawn failure: %+v", got)
	}
}

func TestPR6B2_ResumeScanSurfacesSaveError(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) { c.ExternalRoots = []string{extRoot} })
	w := NewWorker(cfg)
	w.SetTranscodeSpawner(func(selfExe, configPath, jobID string) (int, string, error) {
		return 1 << 30, "stub-start", nil
	})

	const id = "job-6b2-save-fail"
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, filepath.Join(extRoot, "out.mkv"))

	jobDir := filepath.Join(cfg.StateDir, id)
	// Pre-create the lock file so acquireJobLock succeeds on a read-only dir.
	if err := os.WriteFile(filepath.Join(jobDir, ".lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(jobDir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(jobDir, 0755) })

	started, err := w.ResumePostEncode(context.Background(), "test-exe", "")
	if err == nil {
		t.Fatal("a runner identity persistence failure must surface")
	}
	if started != 0 {
		t.Fatalf("started = %d, want 0", started)
	}
}

func TestPR6B2_ResumeScanContinuesPastBadRecord(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) { c.ExternalRoots = []string{extRoot} })
	w := NewWorker(cfg)
	stub := newStubSpawn()
	stub.install(w)

	// A malformed record that must not strand the valid resume candidate.
	badDir := filepath.Join(cfg.StateDir, "job-6b2-bad-record")
	if err := os.MkdirAll(badDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "job.json"), []byte("{bad"), 0644); err != nil {
		t.Fatal(err)
	}

	const goodID = "job-6b2-good-resume"
	localCandidate := filepath.Join(cfg.StateDir, "_work", goodID, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, goodID, localCandidate, filepath.Join(extRoot, "out.mkv"))

	started, err := w.ResumePostEncode(context.Background(), "test-exe", "")
	if err == nil {
		t.Fatal("the malformed record must still be surfaced")
	}
	if started != 1 {
		t.Fatalf("started = %d, want 1 (scan continues past the bad record)", started)
	}
	if stub.n() != 1 {
		t.Fatalf("spawn count = %d, want 1", stub.n())
	}
	good := pr6b1LoadJob(t, cfg.StateDir, goodID)
	if good.PID == 0 {
		t.Fatalf("valid post-encode job must be claimed despite the bad record: %+v", good)
	}
}

func TestPR6B2_ResumeScanDoesNotClobberChildState(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) { c.ExternalRoots = []string{extRoot} })
	w := NewWorker(cfg)

	const id = "job-6b2-child-race"
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	destination := filepath.Join(extRoot, "out.mkv")
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, destination)
	jobFile := filepath.Join(cfg.StateDir, id, "job.json")

	// Simulate the child runner claiming and completing finalization BEFORE the
	// parent stamps the spawn identity. The parent must not overwrite this with
	// its stale pre-spawn snapshot.
	w.SetTranscodeSpawner(func(selfExe, configPath, jobID string) (int, string, error) {
		child := pr6b1LoadJob(t, cfg.StateDir, jobID)
		child.PID = 0
		child.Status = "completed"
		child.FinishedAt = time.Now().UTC()
		child.FinalizationState = string(FinalizationStateCompleted)
		if err := SaveJobAtomic(jobFile, child); err != nil {
			t.Fatalf("simulating child completion: %v", err)
		}
		return 1 << 30, "child-start", nil
	})

	started, err := w.ResumePostEncode(context.Background(), "test-exe", "")
	if err != nil {
		t.Fatalf("ResumePostEncode: %v", err)
	}
	if started != 1 {
		t.Fatalf("started = %d, want 1", started)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "completed" {
		t.Fatalf("child completion must not be clobbered, status = %q", got.Status)
	}
	if got.FinalizationState != string(FinalizationStateCompleted) {
		t.Fatalf("child finalization state must be preserved, got %q", got.FinalizationState)
	}
	if got.FinishedAt.IsZero() {
		t.Fatalf("child finished_at must be preserved")
	}
}

func TestPR6B2_LegacyBlankOperationalFieldsLocalCompatible(t *testing.T) {
	tempDir := t.TempDir()
	cfg := pr6b2Config(tempDir, nil)
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-legacy-raw"
	jobDir := filepath.Join(cfg.StateDir, id)
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(tempDir, "src.mkv")
	cand := filepath.Join(tempDir, "out.mkv")
	raw := fmt.Sprintf(`{"id":%q,"status":"queued","source":%q,"candidate":%q,"profile":"hevc-vt","execution_spec_digest":%q}`,
		id, src, cand, pr5Digest)
	if err := os.WriteFile(filepath.Join(jobDir, "job.json"), []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("dummy-media"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	if rec.encodeIns[0] != src || rec.encodeOuts[0] != cand {
		t.Fatalf("legacy encode paths = %q -> %q, want source -> candidate", rec.encodeIns[0], rec.encodeOuts[0])
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "completed" {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if got.EffectiveInput() != src || got.LocalCandidate() != cand || got.Destination() != cand {
		t.Fatalf("legacy operational helpers must fall back to semantic paths: %+v", got)
	}
}
