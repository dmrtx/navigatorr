package action

import (
	"context"
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
	"github.com/jakenesler/navigatorr/tdarr"
)

type mockTdarrClient struct {
	submitCalls   int32
	statusCalls   int32
	cancelCalls   int32
	submitFunc    func(ctx context.Context, req tdarr.SubmitRequest) (*tdarr.SubmitResponse, error)
	jobStatusFunc func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error)
	cancelFunc    func(ctx context.Context, req tdarr.CancelRequest) error
}

func (m *mockTdarrClient) Status(ctx context.Context) (*tdarr.ServerStatus, error) {
	atomic.AddInt32(&m.statusCalls, 1)
	return &tdarr.ServerStatus{Status: "good", Version: "2.87.01"}, nil
}

func (m *mockTdarrClient) Nodes(ctx context.Context) (map[string]tdarr.Node, error) {
	return map[string]tdarr.Node{}, nil
}

func (m *mockTdarrClient) Submit(ctx context.Context, req tdarr.SubmitRequest) (*tdarr.SubmitResponse, error) {
	atomic.AddInt32(&m.submitCalls, 1)
	if m.submitFunc != nil {
		return m.submitFunc(ctx, req)
	}
	return &tdarr.SubmitResponse{
		Success:   true,
		Reference: "tdarr-job-test-1",
	}, nil
}

func (m *mockTdarrClient) JobStatus(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
	if m.jobStatusFunc != nil {
		return m.jobStatusFunc(ctx, ref)
	}
	return &tdarr.JobStatusResponse{
		Found:    true,
		Status:   "completed",
		Progress: 100,
	}, nil
}

