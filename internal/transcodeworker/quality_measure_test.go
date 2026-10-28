package transcodeworker

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildReferenceExtractionArgs_ArgvExactness(t *testing.T) {
	sourcePath := "/media/movies/movie.mkv"
	refPath := "/state/bench-123/samples/ref_sample_0.mkv"
	firstFrameSec := 0.5
	fromSec, toSec := 12.5, 32.5
	videoIndex := 0

	args := BuildReferenceExtractionArgs(sourcePath, refPath, videoIndex, firstFrameSec, fromSec, toSec)

	expected := []string{
		"-y",
		"-nostats",
		"-copyts",
		"-noaccurate_seek",
		"-seek_timestamp", "1",
		"-ss", "13.000000",
		"-i", sourcePath,
		"-map", "0:0",
		"-filter:v:0", "settb=AVTB,trim=start=13.000000:end=33.000000",
		"-avoid_negative_ts", "make_zero",
		"-c:v", "ffv1",
		"-fps_mode:v:0", "passthrough",
		"-enc_time_base:v:0", "demux",
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

func TestBuildSSIMArgs_ArgvExactness(t *testing.T) {
	// Index pairing without a format conversion keeps SSIM at native precision.
	const ssimPairInputs = "[0:v]settb=AVTB,setpts=N[d];[1:v]settb=AVTB,setpts=N[r];[d][r]"
	candPath := "/tmp/test_workspace/cand_sample_0.mkv"
	refPath := "/tmp/test_workspace/ref_sample_0.mkv"
	statsPath := "/tmp/test_workspace/ssim.log"

	args := BuildSSIMArgs(candPath, refPath, statsPath)
	expected := []string{
		"-nostats",
		"-i", candPath,
		"-i", refPath,
		"-filter_complex", ssimPairInputs + "ssim=stats_file=" + statsPath,
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
	if argsNoStats[6] != ssimPairInputs+"ssim" {
		t.Errorf("expected filter string %q, got %q", ssimPairInputs+"ssim", argsNoStats[6])
	}

	// With special characters in stats path
	statsPathSpecial := "/tmp/with:colon/and'quote/and\\backslash/stats.log"
	argsSpecial := BuildSSIMArgs(candPath, refPath, statsPathSpecial)
	expectedFilterSpecial := ssimPairInputs + "ssim=stats_file=/tmp/with\\\\:colon/and\\\\\\'quote/and\\\\\\\\backslash/stats.log"
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
