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
						stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, model, window.Index, window.StartSeconds, 25, 1, 4, nil)
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

func TestAutomaticQualityPruningSkipsDominatedSettings(t *testing.T) {
	for _, pruning := range []bool{false, true} {
		p5 := 93.0
		record := &BenchmarkRecord{Mode: "quality", ModePolicyVersion: optimization.ModePolicyV2, Automatic: true, QualityPruning: pruning, Metric: "vmaf", SourceDuration: 100, Quality: &transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Target: 96, Minimum: 95, P5Minimum: &p5, GuardrailEnforcement: "reject"}}}
		for i, start := range []float64{15, 45, 75, 30, 60} {
			record.Samples = append(record.Samples, transcode.BenchmarkSampleWindow{Index: i, StartSeconds: start, DurationSeconds: 10})
		}
		var vcs []validatedCandidate
		for _, crf := range []int{26, 20, 23} {
			vcs = append(vcs, validatedCandidate{index: len(vcs), candidate: transcode.BenchmarkCandidate{ID: "medium-" + string(rune('0'+crf/10)) + string(rune('0'+crf%10)), VideoCodec: "libx265", Quality: crf, Preset: "medium"}, bitDepth: 8})
		}
		for _, crf := range []int{16, 18, 20} {
			vcs = append(vcs, validatedCandidate{index: len(vcs), candidate: transcode.BenchmarkCandidate{ID: "fallback-quality-" + string(rune('0'+crf/10)) + string(rune('0'+crf%10)), VideoCodec: "libx265", Quality: crf, Preset: "slow"}, bitDepth: 8})
		}
		measured := map[string]bool{}
		runner := &ProductionBenchmarkRunner{automaticHook: func(_ context.Context, stage *BenchmarkRecord, e *BenchmarkExecutionEvidence, candidates []validatedCandidate) error {
			for _, vc := range candidates {
				measured[vc.candidate.ID] = true
				score := 85.0 // every medium setting fails quality
				if vc.candidate.Preset == "slow" {
					score = 100 - float64(vc.candidate.Quality)/4
				}
				var scores []optimization.SampleScore
				for _, window := range stage.Samples {
					frames := make([]map[string]any, 100)
					for i := range frames {
						frames[i] = map[string]any{"frameNum": i, "metrics": map[string]any{"vmaf": score}}
					}
					data, _ := json.Marshal(map[string]any{"frames": frames})
					model, _ := quality.Model("v1_1080p_3h")
					stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, model, window.Index, window.StartSeconds, 25, 1, 4, nil)
					if err != nil {
						t.Fatal(err)
					}
					e.CandidateSamples = append(e.CandidateSamples, BenchmarkCandidateSampleResult{CandidateID: vc.candidate.ID, SampleIndex: window.Index, SizeBytes: int64(9000 - 100*vc.candidate.Quality), EncodeDurationSec: 1})
					e.MetricSamples = append(e.MetricSamples, BenchmarkMetricSampleResult{CandidateID: vc.candidate.ID, SampleIndex: window.Index, VMAF: &stats.Mean, VMAFStats: &stats})
					scores = append(scores, optimization.SampleScore{SampleIndex: window.Index, Score: score, Valid: true})
				}
				e.CandidateMetrics = append(e.CandidateMetrics, BenchmarkCandidateMetricAggregate{CandidateID: vc.candidate.ID, MetricType: optimization.MetricTypeVMAF, Aggregate: optimization.AggregateSampleScores(optimization.MetricTypeVMAF, scores)})
			}
			return nil
		}}
		evidence := &BenchmarkExecutionEvidence{SourceBitDepth: 8}
		progress := newBenchmarkProgressReporter(nil, record, 100)
		if err := runner.runAutomaticCandidates(context.Background(), nil, record, evidence, "", vcs, mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 25}}}, 100000, time.Time{}, progress); err != nil {
			t.Fatal(err)
		}
		if evidence.Decision.Winner == nil || !strings.HasPrefix(evidence.Decision.Winner.CandidateID, "fallback-quality-") {
			t.Fatalf("pruning=%v: no fallback winner: %+v", pruning, evidence.Decision)
		}
		for _, id := range []string{"fallback-quality-16", "fallback-quality-18", "fallback-quality-20", "medium-20"} {
			if !measured[id] {
				t.Fatalf("pruning=%v: %s was not measured", pruning, id)
			}
		}
		skippedReasons := map[string]string{}
		for _, ev := range evidence.Decision.Evaluations {
			if ev.EvaluationReason == "skipped_quality_dominated" {
				skippedReasons[ev.CandidateID] = ev.EvaluationReason
				if ev.Eligible {
					t.Fatal("skipped candidate marked eligible")
				}
			}
		}
		if !pruning {
			if !measured["medium-23"] || !measured["medium-26"] || len(skippedReasons) != 0 || len(evidence.Automatic.Stages[0].Skipped) != 0 {
				t.Fatalf("pruning disabled but settings skipped: measured=%v skipped=%v", measured, skippedReasons)
			}
			continue
		}
		if measured["medium-23"] || measured["medium-26"] {
			t.Fatalf("dominated settings measured: %v", measured)
		}
		if len(skippedReasons) != 2 || skippedReasons["medium-23"] == "" || skippedReasons["medium-26"] == "" {
			t.Fatalf("skipped settings not explained: %v", skippedReasons)
		}
		if got := evidence.Automatic.Stages[0].Skipped; len(got) != 2 {
			t.Fatalf("initial stage skipped = %v", got)
		}
	}
}

func TestSplitQualityFamilies(t *testing.T) {
	vc := func(id, codec string, q, kbps int, preset string) validatedCandidate {
		return validatedCandidate{candidate: transcode.BenchmarkCandidate{ID: id, VideoCodec: codec, Quality: q, AverageBitrateKbps: kbps, Preset: preset}}
	}
	leaders, followers := splitQualityFamilies([]validatedCandidate{
		vc("x26", "libx265", 26, 0, "medium"), vc("x20", "libx265", 20, 0, "medium"), vc("xs16", "libx265", 16, 0, "slow"),
		vc("vt65", "hevc_videotoolbox", 65, 0, ""), vc("vt75", "hevc_videotoolbox", 75, 0, ""),
		vc("br1", "hevc_videotoolbox", 0, 3200, ""), vc("br2", "hevc_videotoolbox", 0, 3500, ""),
	})
	var ids []string
	for _, l := range leaders {
		ids = append(ids, l.candidate.ID)
	}
	if got := strings.Join(ids, ","); got != "x20,xs16,vt75,br1,br2" {
		t.Fatalf("leaders = %s", got)
	}
	want := map[string]string{"x26": "x20", "vt65": "vt75"}
	if len(followers) != len(want) {
		t.Fatalf("followers = %+v", followers)
	}
	for _, f := range followers {
		if want[f.vc.candidate.ID] != f.leader {
			t.Fatalf("%s led by %s", f.vc.candidate.ID, f.leader)
		}
	}
}
