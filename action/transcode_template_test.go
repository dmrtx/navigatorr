package action

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

type mockTranscodeExecutor struct {
	submitCalls int32
	statusCalls int32
	cancelCalls int32
	doctorCalls int32

	doctorFunc func(ctx context.Context) error
	submitFunc func(ctx context.Context, req transcode.Request) (transcode.Job, error)
	statusFunc func(ctx context.Context, jobID string) (transcode.JobStatus, error)
	cancelFunc func(ctx context.Context, jobID string) error
}

func (m *mockTranscodeExecutor) Doctor(ctx context.Context) error {
	atomic.AddInt32(&m.doctorCalls, 1)
	if m.doctorFunc != nil {
		return m.doctorFunc(ctx)
	}
	return nil
}

func (m *mockTranscodeExecutor) Submit(ctx context.Context, req transcode.Request) (transcode.Job, error) {
	atomic.AddInt32(&m.submitCalls, 1)
	if m.submitFunc != nil {
		return m.submitFunc(ctx, req)
	}
	return transcode.Job{ID: req.ID}, nil
}

func (m *mockTranscodeExecutor) Status(ctx context.Context, jobID string) (transcode.JobStatus, error) {
	atomic.AddInt32(&m.statusCalls, 1)
	if m.statusFunc != nil {
		return m.statusFunc(ctx, jobID)
	}
	return transcode.JobStatus{
		ID:       jobID,
		Status:   transcode.StatusCompleted,
		Progress: 100,
	}, nil
}

func (m *mockTranscodeExecutor) Cancel(ctx context.Context, jobID string) error {
	atomic.AddInt32(&m.cancelCalls, 1)
	if m.cancelFunc != nil {
		return m.cancelFunc(ctx, jobID)
	}
	return nil
}

func createFakeFFprobeScript(t *testing.T, streamsJSON string) string {
	dir := t.TempDir()
	probePath := filepath.Join(dir, "ffprobe")
	script := fmt.Sprintf(`#!/bin/sh
cat << 'JSON'
%s
JSON
`, streamsJSON)
	if err := os.WriteFile(probePath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write fake ffprobe: %v", err)
	}
	return probePath
}

