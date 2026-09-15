package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

// setupMockToolsWithQualityVMAF creates mock ffprobe/ffmpeg where libvmaf returns a
// per-quality score selected from the candidate filename (_q<N> marker) and encoded
// sample sizes scale with quality (higher q => larger file) so selector size
// ordering is deterministic. Qualities missing from vmafByQuality default to 95.5.
func setupMockToolsWithQualityVMAF(t *testing.T, dir string, probeJSON string, vmafByQuality map[int]float64) (string, string, string) {
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

	// Build per-quality VMAF case branches.
	var vmafCases strings.Builder
	qualities := make([]int, 0, len(vmafByQuality))
	for q := range vmafByQuality {
		qualities = append(qualities, q)
	}
	sort.Ints(qualities)
	for _, q := range qualities {
		fmt.Fprintf(&vmafCases, "      *_q%d_*)\n        score=\"%.4f\"\n        ;;\n", q, vmafByQuality[q])
	}

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

case "$*" in
  *"-filter_complex"*"libvmaf"*)
    score="95.5000"
    case "$*" in
%s      *)
        ;;
    esac
    for arg in "$@"; do
      case "$arg" in
        *"log_path="*)
          lpath="${arg#*log_path=}"
          lpath="${lpath%%:*}"
          mkdir -p "$(dirname "$lpath")"
          cat << VMAF_EOF > "$lpath"
{
  "version": "2.3.1",
  "pooled_metrics": {
    "vmaf": {
      "mean": $score
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

case "$out" in
  -*|"")
    ;;
  *)
    mkdir -p "$(dirname "$out")"
    qval=""
    case "$*" in
      *"-q:v 55"*) qval=55 ;;
      *"-q:v 60"*) qval=60 ;;
      *"-q:v 65"*) qval=65 ;;
      *"-q:v 67"*) qval=67 ;;
      *"-q:v 68"*) qval=68 ;;
      *"-q:v 70"*) qval=70 ;;
      *"-q:v 75"*) qval=75 ;;
    esac
    if [ -n "$qval" ]; then
      head -c $((qval * 100)) /dev/zero > "$out"
    else
      echo "fake media data payload for $out" > "$out"
    fi
    ;;
