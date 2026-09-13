package transcodeworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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

func createMockWorkerScript(t *testing.T, dir string) string {
	t.Helper()
	scriptPath := filepath.Join(dir, "mock_worker.sh")
	scriptContent := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0o755); err != nil {
		t.Fatalf("failed creating mock worker script: %v", err)
	}
	return scriptPath
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

func TestBenchmarkSubmit_ValidationAndNamespaceIsolation(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceFile, _ := createTestMediaSource(t, mediaDir, "source.mkv", "source-data-12345")
	mockExe := createMockWorkerScript(t, tempDir)

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 2,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	// 1. Source outside allowed roots
	reqOutside := validWorkerBenchmarkRequest("/outside/root/file.mkv")
	resp, err := worker.BenchmarkSubmit(ctx, reqOutside, mockExe, "")
	if err == nil || !strings.Contains(resp.Error, "outside allowed roots") {
		t.Fatalf("expected outside allowed roots error, got: %v (resp: %+v)", err, resp)
	}

	// 2. Source file does not exist
	reqNotExist := validWorkerBenchmarkRequest(filepath.Join(mediaDir, "nonexistent.mkv"))
	resp, err = worker.BenchmarkSubmit(ctx, reqNotExist, mockExe, "")
	if err == nil || !strings.Contains(resp.Error, "not accessible") {
		t.Fatalf("expected not accessible error, got: %v", err)
	}

	// 3. Source is directory
	reqDir := validWorkerBenchmarkRequest(mediaDir)
	resp, err = worker.BenchmarkSubmit(ctx, reqDir, mockExe, "")
	if err == nil || !strings.Contains(resp.Error, "directory") {
		t.Fatalf("expected directory error, got: %v", err)
	}

	// 4. Path traversal in benchmark job ID
	traversalIDs := []string{
		"bench-../../etc/passwd",
		"bench-..",
		"bench-sub/dir",
		"bench-sub\\dir",
		"bench-foo%2fbar",
		"bench-samples",
		"bench-lock",
	}
	for _, id := range traversalIDs {
		reqTrav := validWorkerBenchmarkRequest(sourceFile)
		reqTrav.ID = id
		resp, err = worker.BenchmarkSubmit(ctx, reqTrav, mockExe, "")
		if err == nil {
			t.Errorf("expected error for illegal job ID %q, got nil", id)
		}
	}

	// 5. Transcode Submit cannot use bench- prefix
	tcReq := SubmitRequest{
		ID:            "bench-transcode-attempt",
		SourcePath:    sourceFile,
		CandidatePath: filepath.Join(mediaDir, "out.mkv"),
	}
	tcResp, tcErr := worker.Submit(ctx, tcReq, mockExe, "")
	if tcErr == nil || !strings.Contains(tcResp.Error, "reserved benchmark prefix") {
		t.Fatalf("expected error when transcode job uses 'bench-' prefix, got: %v (resp: %+v)", tcErr, tcResp)
	}
}

func TestBenchmarkSubmit_SingleWriterConcurrency(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceFile, _ := createTestMediaSource(t, mediaDir, "source.mkv", "concurrency-test")
	mockExe := createMockWorkerScript(t, tempDir)

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 2,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	req := validWorkerBenchmarkRequest(sourceFile)
	req.ID = "bench-concurrent-submit"

	const numGoroutines = 8
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})
	errorsCh := make(chan error, numGoroutines)
	statusesCh := make(chan string, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startBarrier
			resp, err := worker.BenchmarkSubmit(ctx, req, mockExe, "")
			if err != nil {
				errorsCh <- err
			} else {
				statusesCh <- resp.Status
			}
		}()
	}

	// Release all goroutines simultaneously
	close(startBarrier)
	wg.Wait()
	close(errorsCh)
	close(statusesCh)

	for err := range errorsCh {
		t.Fatalf("concurrent submit returned unexpected error: %v", err)
	}

	var countQueued int
	for st := range statusesCh {
		if st == "queued" || st == "running" {
			countQueued++
		} else {
			t.Errorf("unexpected status from concurrent submit: %q", st)
		}
	}
	if countQueued != numGoroutines {
		t.Fatalf("expected all %d goroutines to succeed, got %d", numGoroutines, countQueued)
	}

	// Verify only a single run was created (Attempt == 1)
	benchFile := filepath.Join(stateDir, req.ID, "benchmark.json")
	record, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("loading benchmark record failed: %v", err)
	}
	if record.Attempt != 1 {
		t.Errorf("expected Attempt == 1, got %d", record.Attempt)
	}
	if record.RunToken == "" {
		t.Errorf("expected non-empty RunToken")
	}

	// Clean up spawned process
	if record.PID > 0 {
		_ = syscall.Kill(record.PID, syscall.SIGKILL)
	}
}