func (m *mockTdarrClient) Cancel(ctx context.Context, req tdarr.CancelRequest) error {
	atomic.AddInt32(&m.cancelCalls, 1)
	if m.cancelFunc != nil {
		return m.cancelFunc(ctx, req)
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

func setupTranscodeEngine(t *testing.T, tc tdarr.Client, ffprobePath string, readRoots, writeRoots []string, allowDestructive bool) (*Engine, *store.Store) {
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
		Tdarr: config.TdarrConfig{
			Enabled: true,
			URL:     "http://127.0.0.1:8265",
			PathMappings: []config.PathMapping{
				{Local: readRoots[0], Server: "/media"},
			},
		},
	}

	engine := NewEngine(EngineDeps{
		Store:     st,
		Config:    cfg,
		Fs:        res,
		Ffprobe:   ffprobePath,
		Tdarr:     tc,
		StartTime: time.Now(),
	})

	return engine, st
}

func TestTranscode_PreflightValid(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "Episode1.mkv")
	_ = os.WriteFile(origFile, []byte("dummy-media-content-for-original-12345"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockClient := &mockTdarrClient{
		jobStatusFunc: func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
			return &tdarr.JobStatusResponse{
				Found:      true,
				Status:     "completed",
				OutputPath: origFile,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockClient, probePath, []string{mediaDir}, []string{mediaDir}, false)
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
	mockClient := &mockTdarrClient{}

	engine, _ := setupTranscodeEngine(t, mockClient, probePath, []string{mediaDir}, []string{mediaDir}, false)
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
	if atomic.LoadInt32(&mockClient.submitCalls) != 0 {
		t.Error("should not have called Tdarr submit when path was rejected")
	}
}

func TestTranscode_FirstExecutionSubmitsExactlyOnce(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "Movie.mkv")
	_ = os.WriteFile(origFile, []byte("original movie bytes"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockClient := &mockTdarrClient{
		jobStatusFunc: func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
			return &tdarr.JobStatusResponse{
				Found:    true,
				Status:   "running",
				Progress: 35.0,
				ETA:      "00:10:00",
				FPS:      45.0,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockClient, probePath, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusWaitingExternal {
		t.Fatalf("expected waiting_external status, got %s", res.Status)
	}
	if atomic.LoadInt32(&mockClient.submitCalls) != 1 {
		t.Fatalf("expected exactly 1 submit call, got %d", atomic.LoadInt32(&mockClient.submitCalls))
	}
	if res.Outputs["tdarr_job_id"] != "tdarr-job-test-1" {
		t.Errorf("unexpected job ID: %v", res.Outputs["tdarr_job_id"])
	}
}

func TestTranscode_WaitingExternalWhileTdarrRuns(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "Ep2.mkv")
	_ = os.WriteFile(origFile, []byte("original ep2 bytes"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockClient := &mockTdarrClient{
		jobStatusFunc: func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
			return &tdarr.JobStatusResponse{
				Found:    true,
				Status:   "running",
				Progress: 52.3,
				ETA:      "00:04:12",
				FPS:      72.0,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockClient, probePath, []string{mediaDir}, []string{mediaDir}, false)
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
	mockClient := &mockTdarrClient{
		jobStatusFunc: func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
			statusCalls++
			if statusCalls == 1 {
				return &tdarr.JobStatusResponse{
					Found:    true,
					Status:   "running",
					Progress: 50.0,
				}, nil
			}
			return &tdarr.JobStatusResponse{
				Found:      true,
				Status:     "completed",
				OutputPath: origFile,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockClient, probePath, []string{mediaDir}, []string{mediaDir}, false)
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
	if atomic.LoadInt32(&mockClient.submitCalls) != 1 {
		t.Fatalf("expected 1 submit call, got %d", atomic.LoadInt32(&mockClient.submitCalls))
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
	if atomic.LoadInt32(&mockClient.submitCalls) != 1 {
		t.Fatalf("expected submitCalls to remain 1 after resume, got %d", atomic.LoadInt32(&mockClient.submitCalls))
	}
}

func TestTranscode_TdarrFailureLeavesOriginalUntouched(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "FailedMovie.mkv")
	originalContent := "pristine original content that must never be deleted or modified"
	_ = os.WriteFile(origFile, []byte(originalContent), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockClient := &mockTdarrClient{
		jobStatusFunc: func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
			return &tdarr.JobStatusResponse{
				Found:   true,
				Status:  "failed",
				Error:   "Tdarr transcode node crashed (out of memory)",
				Details: "worker failed with code 137",
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockClient, probePath, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("expected status failed, got %s", res.Status)
	}
	if !strings.Contains(res.Error, "Tdarr transcode failed") {
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
	mockClient := &mockTdarrClient{
		jobStatusFunc: func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
			return &tdarr.JobStatusResponse{
				Found:      true,
				Status:     "completed",
				OutputPath: emptyOutput,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockClient, probePath, []string{mediaDir}, []string{mediaDir}, false)
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

	mockClient := &mockTdarrClient{
		jobStatusFunc: func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
			return &tdarr.JobStatusResponse{
				Found:      true,
				Status:     "completed",
				OutputPath: outputFile,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockClient, probeScript, []string{mediaDir}, []string{mediaDir}, false)
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

	// Original has ASS subtitles; output lost them
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

	mockClient := &mockTdarrClient{
		jobStatusFunc: func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
			return &tdarr.JobStatusResponse{
				Found:      true,
				Status:     "completed",
				OutputPath: outputFile,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockClient, probeScript, []string{mediaDir}, []string{mediaDir}, false)
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

	// Original duration 1420.0s, output only 200.0s
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

	mockClient := &mockTdarrClient{
		jobStatusFunc: func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
			return &tdarr.JobStatusResponse{
				Found:      true,
				Status:     "completed",
				OutputPath: outputFile,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockClient, probeScript, []string{mediaDir}, []string{mediaDir}, false)
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

	mockClient := &mockTdarrClient{
		jobStatusFunc: func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
			return &tdarr.JobStatusResponse{
				Found:    true,
				Status:   "running",
				Progress: 40.0,
			}, nil
		},
	}

	resResolver, _ := fsop.NewResolver([]string{mediaDir}, []string{mediaDir})
	cfg := &config.Config{
		Media: config.MediaConfig{AllowedReadRoots: []string{mediaDir}, AllowedWriteRoots: []string{mediaDir}},
	}
	engine1 := NewEngine(EngineDeps{
		Store:   st1,
		Config:  cfg,
		Fs:      resResolver,
		Ffprobe: probePath,
		Tdarr:   mockClient,
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
	mockClient.jobStatusFunc = func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
		return &tdarr.JobStatusResponse{
			Found:      true,
			Status:     "completed",
			OutputPath: origFile,
		}, nil
	}

	engine2 := NewEngine(EngineDeps{
		Store:   st2,
		Config:  cfg,
		Fs:      resResolver,
		Ffprobe: probePath,
		Tdarr:   mockClient,
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
	if atomic.LoadInt32(&mockClient.submitCalls) != 1 {
		t.Errorf("expected exactly 1 submit call across restart, got %d", atomic.LoadInt32(&mockClient.submitCalls))
	}
}

func TestTranscode_IdempotencyKeyBehavior(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "Idempotent.mkv")
	_ = os.WriteFile(origFile, []byte("content"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockClient := &mockTdarrClient{
		jobStatusFunc: func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
			return &tdarr.JobStatusResponse{
				Found:    true,
				Status:   "running",
				Progress: 10.0,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockClient, probePath, []string{mediaDir}, []string{mediaDir}, false)

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
	if atomic.LoadInt32(&mockClient.submitCalls) != 1 {
		t.Errorf("expected 1 submit call, got %d", atomic.LoadInt32(&mockClient.submitCalls))
	}
}

func TestTranscode_TdarrDisabled(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "NoTdarr.mkv")
	_ = os.WriteFile(origFile, []byte("content"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)

	// nil Tdarr client
	engine, _ := setupTranscodeEngine(t, nil, probePath, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": origFile,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("expected failed status when Tdarr is disabled, got %s", res.Status)
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
	mockClient := &mockTdarrClient{
		jobStatusFunc: func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
			return &tdarr.JobStatusResponse{
				Found:      true,
				Status:     "completed",
				OutputPath: outputFile,
			}, nil
		},
	}

	// replace_original is omitted (defaults to false)
	engine, _ := setupTranscodeEngine(t, mockClient, probePath, []string{mediaDir}, []string{mediaDir}, false)
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

	outputFile := filepath.Join(mediaDir, "TranscodedGate.mkv")
	_ = os.WriteFile(outputFile, []byte("transcoded"), 0644)

	probePath := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
	mockClient := &mockTdarrClient{
		jobStatusFunc: func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
			return &tdarr.JobStatusResponse{
				Found:      true,
				Status:     "completed",
				OutputPath: outputFile,
			}, nil
		},
	}

	// allowDestructive is FALSE, but replace_original is requested TRUE
	engine, _ := setupTranscodeEngine(t, mockClient, probePath, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":             origFile,
		"replace_original": true,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected waiting_decision when allow_destructive is false, got %s", res.Status)
	}
	if !strings.Contains(res.WaitingReason, "allow_destructive is disabled") {
		t.Errorf("expected allow_destructive warning, got: %s", res.WaitingReason)
	}

	// Original still intact!
	c, _ := os.ReadFile(origFile)
	if string(c) != "original" {
		t.Error("original was modified despite safety gate")
	}
}
