package action

import (
	"context"
	"encoding/json"
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

// TestTranscode_ResumePersistedResolutionMismatch verifies that stream attributes
// (Width, Height, BitDepth) survive the SQLite JSON persistence round-trip during
// waiting_external, and that a resumed action correctly detects a candidate resolution
// mismatch during validate_result and fails automated validation (transitions to waiting_decision).
func TestTranscode_ResumePersistedResolutionMismatch(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "Episode1080p.mkv")
	candidateFile := filepath.Join(mediaDir, "EpisodeCandidate720p.mkv")

	origContent := "original-1080p-10bit-source-bytes"
	candContent := "candidate-720p-10bit-transcoded-bytes"
	if err := os.WriteFile(origFile, []byte(origContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidateFile, []byte(candContent), 0644); err != nil {
		t.Fatal(err)
	}

	probeDir := t.TempDir()
	probePath := filepath.Join(probeDir, "ffprobe")

	// Fake ffprobe returns:
	// - 1080p 10-bit for origFile
	// - 720p 10-bit for candidateFile (different resolution from original)
	script := fmt.Sprintf(`#!/bin/sh
if echo "$*" | grep -q "EpisodeCandidate720p"; then
cat << 'JSON'
{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "profile": "Main 10", "pix_fmt": "p010le", "width": 1280, "height": 720, "bits_per_raw_sample": "10"},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2, "tags": {"language": "jpn"}},
    {"index": 2, "codec_type": "subtitle", "codec_name": "subrip", "tags": {"language": "eng"}}
  ],
  "format": {"format_name": "matroska", "duration": "1420.0", "size": "500000000"},
  "chapters": []
}
JSON
else
cat << 'JSON'
{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "profile": "Main 10", "pix_fmt": "yuv420p10le", "width": 1920, "height": 1080, "bits_per_raw_sample": "10"},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2, "tags": {"language": "jpn"}},
    {"index": 2, "codec_type": "subtitle", "codec_name": "subrip", "tags": {"language": "eng"}}
  ],
  "format": {"format_name": "matroska", "duration": "1420.0", "size": "1000000000"},
  "chapters": []
}
JSON
fi
`)
	if err := os.WriteFile(probePath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	var statusCalls int32
	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			call := atomic.AddInt32(&statusCalls, 1)
			if call == 1 {
				// Transcode is still running during preflight / initial run
				return transcode.JobStatus{
					ID:       jobID,
					Status:   transcode.StatusRunning,
					Progress: 50.0,
				}, nil
			}
			// Transcode has completed when resumed
			return transcode.JobStatus{
				ID:            jobID,
				Status:        transcode.StatusCompleted,
				CandidatePath: candidateFile,
			}, nil
		},
	}

	engine, st := setupTranscodeEngine(t, mockExecutor, probePath, []string{mediaDir}, []string{mediaDir}, false)
	ctx := context.Background()

	// 1. Initial run -> executes preflight, submits transcode, pauses in waiting_external
	res1, err := engine.Run(ctx, "transcode_media", map[string]any{
		"path":             origFile,
		"replace_original": false,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res1.Status != StatusWaitingExternal {
		t.Fatalf("expected waiting_external status, got %s", res1.Status)
	}

	// 2. Inspect persisted state directly from SQLite store:
	// Verify that DetailedStream attributes (Width, Height, BitDepth) survived the JSON round trip
	inst, err := st.GetActionInstance(res1.ID)
	if err != nil {
		t.Fatalf("failed to retrieve action instance: %v", err)
	}
	if inst.Status != store.ActionStatusWaitingExternal {
		t.Fatalf("persisted instance status = %s, want %s", inst.Status, store.ActionStatusWaitingExternal)
	}

	var persistedState map[string]any
	if err := json.Unmarshal([]byte(inst.StateJSON), &persistedState); err != nil {
		t.Fatalf("failed to unmarshal persisted StateJSON: %v", err)
	}
	origMap, ok := persistedState["original"].(map[string]any)
	if !ok {
		t.Fatalf("persisted state['original'] is not a map: %T", persistedState["original"])
	}
	persistedVideo := getStreamsList(origMap, "video")
	if len(persistedVideo) != 1 {
		t.Fatalf("expected 1 persisted video stream, got %d", len(persistedVideo))
	}
	if persistedVideo[0].Width != 1920 {
		t.Errorf("persisted Width = %d, want 1920", persistedVideo[0].Width)
	}
	if persistedVideo[0].Height != 1080 {
		t.Errorf("persisted Height = %d, want 1080", persistedVideo[0].Height)
	}
	if persistedVideo[0].BitDepth != 10 {
		t.Errorf("persisted BitDepth = %d, want 10", persistedVideo[0].BitDepth)
	}

	// 3. Simulate process reload / restart by creating a new Engine attached to the same store
	readRoots := []string{mediaDir}
	writeRoots := []string{mediaDir}
	resolver, err := fsop.NewResolver(readRoots, writeRoots)
	if err != nil {
		t.Fatalf("creating resolver: %v", err)
	}
	reloadedCfg := &config.Config{
		AllowDestructive: false,
		Media: config.MediaConfig{
			AllowedReadRoots:  readRoots,
			AllowedWriteRoots: writeRoots,
			FfprobePath:       probePath,
		},
		Transcode: config.TranscodeConfig{
			Enabled:  true,
			Executor: "ssh",
		},
	}
	reloadedEngine := NewEngine(EngineDeps{
		Store:     st,
		Config:    reloadedCfg,
		Fs:        resolver,
		Ffprobe:   probePath,
		Transcode: mockExecutor,
		StartTime: time.Now(),
	})

	// 4. Resume the action: executor reports transcode completed, advancing to validate_result
	res2, err := reloadedEngine.Resume(ctx, res1.ID, "", nil)
	if err != nil {
		t.Fatalf("resume error: %v", err)
	}

	// The resumed action MUST fail validate_result due to resolution mismatch (1920x1080 vs 1280x720).
	// If Width/Height had been lost across JSON persistence (as in older versions),
	// resolution mismatch check would have been skipped and the action would have succeeded.
	if res2.Status == StatusCompleted {
		t.Fatalf("resumed action completed successfully despite resolution mismatch! Width/Height check was bypassed")
	}
	if res2.Status != StatusWaitingDecision {
		t.Fatalf("expected status %s on resolution mismatch, got %s", StatusWaitingDecision, res2.Status)
	}
	if !strings.Contains(res2.WaitingReason, "Video resolution mismatch") ||
		!strings.Contains(res2.WaitingReason, "1920x1080") ||
		!strings.Contains(res2.WaitingReason, "1280x720") {
		t.Fatalf("expected resolution mismatch warning in waiting reason, got: %s", res2.WaitingReason)
	}

	// 5. Rejecting the candidate completes the failure path, leaving original pristine
	res3, err := reloadedEngine.Resume(ctx, res1.ID, "reject", nil)
	if err != nil {
		t.Fatalf("reject resume error: %v", err)
	}
	if res3.Status != StatusFailed {
		t.Fatalf("expected status failed after rejection, got %s", res3.Status)
	}
	if !strings.Contains(res3.Error, "rejected by user decision") {
		t.Fatalf("expected rejection error, got: %s", res3.Error)
	}

	origBytes, err := os.ReadFile(origFile)
	if err != nil {
		t.Fatalf("reading original: %v", err)
	}
	if string(origBytes) != origContent {
		t.Fatalf("original file was modified: got %q, want %q", string(origBytes), origContent)
	}
}
