package transcodeworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

func TestBuildReferenceExtractionArgs_ArgvExactness(t *testing.T) {
	sourcePath := "/media/movies/movie.mkv"
	refPath := "/state/bench-123/samples/ref_sample_0.mkv"
	startSec := 12.5
	durationSec := 20.0
	videoIndex := 0

	args := BuildReferenceExtractionArgs(sourcePath, refPath, videoIndex, startSec, durationSec)

	expected := []string{
		"-y",
		"-nostats",
		"-accurate_seek",
		"-ss", "12.500000",
		"-i", sourcePath,
		"-t", "20.000000",
		"-avoid_negative_ts", "make_zero",
		"-map", "0:0",
		"-c:v", "ffv1",
		"-an",
		"-sn",
		"-dn",
		refPath,
	}

	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}

	for i, arg := range expected {
		if args[i] != arg {
			t.Errorf("arg %d mismatch: expected %q, got %q", i, arg, args[i])
		}
	}
}

func TestBuildCandidateEncodeArgs_ArgvExactness(t *testing.T) {
	refPath := "/state/bench-123/samples/ref_sample_0.mkv"
	candPath := "/state/bench-123/samples/cand_q65_sample_0.mkv"

	t.Run("8-bit main profile", func(t *testing.T) {
		plan := &transcode.Plan{
			VideoCodec:       "hevc_videotoolbox",
			Quality:          65,
			VideoProfile:     "main",
			PixelFormat:      "yuv420p",
			ExpectedBitDepth: 8,
		}

		args, err := BuildCandidateEncodeArgs(refPath, candPath, plan)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		expected := []string{
			"-y",
			"-nostats",
			"-i", refPath,
			"-map", "0:v:0",
			"-c:v", "hevc_videotoolbox",
			"-q:v", "65",
			"-profile:v", "main",
			"-pix_fmt", "yuv420p",
			"-an",
			"-sn",
			"-dn",
			candPath,
		}

		if len(args) != len(expected) {
			t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
		}
		for i, arg := range expected {
			if args[i] != arg {
				t.Errorf("arg %d mismatch: expected %q, got %q", i, arg, args[i])
			}
		}
	})

	t.Run("10-bit main10 profile with optional knobs", func(t *testing.T) {
		prioSpeed := true
		spatialAQ := true
		realtime := false

		plan := &transcode.Plan{
			VideoCodec:       "hevc_videotoolbox",
			Quality:          75,
			VideoProfile:     "main10",
			PixelFormat:      "p010le",
			ExpectedBitDepth: 10,
			PrioritizeSpeed:  &prioSpeed,
			SpatialAQ:        &spatialAQ,
			Realtime:         &realtime,
		}

		args, err := BuildCandidateEncodeArgs(refPath, candPath, plan)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		expected := []string{
			"-y",
			"-nostats",
			"-i", refPath,
			"-map", "0:v:0",
			"-c:v", "hevc_videotoolbox",
			"-q:v", "75",
			"-profile:v", "main10",
			"-pix_fmt", "p010le",
			"-prio_speed", "1",
			"-spatial_aq", "1",
			"-realtime", "0",
			"-an",
			"-sn",
			"-dn",
			candPath,
		}

		if len(args) != len(expected) {
			t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
		}
		for i, arg := range expected {
			if args[i] != arg {
				t.Errorf("arg %d mismatch: expected %q, got %q", i, arg, args[i])
			}
		}
	})
}

func TestResolveCandidateBitDepth_Rules(t *testing.T) {
	// 8-bit source cases
	t.Run("8-bit source defaults", func(t *testing.T) {
		c := &transcode.BenchmarkCandidate{ID: "c1", Quality: 60}
		prof, pix, bd, err := resolveCandidateBitDepth(c, 8)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if prof != "main" || pix != "yuv420p" || bd != 8 {
			t.Errorf("expected main/yuv420p/8, got %s/%s/%d", prof, pix, bd)
		}
	})

	t.Run("8-bit source explicit main passes", func(t *testing.T) {
		c := &transcode.BenchmarkCandidate{ID: "c1", Quality: 60, VideoProfile: "main", PixelFormat: "yuv420p"}
		prof, pix, bd, err := resolveCandidateBitDepth(c, 8)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if prof != "main" || pix != "yuv420p" || bd != 8 {
			t.Errorf("expected main/yuv420p/8, got %s/%s/%d", prof, pix, bd)
		}
	})

	t.Run("8-bit source rejects main10", func(t *testing.T) {
		c := &transcode.BenchmarkCandidate{ID: "c1", Quality: 60, VideoProfile: "main10"}
		_, _, _, err := resolveCandidateBitDepth(c, 8)
		if err == nil {
			t.Fatalf("expected error rejecting main10 for 8-bit source, got nil")
		}
		if !strings.Contains(err.Error(), "cannot convert 8-bit to 10-bit") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("8-bit source rejects p010le", func(t *testing.T) {
		c := &transcode.BenchmarkCandidate{ID: "c1", Quality: 60, PixelFormat: "p010le"}
		_, _, _, err := resolveCandidateBitDepth(c, 8)
		if err == nil {
			t.Fatalf("expected error rejecting p010le for 8-bit source, got nil")
		}
	})

	// 10-bit source cases
	t.Run("10-bit source defaults", func(t *testing.T) {
		c := &transcode.BenchmarkCandidate{ID: "c1", Quality: 70}
		prof, pix, bd, err := resolveCandidateBitDepth(c, 10)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if prof != "main10" || pix != "p010le" || bd != 10 {
			t.Errorf("expected main10/p010le/10, got %s/%s/%d", prof, pix, bd)
		}
	})

	t.Run("10-bit source explicit main10 passes", func(t *testing.T) {
		c := &transcode.BenchmarkCandidate{ID: "c1", Quality: 70, VideoProfile: "main10", PixelFormat: "p010le"}
		prof, pix, bd, err := resolveCandidateBitDepth(c, 10)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if prof != "main10" || pix != "p010le" || bd != 10 {
			t.Errorf("expected main10/p010le/10, got %s/%s/%d", prof, pix, bd)
		}
	})

	t.Run("10-bit source rejects main (downgrade)", func(t *testing.T) {
		c := &transcode.BenchmarkCandidate{ID: "c1", Quality: 70, VideoProfile: "main"}
		_, _, _, err := resolveCandidateBitDepth(c, 10)
		if err == nil {
			t.Fatalf("expected error rejecting 8-bit downgrade for 10-bit source, got nil")
		}
		if !strings.Contains(err.Error(), "cannot downgrade 10-bit source to 8-bit") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("10-bit source rejects yuv420p (downgrade)", func(t *testing.T) {
		c := &transcode.BenchmarkCandidate{ID: "c1", Quality: 70, PixelFormat: "yuv420p"}
		_, _, _, err := resolveCandidateBitDepth(c, 10)
		if err == nil {
			t.Fatalf("expected error rejecting yuv420p for 10-bit source, got nil")
		}
	})

	t.Run("unsupported source bit depth rejects", func(t *testing.T) {
		c := &transcode.BenchmarkCandidate{ID: "c1", Quality: 70}
		_, _, _, err := resolveCandidateBitDepth(c, 12)
		if err == nil {
			t.Fatalf("expected error for 12-bit source, got nil")
		}
	})
}

func TestVerifyChildPath_PathTraversalAndSafety(t *testing.T) {
	dir := t.TempDir()
	samplesDir := filepath.Join(dir, "samples")
	if err := os.MkdirAll(samplesDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	validFile := filepath.Join(samplesDir, "ref_sample_0.mkv")
	if err := verifyChildPath(samplesDir, validFile); err != nil {
		t.Errorf("expected valid file to pass, got: %v", err)
	}

	traversalFile := filepath.Join(samplesDir, "..", "benchmark.json")
	if err := verifyChildPath(samplesDir, traversalFile); err == nil {
		t.Errorf("expected traversal file %s to fail, got nil", traversalFile)
	}

	deepTraversalFile := filepath.Join(samplesDir, "..", "..", "etc", "passwd")
	if err := verifyChildPath(samplesDir, deepTraversalFile); err == nil {
		t.Errorf("expected deep traversal file to fail, got nil")
	}

	sameDir := samplesDir
	if err := verifyChildPath(samplesDir, sameDir); err == nil {
		t.Errorf("expected same directory to fail, got nil")
	}
}

func TestSanitizeCandidateID(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"quality_65", "quality_65"},
		{"cand-1", "cand-1"},
		{"cand.test", "candtest"},
		{"../escape", "escape"},
		{"../../passwd", "passwd"},
		{"", "candidate"},
		{"a_very_long_candidate_id_that_exceeds_thirty_two_characters_total", "a_very_long_candidate_id_that_ex"},
	}

	for _, tc := range cases {
		got := sanitizeCandidateID(tc.input)
		if got != tc.expected {
			t.Errorf("sanitizeCandidateID(%q) = %q, expected %q", tc.input, got, tc.expected)
		}
	}
}

func setupMockTools(t *testing.T, dir string, probeJSON string, ffmpegFailPattern string) (string, string, string) {
	t.Helper()

	mockProbe := filepath.Join(dir, "mock_ffprobe.sh")
	probeScript := fmt.Sprintf(`#!/bin/sh
cat << 'EOF'
%s
EOF
`, probeJSON)
	if err := os.WriteFile(mockProbe, []byte(probeScript), 0755); err != nil {
		t.Fatalf("writing mock ffprobe: %v", err)
	}

	logFile := filepath.Join(dir, "ffmpeg_calls.log")
	mockFFmpeg := filepath.Join(dir, "mock_ffmpeg.sh")
	ffmpegScript := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q

case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1 Copyright (c) 2000-2024 the FFmpeg developers"
    exit 0
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -prio_speed        <boolean>    E..V....... Prioritize encoding speed (default false)
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
  -realtime          <boolean>    E..V....... Realtime (default false)
EOF
    exit 0
    ;;
  *"-encoders"*)
    cat << 'EOF'
Encoders:
 V..... hevc_videotoolbox    VideoToolbox H.265
 V..... ffv1                 FFmpeg video codec #1
EOF
    exit 0
    ;;
  *"-filters"*)
    cat << 'EOF'
Filters:
  .. libvmaf           VV->V      Calculate the VMAF between two video streams.
  TS ssim              VV->V      Calculate the SSIM between two video streams.
  .S scale             V->V       Scale the input video size and/or convert the image format.
  .. format            V->V       Convert the input video to one of the specified pixel formats.
  .. null              V->V       Pass the source unchanged to the output.
  .. fps               V->V       Force constant framerate.
EOF
    exit 0
    ;;
esac

# Check for simulated failure pattern
if [ -n %q ]; then
  case "$*" in
    *%s*)
      echo "simulated ffmpeg failure for: $*" >&2
      exit 1
      ;;
  esac
fi

case "$*" in
  *"-filter_complex"*"libvmaf"*)
    for arg in "$@"; do
      case "$arg" in
        *"log_path="*)
          lpath="${arg#*log_path=}"
          lpath="${lpath%%:*}"
          mkdir -p "$(dirname "$lpath")"
          cat << 'VMAF_EOF' > "$lpath"
{
  "version": "2.3.1",
  "pooled_metrics": {
    "vmaf": {
      "mean": 95.500000
    }
  }
}
VMAF_EOF
          ;;
      esac
    done
    exit 0
    ;;
  *"-filter_complex"*"ssim"*)
    for arg in "$@"; do
      case "$arg" in
        *"stats_file="*)
          spath="${arg#*stats_file=}"
          spath="${spath%%:*}"
          mkdir -p "$(dirname "$spath")"
          cat << 'SSIM_EOF' > "$spath"
n:1 Y:0.980000 U:0.985000 V:0.985000 All:0.980000 (16.99)
SSIM_EOF
          ;;
      esac
    done
    echo "[Parsed_ssim_0] SSIM Y:0.980000 U:0.985000 V:0.985000 All:0.980000 (16.99)" >&2
    exit 0
    ;;
esac

# Find output file (last argument)
out=""
for last; do out="$last"; done

case "$out" in
  -*|"")
    # Not an output file (flag or empty)
    ;;
  *)
    # Write fake dummy media data so os.Stat sees non-zero size
    mkdir -p "$(dirname "$out")"
    echo "fake media data payload for $out" > "$out"
    ;;
