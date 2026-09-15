package transcodeworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	"github.com/jakenesler/navigatorr/transcode"
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

	// PR3 authoritative durable queue: submit under full capacity persists as
	// queued instead of returning "worker busy".
	resp, err := worker.Submit(context.Background(), SubmitRequest{
		ID:            "job-new",
		SourcePath:    sourceFile,
		CandidatePath: filepath.Join(tempDir, "out2.mkv"),
	}, os.Args[0], "")

	if err != nil {
		t.Fatalf("PR3 durable queue: expected queued success under full capacity, got err %v", err)
	}
	if resp.Status != "queued" {
		t.Errorf("expected queued status under full capacity, got %+v", resp)
	}
	loaded, lerr := LoadJob(filepath.Join(tempDir, "jobs", "job-new", "job.json"))
	if lerr != nil || loaded == nil || loaded.Status != "queued" {
		t.Fatalf("expected persisted queued job, got %+v err %v", loaded, lerr)
	}
	if loaded.PID != 0 {
		t.Errorf("queued-behind-capacity job must not spawn yet (PID=0), got %d", loaded.PID)
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

func TestWorker_RecycledPIDSafety(t *testing.T) {
	tempDir := t.TempDir()
	jobDir := filepath.Join(tempDir, "jobs", "job-recycled")
	jobPath := filepath.Join(jobDir, "job.json")

	// Assign our own PID (guaranteed alive), but with a bogus start time from 2000
	job := &JobRecord{
		ID:               "job-recycled",
		Status:           "running",
		PID:              os.Getpid(),
		ProcessStartTime: "Sun Jan 1 00:00:00 2000",
		CreatedAt:        time.Now().UTC(),
		StartedAt:        time.Now().UTC(),
	}
	_ = SaveJobAtomic(jobPath, job)

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(tempDir, "jobs"),
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: 1,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	// 1. IsJobProcessAlive must return false due to start time mismatch
	if IsJobProcessAlive(job) {
		t.Fatalf("expected IsJobProcessAlive to detect recycled PID and return false")
	}

	// 2. Status must detect dead/recycled job and mark it as failed
	st, err := worker.Status(ctx, "job-recycled")
	if err != nil {
		t.Fatalf("status call failed: %v", err)
	}
	if st.Status != "failed" {
		t.Errorf("expected failed status for recycled PID, got %s", st.Status)
	}

	// 3. Cancel must NOT kill our own process! If it did, the test would crash right here.
	cancelResp, err := worker.Cancel(ctx, "job-recycled")
	if err != nil {
		t.Fatalf("cancel call failed: %v", err)
	}
	if cancelResp.Status != "cancelled" {
		t.Errorf("expected cancelled response, got %s", cancelResp.Status)
	}
}

// TestWorker_MP4_MovText_To_MKV_E2E tests real transcode of MP4 (H264 + AC3 + mov_text) -> MKV (HEVC + AC3 copy + subrip).
func TestWorker_MP4_MovText_To_MKV_E2E(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("skipping local synthetic transcode test: requires darwin arm64")
	}

	ffmpegPath := "/opt/homebrew/bin/ffmpeg"
	ffprobePath := "/opt/homebrew/bin/ffprobe"
	if _, err := os.Stat(ffmpegPath); err != nil {
		t.Skip("ffmpeg not found at /opt/homebrew/bin/ffmpeg")
	}

	tempDir := t.TempDir()
	sourceFile := filepath.Join(tempDir, "sample_movtext.mp4")
	srtFile := filepath.Join(tempDir, "sub.srt")
	_ = os.WriteFile(srtFile, []byte("1\n00:00:00,000 --> 00:00:02,000\nHello English subtitle\n"), 0644)

	// Generate MP4 with H264, AC3, and mov_text subtitle
	genCmd := exec.Command(ffmpegPath, "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=24",
		"-f", "lavfi", "-i", "sine=duration=2:frequency=1000",
		"-i", srtFile,
		"-c:v", "libx264",
		"-c:a", "ac3",
		"-c:s", "mov_text",
		"-metadata:s:s:0", "language=eng",
		sourceFile,
	)
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to generate synthetic MP4: %v (%s)", err, out)
	}

	initialSHA, err := fileSHA256(sourceFile)
	if err != nil {
		t.Fatalf("failed to hash source: %v", err)
	}

	candFile := filepath.Join(tempDir, ".navigatorr-candidates", "sample_movtext.job_movtext.mkv")
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
	cfgData := fmt.Sprintf("ffmpeg: %s\nffprobe: %s\nstate_dir: %s\nallowed_roots:\n  - %s\nmax_parallel_jobs: 1\nquality: 65\n",
		ffmpegPath, ffprobePath, stateDir, tempDir)
	_ = os.WriteFile(cfgFile, []byte(cfgData), 0644)

	binPath := filepath.Join(tempDir, "navigatorr-transcode")
	buildCmd := exec.Command("go", "build", "-o", binPath, "github.com/jakenesler/navigatorr/cmd/navigatorr-transcode")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build navigatorr-transcode: %v (%s)", err, out)
	}

	worker := NewWorker(cfg)
	ctx := context.Background()

	subResp, err := worker.Submit(ctx, SubmitRequest{
		ID:            "job-movtext-1",
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

	// Poll status until complete
	deadline := time.Now().Add(30 * time.Second)
	var finalStatus JobStatusResponse
	for time.Now().Before(deadline) {
		st, err := worker.Status(ctx, "job-movtext-1")
		if err == nil {
			finalStatus = st
			if st.Status == "completed" || st.Status == "failed" {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	if finalStatus.Status != "completed" {
		t.Fatalf("transcode failed or timed out: %+v", finalStatus)
	}

	// 1. Verify candidate file exists
	candFi, err := os.Stat(candFile)
	if err != nil || candFi.Size() == 0 {
		t.Fatalf("candidate file missing or empty: %v", err)
	}

	// 2. Verify conversion recorded in status
	if len(finalStatus.Conversions) != 1 {
		t.Fatalf("expected 1 conversion in status, got %d: %+v", len(finalStatus.Conversions), finalStatus.Conversions)
	}
	conv := finalStatus.Conversions[0]
	if conv.FromCodec != "mov_text" || conv.ToCodec != "subrip" {
		t.Errorf("expected mov_text -> subrip conversion, got %+v", conv)
	}

	// 3. Verify video codec == hevc
	vProbe, err := exec.Command(ffprobePath, "-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name",
		"-of", "default=noprint_wrappers=1:nokey=1",
		candFile,
	).Output()
	if err != nil {
		t.Fatalf("probing video: %v", err)
	}
	if strings.TrimSpace(string(vProbe)) != "hevc" {
		t.Errorf("expected hevc video, got %q", string(vProbe))
	}

	// 4. Verify audio codec == ac3 (copied)
	aProbe, err := exec.Command(ffprobePath, "-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=codec_name",
		"-of", "default=noprint_wrappers=1:nokey=1",
		candFile,
	).Output()
	if err != nil {
		t.Fatalf("probing audio: %v", err)
	}
	if strings.TrimSpace(string(aProbe)) != "ac3" {
		t.Errorf("expected ac3 audio, got %q", string(aProbe))
	}

	// 5. Verify subtitle codec == subrip (converted from mov_text for Matroska compatibility)
	sProbe, err := exec.Command(ffprobePath, "-v", "error",
		"-select_streams", "s:0",
		"-show_entries", "stream=codec_name",
		"-of", "default=noprint_wrappers=1:nokey=1",
		candFile,
	).Output()
	if err != nil {
		t.Fatalf("probing subtitle: %v", err)
	}
	if strings.TrimSpace(string(sProbe)) != "subrip" {
		t.Errorf("expected subrip subtitle in candidate, got %q", string(sProbe))
	}

	// 6. Verify original file SHA-256 untouched
	finalSHA, err := fileSHA256(sourceFile)
	if err != nil {
		t.Fatalf("hashing source: %v", err)
	}
	if finalSHA != initialSHA {
		t.Fatalf("INTEGRITY BREACH: original source modified! initial: %s, final: %s", initialSHA, finalSHA)
	}
}

// TestWorker_MKV_ASS_Preserved_E2E verifies that MKV with ASS subtitles preserves ASS without converting to SRT.
func TestWorker_MKV_ASS_Preserved_E2E(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("skipping local synthetic transcode test: requires darwin arm64")
	}

	ffmpegPath := "/opt/homebrew/bin/ffmpeg"
	ffprobePath := "/opt/homebrew/bin/ffprobe"
	if _, err := os.Stat(ffmpegPath); err != nil {
		t.Skip("ffmpeg not found at /opt/homebrew/bin/ffmpeg")
	}

	tempDir := t.TempDir()
	sourceFile := filepath.Join(tempDir, "sample_ass.mkv")
	assFile := filepath.Join(tempDir, "sub.ass")

	assContent := `[Script Info]
ScriptType: v4.00+
[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,20,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,2,2,10,10,10,1
[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:00.00,0:00:02.00,Default,,0,0,0,,Stylized Subtitle Test
`
	_ = os.WriteFile(assFile, []byte(assContent), 0644)

	// Generate MKV with H264, AAC, and ASS subtitle
	genCmd := exec.Command(ffmpegPath, "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=24",
		"-f", "lavfi", "-i", "sine=duration=2:frequency=1000",
		"-i", assFile,
		"-c:v", "libx264",
		"-c:a", "aac",
		"-c:s", "ass",
		"-metadata:s:s:0", "language=jpn",
		sourceFile,
	)
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to generate synthetic MKV: %v (%s)", err, out)
	}

	initialSHA, err := fileSHA256(sourceFile)
	if err != nil {
		t.Fatalf("failed to hash source: %v", err)
	}

	candFile := filepath.Join(tempDir, ".navigatorr-candidates", "sample_ass.job_ass.mkv")
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
	cfgData := fmt.Sprintf("ffmpeg: %s\nffprobe: %s\nstate_dir: %s\nallowed_roots:\n  - %s\nmax_parallel_jobs: 1\nquality: 65\n",
		ffmpegPath, ffprobePath, stateDir, tempDir)
	_ = os.WriteFile(cfgFile, []byte(cfgData), 0644)

	binPath := filepath.Join(tempDir, "navigatorr-transcode")
	buildCmd := exec.Command("go", "build", "-o", binPath, "github.com/jakenesler/navigatorr/cmd/navigatorr-transcode")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build navigatorr-transcode: %v (%s)", err, out)
	}

	worker := NewWorker(cfg)
	ctx := context.Background()

	subResp, err := worker.Submit(ctx, SubmitRequest{
		ID:            "job-ass-1",
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

	// Poll status until complete
	deadline := time.Now().Add(30 * time.Second)
	var finalStatus JobStatusResponse
	for time.Now().Before(deadline) {
		st, err := worker.Status(ctx, "job-ass-1")
		if err == nil {
			finalStatus = st
			if st.Status == "completed" || st.Status == "failed" {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	if finalStatus.Status != "completed" {
		t.Fatalf("transcode failed or timed out: %+v", finalStatus)
	}

	// Verify subtitle codec remains ASS (NOT converted to srt/subrip)
	sProbe, err := exec.Command(ffprobePath, "-v", "error",
		"-select_streams", "s:0",
		"-show_entries", "stream=codec_name",
		"-of", "default=noprint_wrappers=1:nokey=1",
		candFile,
	).Output()
	if err != nil {
		t.Fatalf("probing subtitle: %v", err)
	}
	codec := strings.TrimSpace(string(sProbe))
	if codec != "ass" {
		t.Errorf("CRITICAL VIOLATION: ASS subtitle was converted to %q (expected ass preservation!)", codec)
	}

	// No conversions should have occurred
	if len(finalStatus.Conversions) != 0 {
		t.Errorf("expected 0 conversions for ASS subtitle, got: %+v", finalStatus.Conversions)
	}

	// Verify original file SHA-256 untouched
	finalSHA, err := fileSHA256(sourceFile)
	if err != nil {
		t.Fatalf("hashing source: %v", err)
	}
	if finalSHA != initialSHA {
		t.Fatalf("INTEGRITY BREACH: original source modified! initial: %s, final: %s", initialSHA, finalSHA)
	}
}

func TestWorker_SubmitRejectsInvalidPlanBeforeSpawning(t *testing.T) {
	tempDir := t.TempDir()
	sourceFile := filepath.Join(tempDir, "source.mkv")
	_ = os.WriteFile(sourceFile, []byte("media"), 0644)
	candidateFile := filepath.Join(tempDir, "candidate.mkv")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(tempDir, "jobs"),
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: 1,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	// Create an invalid plan (main10 + yuv420p)
	p := &transcode.Plan{
		Container:           "mkv",
		VideoCodec:          "hevc_videotoolbox",
		Quality:             65,
		VideoProfile:        "main10",
		PixelFormat:         "yuv420p",
		ExpectedBitDepth:    10,
		AudioMode:           "copy",
		SubtitleMode:        "preserve",
		PreserveMetadata:    true,
		PreserveChapters:    true,
		PreserveAttachments: true,
		RecipeVersion:       "test-1.0.0",
		RecipeDigest:        "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Resilience:          transcode.ResiliencePlan{MaxAttempts: 1},
	}
	d, _ := transcode.DigestPlan(p)
	p.PlanDigest = d

	jobID := "invalid-plan-job"
	res, err := worker.Submit(ctx, SubmitRequest{
		ID:            jobID,
		SourcePath:    sourceFile,
		CandidatePath: candidateFile,
		Plan:          p,
	}, os.Args[0], "")

	if err == nil {
		t.Fatal("expected Submit to reject invalid plan, but it succeeded")
	}
	if !strings.Contains(res.Error, "main10 requires pixel format p010le") {
		t.Fatalf("expected error message about main10 and pixel format, got: %s", res.Error)
	}

	// Verify no job directory was created on disk
	jobDir := filepath.Join(cfg.StateDir, jobID)
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("expected job directory %s not to exist, but it was created", jobDir)
	}
}

func TestWorker_JobStatusResponseCompleteness(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &WorkerConfig{
		StateDir:        filepath.Join(tempDir, "jobs"),
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: 1,
	}
	worker := NewWorker(cfg)
	ctx := context.Background()

	t.Run("population and serialization with explicit false booleans", func(t *testing.T) {
		fVal := false
		tVal := true
		jobDir := filepath.Join(cfg.StateDir, "job-explicit-false")
		_ = os.MkdirAll(jobDir, 0755)

		plan := &transcode.Plan{
			Container:        "mkv",
			VideoCodec:       "hevc_videotoolbox",
			Quality:          65,
			VideoProfile:     "main",
			PixelFormat:      "yuv420p",
			PrioritizeSpeed:  &fVal,
			SpatialAQ:        &tVal,
			Realtime:         &fVal,
			ExpectedBitDepth: 8,
			RecipeVersion:    "recipe-v2.0",
			RecipeDigest:     "sha256:aaaa",
			PlanDigest:       "sha256:bbbb",
		}

		job := &JobRecord{
			ID:        "job-explicit-false",
			Status:    "running",
			Source:    filepath.Join(tempDir, "src.mkv"),
			Candidate: filepath.Join(tempDir, "cand.mkv"),
			Profile:   "anime-hevc-balanced",
			Plan:      plan,
			CreatedAt: time.Now().UTC(),
		}
		if err := SaveJobAtomic(filepath.Join(jobDir, "job.json"), job); err != nil {
			t.Fatal(err)
		}

		resp, err := worker.Status(ctx, "job-explicit-false")
		if err != nil {
			t.Fatalf("Status error: %v", err)
		}

		// Verify populated fields
		if resp.RecipeVersion != "recipe-v2.0" {
			t.Errorf("got recipe_version %q, want recipe-v2.0", resp.RecipeVersion)
		}
		if resp.RecipeDigest != "sha256:aaaa" {
			t.Errorf("got recipe_digest %q, want sha256:aaaa", resp.RecipeDigest)
		}
		if resp.PlanDigest != "sha256:bbbb" {
			t.Errorf("got plan_digest %q, want sha256:bbbb", resp.PlanDigest)
		}
		if resp.VideoProfile != "main" {
			t.Errorf("got video_profile %q, want main", resp.VideoProfile)
		}
		if resp.PixelFormat != "yuv420p" {
			t.Errorf("got pixel_format %q, want yuv420p", resp.PixelFormat)
		}
		if resp.ExpectedBitDepth != 8 {
			t.Errorf("got expected_bit_depth %d, want 8", resp.ExpectedBitDepth)
		}
		if resp.PrioritizeSpeed == nil || *resp.PrioritizeSpeed != false {
			t.Errorf("got prioritize_speed %v, want explicit pointer to false", resp.PrioritizeSpeed)
		}
		if resp.SpatialAQ == nil || *resp.SpatialAQ != true {
			t.Errorf("got spatial_aq %v, want explicit pointer to true", resp.SpatialAQ)
		}
		if resp.Realtime == nil || *resp.Realtime != false {
			t.Errorf("got realtime %v, want explicit pointer to false", resp.Realtime)
		}

		// Verify JSON serialization preserves explicit false
		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("json.Marshal failed: %v", err)
		}
		jsonStr := string(data)
		if !strings.Contains(jsonStr, `"prioritize_speed":false`) {
			t.Errorf("JSON should contain \"prioritize_speed\":false, got: %s", jsonStr)
		}
		if !strings.Contains(jsonStr, `"spatial_aq":true`) {
			t.Errorf("JSON should contain \"spatial_aq\":true, got: %s", jsonStr)
		}
		if !strings.Contains(jsonStr, `"realtime":false`) {
			t.Errorf("JSON should contain \"realtime\":false, got: %s", jsonStr)
		}
		if !strings.Contains(jsonStr, `"recipe_version":"recipe-v2.0"`) {
			t.Errorf("JSON should contain recipe_version, got: %s", jsonStr)
		}
		if !strings.Contains(jsonStr, `"plan_digest":"sha256:bbbb"`) {
			t.Errorf("JSON should contain plan_digest, got: %s", jsonStr)
		}

		// Unmarshal back and verify pointers
		var roundtrip JobStatusResponse
		if err := json.Unmarshal(data, &roundtrip); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		if roundtrip.PrioritizeSpeed == nil || *roundtrip.PrioritizeSpeed != false {
			t.Errorf("unmarshaled prioritize_speed should be false, got: %v", roundtrip.PrioritizeSpeed)
		}
		if roundtrip.Realtime == nil || *roundtrip.Realtime != false {
			t.Errorf("unmarshaled realtime should be false, got: %v", roundtrip.Realtime)
		}
	})

	t.Run("population and serialization with nil booleans and omitted fields", func(t *testing.T) {
		jobDir := filepath.Join(cfg.StateDir, "job-nil-fields")
		_ = os.MkdirAll(jobDir, 0755)

		plan := &transcode.Plan{
			Container:  "mkv",
			VideoCodec: "hevc_videotoolbox",
			Quality:    65,
		}

		job := &JobRecord{
			ID:        "job-nil-fields",
			Status:    "running",
			Source:    filepath.Join(tempDir, "src.mkv"),
			Candidate: filepath.Join(tempDir, "cand.mkv"),
			Profile:   "hevc-vt",
			Plan:      plan,
			CreatedAt: time.Now().UTC(),
		}
		if err := SaveJobAtomic(filepath.Join(jobDir, "job.json"), job); err != nil {
			t.Fatal(err)
		}

		resp, err := worker.Status(ctx, "job-nil-fields")
		if err != nil {
			t.Fatalf("Status error: %v", err)
		}

		if resp.PrioritizeSpeed != nil {
			t.Errorf("expected nil prioritize_speed, got %v", resp.PrioritizeSpeed)
		}
		if resp.SpatialAQ != nil {
			t.Errorf("expected nil spatial_aq, got %v", resp.SpatialAQ)
		}
		if resp.Realtime != nil {
			t.Errorf("expected nil realtime, got %v", resp.Realtime)
		}
		if resp.VideoProfile != "" {
			t.Errorf("expected empty video_profile, got %q", resp.VideoProfile)
		}
		if resp.PixelFormat != "" {
			t.Errorf("expected empty pixel_format, got %q", resp.PixelFormat)
		}
		if resp.ExpectedBitDepth != 0 {
			t.Errorf("expected 0 expected_bit_depth, got %d", resp.ExpectedBitDepth)
		}

		// Verify JSON serialization omits nil and zero fields
		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("json.Marshal failed: %v", err)
		}
		jsonStr := string(data)
		for _, omitted := range []string{"prioritize_speed", "spatial_aq", "realtime", "video_profile", "pixel_format", "expected_bit_depth", "recipe_version", "recipe_digest", "plan_digest"} {
			if strings.Contains(jsonStr, `"`+omitted+`"`) {
				t.Errorf("JSON should omit %q, got: %s", omitted, jsonStr)
			}
		}

		// Unmarshal back and verify nil remains nil
		var roundtrip JobStatusResponse
		if err := json.Unmarshal(data, &roundtrip); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		if roundtrip.PrioritizeSpeed != nil {
			t.Errorf("unmarshaled prioritize_speed should be nil, got: %v", roundtrip.PrioritizeSpeed)
		}
		if roundtrip.SpatialAQ != nil {
			t.Errorf("unmarshaled spatial_aq should be nil, got: %v", roundtrip.SpatialAQ)
		}
		if roundtrip.Realtime != nil {
			t.Errorf("unmarshaled realtime should be nil, got: %v", roundtrip.Realtime)
		}
	})
}