const defaultValidFFprobeJSON = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "width": 1920, "height": 1080},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "tags": {"language": "jpn"}},
    {"index": 2, "codec_type": "subtitle", "codec_name": "ass", "tags": {"language": "eng"}},
    {"index": 3, "codec_type": "attachment", "codec_name": "ttf", "tags": {"filename": "font.ttf"}}
  ],
  "format": {
    "format_name": "matroska",
    "duration": "1420.0",
    "size": "500000000"
  },
  "chapters": [{"id": 0}]
}`

func setupTranscodeEngine(t *testing.T, tc transcode.Executor, ffprobePath string, readRoots, writeRoots []string, allowDestructive bool) (*Engine, *store.Store) {
	dbPath := filepath.Join(t.TempDir(), "test_action.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("opening test store: %v", err)
	}

	res, err := fsop.NewResolver(readRoots, writeRoots)
	if err != nil {
		t.Fatalf("creating resolver: %v", err)
	}

	cfg := &config.Config{
		AllowDestructive: allowDestructive,
		Media: config.MediaConfig{
			AllowedReadRoots:  readRoots,
			AllowedWriteRoots: writeRoots,
			FfprobePath:       ffprobePath,
		},
		Transcode: config.TranscodeConfig{
			Enabled:  true,
			Executor: "ssh",
			SSH: config.SSHExecutorConfig{
				Host:    "192.168.68.55",
				User:    "morotxo",
				Command: "/Users/morotxo/.local/bin/navigatorr-transcode",
			},
		},
	}

	engine := NewEngine(EngineDeps{
		Store:     st,
		Config:    cfg,
		Fs:        res,
		Ffprobe:   ffprobePath,
		Transcode: tc,
		StartTime: time.Now(),
	})

	return engine, st
}

func TestTranscode_PreflightValid(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "Episode1.mkv")
	_ = os.WriteFile(origFile, []byte("dummy-media-content-for-original-12345"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:            jobID,
				Status:        transcode.StatusCompleted,
				CandidatePath: origFile,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probePath, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("unexpected error running action: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("expected completed action, got %s (error: %s)", res.Status, res.Error)
	}
	if res.Outputs["original"] == nil {
		t.Error("expected original snapshot in outputs")
	}
}

func TestTranscode_PathOutsideAllowlistRejected(t *testing.T) {
	mediaDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.mkv")
	_ = os.WriteFile(outsideFile, []byte("outside content"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockExecutor := &mockTranscodeExecutor{}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probePath, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": outsideFile,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("expected failed status for path outside allowlist, got %s", res.Status)
	}
	if !strings.Contains(res.Error, "outside allowed read roots") {
		t.Errorf("expected allowlist error message, got: %s", res.Error)
	}
	if atomic.LoadInt32(&mockExecutor.submitCalls) != 0 {
		t.Error("should not have called executor submit when path was rejected")
	}
}

func TestTranscode_FirstExecutionSubmitsExactlyOnce(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "Movie.mkv")
	_ = os.WriteFile(origFile, []byte("original movie bytes"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:       jobID,
				Status:   transcode.StatusRunning,
				Progress: 35.0,
				FPS:      45.0,
				Speed:    2.5,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probePath, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusWaitingExternal {
		t.Fatalf("expected waiting_external status, got %s", res.Status)
	}
	if atomic.LoadInt32(&mockExecutor.submitCalls) != 1 {
		t.Fatalf("expected exactly 1 submit call, got %d", atomic.LoadInt32(&mockExecutor.submitCalls))
	}
	if res.WaitingCondition != "transcode_complete" {
		t.Errorf("unexpected waiting condition: %v", res.WaitingCondition)
	}
}

func TestTranscode_WaitingExternalWhileTranscodeRuns(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "Ep2.mkv")
	_ = os.WriteFile(origFile, []byte("original ep2 bytes"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:       jobID,
				Status:   transcode.StatusRunning,
				Progress: 52.3,
				FPS:      72.0,
				Speed:    4.1,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probePath, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusWaitingExternal {
		t.Fatalf("expected waiting_external, got %s", res.Status)
	}
	if !strings.Contains(res.WaitingReason, "52.3%") {
		t.Errorf("expected progress in waiting reason, got: %s", res.WaitingReason)
	}
}

func TestTranscode_ResumeNoDuplicateSubmit(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "Ep3.mkv")
	_ = os.WriteFile(origFile, []byte("original ep3 bytes"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	statusCalls := 0
	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			statusCalls++
			if statusCalls == 1 {
				return transcode.JobStatus{
					ID:       jobID,
					Status:   transcode.StatusRunning,
					Progress: 50.0,
				}, nil
			}
			return transcode.JobStatus{
				ID:            jobID,
				Status:        transcode.StatusCompleted,
				CandidatePath: origFile,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probePath, []string{mediaDir}, []string{mediaDir}, false)
	// Initial Run
	res1, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run 1 error: %v", err)
	}
	if res1.Status != StatusWaitingExternal {
		t.Fatalf("expected waiting_external, got %s", res1.Status)
	}
	if atomic.LoadInt32(&mockExecutor.submitCalls) != 1 {
		t.Fatalf("expected 1 submit call, got %d", atomic.LoadInt32(&mockExecutor.submitCalls))
	}

	// Resume Action
	res2, err := engine.Resume(context.Background(), res1.ID, "", nil)
	if err != nil {
		t.Fatalf("resume error: %v", err)
	}
	if res2.Status != StatusCompleted {
		t.Fatalf("expected completed status after resume, got %s (error: %s)", res2.Status, res2.Error)
	}
	// CONFIRMATION: Resume did NOT submit a second job!
	if atomic.LoadInt32(&mockExecutor.submitCalls) != 1 {
		t.Fatalf("expected submitCalls to remain 1 after resume, got %d", atomic.LoadInt32(&mockExecutor.submitCalls))
	}
}

func TestTranscode_TranscodeFailureLeavesOriginalUntouched(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "FailedMovie.mkv")
	originalContent := "pristine original content that must never be deleted or modified"
	_ = os.WriteFile(origFile, []byte(originalContent), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:     jobID,
				Status: transcode.StatusFailed,
				Error:  "ffmpeg crashed with exit status 137 (out of memory)",
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probePath, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("expected status failed, got %s", res.Status)
	}
	if !strings.Contains(res.Error, "Transcode failed") {
		t.Errorf("unexpected error: %s", res.Error)
	}

	// Verify original file exists and is intact
	content, err := os.ReadFile(origFile)
	if err != nil {
		t.Fatalf("original file could not be read: %v", err)
	}
	if string(content) != originalContent {
		t.Errorf("original file was modified: got %q, want %q", string(content), originalContent)
	}
}

func TestTranscode_InvalidOutputLeavesOriginalUntouched(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "InvalidOut.mkv")
	originalContent := "original content"
	_ = os.WriteFile(origFile, []byte(originalContent), 0644)

	// Transcoded output is 0 bytes
	emptyOutput := filepath.Join(mediaDir, "EmptyOutput.mkv")
	_ = os.WriteFile(emptyOutput, []byte(""), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:            jobID,
				Status:        transcode.StatusCompleted,
				CandidatePath: emptyOutput,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probePath, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("expected failed status on empty output, got %s", res.Status)
	}

	content, _ := os.ReadFile(origFile)
	if string(content) != originalContent {
		t.Errorf("original was modified: %s", string(content))
	}
}

func TestTranscode_MissingAudioDetected(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "AnimeMultiAudio.mkv")
	_ = os.WriteFile(origFile, []byte("original anime content"), 0644)

	outputFile := filepath.Join(mediaDir, "AnimeOutput.mkv")
	_ = os.WriteFile(outputFile, []byte("transcoded anime content"), 0644)

	// Original has jpn & eng audio; output has only eng audio
	dir := t.TempDir()
	probeScript := filepath.Join(dir, "ffprobe")
	script := fmt.Sprintf(`#!/bin/sh