esac
exit 0
`, logFile, ffmpegFailPattern, ffmpegFailPattern)

	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("writing mock ffmpeg: %v", err)
	}

	return mockFFmpeg, mockProbe, logFile
}

const sdr8BitProbeJSON = `{
  "streams": [
    {
      "index": 0,
      "codec_type": "video",
      "codec_name": "h264",
      "profile": "High",
      "pix_fmt": "yuv420p",
      "r_frame_rate": "24/1",
      "avg_frame_rate": "24/1",
      "width": 1920,
      "height": 1080,
      "bits_per_raw_sample": "8",
      "color_range": "tv",
      "color_space": "bt709",
      "color_primaries": "bt709",
      "color_transfer": "bt709"
    }
  ],
  "format": {
    "format_name": "matroska,webm",
    "duration": "120.000000",
    "size": "50000000"
  },
  "chapters": []
}`

const sdr10BitProbeJSON = `{
  "streams": [
    {
      "index": 0,
      "codec_type": "video",
      "codec_name": "hevc",
      "profile": "Main 10",
      "pix_fmt": "yuv420p10le",
      "r_frame_rate": "24/1",
      "avg_frame_rate": "24/1",
      "width": 1920,
      "height": 1080,
      "bits_per_raw_sample": "10",
      "color_range": "tv",
      "color_space": "bt709",
      "color_primaries": "bt709",
      "color_transfer": "bt709"
    }
  ],
  "format": {
    "format_name": "matroska,webm",
    "duration": "120.000000",
    "size": "50000000"
  },
  "chapters": []
}`

const hdrProbeJSON = `{
  "streams": [
    {
      "index": 0,
      "codec_type": "video",
      "codec_name": "hevc",
      "profile": "Main 10",
      "pix_fmt": "yuv420p10le",
      "r_frame_rate": "24/1",
      "avg_frame_rate": "24/1",
      "width": 3840,
      "height": 2160,
      "bits_per_raw_sample": "10",
      "color_range": "tv",
      "color_space": "bt2020nc",
      "color_primaries": "bt2020",
      "color_transfer": "smpte2084",
      "side_data_list": [
        {
          "side_data_type": "Mastering display metadata"
        }
      ]
    }
  ],
  "format": {
    "format_name": "matroska,webm",
    "duration": "120.000000",
    "size": "100000000"
  },
  "chapters": []
}`

const multiVideoProbeJSON = `{
  "streams": [
    {
      "index": 0,
      "codec_type": "video",
      "codec_name": "h264",
      "profile": "High",
      "pix_fmt": "yuv420p",
      "width": 1920,
      "height": 1080,
      "bits_per_raw_sample": "8"
    },
    {
      "index": 1,
      "codec_type": "video",
      "codec_name": "h264",
      "profile": "High",
      "pix_fmt": "yuv420p",
      "width": 640,
      "height": 480,
      "bits_per_raw_sample": "8"
    }
  ],
  "format": {
    "format_name": "matroska,webm",
    "duration": "120.000000"
  },
  "chapters": []
}`

func TestProductionBenchmarkRunner_8BitSource_ExactArgvAndOrder(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "original_source.mkv")
	sourceContent := []byte("original video content to stay untouched")
	if err := os.WriteFile(sourceFile, sourceContent, 0644); err != nil {
		t.Fatalf("writing source file: %v", err)
	}

	initialHash, err := fileSHA256(sourceFile)
	if err != nil {
		t.Fatalf("computing source hash: %v", err)
	}

	mockFFmpeg, mockProbe, logFile := setupMockTools(t, dir, sdr8BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-test01",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 15.0},
			{Index: 1, StartSeconds: 50.0, DurationSeconds: 15.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_q60", Quality: 60},
			{ID: "cand_q70", Quality: 70},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	ctx := context.Background()

	if err := runner.RunBenchmark(ctx, worker, record); err != nil {
		t.Fatalf("RunBenchmark failed: %v", err)
	}

	// 1. Verify source hash is completely untouched
	postHash, err := fileSHA256(sourceFile)
	if err != nil {
		t.Fatalf("computing post source hash: %v", err)
	}
	if initialHash != postHash {
		t.Fatalf("source file was mutated! Initial: %s, Post: %s", initialHash, postHash)
	}

	// 2. Verify evidence was populated
	if record.Evidence == nil {
		t.Fatalf("expected record.Evidence to be non-nil")
	}
	if record.Evidence.SourceBitDepth != 8 {
		t.Errorf("expected SourceBitDepth=8, got %d", record.Evidence.SourceBitDepth)
	}
	if len(record.Evidence.ReferenceSamples) != 2 {
		t.Errorf("expected 2 reference samples, got %d", len(record.Evidence.ReferenceSamples))
	}
	if len(record.Evidence.CandidateSamples) != 4 { // 2 candidates x 2 samples
		t.Errorf("expected 4 candidate samples, got %d", len(record.Evidence.CandidateSamples))
	}

	// 3. Inspect the argv execution log for exact flags, exact sequential ordering, and no audio/subtitles
	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading ffmpeg calls log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(logBytes)), "\n")

	// Filter out -version, -h, -encoders
	var executionLines []string
	for _, l := range lines {
		if strings.Contains(l, "-c:v ffv1") || strings.Contains(l, "-c:v hevc_videotoolbox") {
			executionLines = append(executionLines, l)
		}
	}

	// Order must be: Ref 0, Ref 1, Cand 0 Sample 0, Cand 0 Sample 1, Cand 1 Sample 0, Cand 1 Sample 1
	if len(executionLines) != 6 {
		t.Fatalf("expected 6 ffmpeg execution lines, got %d:\n%s", len(executionLines), strings.Join(executionLines, "\n"))
	}

	// Line 0: Ref sample 0
	l0 := executionLines[0]
	if !strings.Contains(l0, "-ss 10.000000") || !strings.Contains(l0, "-t 15.000000") || !strings.Contains(l0, "-c:v ffv1") || !strings.Contains(l0, "ref_sample_0.mkv") {
		t.Errorf("unexpected ref 0 command: %s", l0)
	}
	if !strings.Contains(l0, "-an") || !strings.Contains(l0, "-sn") || !strings.Contains(l0, "-dn") {
		t.Errorf("ref 0 command missing -an/-sn/-dn: %s", l0)
	}
	if !strings.Contains(l0, "-avoid_negative_ts make_zero") {
		t.Errorf("ref 0 command missing -avoid_negative_ts make_zero: %s", l0)
	}

	// Line 1: Ref sample 1
	l1 := executionLines[1]
	if !strings.Contains(l1, "-ss 50.000000") || !strings.Contains(l1, "-t 15.000000") || !strings.Contains(l1, "-c:v ffv1") || !strings.Contains(l1, "ref_sample_1.mkv") {
		t.Errorf("unexpected ref 1 command: %s", l1)
	}

	// Line 2: Cand q60 Sample 0
	l2 := executionLines[2]
	if !strings.Contains(l2, "-i") || !strings.Contains(l2, "ref_sample_0.mkv") || !strings.Contains(l2, "-q:v 60") || !strings.Contains(l2, "-profile:v main") || !strings.Contains(l2, "-pix_fmt yuv420p") {
		t.Errorf("unexpected cand q60 sample 0 command: %s", l2)
	}
	if !strings.Contains(l2, "-an") || !strings.Contains(l2, "-sn") || !strings.Contains(l2, "-dn") {
		t.Errorf("cand command missing -an/-sn/-dn: %s", l2)
	}

	// Line 3: Cand q60 Sample 1
	l3 := executionLines[3]
	if !strings.Contains(l3, "ref_sample_1.mkv") || !strings.Contains(l3, "-q:v 60") {
		t.Errorf("unexpected cand q60 sample 1 command: %s", l3)
	}

	// Line 4: Cand q70 Sample 0
	l4 := executionLines[4]
	if !strings.Contains(l4, "ref_sample_0.mkv") || !strings.Contains(l4, "-q:v 70") {
		t.Errorf("unexpected cand q70 sample 0 command: %s", l4)
	}

	// Line 5: Cand q70 Sample 1
	l5 := executionLines[5]
	if !strings.Contains(l5, "ref_sample_1.mkv") || !strings.Contains(l5, "-q:v 70") {
		t.Errorf("unexpected cand q70 sample 1 command: %s", l5)
	}
}

func TestProductionBenchmarkRunner_10BitSource_Success(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "original_source10.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake 10-bit video content"), 0644)

	mockFFmpeg, mockProbe, logFile := setupMockTools(t, dir, sdr10BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-test10",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "ssim",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 20.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_main10", Quality: 75, VideoProfile: "main10", PixelFormat: "p010le"},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	ctx := context.Background()

	if err := runner.RunBenchmark(ctx, worker, record); err != nil {
		t.Fatalf("RunBenchmark failed on 10-bit source: %v", err)
	}

	if record.Evidence == nil {
		t.Fatalf("expected non-nil evidence")
	}
	if record.Evidence.SourceBitDepth != 10 {
		t.Errorf("expected SourceBitDepth=10, got %d", record.Evidence.SourceBitDepth)
	}

	logBytes, _ := os.ReadFile(logFile)
	logContent := string(logBytes)
	if !strings.Contains(logContent, "-profile:v main10") || !strings.Contains(logContent, "-pix_fmt p010le") {
		t.Errorf("expected candidate encode to specify main10 and p010le, got log:\n%s", logContent)
	}
}

func TestProductionBenchmarkRunner_8BitSourceRejectsMain10(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "original_8bit.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake 8-bit video content"), 0644)

	mockFFmpeg, mockProbe, logFile := setupMockTools(t, dir, sdr8BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-test-reject8to10",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_bad", Quality: 75, VideoProfile: "main10", PixelFormat: "p010le"},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	err := runner.RunBenchmark(context.Background(), worker, record)
	if err == nil {
		t.Fatalf("expected error rejecting main10 candidate on 8-bit source, got nil")
	}
	if !strings.Contains(err.Error(), "cannot convert 8-bit to 10-bit") {
		t.Errorf("unexpected error message: %v", err)
	}

	// Confirm no reference extraction or candidate encode happened
	logBytes, _ := os.ReadFile(logFile)
	logContent := string(logBytes)
	if strings.Contains(logContent, "-c:v ffv1") || strings.Contains(logContent, "-c:v hevc_videotoolbox") {
		t.Errorf("ffmpeg encode calls were made despite validation rejection:\n%s", logContent)
	}
}

func TestProductionBenchmarkRunner_10BitSourceRejects8Bit(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "original_10bit.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake 10-bit video content"), 0644)

	mockFFmpeg, mockProbe, logFile := setupMockTools(t, dir, sdr10BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-test-reject10to8",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_bad", Quality: 60, VideoProfile: "main", PixelFormat: "yuv420p"},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	err := runner.RunBenchmark(context.Background(), worker, record)
	if err == nil {
		t.Fatalf("expected error rejecting 8-bit candidate on 10-bit source, got nil")
	}
	if !strings.Contains(err.Error(), "cannot downgrade 10-bit source to 8-bit") {
		t.Errorf("unexpected error message: %v", err)
	}

	// Confirm no encode calls were made
	logBytes, _ := os.ReadFile(logFile)
	logContent := string(logBytes)
	if strings.Contains(logContent, "-c:v ffv1") || strings.Contains(logContent, "-c:v hevc_videotoolbox") {
		t.Errorf("ffmpeg encode calls were made despite validation rejection:\n%s", logContent)
	}
}

func TestProductionBenchmarkRunner_HDRSourceRejected(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "original_hdr.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake HDR video content"), 0644)

	mockFFmpeg, mockProbe, logFile := setupMockTools(t, dir, hdrProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-test-hdr",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_1", Quality: 70},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	err := runner.RunBenchmark(context.Background(), worker, record)
	if err == nil {
		t.Fatalf("expected HDR source to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "HDR/Dolby Vision source not eligible") {
		t.Errorf("unexpected error message: %v", err)
	}

	// Confirm no encode calls were made
	logBytes, _ := os.ReadFile(logFile)
	logContent := string(logBytes)
	if strings.Contains(logContent, "-c:v ffv1") || strings.Contains(logContent, "-c:v hevc_videotoolbox") {
		t.Errorf("ffmpeg encode calls were made for HDR source:\n%s", logContent)
	}
}

func TestProductionBenchmarkRunner_MultipleVideoStreamsRejected(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "original_multi_video.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake multi video content"), 0644)

	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, multiVideoProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-test-multivideo",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_1", Quality: 70},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	err := runner.RunBenchmark(context.Background(), worker, record)
	if err == nil {
		t.Fatalf("expected multi-video source to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "has 2 video streams; benchmark requires exactly 1") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestProductionBenchmarkRunner_FFmpegErrorBoundedAndSourceUntouched(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "original_source_err.mkv")
	_ = os.WriteFile(sourceFile, []byte("source data to verify hash"), 0644)

	initialHash, _ := fileSHA256(sourceFile)

	// Trigger error on cand_q60 encode
	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "-q:v 60")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-test-err",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_q60", Quality: 60},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	err := runner.RunBenchmark(context.Background(), worker, record)
	if err == nil {
		t.Fatalf("expected error from simulated failure, got nil")
	}

	// Verify error message is bounded
	if len(err.Error()) > 2048 {
		t.Errorf("error message exceeds 2048 characters: length=%d", len(err.Error()))
	}

	// Verify source hash is completely untouched
	postHash, _ := fileSHA256(sourceFile)
	if initialHash != postHash {
		t.Errorf("source was modified despite error! %s != %s", initialHash, postHash)
	}
}

func TestProductionBenchmarkRunner_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "original_source_cancel.mkv")
	_ = os.WriteFile(sourceFile, []byte("source content cancel test"), 0644)

	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-test-cancel",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_q65", Quality: 65},
		},
		Attempt: 1,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	runner := &ProductionBenchmarkRunner{}
	err := runner.RunBenchmark(ctx, worker, record)
	if err == nil {
		t.Fatalf("expected error on cancelled context, got nil")
	}
}

func TestProductionBenchmarkRunner_EndToEndWithInternalBenchmarkCleanup(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "original_media.mkv")
	_ = os.WriteFile(sourceFile, []byte("media content for e2e"), 0644)

	initialHash, _ := fileSHA256(sourceFile)

	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	jobID := "bench-e2e4b01"
	jobDir := filepath.Join(cfg.StateDir, jobID)
	_ = os.MkdirAll(jobDir, 0755)
	benchFile := filepath.Join(jobDir, "benchmark.json")

	token := "run-token-e2e4b"
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "queued",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
			{Index: 1, StartSeconds: 25.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "c1", Quality: 65},
		},
		Attempt:   1,
		RunToken:  token,
		CreatedAt: time.Now().UTC(),
	}
	_ = SaveBenchmarkAtomic(benchFile, record)

	// Invoke InternalBenchmark with default runner (ProductionBenchmarkRunner)
	ctx := context.Background()
	if err := worker.InternalBenchmark(ctx, jobID, token); err != nil {
		t.Fatalf("InternalBenchmark failed: %v", err)
	}

	// 1. Verify benchmark record transitioned to completed
	saved, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("loading saved benchmark: %v", err)
	}
	if saved.Status != "completed" {
		t.Errorf("expected status 'completed', got %q (error: %s)", saved.Status, saved.Error)
	}

	// 2. Verify evidence was persisted in benchmark.json
	if saved.Evidence == nil {
		t.Fatalf("expected non-nil Evidence in saved benchmark record")
	}
	if len(saved.Evidence.ReferenceSamples) != 2 {
		t.Errorf("expected 2 reference samples in evidence, got %d", len(saved.Evidence.ReferenceSamples))
	}
	if len(saved.Evidence.CandidateSamples) != 2 {
		t.Errorf("expected 2 candidate samples in evidence, got %d", len(saved.Evidence.CandidateSamples))
	}

	// 3. Verify that samples/ directory was cleaned up by InternalBenchmark
	samplesDir := filepath.Join(jobDir, "samples")
	if _, err := os.Stat(samplesDir); !os.IsNotExist(err) {
		t.Errorf("expected samples/ directory to be cleaned up, but it exists: %s", samplesDir)
	}

	// 4. Verify jobDir and benchmark.json still exist
	if _, err := os.Stat(benchFile); err != nil {
		t.Errorf("expected benchmark.json to remain intact: %v", err)
	}

	// 5. Verify source media hash is completely unchanged
	postHash, _ := fileSHA256(sourceFile)
	if initialHash != postHash {
		t.Errorf("source media was mutated! %s != %s", initialHash, postHash)
	}
}

func TestCandidateFileKey_CollisionFree(t *testing.T) {
	cases := []struct {
		idx1, q1 int
		id1      string
		idx2, q2 int
		id2      string
	}{
		{0, 60, "cand.1", 0, 60, "cand_1"},
		{0, 60, "a_very_long_candidate_id_that_exceeds_thirty_two_characters_prefix_a", 0, 60, "a_very_long_candidate_id_that_exceeds_thirty_two_characters_prefix_b"},
		{0, 60, "cand", 1, 60, "cand"},
		{0, 60, "cand", 0, 70, "cand"},
	}

	for _, tc := range cases {
		k1 := candidateFileKey(tc.idx1, tc.id1, tc.q1)
		k2 := candidateFileKey(tc.idx2, tc.id2, tc.q2)
		if k1 == k2 {
			t.Errorf("candidateFileKey collision between (%d,%s,%d) and (%d,%s,%d): %s",
				tc.idx1, tc.id1, tc.q1, tc.idx2, tc.id2, tc.q2, k1)
		}
	}
}

func TestBoundedBuffer_CapAndTruncation(t *testing.T) {
	b := newBoundedBuffer(512)
	payload := strings.Repeat("x", 2000)
	n, err := b.Write([]byte(payload))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if n != len(payload) {
		t.Errorf("expected Write to report %d bytes written, got %d", len(payload), n)
	}
	if b.buf.Len() > 512 {
		t.Errorf("buffer exceeded max capacity: %d > 512", b.buf.Len())
	}
	if !b.truncated {
		t.Errorf("expected truncated to be true")
	}
	out := b.String()
	if !strings.Contains(out, "... [stderr truncated]") {
		t.Errorf("expected truncation marker in String(), got: %s", out)
	}
}

func TestProductionBenchmarkRunner_SamplesDirSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "original_source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video data"), 0644)

	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	jobID := "bench-symlink-dir"
	jobDir := filepath.Join(cfg.StateDir, jobID)
	_ = os.MkdirAll(jobDir, 0755)

	outsideDir := filepath.Join(dir, "outside_target")
	_ = os.MkdirAll(outsideDir, 0755)

	samplesDir := filepath.Join(jobDir, "samples")
	if err := os.Symlink(outsideDir, samplesDir); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "c1", Quality: 65},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	err := runner.RunBenchmark(context.Background(), worker, record)
	if err == nil {
		t.Fatalf("expected error when samplesDir is a symlink, got nil")
	}
	if !strings.Contains(err.Error(), "is a symlink (fail closed)") {
		t.Errorf("unexpected error message: %v", err)
	}

	// Verify outside target directory remains completely empty
	entries, _ := os.ReadDir(outsideDir)
	if len(entries) > 0 {
		t.Errorf("outside directory was modified despite symlink rejection: %v", entries)
	}
}

func TestProductionBenchmarkRunner_RefPathSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "original_source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video data"), 0644)

	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	jobID := "bench-symlink-ref"
	jobDir := filepath.Join(cfg.StateDir, jobID)
	samplesDir := filepath.Join(jobDir, "samples")
	_ = os.MkdirAll(samplesDir, 0755)

	outsideFile := filepath.Join(dir, "outside_ref_target.txt")
	outsideContent := []byte("critical outside file do not overwrite")
	_ = os.WriteFile(outsideFile, outsideContent, 0644)

	refPath := filepath.Join(samplesDir, "ref_sample_0.mkv")
	if err := os.Symlink(outsideFile, refPath); err != nil {
		t.Fatalf("creating ref symlink: %v", err)
	}

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "c1", Quality: 65},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	err := runner.RunBenchmark(context.Background(), worker, record)
	if err == nil {
		t.Fatalf("expected error when refPath is a symlink, got nil")
	}
	if !strings.Contains(err.Error(), "is a symlink (fail closed)") {
		t.Errorf("unexpected error message: %v", err)
	}

	// Verify outside file was NOT overwritten
	data, _ := os.ReadFile(outsideFile)
	if string(data) != string(outsideContent) {
		t.Errorf("outside file was overwritten through symlink! got: %s", string(data))
	}
}

func TestProductionBenchmarkRunner_CandPathSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "original_source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video data"), 0644)

	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	jobID := "bench-symlink-cand"
	jobDir := filepath.Join(cfg.StateDir, jobID)
	samplesDir := filepath.Join(jobDir, "samples")
	_ = os.MkdirAll(samplesDir, 0755)

	outsideFile := filepath.Join(dir, "outside_cand_target.txt")
	outsideContent := []byte("critical outside cand file")
	_ = os.WriteFile(outsideFile, outsideContent, 0644)

	candKey := candidateFileKey(0, "c1", 65)
	candPath := filepath.Join(samplesDir, fmt.Sprintf("%s_sample_0.mkv", candKey))
	if err := os.Symlink(outsideFile, candPath); err != nil {
		t.Fatalf("creating cand symlink: %v", err)
	}

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "c1", Quality: 65},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	err := runner.RunBenchmark(context.Background(), worker, record)
	if err == nil {
		t.Fatalf("expected error when candPath is a symlink, got nil")
	}
	if !strings.Contains(err.Error(), "is a symlink (fail closed)") {
		t.Errorf("unexpected error message: %v", err)
	}

	data, _ := os.ReadFile(outsideFile)
	if string(data) != string(outsideContent) {
		t.Errorf("outside cand file was overwritten through symlink! got: %s", string(data))
	}
}

func TestProductionBenchmarkRunner_PreExistingRegularFileReplacedCleanly(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "original_source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video data"), 0644)

	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	jobID := "bench-regular-replace"
	jobDir := filepath.Join(cfg.StateDir, jobID)
	samplesDir := filepath.Join(jobDir, "samples")
	_ = os.MkdirAll(samplesDir, 0755)

	refPath := filepath.Join(samplesDir, "ref_sample_0.mkv")
	_ = os.WriteFile(refPath, []byte("stale leftover data from previous aborted attempt"), 0644)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "c1", Quality: 65},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark failed: %v", err)
	}

	refData, err := os.ReadFile(refPath)
	if err != nil {
		t.Fatalf("reading ref file: %v", err)
	}
	if string(refData) == "stale leftover data from previous aborted attempt" {
		t.Errorf("stale file was not replaced!")
	}
}

func TestCleanBenchmarkSamples_SymlinkGuards(t *testing.T) {
	dir := t.TempDir()
	outsideDir := filepath.Join(dir, "outside_precious_data")
	_ = os.MkdirAll(outsideDir, 0755)
	preciousFile := filepath.Join(outsideDir, "keep_me.txt")
	_ = os.WriteFile(preciousFile, []byte("do not delete"), 0644)

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
	}
	worker := NewWorker(cfg)

	t.Run("samplesDir is symlink to outside", func(t *testing.T) {
		jobID := "bench-clean-symlink"
		jobDir := filepath.Join(cfg.StateDir, jobID)
		_ = os.MkdirAll(jobDir, 0755)
		samplesDir := filepath.Join(jobDir, "samples")
		if err := os.Symlink(outsideDir, samplesDir); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		err := worker.CleanBenchmarkSamples(jobID)
		if err != nil {
			t.Fatalf("CleanBenchmarkSamples failed: %v", err)
		}

		// The symlink entry itself should be unlinked
		if _, err := os.Lstat(samplesDir); !os.IsNotExist(err) {
			t.Errorf("expected symlink to be removed, but stat succeeded")
		}

		// Outside directory and its files must be completely intact!
		if _, err := os.Stat(preciousFile); err != nil {
			t.Errorf("outside file was deleted! %v", err)
		}
	})

	t.Run("jobDir is symlink", func(t *testing.T) {
		jobID := "bench-clean-jobsymlink"
		outsideJob := filepath.Join(dir, "outside_job")
		_ = os.MkdirAll(outsideJob, 0755)
		jobDir := filepath.Join(cfg.StateDir, jobID)
		_ = os.Symlink(outsideJob, jobDir)

		err := worker.CleanBenchmarkSamples(jobID)
		if err == nil {
			t.Errorf("expected error when jobDir is a symlink, got nil")
		}
	})
}

func TestProductionBenchmarkRunner_PartialEvidencePreservedOnFailure(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "original_source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video data"), 0644)

	// Simulate failure specifically when encoding sample 1 (ref_sample_1 as input to candidate encode)
	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "ref_sample_1.mkv*hevc_videotoolbox")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	jobID := "bench-partial-evidence"
	jobDir := filepath.Join(cfg.StateDir, jobID)
	_ = os.MkdirAll(jobDir, 0755)
	benchFile := filepath.Join(jobDir, "benchmark.json")

	token := "run-token-partial"
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "queued",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
			{Index: 1, StartSeconds: 25.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "c1", Quality: 65},
		},
		Attempt:   1,
		RunToken:  token,
		CreatedAt: time.Now().UTC(),
	}
	_ = SaveBenchmarkAtomic(benchFile, record)

	ctx := context.Background()
	err := worker.InternalBenchmark(ctx, jobID, token)
	if err == nil {
		t.Fatalf("expected InternalBenchmark to return error on simulated failure, got nil")
	}

	saved, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("loading saved benchmark: %v", err)
	}

	if saved.Status != "failed" {
		t.Errorf("expected status 'failed', got %q", saved.Status)
	}
	if saved.Evidence == nil {
		t.Fatalf("expected non-nil Evidence on failed benchmark to preserve partial work")
	}

	// Both reference samples were extracted successfully
	if len(saved.Evidence.ReferenceSamples) != 2 {
		t.Errorf("expected 2 reference samples in partial evidence, got %d", len(saved.Evidence.ReferenceSamples))
	}

	// Candidate sample 0 succeeded, Candidate sample 1 failed and was recorded with Error
	if len(saved.Evidence.CandidateSamples) != 2 {
		t.Fatalf("expected 2 candidate samples in evidence (1 success, 1 failure), got %d", len(saved.Evidence.CandidateSamples))
	}
	if saved.Evidence.CandidateSamples[0].Error != "" {
		t.Errorf("candidate sample 0 should have no error, got: %s", saved.Evidence.CandidateSamples[0].Error)
	}
	if saved.Evidence.CandidateSamples[1].Error == "" {
		t.Errorf("candidate sample 1 should record failure error, got empty")
	}
}

func TestProductionBenchmarkRunner_DolbyVisionDetection(t *testing.T) {
	cases := []struct {
		name      string
		probeJSON string
	}{
		{
			name: "codec dvh1",
			probeJSON: `{
  "streams": [{
    "index": 0, "codec_type": "video", "codec_name": "dvh1", "pix_fmt": "p010le", "bits_per_raw_sample": "10", "width": 1920, "height": 1080
  }], "format": {"duration": "100.0"}
}`,
		},
		{
			name: "codec dvhe",
			probeJSON: `{
  "streams": [{
    "index": 0, "codec_type": "video", "codec_name": "dvhe", "pix_fmt": "p010le", "bits_per_raw_sample": "10", "width": 1920, "height": 1080
  }], "format": {"duration": "100.0"}
}`,
		},
		{
			name: "profile Dolby Vision",
			probeJSON: `{
  "streams": [{
    "index": 0, "codec_type": "video", "codec_name": "hevc", "profile": "Dolby Vision Profile 5", "pix_fmt": "p010le", "bits_per_raw_sample": "10", "width": 1920, "height": 1080
  }], "format": {"duration": "100.0"}
}`,
		},
		{
			name: "DOVI side data",
			probeJSON: `{
  "streams": [{
    "index": 0, "codec_type": "video", "codec_name": "hevc", "profile": "Main 10", "pix_fmt": "p010le", "bits_per_raw_sample": "10", "width": 1920, "height": 1080,
    "side_data_list": [{"side_data_type": "DOVI configuration record"}]
  }], "format": {"duration": "100.0"}
}`,
		},
		{
			name: "stream tags dovi",
			probeJSON: `{
  "streams": [{
    "index": 0, "codec_type": "video", "codec_name": "hevc", "profile": "Main 10", "pix_fmt": "p010le", "bits_per_raw_sample": "10", "width": 1920, "height": 1080,
    "tags": {"dovi_profile": "5"}
  }], "format": {"duration": "100.0"}
}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sourceFile := filepath.Join(dir, "source_dv.mkv")
			_ = os.WriteFile(sourceFile, []byte("dv media content"), 0644)

			mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, tc.probeJSON, "")

			cfg := &WorkerConfig{
				StateDir:        filepath.Join(dir, "state"),
				AllowedRoots:    []string{dir},
				MaxParallelJobs: 1,
				FFmpeg:          mockFFmpeg,
				FFprobe:         mockProbe,
			}
			worker := NewWorker(cfg)

			record := &BenchmarkRecord{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              "bench-dv",
				Status:          "running",
				Source:          sourceFile,
				Metric:          "vmaf",
				Samples:         []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 1.0, DurationSeconds: 10.0}},
				Candidates:      []transcode.BenchmarkCandidate{{ID: "c1", Quality: 70}},
				Attempt:         1,
			}

			runner := &ProductionBenchmarkRunner{}
			err := runner.RunBenchmark(context.Background(), worker, record)
			if err == nil {
				t.Fatalf("expected Dolby Vision source to be rejected, got nil")
			}
			if !strings.Contains(err.Error(), "HDR/Dolby Vision source not eligible") {
				t.Errorf("unexpected error message: %v", err)
			}
		})
	}
}

