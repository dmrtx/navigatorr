package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

func TestDetectSpatialAQWarning_Helper(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantMatched bool
		wantWarning string
	}{
		{
			name:        "known exact warning",
			input:       "[hevc_videotoolbox @ 0x7fa281008000] This device does not support the spatialaq option. Value ignored.",
			wantMatched: true,
			wantWarning: "This device does not support the spatialaq option. Value ignored.",
		},
		{
			name:        "known exact warning without logger prefix",
			input:       "This device does not support the spatialaq option. Value ignored.",
			wantMatched: true,
			wantWarning: "This device does not support the spatialaq option. Value ignored.",
		},
		{
			name:        "case and spacing variant 1 - lower case with space",
			input:       "[hevc_videotoolbox @ 0x1] this device does not support the Spatial AQ option; value ignored",
			wantMatched: true,
			wantWarning: "this device does not support the Spatial AQ option; value ignored",
		},
		{
			name:        "case and spacing variant 2 - underscore and punctuation",
			input:       "WARNING: spatial_aq is not supported on this hardware, ignored.",
			wantMatched: true,
			wantWarning: "WARNING: spatial_aq is not supported on this hardware, ignored.",
		},
		{
			name:        "case and spacing variant 3 - hyphenated",
			input:       "Spatial-AQ unsupported on this device",
			wantMatched: true,
			wantWarning: "Spatial-AQ unsupported on this device",
		},
		{
			name:        "case and spacing variant 4 - spatialaq not supported",
			input:       "Option spatialaq not supported",
			wantMatched: true,
			wantWarning: "Option spatialaq not supported",
		},
		{
			name:        "case and spacing variant 5 - spatial_aq ignored",
			input:       "spatial_aq: Value ignored",
			wantMatched: true,
			wantWarning: "spatial_aq: Value ignored",
		},
		{
			name:        "unrelated unsupported warning (false)",
			input:       "[hevc_videotoolbox @ 0x123] profile main10 is unsupported",
			wantMatched: false,
		},
		{
			name:        "unrelated unsupported warning 2 (false)",
			input:       "[hevc_videotoolbox @ 0x123] unsupported color range",
			wantMatched: false,
		},
		{
			name:        "unrelated ignored warning (false)",
			input:       "[mp4 @ 0x123] track 1: frame rate ignored",
			wantMatched: false,
		},
		{
			name:        "unrelated ignored warning 2 (false)",
			input:       "[hevc_videotoolbox @ 0x123] unknown metadata tag ignored",
			wantMatched: false,
		},
		{
			name:        "ordinary log with spatial_aq (false)",
			input:       "hevc_videotoolbox AVOptions: -spatial_aq <boolean> spatial adaptive quantization (default false)",
			wantMatched: false,
		},
		{
			name:        "ordinary log with spatial_aq enabled (false)",
			input:       "[hevc_videotoolbox @ 0x123] spatial_aq enabled: 1",
			wantMatched: false,
		},
		{
			name:        "ordinary log stats line (false)",
			input:       "frame=  100 fps= 50 q=28.0 size=     256kB time=00:00:04.00 bitrate= 524.3kbits/s speed=4.0x",
			wantMatched: false,
		},
		{
			name: "multi-line with unrelated unsupported and separate spatial_aq (false)",
			input: strings.Join([]string{
				"[hevc_videotoolbox @ 0x123] profile main10 is unsupported",
				"[mp4 @ 0x123] track 1: frame rate ignored",
				"hevc_videotoolbox AVOptions: -spatial_aq <boolean> spatial adaptive quantization (default false)",
			}, "\n"),
			wantMatched: false,
		},
		{
			name: "multi-line log containing exact warning amongst normal lines (true)",
			input: strings.Join([]string{
				"ffmpeg version 6.1 Copyright (c) 2000-2023 the FFmpeg developers",
				"Stream mapping:",
				"  Stream #0:0 -> #0:0 (h264 (native) -> hevc (hevc_videotoolbox))",
				"[hevc_videotoolbox @ 0x7fa281008000] This device does not support the spatialaq option. Value ignored.",
				"Output #0, matroska, to '/path/to/candidate.mkv':",
				"frame=  100 fps= 50 q=28.0 size= 256kB speed=4.0x",
			}, "\n"),
			wantMatched: true,
			wantWarning: "This device does not support the spatialaq option. Value ignored.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			matched, warning := DetectSpatialAQWarning(tc.input)
			if matched != tc.wantMatched {
				t.Fatalf("DetectSpatialAQWarning(%q) matched=%v, want=%v", tc.input, matched, tc.wantMatched)
			}
			if tc.wantMatched && warning != tc.wantWarning {
				t.Fatalf("DetectSpatialAQWarning(%q) warning=%q, want=%q", tc.input, warning, tc.wantWarning)
			}
			hasWarning := HasSpatialAQUnsupportedWarning(tc.input)
			if hasWarning != tc.wantMatched {
				t.Fatalf("HasSpatialAQUnsupportedWarning(%q)=%v, want=%v", tc.input, hasWarning, tc.wantMatched)
			}
		})
	}
}

