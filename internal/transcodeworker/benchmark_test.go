package transcodeworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func createTestMediaSource(t *testing.T, dir, filename, content string) (string, string) {
	t.Helper()
	p := filepath.Join(dir, filename)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("failed creating test media file: %v", err)
	}
	sum := sha256.Sum256([]byte(content))
	return p, hex.EncodeToString(sum[:])
}

func validWorkerBenchmarkRequest(sourcePath string) transcode.BenchmarkRequest {
	return transcode.BenchmarkRequest{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-unit-test-01",
		SourcePath:      sourcePath,
		SourceDuration:  120.0,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 20.0},
			{Index: 1, StartSeconds: 60.0, DurationSeconds: 20.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "q60", Quality: 60, VideoProfile: "main10", PixelFormat: "p010le"},
			{ID: "q65", Quality: 65, VideoProfile: "main10", PixelFormat: "p010le"},
		},
	}
}

func TestBenchmarkSubmit_ValidationAndPathTraversal(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceFile, _ := createTestMediaSource(t, mediaDir, "source.mkv", "source-data-12345")

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 2,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	// 1. Source outside allowed roots
	reqOutside := validWorkerBenchmarkRequest("/outside/root/file.mkv")
	resp, err := worker.BenchmarkSubmit(ctx, reqOutside, "dummyExe", "")
	if err == nil || !strings.Contains(resp.Error, "outside allowed roots") {
		t.Fatalf("expected outside allowed roots error, got: %v (resp: %+v)", err, resp)
	}

	// 2. Source file does not exist
	reqNotExist := validWorkerBenchmarkRequest(filepath.Join(mediaDir, "nonexistent.mkv"))
	resp, err = worker.BenchmarkSubmit(ctx, reqNotExist, "dummyExe", "")
	if err == nil || !strings.Contains(resp.Error, "not accessible") {
		t.Fatalf("expected not accessible error, got: %v", err)
	}

	// 3. Source is directory
	reqDir := validWorkerBenchmarkRequest(mediaDir)
	resp, err = worker.BenchmarkSubmit(ctx, reqDir, "dummyExe", "")
	if err == nil || !strings.Contains(resp.Error, "directory") {
		t.Fatalf("expected directory error, got: %v", err)
	}

	// 4. Path traversal in job ID
	reqTraversal := validWorkerBenchmarkRequest(sourceFile)
	reqTraversal.ID = "bench-../../etc/passwd"
	resp, err = worker.BenchmarkSubmit(ctx, reqTraversal, "dummyExe", "")
	if err == nil || !strings.Contains(resp.Error, "path traversal") && !strings.Contains(err.Error(), "invalid benchmark request id") {
		t.Fatalf("expected path traversal error, got: %v", err)
	}
}

func TestBenchmarkSubmit_DuplicateIdempotencyAndCollision(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceFile, _ := createTestMediaSource(t, mediaDir, "source.mkv", "source-data-abc")

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 2,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	req := validWorkerBenchmarkRequest(sourceFile)

	// Inject test runner that does nothing to avoid external exec failure
	worker.SetBenchmarkRunner(&mockTestRunner{})

	// 1. First submit (using dummy executable path that won't fail since we pre-populate state or handle)
	// We can manually write the initial record to simulate detached worker start
	benchDir := filepath.Join(stateDir, req.ID)
	benchFile := filepath.Join(benchDir, "benchmark.json")
	reqDigest, _ := transcode.DigestBenchmarkRequest(&req)

	initialRecord := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              req.ID,
		Status:          "queued",
		Source:          sourceFile,
		Metric:          req.Metric,
		RequestDigest:   reqDigest,
		PID:             os.Getpid(),
	}
	if err := SaveBenchmarkAtomic(benchFile, initialRecord); err != nil {
		t.Fatalf("SaveBenchmarkAtomic failed: %v", err)
	}

	// 2. Duplicate submit with identical request must return idempotent queued status
	resp, err := worker.BenchmarkSubmit(ctx, req, "dummyExe", "")
	if err != nil {
		t.Fatalf("expected idempotent success, got error: %v", err)
	}
	if resp.Status != "queued" {
		t.Errorf("got status %q, want 'queued'", resp.Status)
	}

	// 3. Collision: submit different request with same job ID
	reqDifferent := req
	reqDifferent.Metric = "ssim" // change metric -> different digest
	respDiff, err := worker.BenchmarkSubmit(ctx, reqDifferent, "dummyExe", "")
	if err == nil || !strings.Contains(respDiff.Error, "collision") {
		t.Fatalf("expected collision error for different digest, got: %v (resp: %+v)", err, respDiff)
	}

	// 4. Collision with existing transcode job
	transcodeID := "bench-collision-transcode"
	tcJobDir := filepath.Join(stateDir, transcodeID)
	_ = os.MkdirAll(tcJobDir, 0o755)
	_ = os.WriteFile(filepath.Join(tcJobDir, "job.json"), []byte(`{"id": "bench-collision-transcode"}`), 0o644)

	reqColliding := req
	reqColliding.ID = transcodeID
	respTCCol, err := worker.BenchmarkSubmit(ctx, reqColliding, "dummyExe", "")
	if err == nil || !strings.Contains(respTCCol.Error, "collision") {
		t.Fatalf("expected collision error with transcode job, got: %v (resp: %+v)", err, respTCCol)
	}

	// 5. Conversely, transcode Submit must reject collision with benchmark job
	tcReq := SubmitRequest{
		ID:            req.ID, // existing benchmark job ID
		SourcePath:    sourceFile,
		CandidatePath: filepath.Join(mediaDir, "cand.mkv"),
	}
	tcResp, err := worker.Submit(ctx, tcReq, "dummyExe", "")
	if err == nil || !strings.Contains(tcResp.Error, "collision") {
		t.Fatalf("expected transcode submit collision with benchmark job, got: %v", err)
	}
}