func TestProductionBenchmarkRunner_ChromaSubsamplingGating(t *testing.T) {
	cases := []struct {
		name            string
		probeJSON       string
		expectedErrPart string
	}{
		{
			name: "yuv444p 8-bit rejected",
			probeJSON: `{
  "streams": [{
    "index": 0, "codec_type": "video", "codec_name": "h264", "pix_fmt": "yuv444p", "bits_per_raw_sample": "8", "width": 1920, "height": 1080
  }], "format": {"duration": "100.0"}
}`,
			expectedErrPart: "requires 4:2:0 chroma subsampling",
		},
		{
			name: "yuv422p 8-bit rejected",
			probeJSON: `{
  "streams": [{
    "index": 0, "codec_type": "video", "codec_name": "h264", "pix_fmt": "yuv422p", "bits_per_raw_sample": "8", "width": 1920, "height": 1080
  }], "format": {"duration": "100.0"}
}`,
			expectedErrPart: "requires 4:2:0 chroma subsampling",
		},
		{
			name: "yuv444p10le 10-bit rejected",
			probeJSON: `{
  "streams": [{
    "index": 0, "codec_type": "video", "codec_name": "hevc", "profile": "Main 4:4:4 10", "pix_fmt": "yuv444p10le", "bits_per_raw_sample": "10", "width": 1920, "height": 1080
  }], "format": {"duration": "100.0"}
}`,
			expectedErrPart: "requires 4:2:0 chroma subsampling",
		},
		{
			name: "yuvj420p full-range 8-bit rejected",
			probeJSON: `{
  "streams": [{
    "index": 0, "codec_type": "video", "codec_name": "h264", "pix_fmt": "yuvj420p", "bits_per_raw_sample": "8", "width": 1920, "height": 1080
  }], "format": {"duration": "100.0"}
}`,
			expectedErrPart: "full-range yuvj420p is deferred until range-normalized metric pipeline support",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sourceFile := filepath.Join(dir, "source_chroma.mkv")
			_ = os.WriteFile(sourceFile, []byte("chroma test media"), 0644)

			mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, tc.probeJSON, "")

			cfg := &WorkerConfig{
				StateDir:        filepath.Join(dir, "state"),
				AllowedRoots:    []string{dir},
				MaxParallelJobs: 1,
				FFmpeg:          mockFFmpeg,
				FFprobe:         mockProbe,
			}
			worker := NewWorker(cfg)

			record := &BenchmarkRecord{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              "bench-chroma",
				Status:          "running",
				Source:          sourceFile,
				Metric:          "vmaf",
				Samples:         []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 1.0, DurationSeconds: 10.0}},
				Candidates:      []transcode.BenchmarkCandidate{{ID: "c1", Quality: 70}},
				Attempt:         1,
			}

			runner := &ProductionBenchmarkRunner{}
			err := runner.RunBenchmark(context.Background(), worker, record)
			if err == nil {
				t.Fatalf("expected unsupported chroma to be rejected, got nil")
			}
			expected := tc.expectedErrPart
			if expected == "" {
				expected = "requires 4:2:0 chroma subsampling"
			}
			if !strings.Contains(err.Error(), expected) {
				t.Errorf("unexpected error message: %v (expected part: %q)", err, expected)
			}
		})
	}
}

