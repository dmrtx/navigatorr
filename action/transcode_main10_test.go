package action

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestTranscode_Main10RejectsEightBitCandidate(t *testing.T) {
	mediaDir := t.TempDir()
	origFile := filepath.Join(mediaDir, "Kaguya-S01E01.mkv")
	candidateFile := filepath.Join(mediaDir, "Kaguya-S01E01.navigatorr-candidate.mkv")
	originalContent := "original-eight-bit-source"
	if err := os.WriteFile(origFile, []byte(originalContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidateFile, []byte("candidate-eight-bit-hevc"), 0644); err != nil {
		t.Fatal(err)
	}

	probeDir := t.TempDir()
	probePath := filepath.Join(probeDir, "ffprobe")
	script := fmt.Sprintf(`#!/bin/sh
if echo "$*" | grep -q %q; then
cat << 'JSON'
{
  "streams": [
    {"index":0,"codec_type":"video","codec_name":"h264","profile":"High","pix_fmt":"yuv420p","width":1920,"height":1080,"bits_per_raw_sample":"8"}
  ],
  "format":{"format_name":"matroska","duration":"1530.0","size":"1559326340"},
  "chapters":[]
}
JSON
else
cat << 'JSON'
{
  "streams": [
    {"index":0,"codec_type":"video","codec_name":"hevc","profile":"Main","pix_fmt":"yuv420p","width":1920,"height":1080,"bits_per_raw_sample":"8"}
  ],
  "format":{"format_name":"matroska","duration":"1530.0","size":"511581508"},
  "chapters":[]
}
JSON
fi
`, filepath.Base(origFile))
	if err := os.WriteFile(probePath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	mockExecutor := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:            jobID,
				Status:        transcode.StatusCompleted,
				CandidatePath: candidateFile,
			}, nil
		},
	}

	engine, _ := setupTranscodeEngine(t, mockExecutor, probePath, []string{mediaDir}, []string{mediaDir}, false)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":             origFile,
		"profile":          "anime-hevc-main10",
		"replace_original": false,
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("expected Main10 validation failure for 8-bit candidate, got %s", res.Status)
	}
	if !strings.Contains(strings.ToLower(res.Error), "bit depth mismatch") || !strings.Contains(res.Error, "10-bit") || !strings.Contains(res.Error, "8-bit") {
		t.Fatalf("unexpected validation error: %s", res.Error)
	}

	got, err := os.ReadFile(origFile)
	if err != nil {
		t.Fatalf("reading original: %v", err)
	}
	if string(got) != originalContent {
		t.Fatalf("original was modified: got %q want %q", string(got), originalContent)
	}
}
