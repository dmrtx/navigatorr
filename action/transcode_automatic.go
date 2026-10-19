package action

import (
	"fmt"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// automaticFallbacks are the CRF families the worker may try after
// discovery: quality when nothing qualified, savings otherwise. Rules, when
// set, are size v3's frozen search rules for a worker that runs them; size
// supplies its final validation grid.
type automaticFallbacks struct {
	Quality, Savings []int
	Rules            *transcode.AutomaticSearchRules
	Size             *ModeSizePolicy
}

var defaultAutomaticFallbacks = automaticFallbacks{Quality: []int{16, 18, 20}, Savings: []int{23, 26, 28}}

// policyAutomaticFallbacks reads a frozen policy's fallback families. Its
// search rules are sent only to a worker that advertises them (older workers
// reject the field); otherwise the search keeps the earlier rules.
func policyAutomaticFallbacks(policy *ModePolicySnapshot, caps transcode.WorkerCapabilities) automaticFallbacks {
	if policy == nil || policy.Size == nil {
		return defaultAutomaticFallbacks
	}
	families := automaticFallbacks{Quality: policy.Size.FallbackQualityValues, Savings: policy.Size.FallbackSavingsValues, Size: policy.Size}
	if caps.AutomaticSearchRules && policy.Size.AutomaticSearch.Validate() == nil {
		families.Rules = policy.Size.AutomaticSearch.Clone()
	}
	return families
}

// Candidate alternatives are frozen before submission. The worker chooses which
// family to test from measured rejections, without creating another action.
func configureAutomaticRequest(req *transcode.BenchmarkRequest, plan *transcode.Plan, bitDepth int, families automaticFallbacks) error {
	if req.SourceDuration < 50 {
		return nil
	}
	if len(req.Samples) != 3 {
		return fmt.Errorf("automatic search requires three discovery windows")
	}
	req.Automatic = true
	seconds := min(10.0, req.SourceDuration/12)
	for i := range req.Samples {
		req.Samples[i].DurationSeconds = seconds
		req.Samples[i].StartSeconds = req.Samples[i].CenterSeconds - seconds/2
	}
	for _, center := range holdoutCenters(req, seconds, families) {
		req.Samples = append(req.Samples, transcode.BenchmarkSampleWindow{Index: len(req.Samples), StartSeconds: center - seconds/2, DurationSeconds: seconds, CenterSeconds: center})
	}
	if plan.VideoCodec != transcode.VideoCodecLibX265 {
		return nil
	}
	rules := families.Rules
	if rules != nil {
		req.AutomaticSearch = rules.Clone()
	}
	// Without rules the quality family is slow only. With rules it is tried
	// preset by preset in the frozen order; slow keeps its earlier IDs.
	qualityPresets := []string{"slow"}
	if rules != nil && len(rules.QualityFallbackPresets) > 0 {
		qualityPresets = rules.QualityFallbackPresets
	}
	type family struct {
		prefix, preset string
		qualities      []int
	}
	var ordered []family
	for _, preset := range qualityPresets {
		prefix := "fallback-quality-"
		if preset != "slow" {
			prefix += preset + "-"
		}
		ordered = append(ordered, family{prefix, preset, families.Quality})
	}
	ordered = append(ordered, family{"fallback-savings-", "slow", families.Savings})
	for _, f := range ordered {
		alt := *plan
		alt.Preset = f.preset
		candidates, err := buildBenchmarkCandidates(&alt, bitDepth, &recipe.SearchPolicy{MaxCandidates: 3, QualityValues: f.qualities})
		if err != nil {
			return err
		}
		for i := range candidates {
			candidates[i].ID = f.prefix + candidates[i].ID
		}
		req.Candidates = append(req.Candidates, candidates...)
	}
	return nil
}

// holdoutCenters places the two holdout windows. Under rules that ask for it
// they sit on the first and last positions of the final validation grid, the
// start and end of the source that final validation checks and discovery
// (20/50/80%) never samples. Otherwise, or when the grid does not fit beside
// the discovery windows, they stay at 35% and 65%.
func holdoutCenters(req *transcode.BenchmarkRequest, seconds float64, families automaticFallbacks) []float64 {
	legacy := []float64{req.SourceDuration * 0.35, req.SourceDuration * 0.65}
	if families.Rules == nil || families.Rules.HoldoutWindows != transcode.HoldoutWindowsFinalGridEdges || families.Size == nil {
		return legacy
	}
	s := families.Size
	grid := transcode.PlanFinalValidationWindows(req.SourceDuration, float64(s.FinalWindowSeconds), float64(s.FinalWindowEverySeconds), s.FinalWindowsMinimum, s.FinalWindowsMaximum)
	if len(grid) < 2 {
		return legacy
	}
	centers := []float64{grid[0].CenterSeconds, grid[len(grid)-1].CenterSeconds}
	for _, c := range centers {
		start, end := c-seconds/2, c+seconds/2
		if start < 0 || end > req.SourceDuration {
			return legacy
		}
		for _, w := range req.Samples {
			if start < w.StartSeconds+w.DurationSeconds && w.StartSeconds < end {
				return legacy
			}
		}
	}
	return centers
}

// sizeFinalWindows plans size v3's final validation windows from the frozen
// policy: one per FinalWindowEverySeconds of source, within its bounds. Other
// policies, and sources too short for them, keep the benchmark windows (nil).
func sizeFinalWindows(ec *ExecutionContext, duration float64) []transcode.BenchmarkSampleWindow {
	if getString(ec.Inputs, "mode") != "size" {
		return nil
	}
	p, err := freezeModePolicy(ec)
	if err != nil || p == nil || p.Size == nil {
		return nil
	}
	s := p.Size
	return transcode.PlanFinalValidationWindows(duration, float64(s.FinalWindowSeconds), float64(s.FinalWindowEverySeconds), s.FinalWindowsMinimum, s.FinalWindowsMaximum)
}