func TestProductionBenchmarkRunner_SourceStabilityConcurrentlyModified(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source_concur.mkv")
	_ = os.WriteFile(sourceFile, []byte("initial source bytes"), 0644)

	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-concur",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples:         []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 1.0, DurationSeconds: 10.0}},
		Candidates:      []transcode.BenchmarkCandidate{{ID: "c1", Quality: 65}},
		Attempt:         1,
	}

	runner := &ProductionBenchmarkRunner{}
	runner.SetMetricsHook(func(ctx context.Context, w *Worker, record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence) error {
		// Mutate source file size concurrently right before final check
		return os.WriteFile(sourceFile, []byte("tampered content modifying length"), 0644)
	})

	err := runner.RunBenchmark(context.Background(), worker, record)
	if err == nil {
		t.Fatalf("expected error on concurrently modified source file, got nil")
	}
	if !strings.Contains(err.Error(), "concurrently modified") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestProductionBenchmarkRunner_ProcessGroupCancellation_NoOrphans(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source_cancel_tree.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video content for tree cancel"), 0644)

	pidFile := filepath.Join(dir, "child.pid")
	grandchildPidFile := filepath.Join(dir, "grandchild.pid")

	mockProbe := filepath.Join(dir, "mock_probe.sh")
	_ = os.WriteFile(mockProbe, []byte(fmt.Sprintf("#!/bin/sh\ncat << 'EOF'\n%s\nEOF\n", sdr8BitProbeJSON)), 0755)

	mockFFmpeg := filepath.Join(dir, "mock_ffmpeg_tree.sh")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1"
    exit 0
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -prio_speed        <boolean>    E..V....... Prioritize encoding speed (default false)
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
  -realtime          <boolean>    E..V....... Realtime (default false)
EOF
    exit 0
    ;;
  *"-encoders"*)
    cat << 'EOF'
Encoders:
 V..... hevc_videotoolbox    VideoToolbox H.265
 V..... ffv1                 FFmpeg video codec #1
EOF
    exit 0
    ;;
esac

# Record direct child PID
echo $$ > %q

# Spawn grandchild in the background
sleep 300 &
echo $! > %q

# Wait indefinitely for grandchild or until killed
wait
`, pidFile, grandchildPidFile)
	if err := os.WriteFile(mockFFmpeg, []byte(script), 0755); err != nil {
		t.Fatalf("writing script: %v", err)
	}

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-tree-cancel",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples:         []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 1.0, DurationSeconds: 10.0}},
		Candidates:      []transcode.BenchmarkCandidate{{ID: "c1", Quality: 65}},
		Attempt:         1,
	}

	ctx, cancel := context.WithCancel(context.Background())
	runner := &ProductionBenchmarkRunner{}

	errCh := make(chan error, 1)
	go func() {
		errCh <- runner.RunBenchmark(ctx, worker, record)
	}()

	// Wait until both child PID and grandchild PID are recorded and running
	var childPID, gcPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pBytes, err1 := os.ReadFile(pidFile)
		gcBytes, err2 := os.ReadFile(grandchildPidFile)
		if err1 == nil && err2 == nil {
			pStr := strings.TrimSpace(string(pBytes))
			gcStr := strings.TrimSpace(string(gcBytes))
			if pStr != "" && gcStr != "" {
				childPID, _ = strconv.Atoi(pStr)
				gcPID, _ = strconv.Atoi(gcStr)
				if childPID > 0 && gcPID > 0 {
					if syscall.Kill(childPID, 0) == nil && syscall.Kill(gcPID, 0) == nil {
						break
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if childPID == 0 || gcPID == 0 {
		cancel()
		t.Fatalf("timed out waiting for child (%d) and grandchild (%d) to start", childPID, gcPID)
	}

	// Verify they are alive
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("child process %d is not alive: %v", childPID, err)
	}
	if err := syscall.Kill(gcPID, 0); err != nil {
		t.Fatalf("grandchild process %d is not alive: %v", gcPID, err)
	}

	// Now cancel the context
	cancel()

	// Wait for RunBenchmark to return
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("expected error from cancelled RunBenchmark, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for RunBenchmark to exit after cancel")
	}

	// Wait up to 3 seconds for the process group to be reaped/terminated
	killDeadline := time.Now().Add(3 * time.Second)
	childDead := false
	gcDead := false
	for time.Now().Before(killDeadline) {
		if !childDead && syscall.Kill(childPID, 0) != nil {
			childDead = true
		}
		if !gcDead && syscall.Kill(gcPID, 0) != nil {
			gcDead = true
		}
		if childDead && gcDead {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	_ = syscall.Kill(childPID, syscall.SIGKILL)
	_ = syscall.Kill(gcPID, syscall.SIGKILL)

	if !childDead {
		t.Errorf("child process %d is still alive after cancellation! Orphan detected", childPID)
	}
	if !gcDead {
		t.Errorf("grandchild process %d is still alive after cancellation! Process group orphan detected", gcPID)
	}
}

func TestBenchmarkCancel_TerminatesProcessGroupAndMarksCancelled(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source_bench_cancel.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video data"), 0644)

	pidFile := filepath.Join(dir, "ffmpeg_bench.pid")
	gcPidFile := filepath.Join(dir, "ffmpeg_bench_gc.pid")

	mockProbe := filepath.Join(dir, "mock_probe.sh")
	_ = os.WriteFile(mockProbe, []byte(fmt.Sprintf("#!/bin/sh\ncat << 'EOF'\n%s\nEOF\n", sdr8BitProbeJSON)), 0755)

	mockFFmpeg := filepath.Join(dir, "mock_ffmpeg_cancel.sh")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1"
    exit 0
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -prio_speed        <boolean>    E..V....... Prioritize encoding speed (default false)
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
  -realtime          <boolean>    E..V....... Realtime (default false)
EOF
    exit 0
    ;;
  *"-encoders"*)
    cat << 'EOF'
Encoders:
 V..... hevc_videotoolbox    VideoToolbox H.265
 V..... ffv1                 FFmpeg video codec #1
EOF
    exit 0
    ;;
esac

echo $$ > %q
sleep 300 &
echo $! > %q
wait
`, pidFile, gcPidFile)
	_ = os.WriteFile(mockFFmpeg, []byte(script), 0755)

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	jobID := "bench-cancel-e2e"
	jobDir := filepath.Join(cfg.StateDir, jobID)
	_ = os.MkdirAll(jobDir, 0755)
	benchFile := filepath.Join(jobDir, "benchmark.json")

	token := "run-token-cancel-e2e"
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "queued",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples:         []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0}},
		Candidates:      []transcode.BenchmarkCandidate{{ID: "c1", Quality: 65}},
		Attempt:         1,
		RunToken:        token,
		CreatedAt:       time.Now().UTC(),
	}
	_ = SaveBenchmarkAtomic(benchFile, record)

	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- worker.InternalBenchmark(bgCtx, jobID, token)
	}()

	var childPID, gcPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pBytes, err1 := os.ReadFile(pidFile)
		gcBytes, err2 := os.ReadFile(gcPidFile)
		if err1 == nil && err2 == nil {
			pStr := strings.TrimSpace(string(pBytes))
			gcStr := strings.TrimSpace(string(gcBytes))
			if pStr != "" && gcStr != "" {
				childPID, _ = strconv.Atoi(pStr)
				gcPID, _ = strconv.Atoi(gcStr)
				if childPID > 0 && gcPID > 0 {
					if syscall.Kill(childPID, 0) == nil && syscall.Kill(gcPID, 0) == nil {
						break
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if childPID == 0 || gcPID == 0 {
		t.Fatalf("mock ffmpeg child/grandchild did not start in time")
	}

	statusResp, err := worker.BenchmarkCancel(context.Background(), jobID)
	if err != nil {
		t.Fatalf("BenchmarkCancel failed: %v", err)
	}
	if statusResp.Status != "cancelled" {
		t.Errorf("expected BenchmarkCancel status 'cancelled', got %q", statusResp.Status)
	}

	bgCancel()

	select {
	case <-runErrCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("InternalBenchmark did not exit after cancel")
	}

	killDeadline := time.Now().Add(3 * time.Second)
	childDead := false
	gcDead := false
	for time.Now().Before(killDeadline) {
		if !childDead && syscall.Kill(childPID, 0) != nil {
			childDead = true
		}
		if !gcDead && syscall.Kill(gcPID, 0) != nil {
			gcDead = true
		}
		if childDead && gcDead {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	_ = syscall.Kill(childPID, syscall.SIGKILL)
	_ = syscall.Kill(gcPID, syscall.SIGKILL)

	if !childDead {
		t.Errorf("child process %d still alive after BenchmarkCancel", childPID)
	}
	if !gcDead {
		t.Errorf("grandchild process %d still alive after BenchmarkCancel", gcPID)
	}

	saved, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("loading benchmark record: %v", err)
	}
	if saved.Status != "cancelled" {
		t.Errorf("expected saved record status 'cancelled', got %q", saved.Status)
	}
}

func TestInternalBenchmark_CoordinatorCancellation_PreservesPartialEvidence(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source_cancel_evidence.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video data for cancel evidence"), 0644)

	pidFile := filepath.Join(dir, "sample1_ref.pid")

	mockProbe := filepath.Join(dir, "mock_probe.sh")
	_ = os.WriteFile(mockProbe, []byte(fmt.Sprintf("#!/bin/sh\ncat << 'EOF'\n%s\nEOF\n", sdr8BitProbeJSON)), 0755)

	mockFFmpeg := filepath.Join(dir, "mock_ffmpeg_cancel_evidence.sh")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1"
    exit 0
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -prio_speed        <boolean>    E..V....... Prioritize encoding speed (default false)
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
  -realtime          <boolean>    E..V....... Realtime (default false)
EOF
    exit 0
    ;;
  *"-encoders"*)
    cat << 'EOF'
Encoders:
 V..... hevc_videotoolbox    VideoToolbox H.265
 V..... ffv1                 FFmpeg video codec #1
EOF
    exit 0
    ;;
  *"ref_sample_1.mkv"*)
    # When extracting ref sample 1, record PID and sleep indefinitely so cancellation occurs mid-run
    echo $$ > %q
    sleep 300
    exit 0
    ;;
esac

# Find output file (last argument) and create it so os.Stat succeeds
out=""
for last; do out="$last"; done
if [ -n "$out" ]; then
  mkdir -p "$(dirname "$out")"
  echo "fake media data for $out" > "$out"
fi
exit 0
`, pidFile)
	_ = os.WriteFile(mockFFmpeg, []byte(script), 0755)

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	jobID := "bench-cancel-partial"
	jobDir := filepath.Join(cfg.StateDir, jobID)
	_ = os.MkdirAll(jobDir, 0755)
	benchFile := filepath.Join(jobDir, "benchmark.json")

	token := "run-token-cancel-partial"
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "queued",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
			{Index: 1, StartSeconds: 25.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "c1", Quality: 65},
		},
		Attempt:   1,
		RunToken:  token,
		CreatedAt: time.Now().UTC(),
	}
	_ = SaveBenchmarkAtomic(benchFile, record)

	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- worker.InternalBenchmark(bgCtx, jobID, token)
	}()

	// Wait for sample 1 reference extraction to start and record its PID
	var childPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pBytes, err := os.ReadFile(pidFile)
		if err == nil {
			pStr := strings.TrimSpace(string(pBytes))
			if pStr != "" {
				childPID, _ = strconv.Atoi(pStr)
				if childPID > 0 && syscall.Kill(childPID, 0) == nil {
					break
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if childPID == 0 {
		t.Fatalf("timed out waiting for sample 1 reference extraction to start")
	}

	// Cancel via coordinator BenchmarkCancel
	statusResp, err := worker.BenchmarkCancel(context.Background(), jobID)
	if err != nil {
		t.Fatalf("BenchmarkCancel failed: %v", err)
	}
	if statusResp.Status != "cancelled" {
		t.Errorf("expected BenchmarkCancel status 'cancelled', got %q", statusResp.Status)
	}

	bgCancel()

	select {
	case err := <-runErrCh:
		if err != nil {
			t.Errorf("expected InternalBenchmark to return nil on cancelled state, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("InternalBenchmark did not exit after cancel")
	}

	_ = syscall.Kill(childPID, syscall.SIGKILL)

	// Verify benchmark record on disk:
	// 1. Status MUST remain "cancelled" (NO resurrection to completed or failed!)
	// 2. Evidence MUST be non-nil and preserve sample 0 outcomes
	// 3. Samples workspace must be cleaned
	saved, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("loading benchmark record: %v", err)
	}
	if saved.Status != "cancelled" {
		t.Fatalf("state resurrection! Expected status 'cancelled', got %q", saved.Status)
	}
	if saved.Evidence == nil {
		t.Fatalf("expected Evidence to be non-nil on cancelled record with pre-cancellation work")
	}
	if len(saved.Evidence.ReferenceSamples) < 1 {
		t.Errorf("expected at least 1 reference sample in partial evidence, got %d", len(saved.Evidence.ReferenceSamples))
	}
	if saved.Evidence.ReferenceSamples[0].Index != 0 {
		t.Errorf("expected sample 0 reference preserved, got index %d", saved.Evidence.ReferenceSamples[0].Index)
	}

	// Verify samples directory on disk was cleaned up
	samplesDir := filepath.Join(jobDir, "samples")
	if _, err := os.Stat(samplesDir); !os.IsNotExist(err) {
		t.Errorf("expected samples directory to be cleaned, but it exists: %s", samplesDir)
	}
}

func TestBuildVMAFArgs_ArgvExactness(t *testing.T) {
	candPath := "/tmp/test_workspace/cand_sample_0.mkv"
	refPath := "/tmp/test_workspace/ref_sample_0.mkv"
	logPath := "/tmp/test_workspace/vmaf_log.json"

	args := BuildVMAFArgs(candPath, refPath, logPath)
	expected := []string{
		"-nostats",
		"-i", candPath,
		"-i", refPath,
		"-filter_complex", "[0:v][1:v]libvmaf=log_fmt=json:log_path=" + logPath,
		"-f", "null",
		"-",
	}

	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d", len(expected), len(args))
	}
	for i, arg := range args {
		if arg != expected[i] {
			t.Errorf("arg[%d]: expected %q, got %q", i, expected[i], arg)
		}
	}

	// Verify escaping of special characters in log path:
	// Colon requires \\:, single quote requires \\\', backslash requires \\\\
	logPathSpecial := "/tmp/with:colon/and'quote/and\\backslash/log.json"
	argsSpecial := BuildVMAFArgs(candPath, refPath, logPathSpecial)
	expectedFilter := "[0:v][1:v]libvmaf=log_fmt=json:log_path=/tmp/with\\\\:colon/and\\\\\\'quote/and\\\\\\\\backslash/log.json"
	if argsSpecial[6] != expectedFilter {
		t.Errorf("expected filter string %q, got %q", expectedFilter, argsSpecial[6])
	}
}

func TestBuildSSIMArgs_ArgvExactness(t *testing.T) {
	candPath := "/tmp/test_workspace/cand_sample_0.mkv"
	refPath := "/tmp/test_workspace/ref_sample_0.mkv"
	statsPath := "/tmp/test_workspace/ssim.log"

	args := BuildSSIMArgs(candPath, refPath, statsPath)
	expected := []string{
		"-nostats",
		"-i", candPath,
		"-i", refPath,
		"-filter_complex", "[0:v][1:v]ssim=stats_file=" + statsPath,
		"-f", "null",
		"-",
	}

	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d", len(expected), len(args))
	}
	for i, arg := range args {
		if arg != expected[i] {
			t.Errorf("arg[%d]: expected %q, got %q", i, expected[i], arg)
		}
	}

	// Without stats path
	argsNoStats := BuildSSIMArgs(candPath, refPath, "")
	if argsNoStats[6] != "[0:v][1:v]ssim" {
		t.Errorf("expected filter string '[0:v][1:v]ssim', got %q", argsNoStats[6])
	}

	// With special characters in stats path
	statsPathSpecial := "/tmp/with:colon/and'quote/and\\backslash/stats.log"
	argsSpecial := BuildSSIMArgs(candPath, refPath, statsPathSpecial)
	expectedFilterSpecial := "[0:v][1:v]ssim=stats_file=/tmp/with\\\\:colon/and\\\\\\'quote/and\\\\\\\\backslash/stats.log"
	if argsSpecial[6] != expectedFilterSpecial {
		t.Errorf("expected filter string %q, got %q", expectedFilterSpecial, argsSpecial[6])
	}
}

func TestEscapeFFmpegFilterPath_Comprehensive(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "plain path",
			input:    "/var/log/benchmark.json",
			expected: "/var/log/benchmark.json",
		},
		{
			name:     "colon in path",
			input:    "/scratch/task_123:456/vmaf.json",
			expected: "/scratch/task_123\\\\:456/vmaf.json",
		},
		{
			name:     "single quote in path",
			input:    "/scratch/user's folder/ssim.log",
			expected: "/scratch/user\\\\\\'s folder/ssim.log",
		},
		{
			name:     "backslash in path",
			input:    `C:\media\samples\test.mkv`,
			expected: "C\\\\:\\\\\\\\media\\\\\\\\samples\\\\\\\\test.mkv",
		},
		{
			name:     "brackets comma semicolon in path",
			input:    "/tmp/[test,sample;1]/out.log",
			expected: "/tmp/\\\\[test\\\\,sample\\\\;1\\\\]/out.log",
		},
		{
			name:     "combined colons quotes backslashes and spaces",
			input:    `/tmp/run:1/user's \data/out.log`,
			expected: "/tmp/run\\\\:1/user\\\\\\'s \\\\\\\\data/out.log",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := escapeFFmpegFilterPath(tc.input)
			if got != tc.expected {
				t.Errorf("escapeFFmpegFilterPath(%q):\nexpected: %q\ngot:      %q", tc.input, tc.expected, got)
			}
		})
	}
}