if echo "$*" | grep -q "AnimeMultiAudio.mkv"; then
cat << 'JSON'
{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080},
    {"index": 1, "codec_type": "audio", "codec_name": "flac", "tags": {"language": "jpn"}},
    {"index": 2, "codec_type": "audio", "codec_name": "aac", "tags": {"language": "eng"}}
  ],
  "format": {"format_name": "matroska", "duration": "1420.0", "size": "800000000"}
}
JSON
else
cat << 'JSON'
{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "width": 1920, "height": 1080},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "tags": {"language": "eng"}}
  ],
  "format": {"format_name": "matroska", "duration": "1420.0", "size": "300000000"}
}
JSON
fi
`)
	_ = os.WriteFile(probeScript, []byte(script), 0755)

	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:            jobID,
				Status:        transcode.StatusCompleted,
				CandidatePath: outputFile,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probeScript, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected waiting_decision when audio track is lost, got %s", res.Status)
	}
	if !strings.Contains(res.WaitingReason, "Audio stream lost") || !strings.Contains(res.WaitingReason, "jpn") {
		t.Errorf("expected warning about lost Japanese audio, got: %s", res.WaitingReason)
	}
}

func TestTranscode_MissingSubtitleDetected(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "AnimeSub.mkv")
	_ = os.WriteFile(origFile, []byte("original anime content"), 0644)

	outputFile := filepath.Join(mediaDir, "AnimeSubOut.mkv")
	_ = os.WriteFile(outputFile, []byte("transcoded anime content"), 0644)

	dir := t.TempDir()
	probeScript := filepath.Join(dir, "ffprobe")
	script := fmt.Sprintf(`#!/bin/sh
