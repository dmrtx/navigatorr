package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
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

# Find output file (last argument)
out=""
for last; do out="$last"; done

if [ -n "$out" ]; then
  # Write fake dummy media data so os.Stat sees non-zero size
  mkdir -p "$(dirname "$out")"
  echo "fake media data payload for $out" > "$out"
fi
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
		Metric:          "vmaf",
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