func TestFFmpegFilterPath_LiveSmokeTest(t *testing.T) {
	ffmpegBin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg binary not installed, skipping live smoke test")
	}

	// Create a real directory with colons, single quotes, and spaces to challenge FFmpeg filter parsing
	baseDir := t.TempDir()
	specialDir := filepath.Join(baseDir, "path:with'quote and spaces")
	if err := os.MkdirAll(specialDir, 0700); err != nil {
		t.Fatalf("failed creating special dir: %v", err)
	}

	statsFile := filepath.Join(specialDir, "ssim:stats'out.log")
	filterArg := fmt.Sprintf("[0:v][1:v]ssim=stats_file=%s", escapeFFmpegFilterPath(statsFile))

	// Run harmless filter invocation with nullsrc test inputs directly via execve (no shell)
	cmd := exec.Command(ffmpegBin,
		"-nostats",
		"-f", "lavfi", "-i", "nullsrc=s=64x64:d=0.1:r=10",
		"-f", "lavfi", "-i", "nullsrc=s=64x64:d=0.1:r=10",
		"-filter_complex", filterArg,
		"-f", "null",
		"-",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg execution failed with filterArg %q: %v; output:\n%s", filterArg, err, string(out))
	}

	// Verify stats file was created by FFmpeg at the escaped path
	data, err := os.ReadFile(statsFile)
	if err != nil {
		t.Fatalf("expected stats file to be created at %s, but read failed: %v", statsFile, err)
	}
	if len(data) == 0 {
		t.Fatalf("expected non-empty stats file at %s", statsFile)
	}

	score, err := ParseSSIMStatsFile(data)
	if err != nil {
		t.Fatalf("failed parsing ssim stats file from live run: %v", err)
	}
	if score < 0.99 {
		t.Errorf("expected near-perfect SSIM for identical nullsrc inputs, got %v", score)
	}
}

func TestParseVMAFJSON_ValidAndMalformed(t *testing.T) {
	// Valid pooled metrics
	validPooled := []byte(`{
		"version": "2.3.1",
		"pooled_metrics": {
			"vmaf": {
				"mean": 94.750000,
				"min": 91.0,
				"max": 98.0
			}
		}
	}`)
	score, err := ParseVMAFJSON(validPooled)
	if err != nil {
		t.Fatalf("unexpected error parsing valid pooled vmaf: %v", err)
	}
	if math.Abs(score-94.75) > 1e-6 {
		t.Errorf("expected score 94.75, got %v", score)
	}

	// Valid frames fallback
	validFrames := []byte(`{
		"version": "2.3.1",
		"frames": [
			{"frameNum": 0, "metrics": {"vmaf": 92.0}},
			{"frameNum": 1, "metrics": {"vmaf": 96.0}}
		]
	}`)
	scoreFrames, err := ParseVMAFJSON(validFrames)
	if err != nil {
		t.Fatalf("unexpected error parsing frames fallback vmaf: %v", err)
	}
	if math.Abs(scoreFrames-94.0) > 1e-6 {
		t.Errorf("expected score 94.0, got %v", scoreFrames)
	}

	// Empty data
	if _, err := ParseVMAFJSON([]byte{}); err == nil {
		t.Errorf("expected error on empty data, got nil")
	}

	// Malformed JSON
	if _, err := ParseVMAFJSON([]byte(`{"pooled_metrics": ... bad json`)); err == nil {
		t.Errorf("expected error on malformed json, got nil")
	}

	// Missing score
	if _, err := ParseVMAFJSON([]byte(`{"version": "2.3.1"}`)); err == nil {
		t.Errorf("expected error on missing score, got nil")
	}

	// Out of bounds < 0
	if _, err := ParseVMAFJSON([]byte(`{"pooled_metrics": {"vmaf": {"mean": -5.0}}}`)); err == nil {
		t.Errorf("expected error on negative score, got nil")
	}

	// Out of bounds > 100
	if _, err := ParseVMAFJSON([]byte(`{"pooled_metrics": {"vmaf": {"mean": 105.0}}}`)); err == nil {
		t.Errorf("expected error on score > 100, got nil")
	}

	// Oversized
	oversized := make([]byte, MaxMetricLogSizeBytes+1)
	if _, err := ParseVMAFJSON(oversized); err == nil {
		t.Errorf("expected error on oversized data, got nil")
	}
}

func TestParseSSIMStatsFile_ValidAndMalformed(t *testing.T) {
	// Valid single frame
	singleFrame := []byte("n:1 Y:0.985000 U:0.990000 V:0.990000 All:0.985000 (18.23)\n")
	score, err := ParseSSIMStatsFile(singleFrame)
	if err != nil {
		t.Fatalf("unexpected error parsing single frame ssim: %v", err)
	}
	if math.Abs(score-0.985000) > 1e-6 {
		t.Errorf("expected score 0.985000, got %v", score)
	}

	// Valid multi-frame average
	multiFrame := []byte("n:1 All:0.980000\nn:2 All:0.990000\n")
	scoreMulti, err := ParseSSIMStatsFile(multiFrame)
	if err != nil {
		t.Fatalf("unexpected error parsing multi frame ssim: %v", err)
	}
	if math.Abs(scoreMulti-0.985000) > 1e-6 {
		t.Errorf("expected score 0.985000, got %v", scoreMulti)
	}

	// Empty data
	if _, err := ParseSSIMStatsFile([]byte{}); err == nil {
		t.Errorf("expected error on empty data, got nil")
	}

	// Malformed (no All:)
	if _, err := ParseSSIMStatsFile([]byte("invalid format without ssim\n")); err == nil {
		t.Errorf("expected error on missing ssim pattern, got nil")
	}

	// NaN
	if _, err := ParseSSIMStatsFile([]byte("n:1 All:nan\n")); err == nil {
		t.Errorf("expected error on NaN score, got nil")
	}

	// Inf
	if _, err := ParseSSIMStatsFile([]byte("n:1 All:inf\n")); err == nil {
		t.Errorf("expected error on Inf score, got nil")
	}

	// Out of bounds < 0
	if _, err := ParseSSIMStatsFile([]byte("n:1 All:-0.5\n")); err == nil {
		t.Errorf("expected error on negative score, got nil")
	}

	// Out of bounds > 1
	if _, err := ParseSSIMStatsFile([]byte("n:1 All:1.5\n")); err == nil {
		t.Errorf("expected error on score > 1.0, got nil")
	}

	// Oversized
	oversized := make([]byte, MaxMetricLogSizeBytes+1)
	if _, err := ParseSSIMStatsFile(oversized); err == nil {
		t.Errorf("expected error on oversized data, got nil")
	}
}

func TestParseSSIMStderr_ValidAndMalformed(t *testing.T) {
	validStderr := "[Parsed_ssim_0 @ 0x123] SSIM Y:0.980000 U:0.990000 V:0.990000 All:0.985000 (18.23)\n"
	score, err := ParseSSIMStderr(validStderr)
	if err != nil {
		t.Fatalf("unexpected error parsing valid stderr ssim: %v", err)
	}
	if math.Abs(score-0.985000) > 1e-6 {
		t.Errorf("expected score 0.985000, got %v", score)
	}

	// Empty stderr
	if _, err := ParseSSIMStderr(""); err == nil {
		t.Errorf("expected error on empty stderr, got nil")
	}

	// No pattern
	if _, err := ParseSSIMStderr("ffmpeg finished without ssim\n"); err == nil {
		t.Errorf("expected error on missing ssim in stderr, got nil")
	}

	// NaN
	if _, err := ParseSSIMStderr("SSIM All:nan\n"); err == nil {
		t.Errorf("expected error on NaN in stderr, got nil")
	}

	// Out of bounds > 1
	if _, err := ParseSSIMStderr("SSIM All:1.5\n"); err == nil {
		t.Errorf("expected error on out-of-bounds in stderr, got nil")
	}
}

func TestParseSSIMStderr_LastMatchSelection(t *testing.T) {
	// FFmpeg may print multiple progress SSIM lines before printing the final summary line
	multiMatchStderr := `
[Parsed_ssim_0 @ 0x123] SSIM Y:0.910000 U:0.920000 V:0.920000 All:0.915000 (10.00)
frame=   50 fps=0.0 q=-0.0 size=N/A time=00:00:02.00 bitrate=N/A speed=   4x
[Parsed_ssim_0 @ 0x123] SSIM Y:0.930000 U:0.940000 V:0.940000 All:0.935000 (11.87)
frame=  100 fps=0.0 q=-0.0 size=N/A time=00:00:04.00 bitrate=N/A speed=   4x
[Parsed_ssim_0 @ 0x123] SSIM Y:0.980000 U:0.990000 V:0.990000 All:0.985000 (18.23)
`
	score, err := ParseSSIMStderr(multiMatchStderr)
	if err != nil {
		t.Fatalf("unexpected error parsing multi-line stderr: %v", err)
	}
	// Must select the LAST match (0.985000) not the first match (0.915000)
	if math.Abs(score-0.985000) > 1e-6 {
		t.Errorf("expected last match 0.985000, got %v", score)
	}
}

func TestParseVMAFJSON_FrameFallbackContiguityAndValidation(t *testing.T) {
	// Contiguous frames: 0, 1, 2
	validContiguous := []byte(`{
		"frames": [
			{"frameNum": 0, "metrics": {"vmaf": 92.0}},
			{"frameNum": 1, "metrics": {"vmaf": 94.0}},
			{"frameNum": 2, "metrics": {"vmaf": 96.0}}
		]
	}`)
	score, err := ParseVMAFJSON(validContiguous)
	if err != nil {
		t.Fatalf("unexpected error on contiguous frames: %v", err)
	}
	if math.Abs(score-94.0) > 1e-6 {
		t.Errorf("expected score 94.0, got %v", score)
	}

	// Gapped frames: 0, 2 (missing frame 1) -> must fail closed
	gappedFrames := []byte(`{
		"frames": [
			{"frameNum": 0, "metrics": {"vmaf": 92.0}},
			{"frameNum": 2, "metrics": {"vmaf": 96.0}}
		]
	}`)
	if _, err := ParseVMAFJSON(gappedFrames); err == nil {
		t.Errorf("expected error on gapped frames, got nil")
	} else if !strings.Contains(err.Error(), "contiguous") {
		t.Errorf("expected error mentioning contiguous, got: %v", err)
	}

	// Duplicate frames: 0, 1, 1 -> must fail closed
	duplicateFrames := []byte(`{
		"frames": [
			{"frameNum": 0, "metrics": {"vmaf": 92.0}},
			{"frameNum": 1, "metrics": {"vmaf": 94.0}},
			{"frameNum": 1, "metrics": {"vmaf": 96.0}}
		]
	}`)
	if _, err := ParseVMAFJSON(duplicateFrames); err == nil {
		t.Errorf("expected error on duplicate frames, got nil")
	}

	// Out of bounds frame score: > 100
	outOfBoundsHigh := []byte(`{
		"frames": [
			{"frameNum": 0, "metrics": {"vmaf": 105.0}}
		]
	}`)
	if _, err := ParseVMAFJSON(outOfBoundsHigh); err == nil {
		t.Errorf("expected error on frame score > 100, got nil")
	}

	// Out of bounds frame score: < 0
	outOfBoundsLow := []byte(`{
		"frames": [
			{"frameNum": 0, "metrics": {"vmaf": -1.0}}
		]
	}`)
	if _, err := ParseVMAFJSON(outOfBoundsLow); err == nil {
		t.Errorf("expected error on negative frame score, got nil")
	}

	// NaN frame score
	nanFrame := []byte(`{
		"frames": [
			{"frameNum": 0, "metrics": {"vmaf": "NaN"}}
		]
	}`)
	if _, err := ParseVMAFJSON(nanFrame); err == nil {
		t.Errorf("expected error on NaN frame score, got nil")
	}
}

func TestReadMetricLogFile_Hardening(t *testing.T) {
	dir := t.TempDir()

	// 1. Valid regular file
	regularPath := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(regularPath, []byte(`{"version": "1.0"}`), 0600); err != nil {
		t.Fatalf("failed writing test file: %v", err)
	}
	data, err := readMetricLogFile(regularPath)
	if err != nil {
		t.Fatalf("unexpected error reading valid regular file: %v", err)
	}
	if string(data) != `{"version": "1.0"}` {
		t.Errorf("unexpected content: %s", string(data))
	}

	// 2. Symlink to regular file must fail closed
	symlinkPath := filepath.Join(dir, "symlink.json")
	if err := os.Symlink(regularPath, symlinkPath); err != nil {
		t.Fatalf("failed creating test symlink: %v", err)
	}
	if _, err := readMetricLogFile(symlinkPath); err == nil {
		t.Errorf("expected error reading symlink, got nil")
	}

	// 3. Empty file must fail closed
	emptyPath := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(emptyPath, []byte{}, 0600); err != nil {
		t.Fatalf("failed writing empty file: %v", err)
	}
	if _, err := readMetricLogFile(emptyPath); err == nil {
		t.Errorf("expected error reading empty file, got nil")
	}

	// 4. Non-existent file must fail closed
	missingPath := filepath.Join(dir, "missing.json")
	if _, err := readMetricLogFile(missingPath); err == nil {
		t.Errorf("expected error reading non-existent file, got nil")
	}
}