func TestBenchmarkSubmit_DeterministicCollision(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceFile, _ := createTestMediaSource(t, mediaDir, "source.mkv", "source-data-abc")
	mockExe := createMockWorkerScript(t, tempDir)

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 2,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	req := validWorkerBenchmarkRequest(sourceFile)
	req.ID = "bench-collision-test"

	// Pre-create benchmark record in terminal "completed" status
	benchDir := filepath.Join(stateDir, req.ID)
	benchFile := filepath.Join(benchDir, "benchmark.json")
	reqDigest, _ := transcode.DigestBenchmarkRequest(&req)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              req.ID,
		Status:          "completed",
		Source:          sourceFile,
		Metric:          req.Metric,
		RequestDigest:   reqDigest,
		Attempt:         1,
		RunToken:        "token-123",
	}
	if err := SaveBenchmarkAtomic(benchFile, record); err != nil {
		t.Fatalf("SaveBenchmarkAtomic failed: %v", err)
	}

	// 1. Submit identical request -> idempotent completed return
	resp, err := worker.BenchmarkSubmit(ctx, req, mockExe, "")
	if err != nil {
		t.Fatalf("expected idempotent success for completed job, got: %v", err)
	}
	if resp.Status != "completed" {
		t.Errorf("got status %q, want 'completed'", resp.Status)
	}

	// 2. Submit differing request (different metric) with same ID -> MUST FAIL with collision
	reqDiff := req
	reqDiff.Metric = "ssim"
	respDiff, err := worker.BenchmarkSubmit(ctx, reqDiff, mockExe, "")
	if err == nil || !strings.Contains(respDiff.Error, "collision") {
		t.Fatalf("expected collision error for differing request on completed job, got: %v (resp: %+v)", err, respDiff)
	}

	// 3. Repeat collision test across failed and cancelled states
	for _, status := range []string{"failed", "cancelled"} {
		record.Status = status
		_ = SaveBenchmarkAtomic(benchFile, record)

		respCol, err := worker.BenchmarkSubmit(ctx, reqDiff, mockExe, "")
		if err == nil || !strings.Contains(respCol.Error, "collision") {
			t.Fatalf("expected collision error in state %s, got: %v (resp: %+v)", status, err, respCol)
		}
	}

	// 4. Cross-type collision: benchmark submit colliding with existing job.json
	crossBenchID := "bench-has-transcode-job"
	crossBenchDir := filepath.Join(stateDir, crossBenchID)
	_ = os.MkdirAll(crossBenchDir, 0o755)
	_ = os.WriteFile(filepath.Join(crossBenchDir, "job.json"), []byte(`{"id":"`+crossBenchID+`"}`), 0o644)

	reqCrossBench := req
	reqCrossBench.ID = crossBenchID
	respCrossBench, err := worker.BenchmarkSubmit(ctx, reqCrossBench, mockExe, "")
	if err == nil || !strings.Contains(respCrossBench.Error, "collision") {
		t.Fatalf("expected benchmark submit collision with transcode job, got: %v (resp: %+v)", err, respCrossBench)
	}

	// 5. Cross-type collision: transcode submit colliding with existing benchmark.json
	tcID := "job-regular-transcode-colliding"
	tcJobDir := filepath.Join(stateDir, tcID)
	_ = os.MkdirAll(tcJobDir, 0o755)
	_ = os.WriteFile(filepath.Join(tcJobDir, "benchmark.json"), []byte(`{"id":"`+tcID+`"}`), 0o644)

	tcAttempt := SubmitRequest{
		ID:            tcID,
		SourcePath:    sourceFile,
		CandidatePath: filepath.Join(mediaDir, "cand.mkv"),
	}
	tcResp, err := worker.Submit(ctx, tcAttempt, mockExe, "")
	if err == nil || !strings.Contains(tcResp.Error, "collision") {
		t.Fatalf("expected transcode submit collision with benchmark job, got: %v (resp: %+v)", err, tcResp)
	}
}