if echo "$*" | grep -q "AnimeSub.mkv"; then
cat << 'JSON'
{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080},
    {"index": 1, "codec_type": "subtitle", "codec_name": "ass", "tags": {"language": "eng"}}
  ],
  "format": {"format_name": "matroska", "duration": "1420.0", "size": "800000000"}
}
JSON
else
cat << 'JSON'
{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "width": 1920, "height": 1080}
  ],
  "format": {"format_name": "matroska", "duration": "1420.0", "size": "300000000"}
}
JSON
fi
`)
	_ = os.WriteFile(probeScript, []byte(script), 0755)

	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:            jobID,
				Status:        transcode.StatusCompleted,
				CandidatePath: outputFile,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probeScript, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected waiting_decision when subtitle track is lost, got %s", res.Status)
	}
	if !strings.Contains(res.WaitingReason, "Subtitle stream lost") && !strings.Contains(res.WaitingReason, "ASS/SSA") {
		t.Errorf("expected subtitle warning, got: %s", res.WaitingReason)
	}
}

func TestTranscode_DurationMismatchDetected(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "TruncatedOriginal.mkv")
	_ = os.WriteFile(origFile, []byte("original content"), 0644)

	outputFile := filepath.Join(mediaDir, "TruncatedOutput.mkv")
	_ = os.WriteFile(outputFile, []byte("truncated output content"), 0644)

	dir := t.TempDir()
	probeScript := filepath.Join(dir, "ffprobe")
	script := fmt.Sprintf(`#!/bin/sh