func TestTailBuffer_PreservesTail(t *testing.T) {
	buf := newTailBuffer(100) // 100 bytes max
	// Write 500 bytes with distinct sequential numbers
	for i := 0; i < 50; i++ {
		_, _ = buf.Write([]byte(fmt.Sprintf("line_%03d\n", i)))
	}
	result := buf.String()
	if len(result) > 100 {
		t.Errorf("tail buffer length %d exceeds max 100", len(result))
	}
	// Verify that the final lines are preserved in the tail
	if !strings.Contains(result, "line_049") {
		t.Errorf("expected tail buffer to contain the final line_049, got:\n%s", result)
	}
	if strings.Contains(result, "line_001") {
		t.Errorf("expected early line_001 to have been evicted, but it was present:\n%s", result)
	}
}

func TestProductionBenchmarkRunner_CapabilityGating(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video content"), 0644)

	// Mock ffprobe
	mockProbe := filepath.Join(dir, "mock_probe.sh")
	_ = os.WriteFile(mockProbe, []byte(fmt.Sprintf("#!/bin/sh\ncat << 'EOF'\n%s\nEOF\n", sdr8BitProbeJSON)), 0755)

	// Mock FFmpeg with filters WITHOUT libvmaf
	mockFFmpegNoVMAF := filepath.Join(dir, "mock_ffmpeg_no_vmaf.sh")
	_ = os.WriteFile(mockFFmpegNoVMAF, []byte(`#!/bin/sh
case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1"
    exit 0
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -prio_speed        <boolean>    E..V....... Prioritize encoding speed (default false)
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
  -realtime          <boolean>    E..V....... Realtime (default false)
EOF
    exit 0
    ;;
  *"-encoders"*)
    echo "Encoders:\n V..... hevc_videotoolbox VideoToolbox H.265\n V..... ffv1 FFmpeg video codec #1"
    exit 0
    ;;
  *"-filters"*)
    echo "Filters:\n  TS ssim VV->V Calculate the SSIM\n  .. null V->V Pass the source"
    exit 0
    ;;
esac
out=""
for last; do out="$last"; done
if [ -n "$out" ] && [ "$out" != "-" ]; then
  mkdir -p "$(dirname "$out")"
  echo "fake" > "$out"
fi
exit 0
`), 0755)

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpegNoVMAF,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-no-vmaf",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples:         []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 10.0, DurationSeconds: 15.0}},
		Candidates:      []transcode.BenchmarkCandidate{{ID: "cand1", Quality: 60}},
		Attempt:         1,
	}

	runner := &ProductionBenchmarkRunner{}
	err := runner.RunBenchmark(context.Background(), worker, record)
	if err == nil {
		t.Fatalf("expected closed failure when libvmaf filter missing, got nil")
	}
	if !strings.Contains(err.Error(), "libvmaf") {
		t.Errorf("expected error mentioning libvmaf, got: %v", err)
	}

	// Now test missing SSIM filter
	mockFFmpegNoSSIM := filepath.Join(dir, "mock_ffmpeg_no_ssim.sh")
	_ = os.WriteFile(mockFFmpegNoSSIM, []byte(`#!/bin/sh
case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1"
    exit 0
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -prio_speed        <boolean>    E..V....... Prioritize encoding speed (default false)
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
  -realtime          <boolean>    E..V....... Realtime (default false)
EOF
    exit 0
    ;;
  *"-encoders"*)
    echo "Encoders:\n V..... hevc_videotoolbox VideoToolbox H.265\n V..... ffv1 FFmpeg video codec #1"
    exit 0
    ;;
  *"-filters"*)
    echo "Filters:\n  .. libvmaf VV->V Calculate the VMAF\n  .. null V->V Pass the source"
    exit 0
    ;;
esac
out=""
for last; do out="$last"; done
if [ -n "$out" ] && [ "$out" != "-" ]; then
  mkdir -p "$(dirname "$out")"
  echo "fake" > "$out"
fi
exit 0
`), 0755)

	cfgSSIM := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state_ssim"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpegNoSSIM,
		FFprobe:         mockProbe,
	}
	workerSSIM := NewWorker(cfgSSIM)

	recordSSIM := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-no-ssim",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "ssim",
		Samples:         []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 10.0, DurationSeconds: 15.0}},
		Candidates:      []transcode.BenchmarkCandidate{{ID: "cand1", Quality: 60}},
		Attempt:         1,
	}

	errSSIM := runner.RunBenchmark(context.Background(), workerSSIM, recordSSIM)
	if errSSIM == nil {
		t.Fatalf("expected closed failure when ssim filter missing, got nil")
	}
	if !strings.Contains(errSSIM.Error(), "ssim") {
		t.Errorf("expected error mentioning ssim, got: %v", errSSIM)
	}
}

func TestProductionBenchmarkRunner_DeterministicOrder_CandidateXSample(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video content"), 0644)

	mockFFmpeg, mockProbe, logFile := setupMockTools(t, dir, sdr8BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-order",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 15.0},
			{Index: 1, StartSeconds: 40.0, DurationSeconds: 15.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_q60", Quality: 60},
			{ID: "cand_q75", Quality: 75},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark failed: %v", err)
	}

	// Verify MetricSamples: 2 candidates x 2 samples = 4 results
	evidence := record.Evidence
	if evidence == nil {
		t.Fatalf("expected record.Evidence to be non-nil")
	}
	if len(evidence.MetricSamples) != 4 {
		t.Fatalf("expected 4 MetricSamples, got %d", len(evidence.MetricSamples))
	}

	// Verify sequential deterministic order: cand0 sample0, cand0 sample1, cand1 sample0, cand1 sample1
	expectedSeq := []struct {
		candID    string
		sampleIdx int
	}{
		{"cand_q60", 0},
		{"cand_q60", 1},
		{"cand_q75", 0},
		{"cand_q75", 1},
	}
	for i, exp := range expectedSeq {
		m := evidence.MetricSamples[i]
		if m.CandidateID != exp.candID || m.SampleIndex != exp.sampleIdx {
			t.Errorf("MetricSamples[%d]: expected cand %s sample %d, got cand %s sample %d",
				i, exp.candID, exp.sampleIdx, m.CandidateID, m.SampleIndex)
		}
		if m.VMAF == nil || *m.VMAF <= 0 {
			t.Errorf("MetricSamples[%d]: expected valid VMAF score, got %v", i, m.VMAF)
		}
	}

	// Verify CandidateMetrics: 2 candidates
	if len(evidence.CandidateMetrics) != 2 {
		t.Fatalf("expected 2 CandidateMetrics, got %d", len(evidence.CandidateMetrics))
	}
	if evidence.CandidateMetrics[0].CandidateID != "cand_q60" || !evidence.CandidateMetrics[0].Aggregate.Valid {
		t.Errorf("expected valid aggregate for cand_q60, got %v", evidence.CandidateMetrics[0])
	}
	if evidence.CandidateMetrics[1].CandidateID != "cand_q75" || !evidence.CandidateMetrics[1].Aggregate.Valid {
		t.Errorf("expected valid aggregate for cand_q75, got %v", evidence.CandidateMetrics[1])
	}

	// Verify argv execution order in ffmpeg_calls.log
	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading log: %v", err)
	}
	var vmafCalls []string
	for _, line := range strings.Split(string(logBytes), "\n") {
		if strings.Contains(line, "libvmaf") {
			vmafCalls = append(vmafCalls, line)
		}
	}
	if len(vmafCalls) != 4 {
		t.Fatalf("expected 4 libvmaf execution lines, got %d", len(vmafCalls))
	}
	if !strings.Contains(vmafCalls[0], "cand_0") || !strings.Contains(vmafCalls[0], "sample_0") {
		t.Errorf("vmafCall[0] unexpected: %s", vmafCalls[0])
	}
	if !strings.Contains(vmafCalls[1], "cand_0") || !strings.Contains(vmafCalls[1], "sample_1") {
		t.Errorf("vmafCall[1] unexpected: %s", vmafCalls[1])
	}
	if !strings.Contains(vmafCalls[2], "cand_1") || !strings.Contains(vmafCalls[2], "sample_0") {
		t.Errorf("vmafCall[2] unexpected: %s", vmafCalls[2])
	}
	if !strings.Contains(vmafCalls[3], "cand_1") || !strings.Contains(vmafCalls[3], "sample_1") {
		t.Errorf("vmafCall[3] unexpected: %s", vmafCalls[3])
	}
}

func TestProductionBenchmarkRunner_SSIMMetric_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video content"), 0644)

	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-ssim",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "ssim",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 15.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_ssim", Quality: 65},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark with SSIM failed: %v", err)
	}

	if record.Evidence == nil {
		t.Fatalf("expected non-nil evidence")
	}
	if len(record.Evidence.MetricSamples) != 1 {
		t.Fatalf("expected 1 metric sample, got %d", len(record.Evidence.MetricSamples))
	}
	ms := record.Evidence.MetricSamples[0]
	if ms.SSIM == nil || *ms.SSIM <= 0 || *ms.SSIM > 1.0 {
		t.Errorf("expected valid SSIM score in [0, 1], got %v", ms.SSIM)
	}
	if len(record.Evidence.CandidateMetrics) != 1 {
		t.Fatalf("expected 1 CandidateMetrics, got %d", len(record.Evidence.CandidateMetrics))
	}
	cm := record.Evidence.CandidateMetrics[0]
	if cm.MetricType != optimization.MetricTypeSSIM || !cm.Aggregate.Valid {
		t.Errorf("expected valid SSIM aggregate, got %v", cm)
	}
}

func TestProductionBenchmarkRunner_SymlinkAttack_FailsClosed(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video content"), 0644)

	jobID := "bench-symlink"
	samplesDir := filepath.Join(dir, "state", "benchmarks", jobID, "samples")
	outsideSecret := filepath.Join(dir, "secret.txt")
	_ = os.WriteFile(outsideSecret, []byte("secret payload"), 0644)

	mockProbe := filepath.Join(dir, "mock_probe.sh")
	_ = os.WriteFile(mockProbe, []byte(fmt.Sprintf("#!/bin/sh\ncat << 'EOF'\n%s\nEOF\n", sdr8BitProbeJSON)), 0755)

	// Pre-create symlink target for the anticipated log file inside samplesDir during candidate encoding
	candID := "cand_target"
	h := sha256.Sum256([]byte(candID))
	shortHash := hex.EncodeToString(h[:4])
	logName := fmt.Sprintf("metric_vmaf_cand_0_%s_sample_0.json", shortHash)
	symlinkPath := filepath.Join(samplesDir, logName)

	// Mock FFmpeg that plants the symlink during candidate encode
	mockFFmpegSymlink := filepath.Join(dir, "mock_ffmpeg_symlink.sh")
	_ = os.WriteFile(mockFFmpegSymlink, []byte(fmt.Sprintf(`#!/bin/sh
case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1"
    exit 0
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -prio_speed        <boolean>    E..V....... Prioritize encoding speed (default false)
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
  -realtime          <boolean>    E..V....... Realtime (default false)
EOF
    exit 0
    ;;
  *"-encoders"*)
    echo "Encoders:\n V..... hevc_videotoolbox VideoToolbox H.265\n V..... ffv1 FFmpeg video codec #1"
    exit 0
    ;;
  *"-filters"*)
    echo "Filters:\n  .. libvmaf VV->V Calculate the VMAF\n  .. null V->V Pass the source"
    exit 0
    ;;
esac

out=""
for last; do out="$last"; done
if [ -n "$out" ] && [ "$out" != "-" ]; then
  mkdir -p "$(dirname "$out")"
  echo "fake" > "$out"
fi

# If encoding candidate, plant the symlink at metric log destination
case "$*" in
  *"hevc_videotoolbox"*)
    ln -sf %q %q
    ;;
esac
exit 0
`, outsideSecret, symlinkPath)), 0755)

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpegSymlink,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 15.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: candID, Quality: 60},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	// Under candidate failure isolation, a symlink log on a candidate marks that candidate
	// ineligible with an error rather than failing the entire benchmark job.
	err := runner.RunBenchmark(context.Background(), worker, record)
	if err != nil {
		t.Fatalf("unexpected benchmark job error under candidate-level failure semantics: %v", err)
	}

	if record.Evidence == nil || len(record.Evidence.CandidateMetrics) != 1 {
		t.Fatalf("expected evidence with candidate metrics to be persisted")
	}
	cm := record.Evidence.CandidateMetrics[0]
	if cm.Aggregate.Valid {
		t.Errorf("expected candidate aggregate to be invalid on symlink attack")
	}
	if cm.Aggregate.IneligibleReason != optimization.ReasonIncompleteSampleScores {
		t.Errorf("expected IneligibleReason=%s, got %s", optimization.ReasonIncompleteSampleScores, cm.Aggregate.IneligibleReason)
	}

	// Ensure readMetricLogFile itself fails closed on symlink
	if _, err := readMetricLogFile(symlinkPath); err == nil {
		t.Errorf("expected readMetricLogFile to fail on symlink, got nil")
	}

	// Ensure the outside file was NOT overwritten or truncated
	content, _ := os.ReadFile(outsideSecret)
	if string(content) != "secret payload" {
		t.Errorf("outside file was modified through symlink! Content: %s", string(content))
	}
}

func TestProductionBenchmarkRunner_OversizedLog_FailsClosed(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video content"), 0644)

	mockProbe := filepath.Join(dir, "mock_probe.sh")
	_ = os.WriteFile(mockProbe, []byte(fmt.Sprintf("#!/bin/sh\ncat << 'EOF'\n%s\nEOF\n", sdr8BitProbeJSON)), 0755)

	// Mock FFmpeg that writes a > 5 MB log file
	mockFFmpegOversized := filepath.Join(dir, "mock_ffmpeg_oversized.sh")
	_ = os.WriteFile(mockFFmpegOversized, []byte(`#!/bin/sh
case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1"
    exit 0
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -prio_speed        <boolean>    E..V....... Prioritize encoding speed (default false)
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
  -realtime          <boolean>    E..V....... Realtime (default false)
EOF
    exit 0
    ;;
  *"-encoders"*)
    echo "Encoders:\n V..... hevc_videotoolbox VideoToolbox H.265\n V..... ffv1 FFmpeg video codec #1"
    exit 0
    ;;
  *"-filters"*)
    echo "Filters:\n  .. libvmaf VV->V Calculate the VMAF\n  .. null V->V Pass the source"
    exit 0
    ;;
  *"-filter_complex"*"libvmaf"*)
    for arg in "$@"; do
      case "$arg" in
        *"log_path="*)
          lpath="${arg#*log_path=}"
          lpath="${lpath%%:*}"
          mkdir -p "$(dirname "$lpath")"
          # Write 6 MB of data
          head -c 6000000 /dev/zero > "$lpath"
          ;;
      esac
    done
    exit 0
    ;;
esac
out=""
for last; do out="$last"; done
if [ -n "$out" ] && [ "$out" != "-" ]; then
  mkdir -p "$(dirname "$out")"
  echo "fake" > "$out"
fi
exit 0
`), 0755)

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpegOversized,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-oversized",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 15.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_over", Quality: 60},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	// Under candidate failure isolation, an oversized log marks that candidate
	// ineligible rather than failing the entire benchmark job.
	err := runner.RunBenchmark(context.Background(), worker, record)
	if err != nil {
		t.Fatalf("unexpected benchmark job error under candidate-level failure semantics: %v", err)
	}

	if record.Evidence == nil || len(record.Evidence.CandidateMetrics) != 1 {
		t.Fatalf("expected evidence with candidate metrics to be persisted")
	}
	cm := record.Evidence.CandidateMetrics[0]
	if cm.Aggregate.Valid {
		t.Errorf("expected candidate aggregate to be invalid on oversized log")
	}
	if cm.Aggregate.IneligibleReason != optimization.ReasonIncompleteSampleScores {
		t.Errorf("expected IneligibleReason=%s, got %s", optimization.ReasonIncompleteSampleScores, cm.Aggregate.IneligibleReason)
	}
	if len(record.Evidence.MetricSamples) != 1 || !strings.Contains(record.Evidence.MetricSamples[0].Error, "exceeds maximum allowed") {
		t.Errorf("expected metric sample error mentioning exceeds maximum allowed, got %v", record.Evidence.MetricSamples)
	}
}