func TestBenchmarkSubmit_TransportRetryVsTerminalRetry(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceFile, _ := createTestMediaSource(t, mediaDir, "source.mkv", "retry-test-data")
	mockExe := createMockWorkerScript(t, tempDir)

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 2,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	req := validWorkerBenchmarkRequest(sourceFile)
	req.ID = "bench-retry-flow"

	// 1. Initial submit spawns run attempt 1
	resp1, err := worker.BenchmarkSubmit(ctx, req, mockExe, "")
	if err != nil {
		t.Fatalf("initial BenchmarkSubmit failed: %v", err)
	}
	if resp1.Status != "queued" {
		t.Fatalf("expected initial status 'queued', got %q", resp1.Status)
	}

	benchFile := filepath.Join(stateDir, req.ID, "benchmark.json")
	rec1, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("failed loading benchmark record: %v", err)
	}
	if rec1.Attempt != 1 {
		t.Fatalf("expected Attempt 1, got %d", rec1.Attempt)
	}
	token1 := rec1.RunToken
	pid1 := rec1.PID
	if token1 == "" || pid1 <= 0 {
		t.Fatalf("invalid initial token %q or pid %d", token1, pid1)
	}

	// 2. Transport retry: submitting identical request while process is alive
	resp2, err := worker.BenchmarkSubmit(ctx, req, mockExe, "")
	if err != nil {
		t.Fatalf("transport retry failed: %v", err)
	}
	if resp2.Status != "queued" && resp2.Status != "running" {
		t.Fatalf("transport retry got status %q, want 'queued' or 'running'", resp2.Status)
	}

	rec2, _ := LoadBenchmark(benchFile)
	if rec2.Attempt != 1 || rec2.RunToken != token1 || rec2.PID != pid1 {
		t.Fatalf("transport retry modified execution run! rec2: %+v", rec2)
	}

	// 3. Kill the mock process to simulate terminal failure
	if pid1 > 0 {
		_ = syscall.Kill(pid1, syscall.SIGKILL)
	}
	time.Sleep(100 * time.Millisecond)

	rec2.Status = "failed"
	rec2.Error = "simulated worker failure"
	rec2.FinishedAt = time.Now().UTC()
	_ = SaveBenchmarkAtomic(benchFile, rec2)

	// 4. Terminal retry: submitting identical request when job is in failed state
	respRetry, err := worker.BenchmarkSubmit(ctx, req, mockExe, "")
	if err != nil {
		t.Fatalf("terminal retry failed: %v", err)
	}
	if respRetry.Status != "queued" {
		t.Fatalf("expected queued status on retry, got %q", respRetry.Status)
	}

	rec3, _ := LoadBenchmark(benchFile)
	if rec3.Attempt != 2 {
		t.Errorf("expected Attempt == 2 on terminal retry, got %d", rec3.Attempt)
	}
	if rec3.RunToken == token1 || rec3.RunToken == "" {
		t.Errorf("expected fresh RunToken on retry, got %q (old %q)", rec3.RunToken, token1)
	}
	if rec3.PID == pid1 || rec3.PID <= 0 {
		t.Errorf("expected new PID on retry, got %d (old %d)", rec3.PID, pid1)
	}
	if rec3.Error != "" {
		t.Errorf("expected reset error on retry, got %q", rec3.Error)
	}

	// Clean up second process
	if rec3.PID > 0 {
		_ = syscall.Kill(rec3.PID, syscall.SIGKILL)
	}
}

