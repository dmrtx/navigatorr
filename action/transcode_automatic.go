package action

import (
	"fmt"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// Candidate alternatives are frozen before submission. The worker chooses which
// family to test from measured rejections, without creating another action.
func configureAutomaticRequest(req *transcode.BenchmarkRequest, plan *transcode.Plan, bitDepth int) error {
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
	}{{"fallback-quality-", []int{16, 18, 20}}, {"fallback-savings-", []int{23, 26, 28}}} {
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