func TestProductionBenchmarkRunner_MediaUntouchedDuringMetrics(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source_untouched.mkv")
	sourceBytes := []byte("source data to verify immutability throughout benchmark run")
	_ = os.WriteFile(sourceFile, sourceBytes, 0644)
	initialSourceHash, _ := fileSHA256(sourceFile)

	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-immutable",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "c1", Quality: 60},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark failed: %v", err)
	}

	finalSourceHash, _ := fileSHA256(sourceFile)
	if initialSourceHash != finalSourceHash {
		t.Fatalf("source file was modified! initial: %s, final: %s", initialSourceHash, finalSourceHash)
	}
}

func TestProductionBenchmarkRunner_10BitSource_VMAFFailsClosed(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source_10bit.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake 10-bit video content"), 0644)

	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr10BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	// VMAF on 10-bit source must fail closed because worker capabilities do not assert 10-bit VMAF
	recordVMAF := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-10bit-vmaf-fail",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_10bit", Quality: 60},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	err := runner.RunBenchmark(context.Background(), worker, recordVMAF)
	if err == nil {
		t.Fatalf("expected closed failure for 10-bit VMAF without capability assertion, got nil")
	}
	if !strings.Contains(err.Error(), "10-bit VMAF capability") {
		t.Errorf("expected error mentioning 10-bit VMAF capability, got: %v", err)
	}

	// 'both' on 10-bit source must also fail closed
	recordBoth := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-10bit-both-fail",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "both",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_10bit", Quality: 60},
		},
		Attempt: 1,
	}
	errBoth := runner.RunBenchmark(context.Background(), worker, recordBoth)
	if errBoth == nil {
		t.Fatalf("expected closed failure for 10-bit metric='both', got nil")
	}
	if !strings.Contains(errBoth.Error(), "10-bit VMAF capability") {
		t.Errorf("expected error mentioning 10-bit VMAF capability, got: %v", errBoth)
	}
}

func TestProductionBenchmarkRunner_10BitSource_SSIMSucceeds(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source_10bit.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake 10-bit video content"), 0644)

	mockFFmpeg, mockProbe, logFile := setupMockTools(t, dir, sdr10BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	// SSIM on 10-bit source succeeds because native FFmpeg ssim filter supports yuv420p10le safely
	recordSSIM := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-10bit-ssim",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "ssim",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_10bit", Quality: 60},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	if err := runner.RunBenchmark(context.Background(), worker, recordSSIM); err != nil {
		t.Fatalf("RunBenchmark failed for 10-bit SSIM: %v", err)
	}

	if recordSSIM.Evidence == nil {
		t.Fatalf("expected record.Evidence to be non-nil")
	}
	if recordSSIM.Evidence.SourceBitDepth != 10 {
		t.Errorf("expected source bit depth 10, got %d", recordSSIM.Evidence.SourceBitDepth)
	}
	if len(recordSSIM.Evidence.MetricSamples) != 1 {
		t.Fatalf("expected 1 metric sample result, got %d", len(recordSSIM.Evidence.MetricSamples))
	}
	ms := recordSSIM.Evidence.MetricSamples[0]
	if ms.SSIM == nil || *ms.SSIM <= 0 || *ms.SSIM > 1.0 {
		t.Errorf("expected valid SSIM score for 10-bit, got %v", ms.SSIM)
	}

	// Verify log contains exact 10-bit extraction, encoding, and ssim calls without downsampling
	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading log: %v", err)
	}
	logStr := string(logBytes)
	if !strings.Contains(logStr, "-c:v ffv1") {
		t.Errorf("expected reference extraction to use ffv1 lossless master")
	}
	if !strings.Contains(logStr, "main10") || !strings.Contains(logStr, "p010le") {
		t.Errorf("expected candidate encoding to use main10 and p010le")
	}
	if !strings.Contains(logStr, "ssim") {
		t.Errorf("expected ssim to be executed")
	}
	if strings.Contains(logStr, "libvmaf") {
		t.Errorf("libvmaf should not be executed when metric is ssim")
	}
}

func TestProductionBenchmarkRunner_CandidateLevelFailure_Isolation(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video content"), 0644)

	// Simulate failure only on candidate 0 during libvmaf metric calculation
	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "cand_0*libvmaf")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-cand-isolation",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_0", Quality: 60},
			{ID: "cand_1", Quality: 70},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	// RunBenchmark MUST NOT fail the entire job when one candidate fails metric measurement
	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark should not fail whole job on candidate-level metric failure: %v", err)
	}

	evidence := record.Evidence
	if evidence == nil {
		t.Fatalf("expected evidence to be persisted")
	}
	if len(evidence.CandidateMetrics) != 2 {
		t.Fatalf("expected 2 candidate metrics, got %d", len(evidence.CandidateMetrics))
	}

	// Candidate 0 must be marked ineligible with reason ReasonIncompleteSampleScores
	cm0 := evidence.CandidateMetrics[0]
	if cm0.CandidateID != "cand_0" {
		t.Errorf("expected cand_0 at index 0, got %s", cm0.CandidateID)
	}
	if cm0.Aggregate.Valid {
		t.Errorf("candidate 0 aggregate should be invalid")
	}
	if cm0.Aggregate.IneligibleReason != optimization.ReasonIncompleteSampleScores {
		t.Errorf("candidate 0 ineligible reason expected %s, got %s", optimization.ReasonIncompleteSampleScores, cm0.Aggregate.IneligibleReason)
	}

	// Candidate 1 must be eligible with Valid: true and valid score
	cm1 := evidence.CandidateMetrics[1]
	if cm1.CandidateID != "cand_1" {
		t.Errorf("expected cand_1 at index 1, got %s", cm1.CandidateID)
	}
	if !cm1.Aggregate.Valid {
		t.Errorf("candidate 1 aggregate should be valid, got reason %s", cm1.Aggregate.IneligibleReason)
	}
	if cm1.Aggregate.MeanScore <= 0 {
		t.Errorf("candidate 1 mean score should be positive, got %v", cm1.Aggregate.MeanScore)
	}
}

func TestProductionBenchmarkRunner_CandidateLevelFailure_AllCandidatesFail(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video content"), 0644)

	// All libvmaf metric commands fail
	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "libvmaf")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-all-cand-fail",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_0", Quality: 60},
			{ID: "cand_1", Quality: 70},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	// Entire job succeeds, but all candidates are ineligible for Phase 6 selection
	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark should complete without job failure: %v", err)
	}

	evidence := record.Evidence
	if evidence == nil {
		t.Fatalf("expected evidence to be persisted")
	}
	for i, cm := range evidence.CandidateMetrics {
		if cm.Aggregate.Valid {
			t.Errorf("candidate %d aggregate should be invalid", i)
		}
	}
}

func TestProductionBenchmarkRunner_MetricBoth_DualDeterministicAggregates(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video content"), 0644)

	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-metric-both",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "both",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
			{Index: 1, StartSeconds: 20.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_0", Quality: 60},
			{ID: "cand_1", Quality: 70},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark failed for metric='both': %v", err)
	}

	evidence := record.Evidence
	if evidence == nil {
		t.Fatalf("expected non-nil evidence")
	}

	// 2 candidates * 2 metrics (vmaf, ssim) = 4 CandidateMetrics entries in deterministic order:
	// cand_0 VMAF, cand_0 SSIM, cand_1 VMAF, cand_1 SSIM
	if len(evidence.CandidateMetrics) != 4 {
		t.Fatalf("expected 4 CandidateMetrics entries for 2 candidates with metric='both', got %d", len(evidence.CandidateMetrics))
	}

	expectedMetrics := []struct {
		candID string
		metric optimization.MetricType
	}{
		{"cand_0", optimization.MetricTypeVMAF},
		{"cand_0", optimization.MetricTypeSSIM},
		{"cand_1", optimization.MetricTypeVMAF},
		{"cand_1", optimization.MetricTypeSSIM},
	}

	for i, em := range expectedMetrics {
		cm := evidence.CandidateMetrics[i]
		if cm.CandidateID != em.candID {
			t.Errorf("entry %d: expected candidate %s, got %s", i, em.candID, cm.CandidateID)
		}
		if cm.MetricType != em.metric {
			t.Errorf("entry %d: expected metric type %s, got %s", i, em.metric, cm.MetricType)
		}
		if !cm.Aggregate.Valid {
			t.Errorf("entry %d (%s %s): expected valid aggregate, got invalid: %s", i, em.candID, em.metric, cm.Aggregate.IneligibleReason)
		}
		if len(cm.Aggregate.SampleScores) != 2 {
			t.Errorf("entry %d: expected 2 samples aggregated, got %d", i, len(cm.Aggregate.SampleScores))
		}
	}
}

func TestProductionBenchmarkRunner_MetricCancellation_PreservesPartialEvidence(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video content"), 0644)

	ctx, cancel := context.WithCancel(context.Background())

	mockProbe := filepath.Join(dir, "mock_probe.sh")
	_ = os.WriteFile(mockProbe, []byte(fmt.Sprintf("#!/bin/sh\ncat << 'EOF'\n%s\nEOF\n", sdr8BitProbeJSON)), 0755)

	cancelTriggerFile := filepath.Join(dir, "cancel_triggered")
	mockFFmpegCancel := filepath.Join(dir, "mock_ffmpeg_cancel.sh")
	_ = os.WriteFile(mockFFmpegCancel, []byte(fmt.Sprintf(`#!/bin/sh
case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1"
    exit 0
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -prio_speed        <boolean>    E..V....... Prioritize encoding speed (default false)
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
  -realtime          <boolean>    E..V....... Realtime (default false)
EOF
    exit 0
    ;;
  *"-encoders"*)
    echo "Encoders:\n V..... hevc_videotoolbox VideoToolbox H.265\n V..... ffv1 FFmpeg video codec #1"
    exit 0
    ;;
  *"-filters"*)
    echo "Filters:\n  .. libvmaf VV->V Calculate the VMAF\n  .. null V->V Pass the source"
    exit 0
    ;;
  *"-filter_complex"*"libvmaf"*)
    # If this is candidate 1 sample 0, signal cancel and sleep until killed
    case "$*" in
      *"cand_1"*)
        touch %q
        sleep 10
        exit 0
        ;;
    esac
    for arg in "$@"; do
      case "$arg" in
        *"log_path="*)
          lpath="${arg#*log_path=}"
          lpath="${lpath%%:*}"
          mkdir -p "$(dirname "$lpath")"
          cat << 'VMAF_EOF' > "$lpath"
{
  "version": "2.3.1",
  "pooled_metrics": {
    "vmaf": {
      "mean": 95.500000
    }
  }
}
VMAF_EOF
          ;;
      esac
    done
    exit 0
    ;;
esac

out=""
for last; do out="$last"; done
if [ -n "$out" ] && [ "$out" != "-" ]; then
  mkdir -p "$(dirname "$out")"
  echo "fake" > "$out"
fi
exit 0
`, cancelTriggerFile)), 0755)

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpegCancel,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-cancel-metrics",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_0", Quality: 60},
			{ID: "cand_1", Quality: 70},
		},
		Attempt: 1,
	}

	// Goroutine that monitors for cancel trigger
	go func() {
		for i := 0; i < 50; i++ {
			time.Sleep(50 * time.Millisecond)
			if _, err := os.Stat(cancelTriggerFile); err == nil {
				cancel()
				return
			}
		}
	}()

	runner := &ProductionBenchmarkRunner{}
	err := runner.RunBenchmark(ctx, worker, record)
	if err == nil {
		t.Fatalf("expected error on cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("expected context.Canceled error, got: %v", err)
	}

	// Verify partial evidence is preserved:
	// cand_0 sample 0 metric must be preserved in MetricSamples!
	evidence := record.Evidence
	if evidence == nil {
		t.Fatalf("expected evidence to be preserved")
	}
	if len(evidence.MetricSamples) < 1 {
		t.Fatalf("expected at least 1 metric sample in partial evidence, got %d", len(evidence.MetricSamples))
	}
	if evidence.MetricSamples[0].CandidateID != "cand_0" {
		t.Errorf("expected cand_0 in partial evidence, got %s", evidence.MetricSamples[0].CandidateID)
	}
	if evidence.MetricSamples[0].VMAF == nil || *evidence.MetricSamples[0].VMAF != 95.5 {
		t.Errorf("expected VMAF 95.5 in partial evidence, got %v", evidence.MetricSamples[0].VMAF)
	}
}

func TestBenchmarkRunner_ProgressAdvancesDeterministicallyAndCapsBelow100(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	sourceContent := make([]byte, 1024*1024)
	if err := os.WriteFile(sourceFile, sourceContent, 0644); err != nil {
		t.Fatalf("writing source file: %v", err)
	}

	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "")

	stateDir := filepath.Join(dir, "state")
	jobID := "bench-progress-units"
	jobDir := filepath.Join(stateDir, jobID)
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	token := "token-prog-runner"
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		RunToken:        token,
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 15.0},
			{Index: 1, StartSeconds: 50.0, DurationSeconds: 15.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_q60", Quality: 60},
			{ID: "cand_q70", Quality: 70},
		},
		Attempt: 1,
	}
	benchFile := filepath.Join(jobDir, "benchmark.json")
	if err := SaveBenchmarkAtomic(benchFile, record); err != nil {
		t.Fatalf("saving initial benchmark: %v", err)
	}

	runner := &ProductionBenchmarkRunner{}
	ctx := context.Background()

	if err := runner.RunBenchmark(ctx, worker, record); err != nil {
		t.Fatalf("RunBenchmark failed: %v", err)
	}

	// Work units calculation:
	// 2 samples reference extraction = 2 units
	// 2 candidates * 2 samples encodes = 4 units
	// 2 candidates * 2 samples VMAF passes = 4 units
	// Total = 10 units.
	// When all 10 complete, 10/10 = 100%, but must be capped below 100 (99.0%).
	if record.Progress < 90.0 {
		t.Errorf("expected progress to advance across all work units, got %v", record.Progress)
	}
	if record.Progress >= 100.0 {
		t.Errorf("running benchmark progress must remain <100 until terminal completed state, got %v", record.Progress)
	}
	if record.Progress != 99.0 {
		t.Errorf("expected capped progress 99.0 after all work units complete, got %v", record.Progress)
	}
	if record.Phase != "selecting_candidate" {
		t.Errorf("expected final runner phase 'selecting_candidate', got %q", record.Phase)
	}

	// Verify benchmark.json on disk was also updated with monotonic progress < 100
	onDisk, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("loading benchmark from disk: %v", err)
	}
	if onDisk.Progress != 99.0 {
		t.Errorf("expected on-disk progress to be 99.0, got %v", onDisk.Progress)
	}
	if onDisk.Phase != "selecting_candidate" {
		t.Errorf("expected on-disk phase 'selecting_candidate', got %q", onDisk.Phase)
	}
	if onDisk.HeartbeatAt.IsZero() {
		t.Errorf("expected on-disk HeartbeatAt to be set")
	}
}