func TestBenchmarkCancel_FailClosedAndPIDReuseSafety(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceContent := "precious-source-media"
	sourceFile, origSHA := createTestMediaSource(t, mediaDir, "source.mkv", sourceContent)

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 2,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	jobID := "bench-cancel-safety"
	jobDir := filepath.Join(stateDir, jobID)
	benchFile := filepath.Join(jobDir, "benchmark.json")
	samplesDir := filepath.Join(jobDir, "samples")
	_ = os.MkdirAll(samplesDir, 0o755)
	_ = os.WriteFile(filepath.Join(samplesDir, "sample0.mkv"), []byte("scratch"), 0o644)

	// Simulate PID reuse scenario:
	// PID is set to our own process (os.Getpid()) which is definitely ALIVE.
	// But RunToken is a foreign token, and command line does NOT contain this token or _internal_benchmark.
	recycledRecord := &BenchmarkRecord{
		ProtocolVersion:  transcode.WorkerProtocolVersion,
		ID:               jobID,
		Status:           "running",
		Source:           sourceFile,
		PID:              os.Getpid(), // alive process
		RunToken:         "foreign-nonce-abc-123",
		ProcessStartTime: "Sun Jan 1 00:00:00 2000", // stale start time
		CreatedAt:        time.Now().UTC(),
		StartedAt:        time.Now().UTC(),
	}
	if err := SaveBenchmarkAtomic(benchFile, recycledRecord); err != nil {
		t.Fatalf("SaveBenchmarkAtomic failed: %v", err)
	}

	// 1. Verify IsBenchmarkExecutionAlive returns false
	if IsBenchmarkExecutionAlive(recycledRecord) {
		t.Fatalf("IsBenchmarkExecutionAlive should be false for foreign RunToken / recycled PID")
	}

	// 2. Call BenchmarkCancel: must NOT signal os.Getpid()!
	// If it signaled os.Getpid(), the test process would immediately terminate!
	cancelResp, err := worker.BenchmarkCancel(ctx, jobID)
	if err != nil {
		t.Fatalf("BenchmarkCancel failed: %v", err)
	}
	if cancelResp.Status != "cancelled" {
		t.Errorf("expected cancelled status, got %q", cancelResp.Status)
	}

	// 3. Verify status was reconciled to cancelled in benchmark.json
	updated, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("loading updated benchmark failed: %v", err)
	}
	if updated.Status != "cancelled" {
		t.Errorf("expected reconciled status 'cancelled', got %q", updated.Status)
	}

	// 4. Verify scratch workspace samples/ was cleaned up
	if _, err := os.Stat(samplesDir); !os.IsNotExist(err) {
		t.Errorf("expected samples/ workspace to be removed after cancel")
	}

	// 5. Verify source media was NEVER touched
	currSHA, err := fileSHA256(sourceFile)
	if err != nil {
		t.Fatalf("fileSHA256 failed: %v", err)
	}
	if currSHA != origSHA {
		t.Fatalf("FATAL: source media was modified! orig=%s, curr=%s", origSHA, currSHA)
	}
}

func TestBenchmark_InternalBenchmark_RunTokenMismatchFailsClosed(t *testing.T) {
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

	jobID := "bench-token-mismatch"
	jobDir := filepath.Join(stateDir, jobID)
	benchFile := filepath.Join(jobDir, "benchmark.json")

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "queued",
		Source:          sourceFile,
		RunToken:        "authorized-token-12345",
		Attempt:         1,
	}
	_ = SaveBenchmarkAtomic(benchFile, record)

	// Attempt to invoke InternalBenchmark with an incorrect token
	err := worker.InternalBenchmark(ctx, jobID, "attacker-or-stale-token")
	if err == nil {
		t.Fatalf("expected error on run token mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "run token mismatch") {
		t.Errorf("expected 'run token mismatch' error, got: %v", err)
	}

	// Verify state in benchmark.json was untouched (still queued, not running)
	latest, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("failed loading benchmark: %v", err)
	}
	if latest.Status != "queued" {
		t.Errorf("status should remain queued after failed token verification, got %q", latest.Status)
	}
}

