package action

import (
	"context"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// BatchSelection is the small, durable explanation of automatic routing.
// Inputs remain the user's original request. The bounded search is persisted;
// measured results create one temporary recipe without touching the catalog.
type BatchSelection struct {
	Profile  string `json:"profile"`
	Scope    string `json:"scope"`
	Status   string `json:"status"`
	Quality  int    `json:"quality,omitempty"`
	Priority string `json:"priority"`
	Reason   string `json:"reason"`
}

// Scores are measured on samples. Savings are projections for the sampled
// files, not a promise about the whole series or an already completed encode.
type BatchCalibrationEvidence struct {
	AcceptedSamples     int     `json:"accepted_samples"`
	MinimumSampleVMAF   float64 `json:"minimum_sample_vmaf"`
	EstimatedSavingsMin float64 `json:"estimated_savings_percent_min"`
	EstimatedSavingsMax float64 `json:"estimated_savings_percent_max"`
}

func (e *Engine) prepareAutomaticBatch(ctx context.Context, ec *ExecutionContext, isAnime bool) error {
	if getString(ec.State, "batch_auto_profile") != "" {
		return nil
	}
	// Opt in through existing inputs. Bare auto/default requests keep their
	// behavior; adding an intent must not impose calibration on direct users.
	if _, requested := ec.Inputs["priority"]; !requested && !getBool(ec.Inputs, "shared_calibration") {
		return nil
	}
	profile := strings.TrimSpace(getString(ec.Inputs, "profile"))
	if ec.Inputs["profile_config"] != nil || profile != "" && !strings.EqualFold(profile, "auto") {
		return nil
	}
	if disabled, ok := ec.Inputs["shared_calibration"].(bool); ok && !disabled {
		return nil // Explicit legacy routing remains available.
	}
	name, reason := "batch-generated", "bounded sample search; generated label is batch-only, not a catalog profile name"
	resolved := automaticBatchSearchProfile(isAnime)
	priority := getString(ec.Inputs, "priority")
	if priority == "" {
		priority = "balanced"
	}
	resolved = batchPriorityProfile(resolved, priority, "")
	_, digest, _, err := decodeEphemeralProfileInput(map[string]any{"profile_config": resolved})
	if err != nil {
		return err
	}
	ec.State["batch_auto_profile"] = name
	ec.State["batch_priority"] = priority
	ec.State["batch_selection"] = BatchSelection{Profile: name, Scope: "batch_only", Priority: priority, Reason: reason, Status: "pending_samples"}
	ec.State["shared_calibration_profile"] = resolved
	ec.State["shared_calibration_profile_digest"] = digest
	if err := validateBatchCalibrationInputs(ec.Inputs, "auto", ec.State); err != nil {
		return err
	}
	// Persist before creating items, including when resolve is interrupted.
	return e.persistExecutionState(ctx, ec)
}

// Only quality is searched: three candidates, three short windows, one pass.
// These are search bounds and acceptance gates, not a named library recipe.
// Encoder/preservation controls stay fixed to avoid a combinatorial search.
func automaticBatchSearchProfile(isAnime bool) recipe.Profile {
	ptr := func(v float64) *float64 { return &v }
	p := recipe.Profile{
		Container:  "mkv",
		Video:      recipe.VideoProfile{Codec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", Profile: "main", PixelFormat: "yuv420p"},
		Audio:      recipe.AudioProfile{Mode: "copy"},
		Subtitles:  recipe.SubtitleProfile{Mode: "preserve", ConvertIncompatible: true},
		Preserve:   recipe.PreserveProfile{Metadata: true, Chapters: true, Attachments: true},
		Resilience: recipe.ResilienceProfile{MaxAttempts: 1},
		Optimization: &recipe.OptimizationPolicy{
			Enabled:  true,
			Sampling: &recipe.SamplingPolicy{Strategy: "distributed", SampleCount: 3, SampleSeconds: 10, Positions: []float64{0.2, 0.5, 0.8}},
			Search:   &recipe.SearchPolicy{MaxCandidates: 3, QualityValues: []int{20, 23, 26}},
			Quality: &recipe.QualityPolicy{
				PreferredMetric: "vmaf",
				VMAF:            &recipe.MetricTarget{Model: "v1_1080p_3h", Target: 92, Minimum: 88, MarginalTolerance: ptr(2), GuardrailEnforcement: "reject", P5Minimum: ptr(84), WorstWindowMinimum: ptr(85), WorstWindowSeconds: 5},
				Banding:         &recipe.BandingPolicy{Enabled: true, Metric: "cambi", Mode: "full_ref", Enforcement: "reject", MaxMean: ptr(6), MaxPeak: ptr(16)},
				FinalValidation: &recipe.FinalValidationPolicy{Mode: "sampled"},
			},
		},
	}
	if isAnime {
		p.Video.Tune = "animation"
	}
	return p
}

// A single setting must pass on every representative. Size is estimated from
// samples, so use the least favorable saving and score to choose the recipe.
func chooseAutomaticBatchQuality(decisions []*transcode.BenchmarkDecision, search []int, minSavings float64, priority string) int {
	combined := &transcode.BenchmarkDecision{}
	if len(decisions) == 0 {
		return 0
	}
	for _, q := range search {
		var aggregate transcode.BenchmarkCandidateEvaluation
		for i, decision := range decisions {
			found := false
			if decision == nil {
				return 0
			}
			for _, ev := range decision.Evaluations {
				if ev.Quality != q {
					continue
				}
				found = true
				if i == 0 {
					aggregate = ev
				} else {
					aggregate.Eligible = aggregate.Eligible && ev.Eligible
					aggregate.MinimumMet = aggregate.MinimumMet && ev.MinimumMet
					aggregate.TargetReached = aggregate.TargetReached && ev.TargetReached
					aggregate.Score = min(aggregate.Score, ev.Score)
					aggregate.SavingsPercent = min(aggregate.SavingsPercent, ev.SavingsPercent)
					aggregate.EstimatedBytes = min(aggregate.EstimatedBytes, ev.EstimatedBytes)
				}
				if ev.VideoCodec != transcode.VideoCodecLibX265 || ev.MetricType != "vmaf" {
					aggregate.Eligible = false
				}
				break
			}
			if !found {
				aggregate.Eligible = false
				break
			}
		}
		combined.Evaluations = append(combined.Evaluations, aggregate)
	}
	return chooseBatchItemQuality(combined, search, minSavings, priority)
}
