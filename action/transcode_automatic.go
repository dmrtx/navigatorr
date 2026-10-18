package action

import (
	"fmt"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// automaticFallbacks are the slow-preset CRF families the worker may try
// after discovery: quality when nothing qualified, savings otherwise.
type automaticFallbacks struct {
	Quality, Savings []int
}

var defaultAutomaticFallbacks = automaticFallbacks{Quality: []int{16, 18, 20}, Savings: []int{23, 26, 28}}

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
	for _, position := range []float64{0.35, 0.65} {
		req.Samples = append(req.Samples, transcode.BenchmarkSampleWindow{Index: len(req.Samples), StartSeconds: req.SourceDuration*position - seconds/2, DurationSeconds: seconds, CenterSeconds: req.SourceDuration * position})
	}
	if plan.VideoCodec != transcode.VideoCodecLibX265 {
		return nil
	}
	slow := *plan
	slow.Preset = "slow"
	for _, family := range []struct {
		prefix    string
		qualities []int
	}{{"fallback-quality-", families.Quality}, {"fallback-savings-", families.Savings}} {
		candidates, err := buildBenchmarkCandidates(&slow, bitDepth, &recipe.SearchPolicy{MaxCandidates: 3, QualityValues: family.qualities})
		if err != nil {
			return err
		}
		for i := range candidates {
			candidates[i].ID = family.prefix + candidates[i].ID
		}
		req.Candidates = append(req.Candidates, candidates...)
	}
	return nil
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
