package transcodeworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWorker_PathValidation(t *testing.T) {
	tempDir := t.TempDir()
	sourceFile := filepath.Join(tempDir, "sample.mkv")
	_ = os.WriteFile(sourceFile, []byte("dummy-media"), 0644)

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(tempDir, "jobs"),
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: 1,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	// 1. candidate == source rejected
	res, err := worker.Submit(ctx, SubmitRequest{
		ID:            "test-same-path",
		SourcePath:    sourceFile,
		CandidatePath: sourceFile,
	}, os.Args[0], "")
	if err == nil {
		t.Fatalf("expected error when candidate == source, got res: %+v", res)
	}
	if !strings.Contains(res.Error, "cannot equal source_path") {
		t.Errorf("expected error message about candidate_path, got %s", res.Error)
	}

	// 2. source outside allowed roots rejected
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.mkv")
	_ = os.WriteFile(outsideFile, []byte("outside"), 0644)

	res, err = worker.Submit(ctx, SubmitRequest{
		ID:            "test-outside-source",
		SourcePath:    outsideFile,
		CandidatePath: filepath.Join(tempDir, "cand.mkv"),
	}, os.Args[0], "")
	if err == nil {
		t.Fatalf("expected error for source outside allowed roots")
	}
	if !strings.Contains(res.Error, "outside allowed roots") {
		t.Errorf("expected outside allowed roots error, got %s", res.Error)
	}

	// 3. candidate outside allowed roots rejected
	res, err = worker.Submit(ctx, SubmitRequest{
		ID:            "test-outside-cand",
		SourcePath:    sourceFile,
		CandidatePath: filepath.Join(outsideDir, "cand.mkv"),
	}, os.Args[0], "")
	if err == nil {
		t.Fatalf("expected error for candidate outside allowed roots")
	}
	if !strings.Contains(res.Error, "outside allowed roots") {
		t.Errorf("expected outside allowed roots error, got %s", res.Error)
	}
}

func TestWorker_ParseProgress(t *testing.T) {
	tempDir := t.TempDir()
	progFile := filepath.Join(tempDir, "progress.txt")

	content := `frame=240
fps=120.5
stream_0_0_q=-1.0
bitrate= 1024.5kbits/s
total_size=1048576
out_time_us=5000000
out_time_ms=5000000
out_time=00:00:05.000000
dup_frames=0
drop_frames=0
speed=  4.2x
progress=continue
`
	_ = os.WriteFile(progFile, []byte(content), 0644)

	metrics := ParseProgress(progFile, 10.0) // 5s out of 10s = 50.0%
	if metrics.FPS != 120.5 {
		t.Errorf("got fps %v, want 120.5", metrics.FPS)
	}
	if metrics.Speed != 4.2 {
		t.Errorf("got speed %v, want 4.2", metrics.Speed)
	}
	if metrics.Progress != 50.0 {
		t.Errorf("got progress %v, want 50.0", metrics.Progress)
	}

	// Test progress=end
	contentEnd := content + "progress=end\n"
	_ = os.WriteFile(progFile, []byte(contentEnd), 0644)

	metricsEnd := ParseProgress(progFile, 10.0)
	if metricsEnd.Progress != 100.0 {
		t.Errorf("got progress %v, want 100.0", metricsEnd.Progress)
	}
}

func TestWorker_AtomicJobState(t *testing.T) {
	tempDir := t.TempDir()
	jobPath := filepath.Join(tempDir, "jobs", "job-1", "job.json")

	job := &JobRecord{
		ID:        "job-1",
		Status:    "running",
		Source:    "/test/source.mkv",
		Candidate: "/test/cand.mkv",
		CreatedAt: time.Now().UTC(),
	}

	if err := SaveJobAtomic(jobPath, job); err != nil {
		t.Fatalf("saving job failed: %v", err)
	}

	loaded, err := LoadJob(jobPath)
	if err != nil {
		t.Fatalf("loading job failed: %v", err)
	}
	if loaded.ID != job.ID || loaded.Status != job.Status {
		t.Errorf("loaded job does not match saved job: %+v", loaded)
	}
}

func TestWorker_ProcessCrashDetected(t *testing.T) {
	tempDir := t.TempDir()
	jobDir := filepath.Join(tempDir, "jobs", "job-crashed")
	jobPath := filepath.Join(jobDir, "job.json")

	// Use an impossible or dead PID
	deadPID := 999999

	job := &JobRecord{
		ID:        "job-crashed",
		Status:    "running",
		PID:       deadPID,
		CreatedAt: time.Now().UTC(),
		StartedAt: time.Now().UTC(),
	}
	_ = SaveJobAtomic(jobPath, job)

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(tempDir, "jobs"),
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: 1,
	}
	worker := NewWorker(cfg)

	st, err := worker.Status(context.Background(), "job-crashed")
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if st.Status != "failed" {
		t.Errorf("expected failed status on crashed process, got %s", st.Status)
	}
	if !strings.Contains(st.Error, "terminated unexpectedly") {
		t.Errorf("expected crash error message, got %s", st.Error)
	}
}