esac
exit 0
`, logFile, vmafCases.String())

	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("writing mock ffmpeg: %v", err)
	}

	return mockFFmpeg, mockProbe, logFile
}

func adaptiveTestCandidates7() []transcode.BenchmarkCandidate {
	return []transcode.BenchmarkCandidate{
		{ID: "cand_q55", Quality: 55},
		{ID: "cand_q60", Quality: 60},
		{ID: "cand_q65", Quality: 65},
		{ID: "cand_q67", Quality: 67},
		{ID: "cand_q68", Quality: 68},
		{ID: "cand_q70", Quality: 70},
		{ID: "cand_q75", Quality: 75},
	}
}

func adaptiveTestScoresTypical() map[int]float64 {
	return map[int]float64{
		55: 94.0,
		60: 95.0,
		65: 96.5,
		67: 96.6,
		68: 96.7,
		70: 96.75,
		75: 96.8,
	}
}

func runAdaptiveBenchmarkForTest(t *testing.T, adaptive *transcode.BenchmarkAdaptiveConfig, vmafByQuality map[int]float64) (*BenchmarkRecord, string) {
	t.Helper()
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(sourceFile, []byte("fake source media for adaptive test"), 0644); err != nil {
		t.Fatalf("writing source: %v", err)
	}
	mockFFmpeg, mockProbe, logFile := setupMockToolsWithQualityVMAF(t, dir, sdr8BitProbeJSON, vmafByQuality)
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
		ID:              "bench-adaptive-test",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: adaptiveTestCandidates7(),
		Adaptive:   adaptive,
		Attempt:    1,
	}
	runner := &ProductionBenchmarkRunner{}
	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark failed: %v", err)
	}
	return record, logFile
}

func encodeQualitiesFromLog(t *testing.T, logFile string) []int {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading ffmpeg log: %v", err)
	}
	var order []int
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "hevc_videotoolbox") {
			continue
		}
		for _, q := range []int{55, 60, 65, 67, 68, 70, 75} {
			if strings.Contains(line, fmt.Sprintf("-q:v %d", q)) {
				order = append(order, q)
				break
			}
		}
	}
	return order
}

func TestAdaptiveRunner_TypicalQ65SkipsLowest(t *testing.T) {
	record, logFile := runAdaptiveBenchmarkForTest(t,
		&transcode.BenchmarkAdaptiveConfig{Mode: optimization.AdaptiveModeAdaptive, InitialQuality: 65},
		adaptiveTestScoresTypical())

	encoded := encodeQualitiesFromLog(t, logFile)
	encodedSet := map[int]bool{}
	for _, q := range encoded {
		encodedSet[q] = true
	}
	// q55 must never be encoded (skipped as provably below floor).
	if encodedSet[55] {
		t.Fatalf("adaptive encoded skipped quality 55 (encoded order %v)", encoded)
	}
	// All other qualities must be evaluated (6 probes).
	for _, q := range []int{60, 65, 67, 68, 70, 75} {
		if !encodedSet[q] {
			t.Errorf("expected quality %d to be evaluated, encoded order %v", q, encoded)
		}
	}
	if len(record.Evidence.CandidateSamples) != 6 {
		t.Errorf("expected 6 candidate samples (1 sample x 6 evaluated), got %d", len(record.Evidence.CandidateSamples))
	}
	if len(record.Evidence.CandidateMetrics) != 6 {
		t.Errorf("expected 6 candidate metric aggregates, got %d", len(record.Evidence.CandidateMetrics))
	}
	// Deterministic probe order: initial 65 first, high 75 second.
	if len(encoded) < 2 || encoded[0] != 65 || encoded[1] != 75 {
		t.Errorf("expected deterministic probe order [65 75 ...], got %v", encoded)
	}
	// Progress with early stop: monotonic, capped below 100.
	if record.Progress != 99.0 {
		t.Errorf("expected progress 99.0 after early stop, got %v", record.Progress)
	}
	if record.Progress >= 100.0 {
		t.Errorf("progress must remain <100 until terminal completed state, got %v", record.Progress)
	}
	if record.Phase != "selecting_candidate" {
		t.Errorf("expected phase selecting_candidate, got %q", record.Phase)
	}
	if record.Evidence.Decision == nil || record.Evidence.Decision.Winner == nil {
		t.Fatalf("expected winner in adaptive decision")
	}
}

func TestAdaptiveRunner_ExhaustiveCompatibility(t *testing.T) {
	scores := adaptiveTestScoresTypical()
	adaptiveRecord, _ := runAdaptiveBenchmarkForTest(t,
		&transcode.BenchmarkAdaptiveConfig{Mode: optimization.AdaptiveModeAdaptive, InitialQuality: 65},
		scores)
	exhaustiveRecord, _ := runAdaptiveBenchmarkForTest(t, nil, scores)

	aw := adaptiveRecord.Evidence.Decision.Winner
	ew := exhaustiveRecord.Evidence.Decision.Winner
	if aw == nil || ew == nil {
		t.Fatalf("expected winners in both modes (adaptive=%v exhaustive=%v)", aw, ew)
	}
	if aw.CandidateID != ew.CandidateID {
		t.Fatalf("adaptive winner %q != exhaustive winner %q", aw.CandidateID, ew.CandidateID)
	}
	if adaptiveRecord.Evidence.Decision.DecisionReason != exhaustiveRecord.Evidence.Decision.DecisionReason {
		t.Fatalf("adaptive reason %q != exhaustive reason %q",
			adaptiveRecord.Evidence.Decision.DecisionReason, exhaustiveRecord.Evidence.Decision.DecisionReason)
	}
}

func TestAdaptiveRunner_NonMonotonicFallbackEvaluatesAll(t *testing.T) {
	scores := map[int]float64{
		55: 96.0, 60: 96.1, 65: 97.0, 67: 96.9, 68: 96.8, 70: 96.7, 75: 95.0,
	}
	record, logFile := runAdaptiveBenchmarkForTest(t,
		&transcode.BenchmarkAdaptiveConfig{Mode: optimization.AdaptiveModeAdaptive, InitialQuality: 65},
		scores)
	encoded := encodeQualitiesFromLog(t, logFile)
	encodedSet := map[int]bool{}
	for _, q := range encoded {
		encodedSet[q] = true
	}
	for _, q := range []int{55, 60, 65, 67, 68, 70, 75} {
		if !encodedSet[q] {
			t.Errorf("expected fallback to evaluate quality %d, encoded %v", q, encoded)
		}
	}
	if len(record.Evidence.CandidateSamples) != 7 {
		t.Errorf("expected 7 candidate samples after fallback, got %d", len(record.Evidence.CandidateSamples))
	}
	if record.Progress != 99.0 {
		t.Errorf("expected progress 99.0 after fallback, got %v", record.Progress)
	}
}

func TestAdaptiveRunner_CandidateFailureFallbackEvaluatesRemaining(t *testing.T) {
	// q60 returns an out-of-range VMAF score (parse failure => inconclusive probe).
	// Adaptive must fall back to evaluating the remaining candidate (q55) exhaustively.
	scores := map[int]float64{
		55: 94.0,
		60: 999.0,
		65: 96.5,
		67: 96.6,
		68: 96.7,
		70: 96.75,
		75: 96.8,
	}
	record, logFile := runAdaptiveBenchmarkForTest(t,
		&transcode.BenchmarkAdaptiveConfig{Mode: optimization.AdaptiveModeAdaptive, InitialQuality: 65},
		scores)
	encoded := encodeQualitiesFromLog(t, logFile)
	encodedSet := map[int]bool{}
	for _, q := range encoded {
		encodedSet[q] = true
	}
	// Fallback must include the lowest candidate that early stop would have skipped.
	if !encodedSet[55] {
		t.Fatalf("expected fallback to evaluate q55 after inconclusive q60 probe, encoded %v", encoded)
	}
	if len(record.Evidence.CandidateSamples) != 7 {
		t.Errorf("expected 7 candidate samples after failure fallback, got %d", len(record.Evidence.CandidateSamples))
	}
	if record.Evidence.Decision == nil {
		t.Fatalf("expected decision after failure fallback")
	}
}

func TestAdaptiveRunner_InitialBelowTargetProbesUpward(t *testing.T) {
	// q65 below target; adaptive must probe upward to find the lowest passing candidate.
	scores := map[int]float64{
		55: 93.0, 60: 94.0, 65: 95.5, 67: 96.0, 68: 96.6, 70: 96.9, 75: 97.2,
	}
	record, logFile := runAdaptiveBenchmarkForTest(t,
		&transcode.BenchmarkAdaptiveConfig{Mode: optimization.AdaptiveModeAdaptive, InitialQuality: 65},
		scores)
	encoded := encodeQualitiesFromLog(t, logFile)
	encodedSet := map[int]bool{}
	for _, q := range encoded {
		encodedSet[q] = true
	}
	// Upward intermediates must be probed.
	for _, q := range []int{65, 67, 68, 70, 75} {
		if !encodedSet[q] {
			t.Errorf("expected upward probe of quality %d, encoded %v", q, encoded)
		}
	}
	if record.Evidence.Decision == nil || record.Evidence.Decision.Winner == nil {
		t.Fatalf("expected winner after upward search")
	}
	if record.Progress != 99.0 {
		t.Errorf("expected progress 99.0, got %v", record.Progress)
	}
}

func TestAdaptiveRunner_ExhaustiveModeUnchanged(t *testing.T) {
	// Nil Adaptive preserves the exhaustive path: all 7 candidates evaluated.
	record, logFile := runAdaptiveBenchmarkForTest(t, nil, adaptiveTestScoresTypical())
	encoded := encodeQualitiesFromLog(t, logFile)
	if len(encoded) != 7 {
		t.Fatalf("exhaustive path must evaluate all 7 candidates, encoded %v", encoded)
	}
	if len(record.Evidence.CandidateSamples) != 7 {
		t.Fatalf("expected 7 candidate samples in exhaustive mode, got %d", len(record.Evidence.CandidateSamples))
	}
	if record.Progress != 99.0 {
		t.Fatalf("expected progress 99.0, got %v", record.Progress)
	}
}