if echo "$*" | grep -q "TruncatedOriginal.mkv"; then
cat << 'JSON'
{
  "streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080}],
  "format": {"format_name": "matroska", "duration": "1420.0", "size": "800000000"}
}
JSON
else
cat << 'JSON'
{
  "streams": [{"index": 0, "codec_type": "video", "codec_name": "hevc", "width": 1920, "height": 1080}],
  "format": {"format_name": "matroska", "duration": "200.0", "size": "100000000"}
}
JSON
fi
`)
	_ = os.WriteFile(probeScript, []byte(script), 0755)

	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:            jobID,
				Status:        transcode.StatusCompleted,
				CandidatePath: outputFile,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probeScript, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected waiting_decision on duration mismatch, got %s", res.Status)
	}
	if !strings.Contains(res.WaitingReason, "Duration discrepancy") {
		t.Errorf("expected duration discrepancy reason, got: %s", res.WaitingReason)
	}
}

func TestTranscode_RestartPersistResume(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "RestartTest.mkv")
	_ = os.WriteFile(origFile, []byte("content"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	dbPath := filepath.Join(t.TempDir(), "persist.db")
	st1, _ := store.Open(dbPath)

	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:       jobID,
				Status:   transcode.StatusRunning,
				Progress: 40.0,
			}, nil
		},
	}

	resResolver, _ := fsop.NewResolver([]string{mediaDir}, []string{mediaDir})
	cfg := &config.Config{
		Media: config.MediaConfig{AllowedReadRoots: []string{mediaDir}, AllowedWriteRoots: []string{mediaDir}},
		Transcode: config.TranscodeConfig{
			Enabled:  true,
			Executor: "ssh",
		},
	}
	engine1 := NewEngine(EngineDeps{
		Store:     st1,
		Config:    cfg,
		Fs:        resResolver,
		Ffprobe:   probePath,
		Transcode: mockExecutor,
	})

	// Run action to waiting_external
	actionRes, err := engine1.Run(context.Background(), "transcode_media", map[string]any{"path": origFile})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if actionRes.Status != StatusWaitingExternal {
		t.Fatalf("expected waiting_external, got %s", actionRes.Status)
	}

	// Close store 1 and simulate application restart
	_ = st1.Close()

	// Reopen store from same sqlite DB
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	defer st2.Close()

	// Update mock to return completed now
	mockExecutor.statusFunc = func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
		return transcode.JobStatus{
			ID:            jobID,
			Status:        transcode.StatusCompleted,
			CandidatePath: origFile,
		}, nil
	}

	engine2 := NewEngine(EngineDeps{
		Store:     st2,
		Config:    cfg,
		Fs:        resResolver,
		Ffprobe:   probePath,
		Transcode: mockExecutor,
	})

	// Resume action on new engine instance
	resumed, err := engine2.Resume(context.Background(), actionRes.ID, "", nil)
	if err != nil {
		t.Fatalf("resume error: %v", err)
	}
	if resumed.Status != StatusCompleted {
		t.Fatalf("expected completed status on resumed action after restart, got %s (error: %s)", resumed.Status, resumed.Error)
	}
	// Verify submit was NOT called again after restart
	if atomic.LoadInt32(&mockExecutor.submitCalls) != 1 {
		t.Errorf("expected exactly 1 submit call across restart, got %d", atomic.LoadInt32(&mockExecutor.submitCalls))
	}
}

func TestTranscode_IdempotencyKeyBehavior(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "Idempotent.mkv")
	_ = os.WriteFile(origFile, []byte("content"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:       jobID,
				Status:   transcode.StatusRunning,
				Progress: 10.0,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probePath, []string{mediaDir}, []string{mediaDir}, false)

	// Run with idempotency key
	res1, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	}, "idemp-key-1234")
	if err != nil {
		t.Fatalf("run 1 error: %v", err)
	}

	// Run again with SAME idempotency key
	res2, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	}, "idemp-key-1234")
	if err != nil {
		t.Fatalf("run 2 error: %v", err)
	}

	// Should return the exact same action instance ID without duplicate submit
	if res1.ID != res2.ID {
		t.Errorf("expected same action ID %s, got %s", res1.ID, res2.ID)
	}
	if atomic.LoadInt32(&mockExecutor.submitCalls) != 1 {
		t.Errorf("expected 1 submit call, got %d", atomic.LoadInt32(&mockExecutor.submitCalls))
	}
}

func TestTranscode_Disabled(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "NoTranscode.mkv")
	_ = os.WriteFile(origFile, []byte("content"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)

	// nil executor
	engine, _ := setupTranscodeEngine(t, nil, probePath, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("expected failed status when transcode is disabled, got %s", res.Status)
	}
	if !strings.Contains(res.Error, "disabled or not configured") {
		t.Errorf("expected disabled error message, got: %s", res.Error)
	}
}

func TestTranscode_ReplaceOriginal_DefaultFalse(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "OriginalFile.mkv")
	originalContent := "original content to preserve"
	_ = os.WriteFile(origFile, []byte(originalContent), 0644)

	outputFile := filepath.Join(mediaDir, "TranscodedOutput.mkv")
	transcodedContent := "transcoded hevc output"
	_ = os.WriteFile(outputFile, []byte(transcodedContent), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:            jobID,
				Status:        transcode.StatusCompleted,
				CandidatePath: outputFile,
			}, nil
		},
	}

	// replace_original is omitted (defaults to false)
	engine, _ := setupTranscodeEngine(t, mockExecutor, probePath, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("expected completed, got %s (error: %s)", res.Status, res.Error)
	}

	// Verify original file exists and was NOT replaced
	c, _ := os.ReadFile(origFile)
	if string(c) != originalContent {
		t.Errorf("original was unexpectedly modified: got %s", string(c))
	}
	// Verify output file also still exists
	oc, _ := os.ReadFile(outputFile)
	if string(oc) != transcodedContent {
		t.Errorf("output was unexpectedly modified: %s", string(oc))
	}
}

func TestTranscode_ReplaceOriginal_DestructiveGate(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "OriginalGate.mkv")
	_ = os.WriteFile(origFile, []byte("original"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockExecutor := &mockTranscodeExecutor{}

	// replace_original: true must fail explicitly and immediately with rejection
	engine, _ := setupTranscodeEngine(t, mockExecutor, probePath, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":             origFile,
		"replace_original": true,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("expected failed status when replace_original is true, got %s", res.Status)
	}
	if !strings.Contains(res.Error, "destructive replacement (replace_original: true) is not supported") {
		t.Errorf("expected destructive replacement error, got: %s", res.Error)
	}

	// Original still intact!
	c, _ := os.ReadFile(origFile)
	if string(c) != "original" {
		t.Error("original was modified despite safety gate")
	}
	if atomic.LoadInt32(&mockExecutor.submitCalls) != 0 {
		t.Errorf("expected 0 submit calls when replace_original rejected, got %d", atomic.LoadInt32(&mockExecutor.submitCalls))
	}
}

func TestTranscode_ValidationDiscrepancy_ResumeAcceptLoss(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "AnimeLoss.mkv")
	origBytes := []byte("anime original bytes for checksum verification")
	_ = os.WriteFile(origFile, origBytes, 0644)

	outputFile := filepath.Join(mediaDir, "AnimeLossOut.mkv")
	_ = os.WriteFile(outputFile, []byte("transcoded anime candidate bytes"), 0644)

	// Original has jpn & eng audio; output has only eng audio
	dir := t.TempDir()
	probeScript := filepath.Join(dir, "ffprobe")
	script := `#!/bin/sh