func createMockFFmpegScript(t *testing.T, dir, scriptName string, transcodeScriptBody string) string {
	t.Helper()
	path := filepath.Join(dir, scriptName)
	content := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
    if [ "$arg" = "encoder=hevc_videotoolbox" ]; then
        cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
Supported pixel formats: yuv420p p010le
hevc_videotoolbox AVOptions:
  -profile <int>
     main 1
     main10 2
  -prio_speed <boolean>
  -spatial_aq <boolean>
  -realtime <boolean>
EOF
        exit 0
    fi
done

%s
`, transcodeScriptBody)

	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("failed to write mock script %s: %v", path, err)
	}
	return path
}

func TestRunFFmpeg_Exit0_AQIgnored_ReturnsEncoderCapabilityUnsupported(t *testing.T) {
	tempDir := t.TempDir()
	sourcePath := filepath.Join(tempDir, "source.mkv")
	candPath := filepath.Join(tempDir, "cand.mkv")
	progressPath := filepath.Join(tempDir, "progress.txt")
	logPath := filepath.Join(tempDir, "ffmpeg.log")

	_ = os.WriteFile(sourcePath, []byte("fake source media"), 0644)

	mockScript := createMockFFmpegScript(t, tempDir, "ffmpeg_aq_ignored.sh", `
echo "[hevc_videotoolbox @ 0x7fa281008000] This device does not support the spatialaq option. Value ignored." >&2
exit 0
`)

	t.Run("spatial_aq=true fails closed with EncoderCapabilityUnsupported on exit 0", func(t *testing.T) {
		plan := &transcode.Plan{
			VideoCodec:   "hevc_videotoolbox",
			Quality:      65,
			VideoProfile: "main",
			PixelFormat:  "yuv420p",
			SpatialAQ:    boolPtr(true),
		}
		execPlan := &ExecutionPlan{Plan: plan}
		job := &JobRecord{
			ID:        "job-aq-true",
			Source:    sourcePath,
			Candidate: candPath,
			Plan:      plan,
		}

		err := RunFFmpeg(context.Background(), mockScript, execPlan, job, progressPath, logPath)
		if err == nil {
			t.Fatal("expected RunFFmpeg to fail when spatial_aq was ignored, got nil")
		}

		class := resilience.Classify(err.Error())
		if class != resilience.EncoderCapabilityUnsupported {
			t.Fatalf("expected failure class %s, got %s (err: %v)", resilience.EncoderCapabilityUnsupported, class, err)
		}
		if !strings.Contains(err.Error(), "encoder_capability_unsupported") {
			t.Fatalf("expected error message to contain 'encoder_capability_unsupported', got: %v", err)
		}
		if !strings.Contains(err.Error(), "This device does not support the spatialaq option. Value ignored.") {
			t.Fatalf("expected error message to contain warning, got: %v", err)
		}
	})

	t.Run("spatial_aq=false fails closed with EncoderCapabilityUnsupported on exit 0", func(t *testing.T) {
		plan := &transcode.Plan{
			VideoCodec:   "hevc_videotoolbox",
			Quality:      65,
			VideoProfile: "main",
			PixelFormat:  "yuv420p",
			SpatialAQ:    boolPtr(false),
		}
		execPlan := &ExecutionPlan{Plan: plan}
		job := &JobRecord{
			ID:        "job-aq-false",
			Source:    sourcePath,
			Candidate: candPath,
			Plan:      plan,
		}

		err := RunFFmpeg(context.Background(), mockScript, execPlan, job, progressPath, logPath)
		if err == nil {
			t.Fatal("expected RunFFmpeg to fail when spatial_aq=false was ignored, got nil")
		}

		class := resilience.Classify(err.Error())
		if class != resilience.EncoderCapabilityUnsupported {
			t.Fatalf("expected failure class %s, got %s (err: %v)", resilience.EncoderCapabilityUnsupported, class, err)
		}
	})

	t.Run("spatial_aq=nil succeeds when option was not requested", func(t *testing.T) {
		plan := &transcode.Plan{
			VideoCodec:   "hevc_videotoolbox",
			Quality:      65,
			VideoProfile: "main",
			PixelFormat:  "yuv420p",
			SpatialAQ:    nil,
		}
		execPlan := &ExecutionPlan{Plan: plan}
		job := &JobRecord{
			ID:        "job-aq-nil",
			Source:    sourcePath,
			Candidate: candPath,
			Plan:      plan,
		}

		err := RunFFmpeg(context.Background(), mockScript, execPlan, job, progressPath, logPath)
		if err != nil {
			t.Fatalf("expected success when spatial_aq was nil, got err: %v", err)
		}
	})
}

func TestRunFFmpeg_Exit0_AQSupported_Success(t *testing.T) {
	tempDir := t.TempDir()
	sourcePath := filepath.Join(tempDir, "source.mkv")
	candPath := filepath.Join(tempDir, "cand.mkv")
	progressPath := filepath.Join(tempDir, "progress.txt")
	logPath := filepath.Join(tempDir, "ffmpeg.log")

	_ = os.WriteFile(sourcePath, []byte("fake source media"), 0644)

	mockScript := createMockFFmpegScript(t, tempDir, "ffmpeg_normal.sh", `
echo "frame=100 fps=50" >&2
exit 0
`)

	plan := &transcode.Plan{
		VideoCodec:   "hevc_videotoolbox",
		Quality:      65,
		VideoProfile: "main",
		PixelFormat:  "yuv420p",
		SpatialAQ:    boolPtr(true),
	}
	execPlan := &ExecutionPlan{Plan: plan}
	job := &JobRecord{
		ID:        "job-aq-success",
		Source:    sourcePath,
		Candidate: candPath,
		Plan:      plan,
	}

	err := RunFFmpeg(context.Background(), mockScript, execPlan, job, progressPath, logPath)
	if err != nil {
		t.Fatalf("expected RunFFmpeg to succeed when AQ supported, got: %v", err)
	}

	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("failed to read log: %v", readErr)
	}
	if !strings.Contains(string(logData), "frame=100") {
		t.Fatalf("log did not contain expected stderr output: %s", string(logData))
	}
}

func TestRunFFmpeg_NonZeroExit_PreservesProcessError(t *testing.T) {
	tempDir := t.TempDir()
	sourcePath := filepath.Join(tempDir, "source.mkv")
	candPath := filepath.Join(tempDir, "cand.mkv")
	progressPath := filepath.Join(tempDir, "progress.txt")
	logPath := filepath.Join(tempDir, "ffmpeg.log")

	_ = os.WriteFile(sourcePath, []byte("fake source media"), 0644)

	mockScript := createMockFFmpegScript(t, tempDir, "ffmpeg_error.sh", `
echo "[hevc_videotoolbox @ 0x123] Error opening encoder: Invalid argument" >&2
exit 1
`)

	plan := &transcode.Plan{
		VideoCodec:   "hevc_videotoolbox",
		Quality:      65,
		VideoProfile: "main",
		PixelFormat:  "yuv420p",
		SpatialAQ:    boolPtr(true),
	}
	execPlan := &ExecutionPlan{Plan: plan}
	job := &JobRecord{
		ID:        "job-error",
		Source:    sourcePath,
		Candidate: candPath,
		Plan:      plan,
	}

	err := RunFFmpeg(context.Background(), mockScript, execPlan, job, progressPath, logPath)
	if err == nil {
		t.Fatal("expected RunFFmpeg to fail on exit 1")
	}

	if !strings.Contains(err.Error(), "ffmpeg failed") {
		t.Fatalf("expected ffmpeg process error summary, got: %v", err)
	}
	if strings.Contains(err.Error(), "encoder_capability_unsupported") {
		t.Fatalf("should not classify generic nonzero exit as encoder_capability_unsupported: %v", err)
	}
}

func TestRunFFmpeg_ContextCancelled_PreservesCancellation(t *testing.T) {
	tempDir := t.TempDir()
	sourcePath := filepath.Join(tempDir, "source.mkv")
	candPath := filepath.Join(tempDir, "cand.mkv")
	progressPath := filepath.Join(tempDir, "progress.txt")
	logPath := filepath.Join(tempDir, "ffmpeg.log")

	_ = os.WriteFile(sourcePath, []byte("fake source media"), 0644)

	mockScript := createMockFFmpegScript(t, tempDir, "ffmpeg_sleep.sh", `
sleep 10
exit 0
`)

	plan := &transcode.Plan{
		VideoCodec:   "hevc_videotoolbox",
		Quality:      65,
		VideoProfile: "main",
		PixelFormat:  "yuv420p",
		SpatialAQ:    boolPtr(true),
	}
	execPlan := &ExecutionPlan{Plan: plan}
	job := &JobRecord{
		ID:        "job-cancel",
		Source:    sourcePath,
		Candidate: candPath,
		Plan:      plan,
	}

	t.Run("pre-cancelled context returns cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // pre-cancel context

		err := RunFFmpeg(ctx, mockScript, execPlan, job, progressPath, logPath)
		if err == nil {
			t.Fatal("expected RunFFmpeg to fail when context cancelled")
		}
		if strings.Contains(err.Error(), "encoder_capability_unsupported") {
			t.Fatalf("should not classify cancellation as encoder_capability_unsupported: %v", err)
		}
	})

	t.Run("in-flight cancelled context preserves cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		err := RunFFmpeg(ctx, mockScript, execPlan, job, progressPath, logPath)
		if err == nil {
			t.Fatal("expected RunFFmpeg to fail when context cancelled in-flight")
		}
		if strings.Contains(err.Error(), "encoder_capability_unsupported") {
			t.Fatalf("should not classify in-flight cancellation as encoder_capability_unsupported: %v", err)
		}
	})
}

// TestSummarizeFFmpegError_BoundedTailOnly proves the summary only ever
// considers the bounded tail: an error far outside the tail bound is never
// reported even though it is present earlier in the file.
func TestSummarizeFFmpegError_BoundedTailOnly(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "ffmpeg.log")

	head := strings.Repeat("noise line without keywords\n", 2000)
	head += "[hevc_videotoolbox @ 0x1] Error: HEAD_SENTINEL_MUST_NOT_APPEAR\n"
	// More than ffmpegErrorSummaryTailBytes of filler pushes the head sentinel
	// well outside the bounded tail window.
	filler := strings.Repeat("filler line\n", int(ffmpegErrorSummaryTailBytes)/12+200)
	tail := "[hevc_videotoolbox @ 0x2] Error: TAIL_SENTINEL_MUST_APPEAR"
	content := head + filler + tail
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	summary := SummarizeFFmpegError(logPath, errors.New("exit status 1"))
	if !strings.Contains(summary, "TAIL_SENTINEL_MUST_APPEAR") {
		t.Fatalf("summary must include the tail error, got %q", summary)
	}
	if strings.Contains(summary, "HEAD_SENTINEL_MUST_NOT_APPEAR") {
		t.Fatalf("summary must not read beyond the bounded tail, got %q", summary)
	}
}

// TestSummarizeFFmpegError_LargeLogStaysBounded exercises a log much larger
// than the summary tail and asserts the summary remains small and tail-derived.
func TestSummarizeFFmpegError_LargeLogStaysBounded(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "ffmpeg.log")

	total := int(ffmpegErrorSummaryTailBytes) * 64
	buf := make([]byte, total)
	for i := range buf {
		buf[i] = 'x'
	}
	tail := "\n[hevc_videotoolbox @ 0x9] Error: LARGE_LOG_TAIL_ERROR"
	copy(buf[total-len(tail):], tail)
	if err := os.WriteFile(logPath, buf, 0644); err != nil {
		t.Fatal(err)
	}

	summary := SummarizeFFmpegError(logPath, errors.New("exit status 1"))
	if !strings.Contains(summary, "LARGE_LOG_TAIL_ERROR") {
		t.Fatalf("summary must include the tail error, got %q", summary)
	}
	if len(summary) > 600 {
		t.Fatalf("summary must stay bounded, len=%d: %q", len(summary), summary)
	}
}

func TestSummarizeFFmpegError_MissingLogFallsBack(t *testing.T) {
	summary := SummarizeFFmpegError(filepath.Join(t.TempDir(), "absent.log"), errors.New("exit status 1"))
	if !strings.Contains(summary, "ffmpeg execution failed") {
		t.Fatalf("missing log must fall back to generic summary, got %q", summary)
	}
}

// TestRunFFmpeg_AQDetectionUsesCurrentRunStderrOnly ensures spatial AQ
// detection never scrapes the log file. The warning here is written to stdout
// (which lands in ffmpeg.log) and never to stderr, so a log-reading detector
// would false-positive; stderr-only detection must succeed.
func TestRunFFmpeg_AQDetectionUsesCurrentRunStderrOnly(t *testing.T) {
	tempDir := t.TempDir()
	sourcePath := filepath.Join(tempDir, "source.mkv")
	candPath := filepath.Join(tempDir, "cand.mkv")
	progressPath := filepath.Join(tempDir, "progress.txt")
	logPath := filepath.Join(tempDir, "ffmpeg.log")

	_ = os.WriteFile(sourcePath, []byte("fake source media"), 0644)

	mockScript := createMockFFmpegScript(t, tempDir, "ffmpeg_aq_stdout.sh", `
echo "[hevc_videotoolbox @ 0x7fa281008000] This device does not support the spatialaq option. Value ignored."
exit 0
`)

	plan := &transcode.Plan{
		VideoCodec:   "hevc_videotoolbox",
		Quality:      65,
		VideoProfile: "main",
		PixelFormat:  "yuv420p",
		SpatialAQ:    boolPtr(true),
	}
	execPlan := &ExecutionPlan{Plan: plan}
	job := &JobRecord{
		ID:        "job-aq-stdout",
		Source:    sourcePath,
		Candidate: candPath,
		Plan:      plan,
	}

	if err := RunFFmpeg(context.Background(), mockScript, execPlan, job, progressPath, logPath); err != nil {
		t.Fatalf("AQ detection must use current-run stderr only, got: %v", err)
	}

	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("reading log: %v", readErr)
	}
	if !strings.Contains(string(data), "spatialaq") {
		t.Fatalf("test setup: warning should be present in the log via stdout, got %q", string(data))
	}
}

// TestRunFFmpeg_LogTruncatedPerRun locks in per-run truncate semantics: a
// previous run's bytes never persist into the current run's log.
func TestRunFFmpeg_LogTruncatedPerRun(t *testing.T) {
	tempDir := t.TempDir()
	sourcePath := filepath.Join(tempDir, "source.mkv")
	candPath := filepath.Join(tempDir, "cand.mkv")
	progressPath := filepath.Join(tempDir, "progress.txt")
	logPath := filepath.Join(tempDir, "ffmpeg.log")

	_ = os.WriteFile(sourcePath, []byte("fake source media"), 0644)
	if err := os.WriteFile(logPath, []byte("HISTORICAL_STALE_ERROR from a previous run\n"), 0644); err != nil {
		t.Fatal(err)
	}

	mockScript := createMockFFmpegScript(t, tempDir, "ffmpeg_current_run.sh", `
echo "CURRENT_RUN_OUTPUT" >&2
exit 0
`)

	plan := &transcode.Plan{
		VideoCodec:   "hevc_videotoolbox",
		Quality:      65,
		VideoProfile: "main",
		PixelFormat:  "yuv420p",
		SpatialAQ:    boolPtr(true),
	}
	execPlan := &ExecutionPlan{Plan: plan}
	job := &JobRecord{
		ID:        "job-truncate",
		Source:    sourcePath,
		Candidate: candPath,
		Plan:      plan,
	}

	if err := RunFFmpeg(context.Background(), mockScript, execPlan, job, progressPath, logPath); err != nil {
		t.Fatalf("RunFFmpeg: %v", err)
	}

	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("reading log: %v", readErr)
	}
	if !strings.Contains(string(data), "CURRENT_RUN_OUTPUT") {
		t.Fatalf("current run output missing from log: %q", string(data))
	}
	if strings.Contains(string(data), "HISTORICAL_STALE_ERROR") {
		t.Fatalf("per-run truncate must drop prior content, got: %q", string(data))
	}
}