type mockLifecycleRunner struct {
	shouldError bool
	errText     string
	onRun       func()
}

func (m *mockLifecycleRunner) RunBenchmark(ctx context.Context, w *Worker, record *BenchmarkRecord) error {
	if m.onRun != nil {
		m.onRun()
	}
	if m.shouldError {
		return errors.New(m.errText)
	}
	return nil
}

func TestBenchmark_MonotonicLifecycleAndCancelDuringExecution(t *testing.T) {
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

	// 1. Success execution
	worker.SetBenchmarkRunner(&mockLifecycleRunner{shouldError: false})
	jobID := "bench-lifecycle-success"
	benchFile := filepath.Join(stateDir, jobID, "benchmark.json")

	token := "token-lifecycle-1"
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "queued",
		Source:          sourceFile,
		RunToken:        token,
		Attempt:         1,
	}
	_ = SaveBenchmarkAtomic(benchFile, record)

	if err := worker.InternalBenchmark(ctx, jobID, token); err != nil {
		t.Fatalf("InternalBenchmark failed: %v", err)
	}

	updated, _ := LoadBenchmark(benchFile)
	if updated.Status != "completed" {
		t.Errorf("expected completed status, got %q", updated.Status)
	}

	// 2. Cancellation during execution: verify runner completion does NOT overwrite cancelled
	jobIDCancel := "bench-cancel-in-flight"
	benchFileCancel := filepath.Join(stateDir, jobIDCancel, "benchmark.json")
	tokenCancel := "token-cancel-in-flight"

	recordCancel := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobIDCancel,
		Status:          "queued",
		Source:          sourceFile,
		RunToken:        tokenCancel,
		Attempt:         1,
	}
	_ = SaveBenchmarkAtomic(benchFileCancel, recordCancel)

	worker.SetBenchmarkRunner(&mockLifecycleRunner{
		onRun: func() {
			// Simulate coordinator cancellation arriving while runner was in flight
			_, cancelErr := worker.BenchmarkCancel(ctx, jobIDCancel)
			if cancelErr != nil {
				t.Errorf("BenchmarkCancel during run failed: %v", cancelErr)
			}
		},
	})

	_ = worker.InternalBenchmark(ctx, jobIDCancel, tokenCancel)

	// Status must remain cancelled!
	finalRec, _ := LoadBenchmark(benchFileCancel)
	if finalRec.Status != "cancelled" {
		t.Fatalf("expected status to remain 'cancelled', got %q", finalRec.Status)
	}
}

func TestBenchmark_CountActiveJobs_Isolation(t *testing.T) {
	tempDir := t.TempDir()
	stateDir := filepath.Join(tempDir, "jobs")
	_ = os.MkdirAll(stateDir, 0o755)

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		MaxParallelJobs: 2,
	}
	worker := NewWorker(cfg)

	// 1. Create dot-files and directories that must be ignored
	_ = os.MkdirAll(filepath.Join(stateDir, ".hidden-dir"), 0o755)
	_ = os.WriteFile(filepath.Join(stateDir, ".lock"), []byte("lock"), 0o600)
	_ = os.WriteFile(filepath.Join(stateDir, ".DS_Store"), []byte("junk"), 0o644)

	// 2. Create invalid non-job directory
	_ = os.MkdirAll(filepath.Join(stateDir, "random-other-dir"), 0o755)

	count, err := worker.countActiveJobs("")
	if err != nil {
		t.Fatalf("countActiveJobs failed: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 active jobs for hidden/invalid entries, got %d", count)
	}
}