func TestBenchmarkSubmit_WorkerBusySharedCapacity(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceFile, _ := createTestMediaSource(t, mediaDir, "source.mkv", "source-data")

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 1, // Only 1 job allowed across all types
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	// Simulate an active benchmark job running with current test process PID
	activeBenchID := "bench-active-slot"
	activeBenchDir := filepath.Join(stateDir, activeBenchID)
	activeBenchRecord := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              activeBenchID,
		Status:          "running",
		Source:          sourceFile,
		PID:             os.Getpid(), // current test process is alive
	}
	if err := SaveBenchmarkAtomic(filepath.Join(activeBenchDir, "benchmark.json"), activeBenchRecord); err != nil {
		t.Fatalf("failed saving active benchmark: %v", err)
	}

	// Try submitting another benchmark job -> should fail with worker busy
	newBenchReq := validWorkerBenchmarkRequest(sourceFile)
	newBenchReq.ID = "bench-new-job"
	resp, err := worker.BenchmarkSubmit(ctx, newBenchReq, "dummyExe", "")
	if err == nil || !strings.Contains(resp.Error, "worker busy") {
		t.Fatalf("expected worker busy for second benchmark, got: %v", err)
	}

	// Try submitting a transcode job -> should also fail with worker busy
	tcReq := SubmitRequest{
		ID:            "job-transcode-attempt",
		SourcePath:    sourceFile,
		CandidatePath: filepath.Join(mediaDir, "cand.mkv"),
	}
	tcResp, err := worker.Submit(ctx, tcReq, "dummyExe", "")
	if err == nil || !strings.Contains(tcResp.Error, "worker busy") {
		t.Fatalf("expected worker busy for transcode job, got: %v", err)
	}
}

func TestBenchmarkLifecycle_WorkspaceAndCleanup(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceContent := "very-important-original-media-data"
	sourceFile, origSHA := createTestMediaSource(t, mediaDir, "Original.Movie.mkv", sourceContent)
	origFI, _ := os.Stat(sourceFile)
	origModTime := origFI.ModTime()

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 2,
	}
	worker := NewWorker(cfg)

	jobID := "bench-lifecycle-workspace"
	jobDir := filepath.Join(stateDir, jobID)
	benchFile := filepath.Join(jobDir, "benchmark.json")
	samplesDir := filepath.Join(jobDir, "samples")

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "queued",
		Source:          sourceFile,
	}
	_ = SaveBenchmarkAtomic(benchFile, record)
	_ = os.MkdirAll(samplesDir, 0o755)

	// Put dummy scratch sample files in samples/
	sample1 := filepath.Join(samplesDir, "sample_0.mkv")
	_ = os.WriteFile(sample1, []byte("scratch-sample-0"), 0o644)

	// 1. Run CleanBenchmarkSamples
	if err := worker.CleanBenchmarkSamples(jobID); err != nil {
		t.Fatalf("CleanBenchmarkSamples failed: %v", err)
	}

	// 2. Verify samples directory was deleted
	if _, err := os.Stat(samplesDir); !os.IsNotExist(err) {
		t.Errorf("expected samples directory to be deleted, but it exists")
	}

	// 3. Verify jobDir and benchmark.json are preserved
	if _, err := os.Stat(benchFile); err != nil {
		t.Errorf("expected benchmark.json to be preserved, got error: %v", err)
	}

	// 4. Verify source file is completely UNTOUCHED
	currSHA, err := fileSHA256(sourceFile)
	if err != nil {
		t.Fatalf("fileSHA256 failed: %v", err)
	}
	if currSHA != origSHA {
		t.Fatalf("FATAL: source file content changed! orig=%s, curr=%s", origSHA, currSHA)
	}
	currFI, _ := os.Stat(sourceFile)
	if !currFI.ModTime().Equal(origModTime) {
		t.Fatalf("FATAL: source file mod time changed!")
	}
}

