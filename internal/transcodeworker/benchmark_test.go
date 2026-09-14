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

func TestMatchesExactBenchmarkArgs_ExactVsSubstring(t *testing.T) {
	jobID := "bench-job-123"
	runToken := "run-token-abc"

	// 1. Exact match in full argv
	exactArgv := []string{"/usr/local/bin/navigatorr-transcode", "--config", "/etc/config.yaml", "_internal_benchmark", jobID, runToken}
	if !MatchesExactBenchmarkArgs(exactArgv, jobID, runToken) {
		t.Fatalf("expected exact argv sequence to match")
	}

	// 2. Substring false positives that MUST be rejected
	substringCases := []struct {
		name string
		argv []string
	}{
		{
			name: "run token has extra suffix",
			argv: []string{"bin", "_internal_benchmark", jobID, runToken + "-extra"},
		},
		{
			name: "run token has extra prefix",
			argv: []string{"bin", "_internal_benchmark", jobID, "prefix-" + runToken},
		},
		{
			name: "job ID has extra suffix",
			argv: []string{"bin", "_internal_benchmark", jobID + "-extra", runToken},
		},
		{
			name: "job ID has extra prefix",
			argv: []string{"bin", "_internal_benchmark", "prefix-" + jobID, runToken},
		},
		{
			name: "command has extra suffix",
			argv: []string{"bin", "_internal_benchmark_run", jobID, runToken},
		},
		{
			name: "tokens not contiguous",
			argv: []string{"bin", "_internal_benchmark", "--flag", jobID, runToken},
		},
		{
			name: "tokens out of order",
			argv: []string{"bin", "_internal_benchmark", runToken, jobID},
		},
		{
			name: "too short",
			argv: []string{"_internal_benchmark", jobID},
		},
		{
			name: "empty argv",
			argv: []string{},
		},
	}

	for _, tc := range substringCases {
		t.Run(tc.name, func(t *testing.T) {
			if MatchesExactBenchmarkArgs(tc.argv, jobID, runToken) {
				t.Fatalf("expected MatchesExactBenchmarkArgs to reject %s, but it passed", tc.name)
			}
		})
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

	// Verify only a single run was created
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

func TestBenchmarkSubmit_IdempotencyAcrossAllStates_NoSpawn(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceFile, _ := createTestMediaSource(t, mediaDir, "source.mkv", "idempotency-test-data")
	mockExe := createMockWorkerScript(t, tempDir)

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 2,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	req := validWorkerBenchmarkRequest(sourceFile)
	req.ID = "bench-idempotency-all-states"

	// 1. Initial submit spawns execution
	resp1, err := worker.BenchmarkSubmit(ctx, req, mockExe, "")
	if err != nil {
		t.Fatalf("initial submit failed: %v", err)
	}
	if resp1.Status != "queued" {
		t.Fatalf("expected queued status, got %q", resp1.Status)
	}

	benchFile := filepath.Join(stateDir, req.ID, "benchmark.json")
	rec1, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("loading benchmark failed: %v", err)
	}
	initialPID := rec1.PID
	initialToken := rec1.RunToken
	if initialPID <= 0 || initialToken == "" {
		t.Fatalf("invalid initial PID %d or token %q", initialPID, initialToken)
	}

	// 2. In-flight submit (queued/running): returns active status, no new spawn, token unchanged
	resp2, err := worker.BenchmarkSubmit(ctx, req, mockExe, "")
	if err != nil {
		t.Fatalf("in-flight submit failed: %v", err)
	}
	if resp2.Status != "queued" && resp2.Status != "running" {
		t.Fatalf("expected active status, got %q", resp2.Status)
	}
	rec2, _ := LoadBenchmark(benchFile)
	if rec2.PID != initialPID || rec2.RunToken != initialToken {
		t.Fatalf("in-flight submit modified PID or token!")
	}

	// Kill initial process before moving to terminal state tests
	if initialPID > 0 {
		_ = syscall.Kill(initialPID, syscall.SIGKILL)
	}

	// 3. Late submit after completed: returns completed, no spawn, token unchanged
	rec2.Status = "completed"
	rec2.FinishedAt = time.Now().UTC()
	_ = SaveBenchmarkAtomic(benchFile, rec2)

	respComp, err := worker.BenchmarkSubmit(ctx, req, mockExe, "")
	if err != nil {
		t.Fatalf("submit on completed failed: %v", err)
	}
	if respComp.Status != "completed" {
		t.Fatalf("expected completed status, got %q", respComp.Status)
	}
	recComp, _ := LoadBenchmark(benchFile)
	if recComp.PID != initialPID || recComp.RunToken != initialToken {
		t.Fatalf("submit on completed modified PID or token!")
	}

	// 4. Late submit after failed: returns failed, no spawn, token unchanged
	recComp.Status = "failed"
	recComp.Error = "simulated failure"
	_ = SaveBenchmarkAtomic(benchFile, recComp)

	respFailed, err := worker.BenchmarkSubmit(ctx, req, mockExe, "")
	if err != nil {
		t.Fatalf("submit on failed returned error: %v", err)
	}
	if respFailed.Status != "failed" {
		t.Fatalf("expected failed status on late retry, got %q", respFailed.Status)
	}
	recFailed, _ := LoadBenchmark(benchFile)
	if recFailed.PID != initialPID || recFailed.RunToken != initialToken {
		t.Fatalf("submit on failed modified PID or token! (must not respawn)")
	}

	// 5. Late submit after cancelled: returns cancelled, no spawn, token unchanged
	recFailed.Status = "cancelled"
	recFailed.Error = "cancelled by coordinator"
	_ = SaveBenchmarkAtomic(benchFile, recFailed)

	respCancelled, err := worker.BenchmarkSubmit(ctx, req, mockExe, "")
	if err != nil {
		t.Fatalf("submit on cancelled returned error: %v", err)
	}
	if respCancelled.Status != "cancelled" {
		t.Fatalf("expected cancelled status on late retry, got %q", respCancelled.Status)
	}
	recCancelled, _ := LoadBenchmark(benchFile)
	if recCancelled.PID != initialPID || recCancelled.RunToken != initialToken {
		t.Fatalf("submit on cancelled modified PID or token! (must not respawn)")
	}

	// 6. Caller explicitly uses a NEW job ID to retry the same workload -> succeeds and spawns new execution
	newReq := req
	newReq.ID = "bench-explicit-retry-new-id"
	respNew, err := worker.BenchmarkSubmit(ctx, newReq, mockExe, "")
	if err != nil {
		t.Fatalf("submit with new job ID failed: %v", err)
	}
	if respNew.Status != "queued" {
		t.Fatalf("expected queued status for new job ID, got %q", respNew.Status)
	}

	recNew, _ := LoadBenchmark(filepath.Join(stateDir, newReq.ID, "benchmark.json"))
	if recNew.RunToken == initialToken || recNew.PID == initialPID {
		t.Fatalf("expected new job ID to have new token and PID")
	}

	if recNew.PID > 0 {
		_ = syscall.Kill(recNew.PID, syscall.SIGKILL)
	}
}

func TestBenchmarkSubmit_MaxParallelJobs_CrossIDConcurrency(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceFile, _ := createTestMediaSource(t, mediaDir, "source.mkv", "capacity-race-data")
	mockExe := createMockWorkerScript(t, tempDir)

	// MaxParallelJobs is strictly 1: exactly one job may be queued/running across ALL types
	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 1,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	// Prepare 2 different benchmark requests and 1 transcode request
	req1 := validWorkerBenchmarkRequest(sourceFile)
	req1.ID = "bench-job-alpha"

	req2 := validWorkerBenchmarkRequest(sourceFile)
	req2.ID = "bench-job-beta"

	tcReq := SubmitRequest{
		ID:            "job-transcode-gamma",
		SourcePath:    sourceFile,
		CandidatePath: filepath.Join(mediaDir, "out_gamma.mkv"),
	}

	type submitResult struct {
		name   string
		status string
		err    error
	}

	resultsCh := make(chan submitResult, 3)
	startBarrier := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(3)
	go func() {
		defer wg.Done()
		<-startBarrier
		resp, err := worker.BenchmarkSubmit(ctx, req1, mockExe, "")
		resultsCh <- submitResult{name: req1.ID, status: resp.Status, err: err}
	}()

	go func() {
		defer wg.Done()
		<-startBarrier
		resp, err := worker.BenchmarkSubmit(ctx, req2, mockExe, "")
		resultsCh <- submitResult{name: req2.ID, status: resp.Status, err: err}
	}()

	go func() {
		defer wg.Done()
		<-startBarrier
		resp, err := worker.Submit(ctx, tcReq, mockExe, "")
		resultsCh <- submitResult{name: tcReq.ID, status: resp.Status, err: err}
	}()

	// Release all three requests concurrently
	close(startBarrier)
	wg.Wait()
	close(resultsCh)

	var successCount int
	var busyCount int

	for res := range resultsCh {
		if res.err == nil && res.status == "queued" {
			successCount++
		} else if res.err != nil && strings.Contains(res.err.Error(), "worker busy") {
			busyCount++
		} else {
			t.Errorf("unexpected outcome for %s: status=%q err=%v", res.name, res.status, res.err)
		}
	}

	if successCount != 1 {
		t.Fatalf("expected EXACTLY 1 job to succeed under MaxParallelJobs=1, got %d", successCount)
	}
	if busyCount != 2 {
		t.Fatalf("expected EXACTLY 2 jobs to be rejected with worker busy, got %d", busyCount)
	}

	// Clean up any spawned process
	for _, id := range []string{req1.ID, req2.ID} {
		if b, err := LoadBenchmark(filepath.Join(stateDir, id, "benchmark.json")); err == nil && b.PID > 0 {
			_ = syscall.Kill(b.PID, syscall.SIGKILL)
		}
	}
	if j, err := LoadJob(filepath.Join(stateDir, tcReq.ID, "job.json")); err == nil && j.PID > 0 {
		_ = syscall.Kill(j.PID, syscall.SIGKILL)
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
	_ = os.WriteFile(filepath.Join(stateDir, ".capacity.lock"), []byte("lock"), 0o600)
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

func TestBenchmarkSubmit_PostSpawnPersistenceFailureKillsProcess(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(mediaDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	sourceFile, _ := createTestMediaSource(t, mediaDir, "source.mkv", "post-spawn-test")
	mockExe := createMockWorkerScript(t, tempDir)

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{mediaDir},
		MaxParallelJobs: 2,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	req := validWorkerBenchmarkRequest(sourceFile)
	req.ID = "bench-post-spawn-fail"
	jobDir := filepath.Join(stateDir, req.ID)

	var capturedPID int
	worker.afterSpawnHook = func(dir string, pid int) {
		capturedPID = pid
		// Make directory read-only so that SaveBenchmarkAtomic fails when writing temp file
		_ = os.Chmod(dir, 0o555)
	}
	defer func() {
		_ = os.Chmod(jobDir, 0o755)
	}()

	resp, err := worker.BenchmarkSubmit(ctx, req, mockExe, "")
	if err == nil {
		t.Fatalf("expected error from failed persistence after spawn, got nil (resp: %+v)", resp)
	}

	// Restore permissions
	_ = os.Chmod(jobDir, 0o755)

	if capturedPID <= 0 {
		t.Fatalf("expected capturedPID to be positive")
	}

	// Verify that the spawned process was immediately terminated and not left orphaned
	if IsProcessAlive(capturedPID) {
		_ = syscall.Kill(capturedPID, syscall.SIGKILL)
		t.Fatalf("spawned process %d was not killed after post-spawn persistence failure!", capturedPID)
	}
}

func TestBenchmarkStatus_MapsProgressPhaseHeartbeat(t *testing.T) {
	tempDir := t.TempDir()
	stateDir := filepath.Join(tempDir, "state")
	jobID := "bench-map-test"
	jobDir := filepath.Join(stateDir, jobID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	fixedTime := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	record := &BenchmarkRecord{
		ProtocolVersion:  transcode.WorkerProtocolVersion,
		ID:               jobID,
		Status:           "running",
		Source:           "/media/source.mkv",
		Metric:           "vmaf",
		Progress:         45.5,
		Phase:            "encoding_candidates",
		HeartbeatAt:      fixedTime,
		RunToken:         "token-map-123",
		PID:              os.Getpid(),
		ProcessStartTime: "test-lstart",
		CreatedAt:        fixedTime.Add(-time.Hour),
		StartedAt:        fixedTime.Add(-30 * time.Minute),
	}
	benchFile := filepath.Join(jobDir, "benchmark.json")
	if err := SaveBenchmarkAtomic(benchFile, record); err != nil {
		t.Fatalf("saving benchmark: %v", err)
	}

	worker := NewWorker(&WorkerConfig{StateDir: stateDir})
	st, err := worker.BenchmarkStatus(context.Background(), jobID)
	if err != nil {
		t.Fatalf("BenchmarkStatus failed: %v", err)
	}

	if st.Progress != 45.5 {
		t.Errorf("expected Progress=45.5, got %v", st.Progress)
	}
	if st.Phase != "encoding_candidates" {
		t.Errorf("expected Phase='encoding_candidates', got %q", st.Phase)
	}
	if !st.HeartbeatAt.Equal(fixedTime) {
		t.Errorf("expected HeartbeatAt=%v, got %v", fixedTime, st.HeartbeatAt)
	}
}

func TestWorker_UpdateBenchmarkProgress_MonotonicAndPreservesFields(t *testing.T) {
	tempDir := t.TempDir()
	stateDir := filepath.Join(tempDir, "state")
	jobID := "bench-mono-test"
	jobDir := filepath.Join(stateDir, jobID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	fixedCreated := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	fixedStarted := time.Date(2026, 9, 14, 10, 5, 0, 0, time.UTC)
	initialEvidence := &BenchmarkExecutionEvidence{
		SourceResolution: "1920x1080",
	}

	record := &BenchmarkRecord{
		ProtocolVersion:  transcode.WorkerProtocolVersion,
		ID:               jobID,
		Status:           "running",
		Source:           "/media/source.mkv",
		Metric:           "vmaf",
		Progress:         20.0,
		Phase:            "extracting_samples",
		RunToken:         "token-mono-456",
		PID:              12345,
		ProcessStartTime: "test-lstart-time",
		CreatedAt:        fixedCreated,
		StartedAt:        fixedStarted,
		Evidence:         initialEvidence,
	}
	benchFile := filepath.Join(jobDir, "benchmark.json")
	if err := SaveBenchmarkAtomic(benchFile, record); err != nil {
		t.Fatalf("saving benchmark: %v", err)
	}

	worker := NewWorker(&WorkerConfig{StateDir: stateDir})

	// 1. Monotonic advance to 50.0
	err := worker.UpdateBenchmarkProgress(jobID, "token-mono-456", 50.0, "encoding_candidates")
	if err != nil {
		t.Fatalf("UpdateBenchmarkProgress to 50.0 failed: %v", err)
	}

	loaded, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("loading benchmark: %v", err)
	}
	if loaded.Progress != 50.0 {
		t.Errorf("expected Progress=50.0, got %v", loaded.Progress)
	}
	if loaded.Phase != "encoding_candidates" {
		t.Errorf("expected Phase='encoding_candidates', got %q", loaded.Phase)
	}
	if loaded.HeartbeatAt.IsZero() {
		t.Errorf("expected HeartbeatAt to be populated")
	}
	// Verify preservation of other fields
	if loaded.PID != 12345 || loaded.ProcessStartTime != "test-lstart-time" {
		t.Errorf("PID/ProcessStartTime clobbered: %d / %q", loaded.PID, loaded.ProcessStartTime)
	}
	if loaded.Source != "/media/source.mkv" || loaded.Metric != "vmaf" {
		t.Errorf("Source/Metric clobbered: %q / %q", loaded.Source, loaded.Metric)
	}
	if loaded.Evidence == nil || loaded.Evidence.SourceResolution != "1920x1080" {
		t.Errorf("Evidence clobbered: %+v", loaded.Evidence)
	}

	// 2. Attempt regression to 30.0 - progress must remain 50.0
	firstHeartbeat := loaded.HeartbeatAt
	err = worker.UpdateBenchmarkProgress(jobID, "token-mono-456", 30.0, "evaluating_metrics")
	if err != nil {
		t.Fatalf("UpdateBenchmarkProgress with lower progress failed: %v", err)
	}

	loaded2, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("loading benchmark: %v", err)
	}
	if loaded2.Progress != 50.0 {
		t.Errorf("expected Progress to stay 50.0 (monotonic), got %v", loaded2.Progress)
	}
	if loaded2.Phase != "evaluating_metrics" {
		t.Errorf("expected Phase to update to 'evaluating_metrics', got %q", loaded2.Phase)
	}
	if loaded2.HeartbeatAt.Before(firstHeartbeat) {
		t.Errorf("expected HeartbeatAt to be updated")
	}
}

func TestWorker_UpdateBenchmarkProgress_StaleTokenAndCancelled(t *testing.T) {
	tempDir := t.TempDir()
	stateDir := filepath.Join(tempDir, "state")
	worker := NewWorker(&WorkerConfig{StateDir: stateDir})

	// Case A: Stale RunToken cannot overwrite
	jobIDA := "bench-stale-token"
	jobDirA := filepath.Join(stateDir, jobIDA)
	_ = os.MkdirAll(jobDirA, 0o755)
	recA := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobIDA,
		Status:          "running",
		Progress:        15.0,
		RunToken:        "valid-token-111",
	}
	benchFileA := filepath.Join(jobDirA, "benchmark.json")
	_ = SaveBenchmarkAtomic(benchFileA, recA)

	err := worker.UpdateBenchmarkProgress(jobIDA, "stale-foreign-token", 80.0, "fake_phase")
	if err == nil {
		t.Fatalf("expected error on stale RunToken, got nil")
	}
	if !errors.Is(err, ErrBenchmarkRunTokenMismatch) {
		t.Errorf("expected ErrBenchmarkRunTokenMismatch, got %v", err)
	}
	checkA, _ := LoadBenchmark(benchFileA)
	if checkA.Progress != 15.0 {
		t.Errorf("expected record Progress to remain 15.0, got %v", checkA.Progress)
	}

	// Case B: Cancelled record cannot be updated or resurrected
	jobIDB := "bench-cancelled"
	jobDirB := filepath.Join(stateDir, jobIDB)
	_ = os.MkdirAll(jobDirB, 0o755)
	recB := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobIDB,
		Status:          "cancelled",
		Progress:        22.0,
		RunToken:        "token-cancel-222",
	}
	benchFileB := filepath.Join(jobDirB, "benchmark.json")
	_ = SaveBenchmarkAtomic(benchFileB, recB)

	err = worker.UpdateBenchmarkProgress(jobIDB, "token-cancel-222", 90.0, "running_phase")
	if err == nil {
		t.Fatalf("expected error on cancelled benchmark, got nil")
	}
	if !errors.Is(err, ErrBenchmarkNotActive) {
		t.Errorf("expected ErrBenchmarkNotActive, got %v", err)
	}
	checkB, _ := LoadBenchmark(benchFileB)
	if checkB.Status != "cancelled" {
		t.Errorf("expected Status to remain cancelled, got %q", checkB.Status)
	}
	if checkB.Progress != 22.0 {
		t.Errorf("expected Progress to remain 22.0, got %v", checkB.Progress)
	}
}

func TestBenchmark_InternalBenchmark_ReportsProgress100OnCompletion(t *testing.T) {
	tempDir := t.TempDir()
	stateDir := filepath.Join(tempDir, "state")
	jobID := "bench-terminal-100"
	jobDir := filepath.Join(stateDir, jobID)
	_ = os.MkdirAll(jobDir, 0o755)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "queued",
		RunToken:        "token-comp-333",
		Metric:          "vmaf",
		Source:          "/media/test.mkv",
		Progress:        0,
	}
	benchFile := filepath.Join(jobDir, "benchmark.json")
	_ = SaveBenchmarkAtomic(benchFile, record)

	worker := NewWorker(&WorkerConfig{StateDir: stateDir})

	// Inject runner that updates progress to 60.0 during execution and returns success
	worker.SetBenchmarkRunner(&mockLifecycleRunner{
		onRun: func() {
			_ = worker.UpdateBenchmarkProgress(jobID, "token-comp-333", 60.0, "encoding_candidates")
		},
	})

	err := worker.InternalBenchmark(context.Background(), jobID, "token-comp-333")
	if err != nil {
		t.Fatalf("InternalBenchmark failed: %v", err)
	}

	st, err := worker.BenchmarkStatus(context.Background(), jobID)
	if err != nil {
		t.Fatalf("BenchmarkStatus failed: %v", err)
	}
	if st.Status != "completed" {
		t.Errorf("expected Status='completed', got %q", st.Status)
	}
	if st.Progress != 100 {
		t.Errorf("expected Progress=100 on completed, got %v", st.Progress)
	}
	if st.Phase != "completed" {
		t.Errorf("expected Phase='completed' on completed, got %q", st.Phase)
	}
	if st.HeartbeatAt.IsZero() {
		t.Errorf("expected HeartbeatAt to be set")
	}
}
