package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

// setupMockToolsWithQualitySizes creates mock tools where candidates with quality 60
// produce smaller dummy files than candidates with quality 70.
func setupMockToolsWithQualitySizes(t *testing.T, dir string, probeJSON string) (string, string, string) {
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
esac

out=""
for last; do out="$last"; done

case "$out" in
  -*|"")
    ;;
  *)
    mkdir -p "$(dirname "$out")"
    case "$*" in
      *"-q:v 60"*)
        echo "short" > "$out"
        ;;
      *"-q:v 70"*)
        echo "much longer payload for quality 70 to guarantee larger sample size" > "$out"
        ;;
      *)
        echo "fake media data payload for $out" > "$out"
        ;;
    esac
    ;;
esac
exit 0
`, logFile)

	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("writing mock ffmpeg: %v", err)
	}

	return mockFFmpeg, mockProbe, logFile
}

// probeJSONWithNoAudioBitrate defines an 8-bit SDR source with an audio stream lacking bitrate.
const probeJSONWithNoAudioBitrate = `{
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
    },
    {
      "index": 1,
      "codec_type": "audio",
      "codec_name": "aac",
      "channels": 2
    }
  ],
  "format": {
    "format_name": "matroska,webm",
    "duration": "120.000000",
    "size": "50000000"
  },
  "chapters": []
}`

func TestProductionBenchmarkRunner_Phase6_WinnerReachesTarget(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(sourceFile, []byte("fake video content for benchmark"), 0644); err != nil {
		t.Fatalf("writing source file: %v", err)
	}

	mockFFmpeg, mockProbe, _ := setupMockToolsWithQualitySizes(t, dir, sdr8BitProbeJSON)

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
		ID:              "bench-target-reached",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_0_q60", Quality: 60},
			{ID: "cand_1_q70", Quality: 70},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	runner.SetMetricsHook(func(ctx context.Context, w *Worker, rec *BenchmarkRecord, ev *BenchmarkExecutionEvidence) error {
		// Both reach target (96.0). cand_0 has score 96.5, cand_1 has score 96.8 (within 0.5 marginal tolerance).
		ev.CandidateMetrics = append(ev.CandidateMetrics,
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_0_q60",
				CandidateIndex: 0,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeVMAF,
					Valid:      true,
					MeanScore:  96.5,
					MinScore:   96.5,
					MaxScore:   96.5,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 96.5, Valid: true},
					},
				},
			},
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_1_q70",
				CandidateIndex: 1,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeVMAF,
					Valid:      true,
					MeanScore:  96.8,
					MinScore:   96.8,
					MaxScore:   96.8,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 96.8, Valid: true},
					},
				},
			},
		)
		return nil
	})

	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark failed: %v", err)
	}

	ev := record.Evidence
	if ev == nil || ev.Decision == nil {
		t.Fatalf("expected non-nil decision in evidence")
	}

	dec := ev.Decision
	if dec.DecisionReason != optimization.ReasonTargetReachedSmallestSize {
		t.Errorf("DecisionReason = %q, want %q", dec.DecisionReason, optimization.ReasonTargetReachedSmallestSize)
	}
	if dec.Winner == nil {
		t.Fatalf("expected non-nil winner")
	}

	// cand_0_q60 should win because it reaches target with smaller or equal size
	if dec.Winner.CandidateID != "cand_0_q60" {
		t.Errorf("Winner ID = %q, want %q", dec.Winner.CandidateID, "cand_0_q60")
	}
	if dec.Winner.CandidateIndex != 0 {
		t.Errorf("Winner Index = %d, want 0", dec.Winner.CandidateIndex)
	}
	if dec.Winner.Quality != 60 {
		t.Errorf("Winner Quality = %d, want 60", dec.Winner.Quality)
	}
	if dec.Winner.VideoProfile != "main" {
		t.Errorf("Winner VideoProfile = %q, want %q", dec.Winner.VideoProfile, "main")
	}
	if dec.Winner.PixelFormat != "yuv420p" {
		t.Errorf("Winner PixelFormat = %q, want %q", dec.Winner.PixelFormat, "yuv420p")
	}
	if dec.Winner.ExpectedBitDepth != 8 {
		t.Errorf("Winner ExpectedBitDepth = %d, want 8", dec.Winner.ExpectedBitDepth)
	}
	if dec.Winner.Score != 96.5 {
		t.Errorf("Winner Score = %v, want 96.5", dec.Winner.Score)
	}
	if !dec.Winner.TargetReached || !dec.Winner.MinimumMet {
		t.Errorf("Winner flags expected TargetReached=true and MinimumMet=true, got %v, %v",
			dec.Winner.TargetReached, dec.Winner.MinimumMet)
	}
	if dec.Winner.EstimatedVideoBytes <= 0 || dec.Winner.EstimatedTotalBytes <= 0 {
		t.Errorf("Winner estimated bytes should be positive, got video=%d, total=%d",
			dec.Winner.EstimatedVideoBytes, dec.Winner.EstimatedTotalBytes)
	}

	// Verify evaluations list
	if len(dec.Evaluations) != 2 {
		t.Fatalf("expected 2 evaluations, got %d", len(dec.Evaluations))
	}
	for i, eval := range dec.Evaluations {
		if eval.CandidateIndex != i {
			t.Errorf("eval %d: CandidateIndex = %d, want %d", i, eval.CandidateIndex, i)
		}
		if !eval.Eligible || !eval.TargetReached || !eval.MinimumMet {
			t.Errorf("eval %d: expected all eligibility flags true", i)
		}
	}
}

func TestProductionBenchmarkRunner_Phase6_FallbackWinnerAboveMinimumBelowTarget(t *testing.T) {
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

	// Target 98.0, Minimum 95.0
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-fallback-winner",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Quality: &transcode.BenchmarkQualityConfig{
			VMAF: &transcode.BenchmarkQualityThresholds{
				Target:  98.0,
				Minimum: 95.0,
			},
		},
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_0_q60", Quality: 60},
			{ID: "cand_1_q70", Quality: 70},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	runner.SetMetricsHook(func(ctx context.Context, w *Worker, rec *BenchmarkRecord, ev *BenchmarkExecutionEvidence) error {
		// Neither reaches target (98.0), but both exceed min (95.0).
		// cand_0: 95.3, cand_1: 96.1 -> cand_1 has higher quality and wins!
		ev.CandidateMetrics = append(ev.CandidateMetrics,
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_0_q60",
				CandidateIndex: 0,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeVMAF,
					Valid:      true,
					MeanScore:  95.3,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 95.3, Valid: true},
					},
				},
			},
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_1_q70",
				CandidateIndex: 1,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeVMAF,
					Valid:      true,
					MeanScore:  96.1,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 96.1, Valid: true},
					},
				},
			},
		)
		return nil
	})

	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark failed: %v", err)
	}

	dec := record.Evidence.Decision
	if dec == nil || dec.Winner == nil {
		t.Fatalf("expected non-nil decision and winner")
	}

	if dec.DecisionReason != optimization.ReasonMinimumMetHighestQuality {
		t.Errorf("DecisionReason = %q, want %q", dec.DecisionReason, optimization.ReasonMinimumMetHighestQuality)
	}
	if dec.Winner.CandidateID != "cand_1_q70" {
		t.Errorf("Winner ID = %q, want %q (highest quality meeting minimum)", dec.Winner.CandidateID, "cand_1_q70")
	}
	if dec.Winner.TargetReached {
		t.Errorf("TargetReached should be false")
	}
	if !dec.Winner.MinimumMet {
		t.Errorf("MinimumMet should be true")
	}
}

func TestProductionBenchmarkRunner_Phase6_NoEligibleWinner(t *testing.T) {
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

	// Target 96.0, Minimum 95.0
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-no-winner",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_0_low", Quality: 50},
			{ID: "cand_1_low", Quality: 55},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	runner.SetMetricsHook(func(ctx context.Context, w *Worker, rec *BenchmarkRecord, ev *BenchmarkExecutionEvidence) error {
		// Both below minimum quality (95.0)
		ev.CandidateMetrics = append(ev.CandidateMetrics,
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_0_low",
				CandidateIndex: 0,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeVMAF,
					Valid:      true,
					MeanScore:  93.2,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 93.2, Valid: true},
					},
				},
			},
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_1_low",
				CandidateIndex: 1,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeVMAF,
					Valid:      true,
					MeanScore:  94.1,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 94.1, Valid: true},
					},
				},
			},
		)
		return nil
	})

	// Job completes without error; no winner is fabricated
	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark should complete without error: %v", err)
	}

	dec := record.Evidence.Decision
	if dec == nil {
		t.Fatalf("expected non-nil decision")
	}
	if dec.Winner != nil {
		t.Errorf("expected Winner == nil, got %+v", dec.Winner)
	}
	if dec.DecisionReason != optimization.ReasonNoCandidateMetMinimumQuality {
		t.Errorf("DecisionReason = %q, want %q", dec.DecisionReason, optimization.ReasonNoCandidateMetMinimumQuality)
	}
	for _, eval := range dec.Evaluations {
		if eval.Eligible {
			t.Errorf("candidate %s should not be eligible", eval.CandidateID)
		}
		if eval.EvaluationReason != optimization.ReasonBelowMinimumQuality && eval.EvaluationReason != optimization.ReasonSampleBelowMinimum {
			t.Errorf("candidate %s EvaluationReason = %q, want below minimum quality reason", eval.CandidateID, eval.EvaluationReason)
		}
	}
}

func TestProductionBenchmarkRunner_Phase6_OneInvalidCandidateWhileAnotherWins(t *testing.T) {
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
		ID:              "bench-cand-invalid-one-wins",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_0_broken", Quality: 60},
			{ID: "cand_1_healthy", Quality: 70},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	runner.SetMetricsHook(func(ctx context.Context, w *Worker, rec *BenchmarkRecord, ev *BenchmarkExecutionEvidence) error {
		// cand_0 suffered metric failure; cand_1 succeeded with target-reaching score
		ev.CandidateMetrics = append(ev.CandidateMetrics,
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_0_broken",
				CandidateIndex: 0,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType:       optimization.MetricTypeVMAF,
					Valid:            false,
					IneligibleReason: optimization.ReasonIncompleteSampleScores,
				},
			},
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_1_healthy",
				CandidateIndex: 1,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeVMAF,
					Valid:      true,
					MeanScore:  96.8,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 96.8, Valid: true},
					},
				},
			},
		)
		return nil
	})

	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark should not fail: %v", err)
	}

	dec := record.Evidence.Decision
	if dec == nil || dec.Winner == nil {
		t.Fatalf("expected non-nil decision and winner")
	}

	if dec.Winner.CandidateID != "cand_1_healthy" {
		t.Errorf("Winner ID = %q, want %q", dec.Winner.CandidateID, "cand_1_healthy")
	}
	if dec.Evaluations[0].Eligible {
		t.Errorf("broken candidate 0 should not be eligible")
	}
	if dec.Evaluations[0].EvaluationReason != optimization.ReasonIncompleteSampleScores {
		t.Errorf("broken candidate 0 EvaluationReason = %q, want %q",
			dec.Evaluations[0].EvaluationReason, optimization.ReasonIncompleteSampleScores)
	}
}

func TestProductionBenchmarkRunner_Phase6_DeterministicTieBreak(t *testing.T) {
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

	// Target 98.0, Minimum 95.0. Both candidates have identical score 95.5.
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-tiebreak",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Quality: &transcode.BenchmarkQualityConfig{
			VMAF: &transcode.BenchmarkQualityThresholds{
				Target:  98.0,
				Minimum: 95.0,
			},
		},
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_b", Quality: 65},
			{ID: "cand_a", Quality: 65},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	runner.SetMetricsHook(func(ctx context.Context, w *Worker, rec *BenchmarkRecord, ev *BenchmarkExecutionEvidence) error {
		// Both have identical scores: 95.5
		ev.CandidateMetrics = append(ev.CandidateMetrics,
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_b",
				CandidateIndex: 0,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeVMAF,
					Valid:      true,
					MeanScore:  95.5,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 95.5, Valid: true},
					},
				},
			},
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_a",
				CandidateIndex: 1,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeVMAF,
					Valid:      true,
					MeanScore:  95.5,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 95.5, Valid: true},
					},
				},
			},
		)
		return nil
	})

	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark failed: %v", err)
	}

	dec := record.Evidence.Decision
	if dec == nil || dec.Winner == nil {
		t.Fatalf("expected non-nil decision and winner")
	}

	// Since mock creates identical sample files (same size), tie-break falls back to CandidateID alphabetically
	// "cand_a" < "cand_b"
	if dec.Winner.CandidateID != "cand_a" {
		t.Errorf("Winner ID = %q, want %q (alphabetical tie-break)", dec.Winner.CandidateID, "cand_a")
	}
}

func TestProductionBenchmarkRunner_Phase6_MetricVMAF_SSIM_Both(t *testing.T) {
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

	// 1. Metric = SSIM
	recordSSIM := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-metric-ssim",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "ssim",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_ssim_0", Quality: 60},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	runner.SetMetricsHook(func(ctx context.Context, w *Worker, rec *BenchmarkRecord, ev *BenchmarkExecutionEvidence) error {
		ev.CandidateMetrics = append(ev.CandidateMetrics,
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_ssim_0",
				CandidateIndex: 0,
				MetricType:     optimization.MetricTypeSSIM,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeSSIM,
					Valid:      true,
					MeanScore:  0.995,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 0.995, Valid: true},
					},
				},
			},
		)
		return nil
	})

	if err := runner.RunBenchmark(context.Background(), worker, recordSSIM); err != nil {
		t.Fatalf("RunBenchmark for SSIM failed: %v", err)
	}

	decSSIM := recordSSIM.Evidence.Decision
	if decSSIM == nil || decSSIM.Winner == nil {
		t.Fatalf("expected SSIM decision and winner")
	}
	if decSSIM.Winner.MetricType != "ssim" {
		t.Errorf("SSIM Winner MetricType = %q, want ssim", decSSIM.Winner.MetricType)
	}
	if decSSIM.Winner.Score != 0.995 {
		t.Errorf("SSIM Winner Score = %v, want 0.995", decSSIM.Winner.Score)
	}

	// 2. Metric = both
	recordBoth := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-metric-both-eval",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "both",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_both_0", Quality: 65},
		},
		Attempt: 1,
	}

	runnerBoth := &ProductionBenchmarkRunner{}
	runnerBoth.SetMetricsHook(func(ctx context.Context, w *Worker, rec *BenchmarkRecord, ev *BenchmarkExecutionEvidence) error {
		ev.CandidateMetrics = append(ev.CandidateMetrics,
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_both_0",
				CandidateIndex: 0,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeVMAF,
					Valid:      true,
					MeanScore:  96.5,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 96.5, Valid: true},
					},
				},
			},
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_both_0",
				CandidateIndex: 0,
				MetricType:     optimization.MetricTypeSSIM,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeSSIM,
					Valid:      true,
					MeanScore:  0.992,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 0.992, Valid: true},
					},
				},
			},
		)
		return nil
	})

	if err := runnerBoth.RunBenchmark(context.Background(), worker, recordBoth); err != nil {
		t.Fatalf("RunBenchmark for both failed: %v", err)
	}

	decBoth := recordBoth.Evidence.Decision
	if decBoth == nil || decBoth.Winner == nil {
		t.Fatalf("expected both decision and winner")
	}
	// Default preferred metric is VMAF
	if decBoth.Winner.MetricType != "vmaf" {
		t.Errorf("both Winner MetricType = %q, want vmaf", decBoth.Winner.MetricType)
	}
}

func TestProductionBenchmarkRunner_Phase6_SizeEstimationFallback(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source_fallback.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video content with audio stream"), 0644)

	// Probe JSON with an audio stream that has 0/null bitrate
	mockFFmpeg, mockProbe, _ := setupMockTools(t, dir, probeJSONWithNoAudioBitrate, "")

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)

	// Case A: FallbackAudioBitrateBps provided -> candidate is eligible and wins!
	recordWithFallback := &BenchmarkRecord{
		ProtocolVersion:         transcode.WorkerProtocolVersion,
		ID:                      "bench-size-fallback",
		Status:                  "running",
		Source:                  sourceFile,
		Metric:                  "vmaf",
		FallbackAudioBitrateBps: 384000,
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_fallback_ok", Quality: 60},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	runner.SetMetricsHook(func(ctx context.Context, w *Worker, rec *BenchmarkRecord, ev *BenchmarkExecutionEvidence) error {
		ev.CandidateMetrics = append(ev.CandidateMetrics,
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_fallback_ok",
				CandidateIndex: 0,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeVMAF,
					Valid:      true,
					MeanScore:  96.5,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 96.5, Valid: true},
					},
				},
			},
		)
		return nil
	})

	if err := runner.RunBenchmark(context.Background(), worker, recordWithFallback); err != nil {
		t.Fatalf("RunBenchmark with fallback failed: %v", err)
	}

	dec := recordWithFallback.Evidence.Decision
	if dec == nil || dec.Winner == nil {
		t.Fatalf("expected winner when audio fallback is provided")
	}

	hasAudioUncertainty := false
	for _, u := range dec.Winner.Uncertainties {
		if strings.HasPrefix(u, optimization.ReasonAudioBitrateFallback) {
			hasAudioUncertainty = true
			break
		}
	}
	if !hasAudioUncertainty {
		t.Errorf("expected %s in winner uncertainties, got: %v",
			optimization.ReasonAudioBitrateFallback, dec.Winner.Uncertainties)
	}

	// Case B: No Fallback provided -> audio has no bitrate -> estimate is unusable -> candidate cannot win!
	recordWithoutFallback := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-size-no-fallback",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_no_fallback", Quality: 60},
		},
		Attempt: 1,
	}

	runnerB := &ProductionBenchmarkRunner{}
	runnerB.SetMetricsHook(func(ctx context.Context, w *Worker, rec *BenchmarkRecord, ev *BenchmarkExecutionEvidence) error {
		ev.CandidateMetrics = append(ev.CandidateMetrics,
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_no_fallback",
				CandidateIndex: 0,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeVMAF,
					Valid:      true,
					MeanScore:  96.5,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 96.5, Valid: true},
					},
				},
			},
		)
		return nil
	})

	if err := runnerB.RunBenchmark(context.Background(), worker, recordWithoutFallback); err != nil {
		t.Fatalf("RunBenchmark without fallback should not crash: %v", err)
	}

	decB := recordWithoutFallback.Evidence.Decision
	if decB == nil {
		t.Fatalf("expected non-nil decision")
	}
	if decB.Winner != nil {
		t.Errorf("expected Winner == nil when estimate is unusable, got %+v", decB.Winner)
	}
	if decB.DecisionReason != optimization.ReasonAllCandidatesInvalid {
		t.Errorf("DecisionReason = %q, want %q", decB.DecisionReason, optimization.ReasonAllCandidatesInvalid)
	}
	if decB.Evaluations[0].EvaluationReason != optimization.ReasonMissingStreamBitrate {
		t.Errorf("EvaluationReason = %q, want %q",
			decB.Evaluations[0].EvaluationReason, optimization.ReasonMissingStreamBitrate)
	}
}

func TestProductionBenchmarkRunner_Phase6_MissingIncompleteMetricEvidence(t *testing.T) {
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
		ID:              "bench-missing-metric-evidence",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_missing_metric", Quality: 60},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	runner.SetMetricsHook(func(ctx context.Context, w *Worker, rec *BenchmarkRecord, ev *BenchmarkExecutionEvidence) error {
		// No metrics added for cand_missing_metric
		return nil
	})

	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark should not fail whole job: %v", err)
	}

	dec := record.Evidence.Decision
	if dec == nil {
		t.Fatalf("expected non-nil decision")
	}
	if dec.Winner != nil {
		t.Errorf("expected Winner == nil, got %+v", dec.Winner)
	}
	if dec.Evaluations[0].Eligible {
		t.Errorf("expected Eligible == false for candidate lacking metric evidence")
	}
	if dec.Evaluations[0].EvaluationReason != optimization.ReasonMetricMissing {
		t.Errorf("EvaluationReason = %q, want %q",
			dec.Evaluations[0].EvaluationReason, optimization.ReasonMetricMissing)
	}
}

func TestProductionBenchmarkRunner_Phase6_CandidateOrderDeterminism(t *testing.T) {
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

	// Run 1: Candidates [cand_a, cand_b]
	record1 := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-order-1",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_a", Quality: 60},
			{ID: "cand_b", Quality: 70},
		},
		Attempt: 1,
	}

	// Run 2: Candidates [cand_b, cand_a] (inverted order)
	record2 := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-order-2",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_b", Quality: 70},
			{ID: "cand_a", Quality: 60},
		},
		Attempt: 1,
	}

	metricHook := func(ctx context.Context, w *Worker, rec *BenchmarkRecord, ev *BenchmarkExecutionEvidence) error {
		for i, c := range rec.Candidates {
			score := 96.5
			if c.ID == "cand_b" {
				score = 97.5
			}
			ev.CandidateMetrics = append(ev.CandidateMetrics, BenchmarkCandidateMetricAggregate{
				CandidateID:    c.ID,
				CandidateIndex: i,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeVMAF,
					Valid:      true,
					MeanScore:  score,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: score, Valid: true},
					},
				},
			})
		}
		return nil
	}

	runner1 := &ProductionBenchmarkRunner{}
	runner1.SetMetricsHook(metricHook)
	if err := runner1.RunBenchmark(context.Background(), worker, record1); err != nil {
		t.Fatalf("RunBenchmark 1 failed: %v", err)
	}

	runner2 := &ProductionBenchmarkRunner{}
	runner2.SetMetricsHook(metricHook)
	if err := runner2.RunBenchmark(context.Background(), worker, record2); err != nil {
		t.Fatalf("RunBenchmark 2 failed: %v", err)
	}

	dec1 := record1.Evidence.Decision
	dec2 := record2.Evidence.Decision

	// Winner must be identical regardless of candidate order
	if dec1.Winner.CandidateID != dec2.Winner.CandidateID {
		t.Errorf("Winner ID mismatch: run1=%s, run2=%s", dec1.Winner.CandidateID, dec2.Winner.CandidateID)
	}
	if dec1.DecisionReason != dec2.DecisionReason {
		t.Errorf("DecisionReason mismatch: run1=%s, run2=%s", dec1.DecisionReason, dec2.DecisionReason)
	}

	// Evaluations list preserves the request candidate order
	if dec1.Evaluations[0].CandidateID != "cand_a" || dec1.Evaluations[1].CandidateID != "cand_b" {
		t.Errorf("run1 evaluations order incorrect: %s, %s",
			dec1.Evaluations[0].CandidateID, dec1.Evaluations[1].CandidateID)
	}
	if dec2.Evaluations[0].CandidateID != "cand_b" || dec2.Evaluations[1].CandidateID != "cand_a" {
		t.Errorf("run2 evaluations order incorrect: %s, %s",
			dec2.Evaluations[0].CandidateID, dec2.Evaluations[1].CandidateID)
	}
}

func TestProductionBenchmarkRunner_Phase6_StatusDecisionPersistence(t *testing.T) {
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
		ID:              "bench-status-decision",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 10.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_0", Quality: 60},
		},
		Attempt: 1,
	}

	runner := &ProductionBenchmarkRunner{}
	runner.SetMetricsHook(func(ctx context.Context, w *Worker, rec *BenchmarkRecord, ev *BenchmarkExecutionEvidence) error {
		ev.CandidateMetrics = append(ev.CandidateMetrics,
			BenchmarkCandidateMetricAggregate{
				CandidateID:    "cand_0",
				CandidateIndex: 0,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate: optimization.MetricAggregate{
					MetricType: optimization.MetricTypeVMAF,
					Valid:      true,
					MeanScore:  96.5,
					SampleScores: []optimization.SampleScore{
						{SampleIndex: 0, Score: 96.5, Valid: true},
					},
				},
			},
		)
		return nil
	})

	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark failed: %v", err)
	}

	// Persist benchmark record to disk to test BenchmarkStatus
	jobDir := filepath.Join(cfg.StateDir, record.ID)
	_ = os.MkdirAll(jobDir, 0755)
	benchFile := filepath.Join(jobDir, "benchmark.json")
	if err := SaveBenchmarkAtomic(benchFile, record); err != nil {
		t.Fatalf("saving benchmark record: %v", err)
	}

	st, err := worker.BenchmarkStatus(context.Background(), record.ID)
	if err != nil {
		t.Fatalf("BenchmarkStatus failed: %v", err)
	}

	if st.Decision == nil {
		t.Fatalf("expected st.Decision to be non-nil")
	}
	if st.Decision.Winner == nil {
		t.Fatalf("expected st.Decision.Winner to be non-nil")
	}
	if st.Decision.Winner.CandidateID != "cand_0" {
		t.Errorf("st.Decision.Winner.CandidateID = %q, want cand_0", st.Decision.Winner.CandidateID)
	}
	if len(st.Decision.Evaluations) != 1 {
		t.Errorf("expected 1 evaluation in st.Decision, got %d", len(st.Decision.Evaluations))
	}
}