func TestWorker_BusyWorker(t *testing.T) {
	tempDir := t.TempDir()
	sourceFile := filepath.Join(tempDir, "busy.mkv")
	_ = os.WriteFile(sourceFile, []byte("busy-media"), 0644)

	// Create an existing running job whose PID is our own process (guaranteed alive)
	existingJobDir := filepath.Join(tempDir, "jobs", "job-running")
	existingJobPath := filepath.Join(existingJobDir, "job.json")

	existingJob := &JobRecord{
		ID:        "job-running",
		Status:    "running",
		PID:       os.Getpid(), // alive
		Source:    sourceFile,
		Candidate: filepath.Join(tempDir, "out1.mkv"),
		CreatedAt: time.Now().UTC(),
	}
	_ = SaveJobAtomic(existingJobPath, existingJob)

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(tempDir, "jobs"),
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: 1, // only 1 allowed
	}
	worker := NewWorker(cfg)

	// Submit a new job while another is running
	_, err := worker.Submit(context.Background(), SubmitRequest{
		ID:            "job-new",
		SourcePath:    sourceFile,
		CandidatePath: filepath.Join(tempDir, "out2.mkv"),
	}, os.Args[0], "")

	if err == nil {
		t.Fatalf("expected error when worker is busy, got nil")
	}
	if !strings.Contains(err.Error(), "busy") {
		t.Errorf("expected busy error, got %v", err)
	}
}

func TestWorker_IdempotentSubmit(t *testing.T) {
	tempDir := t.TempDir()
	sourceFile := filepath.Join(tempDir, "idemp.mkv")
	_ = os.WriteFile(sourceFile, []byte("idemp-media"), 0644)

	jobDir := filepath.Join(tempDir, "jobs", "job-idemp")
	jobPath := filepath.Join(jobDir, "job.json")

	existingJob := &JobRecord{
		ID:        "job-idemp",
		Status:    "running",
		PID:       os.Getpid(), // alive
		Source:    sourceFile,
		Candidate: filepath.Join(tempDir, "out-idemp.mkv"),
		CreatedAt: time.Now().UTC(),
	}
	_ = SaveJobAtomic(jobPath, existingJob)

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(tempDir, "jobs"),
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: 1,
	}
	worker := NewWorker(cfg)

	// Submitting with same ID returns existing job
	resp, err := worker.Submit(context.Background(), SubmitRequest{
		ID:            "job-idemp",
		SourcePath:    sourceFile,
		CandidatePath: filepath.Join(tempDir, "out-idemp.mkv"),
	}, os.Args[0], "")

	if err != nil {
		t.Fatalf("unexpected error on idempotent submit: %v", err)
	}
	if resp.ID != "job-idemp" || resp.Status != "running" {
		t.Errorf("expected running job, got %+v", resp)
	}
}

func TestWorker_CancelSafety(t *testing.T) {
	tempDir := t.TempDir()
	sourceFile := filepath.Join(tempDir, "cancel_source.mkv")
	sourceContent := "precious original content"
	_ = os.WriteFile(sourceFile, []byte(sourceContent), 0644)

	candFile := filepath.Join(tempDir, "cancel_candidate.mkv")
	_ = os.WriteFile(candFile, []byte("partial candidate output"), 0644)

	jobDir := filepath.Join(tempDir, "jobs", "job-cancel")
	jobPath := filepath.Join(jobDir, "job.json")

	// Spawn a benign sleep process to simulate a running worker with its own process group
	sleepCmd := exec.Command("sleep", "30")
	sleepCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sleepCmd.Start(); err != nil {
		t.Fatalf("failed to start sleep: %v", err)
	}
	defer func() {
		_ = sleepCmd.Process.Kill()
	}()

	job := &JobRecord{
		ID:        "job-cancel",
		Status:    "running",
		PID:       sleepCmd.Process.Pid,
		Source:    sourceFile,
		Candidate: candFile,
		CreatedAt: time.Now().UTC(),
	}
	_ = SaveJobAtomic(jobPath, job)

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(tempDir, "jobs"),
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: 1,
	}
	worker := NewWorker(cfg)

	res, err := worker.Cancel(context.Background(), "job-cancel")
	if err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	if res.Status != "cancelled" {
		t.Errorf("expected cancelled status, got %s", res.Status)
	}

	// 1. Verify sleep process was terminated
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- sleepCmd.Wait()
	}()
	select {
	case <-waitCh:
		// Process terminated as expected
	case <-time.After(2 * time.Second):
		t.Errorf("expected process to be terminated within 2s after cancel")
	}

	// 2. Verify source was NEVER touched or deleted
	srcBytes, err := os.ReadFile(sourceFile)
	if err != nil {
		t.Fatalf("source file was deleted! %v", err)
	}
	if string(srcBytes) != sourceContent {
		t.Errorf("source file was modified! got %q, want %q", string(srcBytes), sourceContent)
	}

	// 3. Verify incomplete candidate was cleaned up
	if _, err := os.Stat(candFile); !os.IsNotExist(err) {
		t.Errorf("expected incomplete candidate to be cleaned up after cancel")
	}
}

func fileSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// TestWorker_LocalSyntheticTranscodeE2E tests full transcode flow with ffmpeg on darwin arm64.
func TestWorker_LocalSyntheticTranscodeE2E(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("skipping local synthetic transcode test: requires darwin arm64")
	}

	ffmpegPath := "/opt/homebrew/bin/ffmpeg"
	ffprobePath := "/opt/homebrew/bin/ffprobe"
	if _, err := os.Stat(ffmpegPath); err != nil {
		t.Skip("ffmpeg not found at /opt/homebrew/bin/ffmpeg")
	}

	tempDir := t.TempDir()
	sourceFile := filepath.Join(tempDir, "synthetic_2s.mkv")

	// Generate 2-second test video with video, audio, and subtitle streams
	genCmd := exec.Command(ffmpegPath, "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=24",
		"-f", "lavfi", "-i", "sine=duration=2:frequency=1000",
		"-c:v", "libx264",
		"-c:a", "aac",
		sourceFile,
	)
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to generate synthetic input: %v (output: %s)", err, out)
	}

	initialSHA, err := fileSHA256(sourceFile)
	if err != nil {
		t.Fatalf("failed to hash source: %v", err)
	}

	candFile := filepath.Join(tempDir, ".navigatorr-candidates", "synthetic_2s.job1.mkv")
	stateDir := filepath.Join(tempDir, "jobs")
	cfg := &WorkerConfig{
		FFmpeg:          ffmpegPath,
		FFprobe:         ffprobePath,
		StateDir:        stateDir,
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: 1,
		Quality:         65,
	}

	cfgFile := filepath.Join(tempDir, "config.yaml")
	cfgData := fmt.Sprintf(`ffmpeg: %s
ffprobe: %s
state_dir: %s
allowed_roots:
  - %s
max_parallel_jobs: 1
quality: 65
`, ffmpegPath, ffprobePath, stateDir, tempDir)
	if err := os.WriteFile(cfgFile, []byte(cfgData), 0644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	worker := NewWorker(cfg)
	ctx := context.Background()

	// 1. Build navigatorr-transcode binary into temp directory to use as selfExe
	binPath := filepath.Join(tempDir, "navigatorr-transcode")
	buildCmd := exec.Command("go", "build", "-o", binPath, "github.com/jakenesler/navigatorr/cmd/navigatorr-transcode")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build navigatorr-transcode: %v (%s)", err, out)
	}

	// 2. Submit job
	subResp, err := worker.Submit(ctx, SubmitRequest{
		ID:            "synthetic-job-1",
		SourcePath:    sourceFile,
		CandidatePath: candFile,
		Profile:       "hevc-vt",
	}, binPath, cfgFile)
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	if subResp.Status != "queued" {
		t.Errorf("expected queued status, got %s", subResp.Status)
	}

	// 3. Poll status until completed or timeout
	deadline := time.Now().Add(30 * time.Second)
	var finalStatus JobStatusResponse
	for time.Now().Before(deadline) {
		st, err := worker.Status(ctx, "synthetic-job-1")
		if err == nil {
			finalStatus = st
			if st.Status == "completed" || st.Status == "failed" {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	if finalStatus.Status != "completed" {
		t.Fatalf("transcode did not complete, final status: %+v", finalStatus)
	}

	// 4. Validate candidate file exists
	candFi, err := os.Stat(candFile)
	if err != nil || candFi.Size() == 0 {
		t.Fatalf("candidate file missing or 0 bytes: %v", err)
	}

	// 5. ffprobe candidate video codec == hevc
	probeCmd := exec.Command(ffprobePath, "-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name",
		"-of", "default=noprint_wrappers=1:nokey=1",
		candFile,
	)
	codecOut, err := probeCmd.Output()
	if err != nil {
		t.Fatalf("probing candidate failed: %v", err)
	}
	codec := strings.TrimSpace(string(codecOut))
	if codec != "hevc" {
		t.Errorf("expected candidate video codec to be hevc, got %q", codec)
	}

	// 6. ffprobe audio codec preserved
	probeAudioCmd := exec.Command(ffprobePath, "-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=codec_name",
		"-of", "default=noprint_wrappers=1:nokey=1",
		candFile,
	)
	audioOut, err := probeAudioCmd.Output()
	if err != nil {
		t.Fatalf("probing audio failed: %v", err)
	}
	audioCodec := strings.TrimSpace(string(audioOut))
	if audioCodec != "aac" {
		t.Errorf("expected candidate audio codec to be aac, got %q", audioCodec)
	}

	// 7. Verify original file SHA is 100% UNCHANGED
	finalSHA, err := fileSHA256(sourceFile)
	if err != nil {
		t.Fatalf("failed to hash source after transcode: %v", err)
	}
	if finalSHA != initialSHA {
		t.Fatalf("INTEGRITY BREACH: original source file was modified! initial: %s, final: %s", initialSHA, finalSHA)
	}
}