if echo "$*" | grep -q "AnimeLoss.mkv"; then
cat << 'JSON'
{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080},
    {"index": 1, "codec_type": "audio", "codec_name": "flac", "tags": {"language": "jpn"}},
    {"index": 2, "codec_type": "audio", "codec_name": "aac", "tags": {"language": "eng"}}
  ],
  "format": {"format_name": "matroska", "duration": "1420.0", "size": "800000000"}
}
JSON
else
cat << 'JSON'
{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "width": 1920, "height": 1080},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "tags": {"language": "eng"}}
  ],
  "format": {"format_name": "matroska", "duration": "1420.0", "size": "300000000"}
}
JSON
fi
`
	_ = os.WriteFile(probeScript, []byte(script), 0755)

	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:            jobID,
				Status:        transcode.StatusCompleted,
				CandidatePath: outputFile,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probeScript, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected waiting_decision when audio track is lost, got %s", res.Status)
	}

	// Resume action with decision="accept_loss"
	resumed, err := engine.Resume(context.Background(), res.ID, "accept_loss", nil)
	if err != nil {
		t.Fatalf("resume error: %v", err)
	}
	if resumed.Status != StatusCompleted {
		t.Fatalf("expected completed status after accepting loss, got %s (error: %s)", resumed.Status, resumed.Error)
	}

	// Check original file is still 100% intact
	curBytes, err := os.ReadFile(origFile)
	if err != nil {
		t.Fatalf("failed reading original file: %v", err)
	}
	if !bytes.Equal(curBytes, origBytes) {
		t.Error("original file bytes modified after resume accept_loss!")
	}

	// Outputs verify candidate_path and original_intact
	if resumed.Outputs["candidate_path"] != outputFile {
		t.Errorf("expected candidate_path %s, got %v", outputFile, resumed.Outputs["candidate_path"])
	}
	if resumed.Outputs["original_intact"] != true {
		t.Errorf("expected original_intact true, got %v", resumed.Outputs["original_intact"])
	}
}

func TestTranscode_ValidationDiscrepancy_ResumeReject(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "AnimeReject.mkv")
	origBytes := []byte("anime original bytes must be preserved on reject")
	_ = os.WriteFile(origFile, origBytes, 0644)

	outputFile := filepath.Join(mediaDir, "AnimeRejectOut.mkv")
	_ = os.WriteFile(outputFile, []byte("bad output bytes"), 0644)

	dir := t.TempDir()
	probeScript := filepath.Join(dir, "ffprobe")
	script := `#!/bin/sh