func TestBenchmarkLifecycle_Cancellation(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceFile, _ := createTestMediaSource(t, mediaDir, "source.mkv", "source-content")

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 2,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	jobID := "bench-cancel-test"
	jobDir := filepath.Join(stateDir, jobID)
	benchFile := filepath.Join(jobDir, "benchmark.json")
	samplesDir := filepath.Join(jobDir, "samples")
	_ = os.MkdirAll(samplesDir, 0o755)
	_ = os.WriteFile(filepath.Join(samplesDir, "temp.mkv"), []byte("temp"), 0o644)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "running",
		Source:          sourceFile,
		PID:             1, // dummy PID
	}
	_ = SaveBenchmarkAtomic(benchFile, record)

	// Cancel
	resp, err := worker.BenchmarkCancel(ctx, jobID)
	if err != nil {
		t.Fatalf("BenchmarkCancel failed: %v", err)
	}
	if resp.Status != "cancelled" {
		t.Errorf("expected status 'cancelled', got %q", resp.Status)
	}

	// Verify samples directory was deleted
	if _, err := os.Stat(samplesDir); !os.IsNotExist(err) {
		t.Errorf("expected samples directory to be deleted on cancel")
	}

	// Verify status updated in benchmark.json
	updated, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("LoadBenchmark failed: %v", err)
	}
	if updated.Status != "cancelled" {
		t.Errorf("expected updated status 'cancelled', got %q", updated.Status)
	}

	// Cancel again (idempotent)
	resp2, err := worker.BenchmarkCancel(ctx, jobID)
	if err != nil || resp2.Status != "cancelled" {
		t.Fatalf("idempotent cancel failed: %v, resp: %+v", err, resp2)
	}
}

func TestBenchmark_ProductionRunnerFailsClosed(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceFile, _ := createTestMediaSource(t, mediaDir, "source.mkv", "source-content")

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 2,
	}
	// Worker initialized with default (production) runner
	worker := NewWorker(cfg)
	ctx := context.Background()

	jobID := "bench-prod-fail-closed"
	jobDir := filepath.Join(stateDir, jobID)
	benchFile := filepath.Join(jobDir, "benchmark.json")
	samplesDir := filepath.Join(jobDir, "samples")
	_ = os.MkdirAll(samplesDir, 0o755)
	_ = os.WriteFile(filepath.Join(samplesDir, "sample.mkv"), []byte("sample"), 0o644)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "queued",
		Source:          sourceFile,
	}
	_ = SaveBenchmarkAtomic(benchFile, record)

	// Execute InternalBenchmark
	err := worker.InternalBenchmark(ctx, jobID)
	if err == nil {
		t.Fatalf("expected failure from production runner, got nil")
	}
	if !strings.Contains(err.Error(), "benchmark runner not implemented (fail closed; awaiting Phase 4B)") {
		t.Errorf("unexpected error message: %v", err)
	}

	// Verify state record is marked failed
	updated, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("failed loading updated benchmark: %v", err)
	}
	if updated.Status != "failed" {
		t.Errorf("expected status 'failed', got %q", updated.Status)
	}
	if !strings.Contains(updated.Error, "benchmark runner not implemented") {
		t.Errorf("expected runner error in record, got %q", updated.Error)
	}

	// Verify samples directory was cleaned up on failure
	if _, err := os.Stat(samplesDir); !os.IsNotExist(err) {
		t.Errorf("expected samples directory cleaned up on failure")
	}
}

type mockTestRunner struct {
	shouldError bool
	errText     string
}

func (m *mockTestRunner) RunBenchmark(ctx context.Context, w *Worker, record *BenchmarkRecord) error {
	if m.shouldError {
		return errors.New(m.errText)
	}
	return nil
}

func TestBenchmark_CustomRunnerSimulation(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceFile, _ := createTestMediaSource(t, mediaDir, "source.mkv", "source-content")

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 2,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	// 1. Success simulation
	worker.SetBenchmarkRunner(&mockTestRunner{shouldError: false})

	jobID := "bench-success-sim"
	jobDir := filepath.Join(stateDir, jobID)
	benchFile := filepath.Join(jobDir, "benchmark.json")
	samplesDir := filepath.Join(jobDir, "samples")
	_ = os.MkdirAll(samplesDir, 0o755)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "queued",
		Source:          sourceFile,
	}
	_ = SaveBenchmarkAtomic(benchFile, record)

	if err := worker.InternalBenchmark(ctx, jobID); err != nil {
		t.Fatalf("expected success with mock runner, got: %v", err)
	}

	updated, _ := LoadBenchmark(benchFile)
	if updated.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", updated.Status)
	}

	// 2. Failure simulation
	worker.SetBenchmarkRunner(&mockTestRunner{shouldError: true, errText: "simulated metric error"})
	jobID2 := "bench-fail-sim"
	benchFile2 := filepath.Join(stateDir, jobID2, "benchmark.json")
	record2 := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID2,
		Status:          "queued",
		Source:          sourceFile,
	}
	_ = SaveBenchmarkAtomic(benchFile2, record2)

	if err := worker.InternalBenchmark(ctx, jobID2); err == nil || !strings.Contains(err.Error(), "simulated metric error") {
		t.Fatalf("expected simulated error, got: %v", err)
	}

	updated2, _ := LoadBenchmark(benchFile2)
	if updated2.Status != "failed" {
		t.Errorf("expected status 'failed', got %q", updated2.Status)
	}
}