func setupMockToolsWithCustomVMAF(t *testing.T, dir string, probeJSON string, vmafPayload string, ffmpegFailPattern string) (string, string, string) {
	t.Helper()

	mockProbe := filepath.Join(dir, "mock_ffprobe.sh")
	probeScript := fmt.Sprintf(`#!/bin/sh
cat << 'EOF'
%s
EOF
`, probeJSON)
	if err := os.WriteFile(mockProbe, []byte(probeScript), 0755); err != nil {
		t.Fatalf("writing mock ffprobe: %v", err)
	}

	logFile := filepath.Join(dir, "ffmpeg_calls.log")
	mockFFmpeg := filepath.Join(dir, "mock_ffmpeg.sh")
	ffmpegScript := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q

case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1 Copyright (c) 2000-2024 the FFmpeg developers"
    exit 0
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -prio_speed        <boolean>    E..V....... Prioritize encoding speed (default false)
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
  -realtime          <boolean>    E..V....... Realtime (default false)
EOF
    exit 0
    ;;
  *"-encoders"*)
    cat << 'EOF'
Encoders:
 V..... hevc_videotoolbox    VideoToolbox H.265
 V..... ffv1                 FFmpeg video codec #1
EOF
    exit 0
    ;;
  *"-filters"*)
    cat << 'EOF'
Filters:
  .. libvmaf           VV->V      Calculate the VMAF between two video streams.
  TS ssim              VV->V      Calculate the SSIM between two video streams.
  .S scale             V->V       Scale the input video size and/or convert the image format.
  .. format            V->V       Convert the input video to one of the specified pixel formats.
  .. null              V->V       Pass the source unchanged to the output.
  .. fps               V->V       Force constant framerate.
EOF
    exit 0
    ;;
esac

# Check for simulated failure pattern
if [ -n %q ]; then
  case "$*" in
    *%s*)
      echo "simulated ffmpeg failure for: $*" >&2
      exit 1
      ;;
  esac
fi

case "$*" in
  *"-filter_complex"*"libvmaf"*)
    for arg in "$@"; do
      case "$arg" in
        *"log_path="*)
          lpath="${arg#*log_path=}"
          lpath="${lpath%%%%:*}"
          mkdir -p "$(dirname "$lpath")"
          cat << 'VMAF_EOF' > "$lpath"
%s
VMAF_EOF
          ;;
      esac
    done
    exit 0
    ;;
  *"-filter_complex"*"ssim"*)
    for arg in "$@"; do
      case "$arg" in
        *"stats_file="*)
          spath="${arg#*stats_file=}"
          spath="${spath%%%%:*}"
          mkdir -p "$(dirname "$spath")"
          cat << 'SSIM_EOF' > "$spath"
n:1 Y:0.980000 U:0.985000 V:0.985000 All:0.980000 (16.99)
SSIM_EOF
          ;;
      esac
    done
    echo "[Parsed_ssim_0] SSIM Y:0.980000 U:0.985000 V:0.985000 All:0.980000 (16.99)" >&2
    exit 0
    ;;
esac

# Find output file (last argument)
out=""
for last; do out="$last"; done

case "$out" in
  -*|"")
    # Not an output file (flag or empty)
    ;;
  *)
    # Write fake dummy media data so os.Stat sees non-zero size
    mkdir -p "$(dirname "$out")"
    echo "fake media data payload for $out" > "$out"
    ;;
esac
exit 0
`, logFile, ffmpegFailPattern, ffmpegFailPattern, vmafPayload)

	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("writing mock ffmpeg: %v", err)
	}

	return mockFFmpeg, mockProbe, logFile
}

func TestBenchmarkProgressReporter_ResolveAndSkipUnits(t *testing.T) {
	record := &BenchmarkRecord{
		ID:       "bench-test-rep",
		RunToken: "token-rep",
	}
	reporter := newBenchmarkProgressReporter(nil, record, 5)

	if reporter.TotalUnits() != 5 {
		t.Errorf("expected total units 5, got %d", reporter.TotalUnits())
	}
	if reporter.CompletedUnits() != 0 {
		t.Errorf("expected completed units 0, got %d", reporter.CompletedUnits())
	}

	// Start a unit
	reporter.StartUnit("extracting_samples")
	if record.Phase != "extracting_samples" {
		t.Errorf("expected phase extracting_samples, got %s", record.Phase)
	}

	// Resolve 1 unit
	reporter.ResolveUnit()
	if reporter.CompletedUnits() != 1 {
		t.Errorf("expected completed units 1, got %d", reporter.CompletedUnits())
	}
	// 1/5 = 20.0%
	if record.Progress != 20.0 {
		t.Errorf("expected progress 20.0, got %v", record.Progress)
	}

	// CompleteUnit is alias for ResolveUnits(1)
	reporter.CompleteUnit()
	if reporter.CompletedUnits() != 2 {
		t.Errorf("expected completed units 2, got %d", reporter.CompletedUnits())
	}
	// 2/5 = 40.0%
	if record.Progress != 40.0 {
		t.Errorf("expected progress 40.0, got %v", record.Progress)
	}

	// Skip 2 units
	reporter.SkipUnits(2)
	if reporter.CompletedUnits() != 4 {
		t.Errorf("expected completed units 4, got %d", reporter.CompletedUnits())
	}
	// 4/5 = 80.0%
	if record.Progress != 80.0 {
		t.Errorf("expected progress 80.0, got %v", record.Progress)
	}

	// SkipUnits with 0 or negative does nothing
	reporter.SkipUnits(0)
	reporter.SkipUnits(-1)
	if reporter.CompletedUnits() != 4 {
		t.Errorf("expected completed units unchanged at 4, got %d", reporter.CompletedUnits())
	}

	// Resolve remaining unit (5/5 = 100%, capped at 99.0%)
	reporter.ResolveUnit()
	if reporter.CompletedUnits() != 5 {
		t.Errorf("expected completed units 5, got %d", reporter.CompletedUnits())
	}
	if record.Progress != 99.0 {
		t.Errorf("expected progress capped at 99.0, got %v", record.Progress)
	}

	// Further resolutions stay capped at 99.0 and progress does not decrease
	reporter.ResolveUnits(2)
	if record.Progress != 99.0 {
		t.Errorf("expected progress to remain capped at 99.0, got %v", record.Progress)
	}
}

func TestBenchmarkRunner_ProgressResolvesUnitWhenVMAFParseFails(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(sourceFile, make([]byte, 1024*1024), 0644); err != nil {
		t.Fatalf("writing source file: %v", err)
	}

	// Corrupt JSON payload: FFmpeg succeeds (exit 0) but ParseVMAFJSON fails
	corruptedVMAF := `{"broken_json": [ unclosed`
	mockFFmpeg, mockProbe, _ := setupMockToolsWithCustomVMAF(t, dir, sdr8BitProbeJSON, corruptedVMAF, "")

	stateDir := filepath.Join(dir, "state")
	jobID := "bench-vmaf-parse-fail"
	jobDir := filepath.Join(stateDir, jobID)
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	token := "token-vmaf-parse-fail"
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		RunToken:        token,
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 15.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_q60", Quality: 60},
		},
		Attempt: 1,
	}
	benchFile := filepath.Join(jobDir, "benchmark.json")
	if err := SaveBenchmarkAtomic(benchFile, record); err != nil {
		t.Fatalf("saving initial benchmark: %v", err)
	}

	runner := &ProductionBenchmarkRunner{}
	ctx := context.Background()

	// RunBenchmark must not fail the job on candidate-level metric failure
	if err := runner.RunBenchmark(ctx, worker, record); err != nil {
		t.Fatalf("RunBenchmark failed: %v", err)
	}

	// Total planned work units:
	// 1 sample ref extraction = 1 unit
	// 1 candidate sample encode = 1 unit
	// 1 candidate sample VMAF pass = 1 unit
	// Total = 3 units.
	// Even though VMAF log parse failed, the FFmpeg invocation was completed and its planned unit resolved.
	// 3/3 resolved -> running progress advances to 99.0% (capped below 100).
	if record.Progress != 99.0 {
		t.Errorf("expected running progress 99.0 when failed VMAF parse unit is resolved, got %v", record.Progress)
	}
	if record.Phase != "selecting_candidate" {
		t.Errorf("expected phase 'selecting_candidate', got %q", record.Phase)
	}

	// Verify on-disk benchmark.json
	onDisk, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("loading benchmark from disk: %v", err)
	}
	if onDisk.Progress != 99.0 {
		t.Errorf("expected on-disk progress 99.0, got %v", onDisk.Progress)
	}

	// Verify failure/evidence semantics remain unchanged
	evidence := record.Evidence
	if evidence == nil {
		t.Fatalf("expected evidence to be recorded")
	}
	if len(evidence.CandidateMetrics) != 1 {
		t.Fatalf("expected 1 candidate metric aggregate, got %d", len(evidence.CandidateMetrics))
	}
	cm := evidence.CandidateMetrics[0]
	if cm.CandidateID != "cand_q60" {
		t.Errorf("expected candidate cand_q60, got %s", cm.CandidateID)
	}
	if cm.Aggregate.Valid {
		t.Errorf("expected candidate aggregate to be invalid due to parse failure")
	}
	if cm.Aggregate.IneligibleReason != optimization.ReasonIncompleteSampleScores {
		t.Errorf("expected ineligible reason %s, got %s", optimization.ReasonIncompleteSampleScores, cm.Aggregate.IneligibleReason)
	}
	if len(evidence.MetricSamples) != 1 {
		t.Fatalf("expected 1 metric sample, got %d", len(evidence.MetricSamples))
	}
	if !strings.Contains(evidence.MetricSamples[0].Error, "parse vmaf log") {
		t.Errorf("expected metric sample error to mention 'parse vmaf log', got %q", evidence.MetricSamples[0].Error)
	}
}

func TestBenchmarkRunner_ProgressResolvesBothUnitsWhenVMAFFailsInBothMetric(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(sourceFile, make([]byte, 1024*1024), 0644); err != nil {
		t.Fatalf("writing source file: %v", err)
	}

	// Simulate libvmaf subprocess failure (exit 1)
	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "libvmaf")

	stateDir := filepath.Join(dir, "state")
	jobID := "bench-both-metric-skip"
	jobDir := filepath.Join(stateDir, jobID)
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfg := &WorkerConfig{
		StateDir:        stateDir,
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	token := "token-both-metric-skip"
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              jobID,
		Status:          "running",
		Source:          sourceFile,
		Metric:          "both",
		RunToken:        token,
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 15.0},
			{Index: 1, StartSeconds: 40.0, DurationSeconds: 15.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_q60", Quality: 60},
		},
		Attempt: 1,
	}
	benchFile := filepath.Join(jobDir, "benchmark.json")
	if err := SaveBenchmarkAtomic(benchFile, record); err != nil {
		t.Fatalf("saving initial benchmark: %v", err)
	}

	runner := &ProductionBenchmarkRunner{}
	ctx := context.Background()

	// RunBenchmark must not fail the job on candidate-level metric failure
	if err := runner.RunBenchmark(ctx, worker, record); err != nil {
		t.Fatalf("RunBenchmark failed: %v", err)
	}

	// Planned work units calculation:
	// 2 samples reference extraction = 2 units
	// 1 candidate * 2 samples encodes = 2 units
	// 1 candidate * 2 samples * 2 passes (VMAF + SSIM) = 4 metric units
	// Total = 8 units.
	// For sample 0:
	// - VMAF FFmpeg fails -> resolved as 1 unit.
	// - SSIM is skipped due to VMAF failure -> resolved as 1 unit.
	// For sample 1:
	// - Candidate is already failed -> both planned passes (VMAF + SSIM) are skipped -> resolved as 2 units.
	// All 8 units are resolved. Denominator does NOT stall.
	// Progress reaches 99.0% (capped below 100).
	if record.Progress != 99.0 {
		t.Errorf("expected running progress 99.0 with all units resolved in metric='both', got %v", record.Progress)
	}
	if record.Phase != "selecting_candidate" {
		t.Errorf("expected final phase 'selecting_candidate', got %q", record.Phase)
	}

	// Verify on-disk benchmark.json
	onDisk, err := LoadBenchmark(benchFile)
	if err != nil {
		t.Fatalf("loading benchmark from disk: %v", err)
	}
	if onDisk.Progress != 99.0 {
		t.Errorf("expected on-disk progress 99.0, got %v", onDisk.Progress)
	}

	// Verify failure/evidence semantics remain unchanged
	evidence := record.Evidence
	if evidence == nil {
		t.Fatalf("expected evidence to be recorded")
	}
	// Both VMAF and SSIM aggregates exist in deterministic order
	if len(evidence.CandidateMetrics) != 2 {
		t.Fatalf("expected 2 candidate metric aggregates (VMAF + SSIM), got %d", len(evidence.CandidateMetrics))
	}
	vmafAgg := evidence.CandidateMetrics[0]
	if vmafAgg.MetricType != optimization.MetricTypeVMAF {
		t.Errorf("expected first aggregate to be VMAF, got %v", vmafAgg.MetricType)
	}
	if vmafAgg.Aggregate.Valid {
		t.Errorf("expected VMAF aggregate to be invalid")
	}
	if vmafAgg.Aggregate.IneligibleReason != optimization.ReasonIncompleteSampleScores {
		t.Errorf("expected VMAF IneligibleReason %s, got %s", optimization.ReasonIncompleteSampleScores, vmafAgg.Aggregate.IneligibleReason)
	}

	ssimAgg := evidence.CandidateMetrics[1]
	if ssimAgg.MetricType != optimization.MetricTypeSSIM {
		t.Errorf("expected second aggregate to be SSIM, got %v", ssimAgg.MetricType)
	}
	if ssimAgg.Aggregate.Valid {
		t.Errorf("expected SSIM aggregate to be invalid")
	}
	if ssimAgg.Aggregate.IneligibleReason != optimization.ReasonIncompleteSampleScores {
		t.Errorf("expected SSIM IneligibleReason %s, got %s", optimization.ReasonIncompleteSampleScores, ssimAgg.Aggregate.IneligibleReason)
	}

	// Both samples recorded in evidence.MetricSamples
	if len(evidence.MetricSamples) != 2 {
		t.Fatalf("expected 2 metric sample records, got %d", len(evidence.MetricSamples))
	}
	if !strings.Contains(evidence.MetricSamples[0].Error, "ffmpeg vmaf failed") {
		t.Errorf("expected sample 0 error to mention ffmpeg vmaf failed, got %q", evidence.MetricSamples[0].Error)
	}
	if !strings.Contains(evidence.MetricSamples[1].Error, "ffmpeg vmaf failed") {
		t.Errorf("expected sample 1 error to carry candidate failure reason, got %q", evidence.MetricSamples[1].Error)
	}
}