if echo "$*" | grep -q "AnimeReject.mkv"; then
cat << 'JSON'
{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080},
    {"index": 1, "codec_type": "audio", "codec_name": "flac", "tags": {"language": "jpn"}}
  ],
  "format": {"format_name": "matroska", "duration": "1420.0", "size": "800000000"}
}
JSON
else
cat << 'JSON'
{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "width": 1920, "height": 1080}
  ],
  "format": {"format_name": "matroska", "duration": "1420.0", "size": "300000000"}
}
JSON
fi
`
	_ = os.WriteFile(probeScript, []byte(script), 0755)

	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:            jobID,
				Status:        transcode.StatusCompleted,
				CandidatePath: outputFile,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probeScript, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected waiting_decision, got %s", res.Status)
	}

	// Resume action with decision="reject"
	resumed, err := engine.Resume(context.Background(), res.ID, "reject", nil)
	if err != nil {
		t.Fatalf("resume error: %v", err)
	}
	if resumed.Status != StatusFailed {
		t.Fatalf("expected failed status after reject decision, got %s", resumed.Status)
	}
	if !strings.Contains(resumed.Error, "rejected by user decision") {
		t.Errorf("expected reject error message, got: %s", resumed.Error)
	}

	// Check original file is untouched
	curBytes, err := os.ReadFile(origFile)
	if err != nil {
		t.Fatalf("failed reading original file: %v", err)
	}
	if !bytes.Equal(curBytes, origBytes) {
		t.Error("original file bytes modified after reject!")
	}
}

func TestTranscode_CandidateOutputReported(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "CandidateSource.mkv")
	origBytes := []byte("original pristine source bytes for candidate test")
	_ = os.WriteFile(origFile, origBytes, 0644)

	outputDir := t.TempDir()
	candidateFile := filepath.Join(outputDir, "CandidateSource.mkv")
	_ = os.WriteFile(candidateFile, []byte("candidate transcoded bytes in output folder"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:            jobID,
				Status:        transcode.StatusCompleted,
				CandidatePath: candidateFile,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probePath, []string{mediaDir, outputDir}, []string{mediaDir, outputDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("expected completed status, got %s (error: %s)", res.Status, res.Error)
	}

	// Verify outputs contain candidate_path and output_path matching candidateFile
	if res.Outputs["candidate_path"] != candidateFile {
		t.Errorf("expected candidate_path == %s, got %v", candidateFile, res.Outputs["candidate_path"])
	}
	if res.Outputs["output_path"] != candidateFile {
		t.Errorf("expected output_path == %s, got %v", candidateFile, res.Outputs["output_path"])
	}
	resolvedOrig, err := filepath.EvalSymlinks(origFile)
	if err != nil {
		resolvedOrig = origFile
	}
	if res.Outputs["original_path"] != resolvedOrig {
		t.Errorf("expected original_path == %s, got %v", resolvedOrig, res.Outputs["original_path"])
	}
	if res.Outputs["original_intact"] != true {
		t.Errorf("expected original_intact == true, got %v", res.Outputs["original_intact"])
	}
	if res.Outputs["replace_original"] != false {
		t.Errorf("expected replace_original == false, got %v", res.Outputs["replace_original"])
	}

	// Verify original file checksum
	h := sha256.Sum256(origBytes)
	expectedSHA := fmt.Sprintf("%x", h[:])
	if res.Outputs["original_sha256"] != expectedSHA {
		t.Errorf("expected original_sha256 %s, got %v", expectedSHA, res.Outputs["original_sha256"])
	}

	// Verify original file on disk is bit-for-bit identical
	curBytes, err := os.ReadFile(origFile)
	if err != nil {
		t.Fatalf("failed reading original: %v", err)
	}
	if !bytes.Equal(curBytes, origBytes) {
		t.Errorf("original file was modified on disk!")
	}
}
