package transcodeworker

// Phase 6B2 tests: the operational execution state machine (staging, encode to
// LocalCandidate, the EncodeComplete checkpoint, and destination finalization).
// Deterministic without real ffmpeg/ffprobe via the injected probe/encoder
// seams. These tests also prove the semantic Source/Candidate/PlanDigest and
// execution-spec digest are never rewritten by operational execution.

import (
	"bytes"
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
	// Full candidate validation probes the worker-local candidate before
	// publish, so two probes are expected: the staged source and the local
	// candidate. The first must be the staged input.
	if len(rec.probePaths) != 2 || rec.probePaths[0] != wantStaged {
		t.Fatalf("probe paths = %v, want [staged input %q, local candidate]", rec.probePaths, wantStaged)
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
	if failed.FailureClassification != "storage_io_error" {
		t.Fatalf("classification = %q, want storage_io_error", failed.FailureClassification)
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

// TestPR6B2_FinalizeFallsBackToExclusiveCopyWhenHardLinksUnsupported drives the
// full post-encode finalization on a simulated macOS SMB/smbfs destination
// where neither an exclusive rename nor hard links work, and proves the job
// still reaches a completed/finalization-completed terminal state.
func TestPR6B2_FinalizeFallsBackToExclusiveCopyWhenHardLinksUnsupported(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-smb-fallback"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, destination)

	w.SetFinalizeOutput(func(ctx context.Context, local, dest, jobID string) error {
		return finalizeOutputAtomicWith(ctx, local, dest, jobID, nil, smbCommitFallback())
	})

	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	if rec.encodeCalls != 0 {
		t.Fatalf("resume must not encode, got %d", rec.encodeCalls)
	}
	if b, err := os.ReadFile(destination); err != nil || string(b) != "encoded-media" {
		t.Fatalf("destination = %q err=%v, want finalized output", b, err)
	}
	if exists, _ := pathExists(PartialPathFor(destination, id)); exists {
		t.Fatal("partial must be consumed by the exclusive-copy finalization")
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "completed" || got.FinalizationState != string(FinalizationStateCompleted) {
		t.Fatalf("job = %+v, want completed/finalization completed", got)
	}
}

// TestPR6B2_ExclusiveCopyFinalizationFailureResumesToCompleted proves that a
// failed exclusive-copy publish leaves the job resumable with the storage
// classification, exposes no destination, and succeeds on retry.
func TestPR6B2_ExclusiveCopyFinalizationFailureResumesToCompleted(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-smb-retry"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, destination)

	attempts := 0
	w.SetFinalizeOutput(func(ctx context.Context, local, dest, jobID string) error {
		attempts++
		if attempts == 1 {
			return fmt.Errorf("%w: simulated smb publish failure", ErrStorageIO)
		}
		return finalizeOutputAtomicWith(ctx, local, dest, jobID, nil, smbCommitFallback())
	})

	if err := pr6b2Run(t, w, id); err == nil {
		t.Fatal("first finalization failure must surface an error")
	}
	failed := pr6b1LoadJob(t, cfg.StateDir, id)
	if failed.Status != "running" || failed.PID != 0 || !failed.EncodeComplete {
		t.Fatalf("failed finalization must stay nonterminal/resumable: %+v", failed)
	}
	if failed.FailureClassification != "storage_io_error" {
		t.Fatalf("classification = %q, want storage_io_error", failed.FailureClassification)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("destination must not exist after a failed exclusive-copy publish")
	}

	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("second resume must succeed: %v", err)
	}
	if b, err := os.ReadFile(destination); err != nil || string(b) != "encoded-media" {
		t.Fatalf("destination = %q err=%v, want finalized output", b, err)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "completed" || got.FinalizationState != string(FinalizationStateCompleted) {
		t.Fatalf("second resume job = %+v, want completed", got)
	}
	if rec.encodeCalls != 0 {
		t.Fatalf("finalization resume must never encode, got %d", rec.encodeCalls)
	}
}

// TestPR6B2_AmbiguousPublicationRealStoragePathThenResumesCompleted drives the
// REAL failing storage path (the exclusive-copy fallback whose post-close
// visibility cannot be established), proves the durable ambiguous
// classification is produced, then resumes and completes by byte-for-byte
// content equality without ever re-encoding.
func TestPR6B2_AmbiguousPublicationRealStoragePathThenResumesCompleted(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-ambiguous-real"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, destination)

	// Force the real exclusive-copy fallback, then make its post-close
	// destination stat persistently unobservable (the smbfs race).
	w.SetFinalizeOutput(func(ctx context.Context, local, dest, jobID string) error {
		return finalizeOutputAtomicWith(ctx, local, dest, jobID, nil, smbCommitFallback())
	})

	origStat, origSleep := statPath, sleepPath
	defer func() { statPath, sleepPath = origStat, origSleep }()
	var seam verifySeam
	statPath = func(p string) (os.FileInfo, error) {
		seam.calls++
		if p == destination {
			return nil, transientENOENT(p)
		}
		return os.Stat(p)
	}
	sleepPath = func(time.Duration) { seam.sleeps++ }

	err := pr6b2Run(t, w, id)
	if err == nil {
		t.Fatal("ambiguous publication must surface an error")
	}
	if !errors.Is(err, ErrAmbiguousPublication) {
		t.Fatalf("error = %v, want ErrAmbiguousPublication", err)
	}
	failed := pr6b1LoadJob(t, cfg.StateDir, id)
	if failed.Status != "running" || failed.PID != 0 || !failed.EncodeComplete {
		t.Fatalf("ambiguous failure must stay resumable: %+v", failed)
	}
	if failed.FinalizationState != string(FinalizationStateFinalizing) {
		t.Fatalf("FinalizationState = %q, want finalizing", failed.FinalizationState)
	}
	if failed.FailureClassification != FailureStoragePublicationAmbiguous {
		t.Fatalf("classification = %q, want %q", failed.FailureClassification, FailureStoragePublicationAmbiguous)
	}
	// The real copy wrote the bytes; only observation failed, so the
	// destination is preserved for content-equality recovery.
	content, rerr := os.ReadFile(localCandidate)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if b, rerr := os.ReadFile(destination); rerr != nil || !bytes.Equal(b, content) {
		t.Fatalf("destination = %q (%v), want preserved encoded output", b, rerr)
	}
	if _, err := os.Stat(localCandidate); err != nil {
		t.Fatalf("local candidate must be preserved: %v", err)
	}
	if rec.encodeCalls != 0 {
		t.Fatalf("ambiguous publication must never re-encode, got %d", rec.encodeCalls)
	}

	// Resume with a healthy stat: the ambiguous destination is byte-identical
	// to the candidate, so the job completes without re-encoding.
	statPath, sleepPath = origStat, origSleep
	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("recovery InternalRun: %v", err)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "completed" || got.FinalizationState != string(FinalizationStateCompleted) {
		t.Fatalf("recovered job = %+v, want completed", got)
	}
	if got.FailureClassification != "" {
		t.Fatalf("classification = %q, want cleared on completion", got.FailureClassification)
	}
	if b, rerr := os.ReadFile(destination); rerr != nil || !bytes.Equal(b, content) {
		t.Fatalf("destination = %q (%v), want unchanged encoded output", b, rerr)
	}
	if rec.encodeCalls != 0 {
		t.Fatalf("recovery must never re-encode, got %d", rec.encodeCalls)
	}
	if _, err := os.Stat(localCandidate); !os.IsNotExist(err) {
		t.Fatalf("local candidate must be cleaned after completion: %v", err)
	}
}

// legacyOldAmbiguousError reconstructs, independently of the production
// recognizer, the exact error an old worker persisted for this job's own
// partial and destination when the exclusive-copy post-close stat saw ENOENT.
func legacyOldAmbiguousError(partial, destination string) string {
	return fmt.Sprintf(
		"finalization failed: transcode storage i/o failure: publishing %s to %s: transcode storage i/o failure: statting %s: stat %s: no such file or directory",
		partial, destination, destination, destination,
	)
}

// seedLegacyDifferingDestination seeds a finalizing/storage_finalization_failed
// job with a same-size destination whose tail is zero-filled (models a partial
// write), an intact own partial identical to the candidate, and the supplied
// persisted error. It returns the candidate content.
func seedLegacyDifferingDestination(t *testing.T, cfg *WorkerConfig, id, destination, localCandidate, partial, persistedError string) []byte {
	t.Helper()
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, destination)
	content, err := os.ReadFile(localCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	diff := append([]byte(nil), content...)
	for i := len(diff) / 2; i < len(diff); i++ {
		diff[i] = 0
	}
	if len(diff) > 0 && bytes.Equal(diff, content) {
		t.Fatal("test setup requires a differing destination")
	}
	mustWriteFile(t, destination, diff, 0o644)
	mustWriteFile(t, partial, content, 0o644)
	seed := pr6b1LoadJob(t, cfg.StateDir, id)
	seed.FinalizationState = string(FinalizationStateFinalizing)
	seed.FailureClassification = FailureStorageFinalization
	seed.Error = persistedError
	pr6b2Seed(t, cfg.StateDir, seed)
	return content
}

// TestPR6B2_LegacyAmbiguousPublicationModelsE04 models the live E04 job: an old
// generic storage_finalization_failed job whose exact old ambiguous-publication
// error names its own partial and destination, whose own partial is intact and
// identical to the candidate, and whose destination is the same size but
// differs after a partial write. Recovery must remove only the proven own
// destination, republish from the candidate, and complete without re-encoding.
func TestPR6B2_LegacyAmbiguousPublicationModelsE04(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-act-transcode-media-3138633435326235"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	partial := PartialPathFor(destination, id)
	content := seedLegacyDifferingDestination(t, cfg, id, destination, localCandidate, partial, legacyOldAmbiguousError(partial, destination))

	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("E04 recovery: %v", err)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "completed" || got.FinalizationState != string(FinalizationStateCompleted) {
		t.Fatalf("recovered E04 job = %+v, want completed", got)
	}
	if got.FailureClassification != "" {
		t.Fatalf("classification = %q, want cleared", got.FailureClassification)
	}
	if b, rerr := os.ReadFile(destination); rerr != nil || !bytes.Equal(b, content) {
		t.Fatalf("destination = %q (%v), want republished candidate", b, rerr)
	}
	if rec.encodeCalls != 0 {
		t.Fatalf("E04 recovery must never re-encode, got %d", rec.encodeCalls)
	}
}

// TestPR6B2_TypedAmbiguousDifferentDestinationRepublished proves the typed
// marker alone authorises destructive recovery: a differing destination known
// to come from this job's O_EXCL publication is removed and republished.
func TestPR6B2_TypedAmbiguousDifferentDestinationRepublished(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-typed-different"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, destination)

	seed := pr6b1LoadJob(t, cfg.StateDir, id)
	seed.FinalizationState = string(FinalizationStateFinalizing)
	seed.FailureClassification = FailureStoragePublicationAmbiguous
	pr6b2Seed(t, cfg.StateDir, seed)

	content, err := os.ReadFile(localCandidate)
	if err != nil {
		t.Fatal(err)
	}
	unrelated := bytes.ToUpper(content)
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, destination, unrelated, 0o644)
	mustWriteFile(t, PartialPathFor(destination, id), content, 0o644)

	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("typed republish: %v", err)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "completed" {
		t.Fatalf("job = %+v, want completed", got)
	}
	if b, rerr := os.ReadFile(destination); rerr != nil || !bytes.Equal(b, content) {
		t.Fatalf("destination = %q (%v), want republished candidate", b, rerr)
	}
	if rec.encodeCalls != 0 {
		t.Fatalf("republish must never re-encode, got %d", rec.encodeCalls)
	}
}

// TestPR6B2_LegacyWrongPartialInErrorNoDeletion proves the recognizer is not a
// substring/partial heuristic: if the persisted error names a different partial
// path, no destructive recovery happens and the destination is left untouched.
func TestPR6B2_LegacyWrongPartialInErrorNoDeletion(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-legacy-wrong-partial"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	partial := PartialPathFor(destination, id)
	wrongPartial := PartialPathFor(destination, "someone-else")
	seedLegacyDifferingDestination(t, cfg, id, destination, localCandidate, partial, legacyOldAmbiguousError(wrongPartial, destination))

	diff, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	runErr := pr6b2Run(t, w, id)
	if runErr == nil || !IsDestinationExists(runErr) {
		t.Fatalf("error = %v, want ErrDestinationExists", runErr)
	}
	if b, rerr := os.ReadFile(destination); rerr != nil || !bytes.Equal(b, diff) {
		t.Fatalf("destination = %q (%v), must not be deleted", b, rerr)
	}
	if rec.encodeCalls != 0 {
		t.Fatalf("no deletion path must never re-encode, got %d", rec.encodeCalls)
	}
}

// TestPR6B2_LegacyWrongDestinationInErrorNoDeletion proves a persisted error
// naming a different destination never authorises deletion of this destination.
func TestPR6B2_LegacyWrongDestinationInErrorNoDeletion(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-legacy-wrong-dest"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	partial := PartialPathFor(destination, id)
	wrongDestination := filepath.Join(extRoot, "other.mkv")
	seedLegacyDifferingDestination(t, cfg, id, destination, localCandidate, partial, legacyOldAmbiguousError(partial, wrongDestination))

	diff, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	runErr := pr6b2Run(t, w, id)
	if runErr == nil || !IsDestinationExists(runErr) {
		t.Fatalf("error = %v, want ErrDestinationExists", runErr)
	}
	if b, rerr := os.ReadFile(destination); rerr != nil || !bytes.Equal(b, diff) {
		t.Fatalf("destination = %q (%v), must not be deleted", b, rerr)
	}
}

// TestPR6B2_LegacyNoExactSignatureNoDeletion proves a generic
// storage_finalization_failed job whose error is not the exact legacy
// ambiguous-publication shape never authorises deletion.
func TestPR6B2_LegacyNoExactSignatureNoDeletion(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-legacy-no-signature"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	partial := PartialPathFor(destination, id)
	seedLegacyDifferingDestination(t, cfg, id, destination, localCandidate, partial, "finalization failed: transcode storage i/o failure: some unrelated failure")

	diff, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	runErr := pr6b2Run(t, w, id)
	if runErr == nil || !IsDestinationExists(runErr) {
		t.Fatalf("error = %v, want ErrDestinationExists", runErr)
	}
	if b, rerr := os.ReadFile(destination); rerr != nil || !bytes.Equal(b, diff) {
		t.Fatalf("destination = %q (%v), must not be deleted", b, rerr)
	}
}

// TestPR6B2_LegacyCandidatePartialMismatchNoDeletion proves the exact legacy
// signature is not sufficient on its own: the own partial must still be
// byte-for-byte identical to the local candidate or no deletion occurs.
func TestPR6B2_LegacyCandidatePartialMismatchNoDeletion(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-legacy-partial-mismatch"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	partial := PartialPathFor(destination, id)
	content := seedLegacyDifferingDestination(t, cfg, id, destination, localCandidate, partial, legacyOldAmbiguousError(partial, destination))
	// Replace the intact partial with different content of the same size.
	mustWriteFile(t, partial, bytes.ToUpper(content), 0o644)

	diff, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	runErr := pr6b2Run(t, w, id)
	if runErr == nil || !IsDestinationExists(runErr) {
		t.Fatalf("error = %v, want ErrDestinationExists", runErr)
	}
	if b, rerr := os.ReadFile(destination); rerr != nil || !bytes.Equal(b, diff) {
		t.Fatalf("destination = %q (%v), must not be deleted", b, rerr)
	}
}

// TestRemoveAmbiguousDestinationIfSafeGuards proves destination removal refuses
// source/local-candidate/partial collisions and non-regular paths, while still
// removing a genuine proven destination (whose semantic Candidate normally
// equals the destination).
func TestRemoveAmbiguousDestinationIfSafeGuards(t *testing.T) {
	base := t.TempDir()
	w := NewWorker(pr6b2Config(base, nil))
	dest := filepath.Join(base, "dest.mkv")
	src := filepath.Join(base, "src.mkv")
	cand := filepath.Join(base, "cand.mkv")
	partial := filepath.Join(base, "dest.mkv.partial.job-1")
	for _, p := range []string{dest, src, cand, partial} {
		mustWriteFile(t, p, []byte("keep"), 0o644)
	}
	job := &JobRecord{Source: src, Candidate: dest}

	collisions := []struct {
		name    string
		dest    string
		partial string
		local   string
	}{
		{"source", src, partial, cand},
		{"local candidate", cand, partial, cand},
		{"own partial", partial, partial, cand},
	}
	for _, tc := range collisions {
		removed, err := w.removeAmbiguousDestinationIfSafe(job, &resolvedOperational{destination: tc.dest, localCandidate: tc.local, partial: tc.partial})
		if err == nil || removed {
			t.Fatalf("%s collision: removed=%v err=%v, want refusal", tc.name, removed, err)
		}
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source must survive: %v", err)
	}
	if _, err := os.Stat(cand); err != nil {
		t.Fatalf("local candidate must survive: %v", err)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("partial must survive: %v", err)
	}

	if removed, err := w.removeAmbiguousDestinationIfSafe(job, &resolvedOperational{destination: dest, localCandidate: cand, partial: partial}); err != nil || !removed {
		t.Fatalf("genuine destination: removed=%v err=%v, want true,nil", removed, err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("genuine destination must be removed: %v", err)
	}
}

// TestPR6B2_FinalizingWithoutFailureMarkerDoesNotReconcile proves the trust
// boundary: a durable finalizing state that was persisted before the publish
// call is NOT proof a publication was attempted. Even with a byte-identical
// destination and own partial present, recovery must not fire without a
// durable failure marker; strict no-clobber is preserved.
func TestPR6B2_FinalizingWithoutFailureMarkerDoesNotReconcile(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-finalizing-no-marker"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, destination)

	seed := pr6b1LoadJob(t, cfg.StateDir, id)
	seed.FinalizationState = string(FinalizationStateFinalizing)
	seed.FailureClassification = ""
	pr6b2Seed(t, cfg.StateDir, seed)

	content, err := os.ReadFile(localCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	destinationContent := append([]byte(nil), content...)
	mustWriteFile(t, destination, destinationContent, 0o644)
	mustWriteFile(t, PartialPathFor(destination, id), content, 0o644)

	err = pr6b2Run(t, w, id)
	if err == nil {
		t.Fatal("finalizing without the failure marker must not reconcile")
	}
	if !IsDestinationExists(err) {
		t.Fatalf("error = %v, want ErrDestinationExists", err)
	}
	if b, rerr := os.ReadFile(destination); rerr != nil || !bytes.Equal(b, destinationContent) {
		t.Fatalf("destination = %q (%v), must not be overwritten", b, rerr)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "running" || !got.EncodeComplete {
		t.Fatalf("job = %+v, want resumable running/encode-complete", got)
	}
	if _, err := os.Stat(localCandidate); err != nil {
		t.Fatalf("local candidate must be preserved on refusal: %v", err)
	}
	if rec.encodeCalls != 0 {
		t.Fatalf("refusal must never re-encode, got %d", rec.encodeCalls)
	}
}

// TestPR6B2_FreshConflictWithStalePartialNeverReconciles proves a normal fresh
// ErrDestinationExists conflict (same-size unrelated destination) is recorded
// as the generic failure and, on resume, still refuses because content differs.
// A stale own partial is cleaned when the conflict is recorded and never turns
// the conflict into success.
func TestPR6B2_FreshConflictWithStalePartialNeverReconciles(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-fresh-conflict"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, destination)

	content, err := os.ReadFile(localCandidate)
	if err != nil {
		t.Fatal(err)
	}
	sameSizeUnrelated := bytes.ToUpper(content)
	if len(sameSizeUnrelated) != len(content) || bytes.Equal(sameSizeUnrelated, content) {
		t.Fatalf("test setup requires an unrelated same-size destination")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, destination, sameSizeUnrelated, 0o644)
	// Stale own partial left by an earlier crash; the destination-exists
	// precheck returns before the finalizer would have cleaned it.
	mustWriteFile(t, PartialPathFor(destination, id), content, 0o644)

	err = pr6b2Run(t, w, id)
	if err == nil {
		t.Fatal("fresh conflict must fail")
	}
	if !IsDestinationExists(err) {
		t.Fatalf("first run error = %v, want ErrDestinationExists", err)
	}
	failed := pr6b1LoadJob(t, cfg.StateDir, id)
	if failed.FinalizationState != string(FinalizationStateFinalizing) ||
		failed.FailureClassification != "idempotency_conflict" || !failed.NextFinalizationAt.IsZero() {
		t.Fatalf("recorded failure state = %+v, want finalizing/idempotency_conflict without scheduled retry", failed)
	}
	if exists, _ := pathExists(PartialPathFor(destination, id)); exists {
		t.Fatal("a recorded conflict must clean this job's stale own partial")
	}

	err = pr6b2Run(t, w, id)
	if err == nil {
		t.Fatal("resume must not reconcile an unrelated same-size destination")
	}
	if !IsDestinationExists(err) {
		t.Fatalf("resume error = %v, want ErrDestinationExists", err)
	}
	if b, rerr := os.ReadFile(destination); rerr != nil || !bytes.Equal(b, sameSizeUnrelated) {
		t.Fatalf("unrelated destination = %q (%v), must not be overwritten", b, rerr)
	}
	if rec.encodeCalls != 0 {
		t.Fatalf("conflict handling must never re-encode, got %d", rec.encodeCalls)
	}
}

// TestPR6B2_UnrelatedPartialIgnoredAndPreserved proves only this job's exact
// partial is ever targeted. A byte-identical destination completes by content
// equality regardless of an unrelated partial, and the unrelated partial is
// left untouched.
func TestPR6B2_UnrelatedPartialIgnoredAndPreserved(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-unrelated-partial"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, destination)

	seed := pr6b1LoadJob(t, cfg.StateDir, id)
	seed.FinalizationState = string(FinalizationStateFinalizing)
	seed.FailureClassification = FailureStoragePublicationAmbiguous
	pr6b2Seed(t, cfg.StateDir, seed)

	content, err := os.ReadFile(localCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, destination, content, 0o644)
	unrelated := PartialPathFor(destination, "somebody-else")
	mustWriteFile(t, unrelated, []byte("keep me"), 0o644)

	if err := pr6b2Run(t, w, id); err != nil {
		t.Fatalf("content-equal recovery: %v", err)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "completed" {
		t.Fatalf("job = %+v, want completed", got)
	}
	if b, rerr := os.ReadFile(unrelated); rerr != nil || string(b) != "keep me" {
		t.Fatalf("unrelated partial = %q (%v), must be preserved", b, rerr)
	}
	if rec.encodeCalls != 0 {
		t.Fatalf("recovery must never re-encode, got %d", rec.encodeCalls)
	}
}

// TestPR6B2_SymlinkedOwnPartialCleanedWithoutFollowing proves partial cleanup
// never follows a symlink: in an unproven conflict (generic failure with no
// legacy signature), the symlink at this job's exact partial path is removed as
// a link and the target it pointed at (the local candidate) is preserved.
func TestPR6B2_SymlinkedOwnPartialCleanedWithoutFollowing(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)

	const id = "job-6b2-symlink-partial"
	destination := filepath.Join(extRoot, "out.mkv")
	localCandidate := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	pr6b2SeedPostEncode(t, cfg, id, localCandidate, destination)

	seed := pr6b1LoadJob(t, cfg.StateDir, id)
	seed.FinalizationState = string(FinalizationStateFinalizing)
	seed.FailureClassification = FailureStorageFinalization
	seed.Error = "finalization failed: transcode storage i/o failure: unrelated"
	pr6b2Seed(t, cfg.StateDir, seed)

	content, err := os.ReadFile(localCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, destination, bytes.ToUpper(content), 0o644)
	partial := PartialPathFor(destination, id)
	if err := os.Symlink(localCandidate, partial); err != nil {
		t.Fatal(err)
	}

	err = pr6b2Run(t, w, id)
	if err == nil {
		t.Fatal("an unproven differing destination must remain a conflict")
	}
	if !IsDestinationExists(err) {
		t.Fatalf("error = %v, want ErrDestinationExists", err)
	}
	if li, lerr := os.Lstat(partial); lerr == nil && li.Mode()&os.ModeSymlink != 0 {
		t.Fatal("symlinked own partial must be removed")
	}
	if b, rerr := os.ReadFile(localCandidate); rerr != nil || !bytes.Equal(b, content) {
		t.Fatalf("symlink target (candidate) must be preserved: %q (%v)", b, rerr)
	}
	if rec.encodeCalls != 0 {
		t.Fatalf("refusal must never re-encode, got %d", rec.encodeCalls)
	}
}

// TestRemoveOwnPartialIfSafeGuards proves partial cleanup refuses to touch a
// path that collides with the semantic source, semantic candidate, or intended
// destination, while still removing a genuine own partial.
func TestRemoveOwnPartialIfSafeGuards(t *testing.T) {
	base := t.TempDir()
	w := NewWorker(pr6b2Config(base, nil))
	dest := filepath.Join(base, "dest.mkv")
	src := filepath.Join(base, "src.mkv")
	cand := filepath.Join(base, "cand.mkv")
	for _, p := range []string{dest, src, cand} {
		mustWriteFile(t, p, []byte("keep"), 0o644)
	}
	job := &JobRecord{Source: src, Candidate: cand}

	for _, tc := range []struct {
		name    string
		partial string
	}{
		{"destination", dest},
		{"source", src},
		{"candidate", cand},
	} {
		w.removeOwnPartialIfSafe(job, &resolvedOperational{destination: dest, partial: tc.partial})
		if _, err := os.Stat(tc.partial); err != nil {
			t.Fatalf("protected %s path must not be removed: %v", tc.name, err)
		}
	}

	ownPartial := PartialPathFor(dest, "job-1")
	mustWriteFile(t, ownPartial, []byte("temp"), 0o644)
	w.removeOwnPartialIfSafe(job, &resolvedOperational{destination: dest, partial: ownPartial})
	if _, err := os.Stat(ownPartial); !os.IsNotExist(err) {
		t.Fatalf("genuine own partial must be removed: %v", err)
	}
}
