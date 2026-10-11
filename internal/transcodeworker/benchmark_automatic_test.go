package transcodeworker

import (
	"context"
	"encoding/json"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/quality"
	"strings"
	"testing"
	"time"
)

func TestAutomaticSearchFallbackAndIndependentVerification(t *testing.T) {
	for _, scenario := range []string{"initial_pass", "initial_rejected", "holdout_rejected", "all_rejected"} {
		t.Run(scenario, func(t *testing.T) {
			p5 := 93.0
			record := &BenchmarkRecord{Mode: "quality", ModePolicyVersion: optimization.ModePolicyV2, Automatic: true, Metric: "vmaf", SourceDuration: 100, Quality: &transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Target: 96, Minimum: 95, P5Minimum: &p5, GuardrailEnforcement: "reject"}}}
			for i, start := range []float64{15, 45, 75, 30, 60} {
				record.Samples = append(record.Samples, transcode.BenchmarkSampleWindow{Index: i, StartSeconds: start, DurationSeconds: 10})
			}
			vcs := []validatedCandidate{{index: 0, candidate: transcode.BenchmarkCandidate{ID: "initial", VideoCodec: "libx265", Quality: 20, Preset: "medium"}, bitDepth: 8}, {index: 1, candidate: transcode.BenchmarkCandidate{ID: "fallback-quality-16", VideoCodec: "libx265", Quality: 16, Preset: "slow"}, bitDepth: 8}}
			evidence := &BenchmarkExecutionEvidence{SourceBitDepth: 8}
			seen := map[string]bool{}
			runner := &ProductionBenchmarkRunner{automaticHook: func(_ context.Context, stage *BenchmarkRecord, e *BenchmarkExecutionEvidence, candidates []validatedCandidate) error {
				for _, vc := range candidates {
					score := 98.0
					if scenario == "all_rejected" || scenario == "initial_rejected" && vc.candidate.ID == "initial" || scenario == "holdout_rejected" && vc.candidate.ID == "initial" && stage.Samples[0].Index == 3 {
						score = 85
					}
					var scores []optimization.SampleScore
					for _, window := range stage.Samples {
						key := vc.candidate.ID + string(rune(window.Index))
						if seen[key] {
							t.Fatal("sample was repeated")
						}
						seen[key] = true
						frames := make([]map[string]any, 100)
						for i := range frames {
							frames[i] = map[string]any{"frameNum": i, "metrics": map[string]any{"vmaf": score}}
						}
						data, _ := json.Marshal(map[string]any{"frames": frames})
						model, _ := quality.Model("v1_1080p_3h")
						stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, model, window.Index, window.StartSeconds, 25, 4, nil)
						if err != nil {
							t.Fatal(err)
						}
						e.CandidateSamples = append(e.CandidateSamples, BenchmarkCandidateSampleResult{CandidateID: vc.candidate.ID, SampleIndex: window.Index, SizeBytes: 8000, EncodeDurationSec: 1})
						e.MetricSamples = append(e.MetricSamples, BenchmarkMetricSampleResult{CandidateID: vc.candidate.ID, SampleIndex: window.Index, VMAF: &stats.Mean, VMAFStats: &stats})
						scores = append(scores, optimization.SampleScore{SampleIndex: window.Index, Score: score, Valid: true})
					}
					e.CandidateMetrics = append(e.CandidateMetrics, BenchmarkCandidateMetricAggregate{CandidateID: vc.candidate.ID, MetricType: optimization.MetricTypeVMAF, Aggregate: optimization.AggregateSampleScores(optimization.MetricTypeVMAF, scores)})
				}
				return nil
			}}
			progress := newBenchmarkProgressReporter(nil, record, 100)
			err := runner.runAutomaticCandidates(context.Background(), nil, record, evidence, "", vcs, mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 25}}}, 100000, time.Time{}, progress)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "all_rejected" {
				if evidence.Decision.Winner != nil || evidence.Automatic.VerifiedSamples != 0 {
					t.Fatal("rejected search claimed success")
				}
				return
			}
			if evidence.Decision.Winner == nil || evidence.Automatic.VerifiedSamples != 5 {
				t.Fatalf("no verified winner: %+v", evidence.Decision)
			}
			if scenario != "initial_pass" && !strings.HasPrefix(evidence.Decision.Winner.CandidateID, "fallback-quality-") {
				t.Fatal("failed strategy was reused")
			}
			if len(seen) > 11 {
				t.Fatal("search exceeded bounded tests")
			}
		})
	}
}
