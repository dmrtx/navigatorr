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
		if families.Rules != nil && !caps.AutomaticGridVerification {
			// A worker without grid verification rejects its rule fields and its
			// holdout value: it verifies on the two edge windows, twice at most.
			families.Rules.VerifyUntilExhausted = false
			families.Rules.FinalFreshWindows = 0
			if families.Rules.HoldoutWindows == transcode.HoldoutWindowsFinalGrid {
				families.Rules.HoldoutWindows = transcode.HoldoutWindowsFinalGridEdges
			}
			restoreEdgeSelectionMargin(families.Rules)
		}
	}
	return families
}

// restoreEdgeSelectionMargin gives a search that cannot verify on the final
// grid the selection margin the edge holdouts need. A grid-verifying snapshot
// freezes none, because grid verification measures every final window; two
// edge windows do not, so a thin pass there is a risk the margin was added for.
// A margin the snapshot froze is kept.
func restoreEdgeSelectionMargin(rules *transcode.AutomaticSearchRules) {
	if rules.SelectionMarginVMAF <= 0 {
		rules.SelectionMarginVMAF = sizeEdgeSelectionMarginVMAF
	}
}

// edgeMarginRestored reports a request whose margin was restored over a
// snapshot that froze none.
func edgeMarginRestored(req *transcode.BenchmarkRequest, policy *ModePolicySnapshot) bool {
	return req.AutomaticSearch != nil && req.AutomaticSearch.SelectionMarginVMAF > 0 && policy.Size != nil && policy.Size.AutomaticSearch != nil && policy.Size.AutomaticSearch.SelectionMarginVMAF <= 0
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
	// Verification on the final validation grid replaces the discovery and
	// holdout windows with the grid itself: a candidate is then verified on the
	// windows final validation re-measures on its full encode.
	grid := gridVerificationWindows(req, plan, families)
	if grid != nil {
		req.Samples = grid
	} else {
		seconds := min(10.0, req.SourceDuration/12)
		for i := range req.Samples {
			req.Samples[i].DurationSeconds = seconds
			req.Samples[i].StartSeconds = req.Samples[i].CenterSeconds - seconds/2
		}
		for _, center := range holdoutCenters(req, seconds, families) {
			req.Samples = append(req.Samples, transcode.BenchmarkSampleWindow{Index: len(req.Samples), StartSeconds: center - seconds/2, DurationSeconds: seconds, CenterSeconds: center})
		}
	}
	if plan.VideoCodec != transcode.VideoCodecLibX265 {
		return nil
	}
	rules := families.Rules
	if rules != nil {
		req.AutomaticSearch = rules.Clone()
		if grid == nil {
			// A source too short for the grid keeps the earlier 35%/65% holdouts
			// and no fresh windows (there is no room for them).
			if req.AutomaticSearch.HoldoutWindows == transcode.HoldoutWindowsFinalGrid {
				req.AutomaticSearch.HoldoutWindows = ""
				restoreEdgeSelectionMargin(req.AutomaticSearch)
			}
			req.AutomaticSearch.FinalFreshWindows = 0
		}
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

// gridVerificationWindows lays out the sample windows of a search whose rules
// verify on the final validation grid: three of its interior windows for
// discovery, the rest for verification. It is nil for any other search and for
// a source too short for the grid.
func gridVerificationWindows(req *transcode.BenchmarkRequest, plan *transcode.Plan, families automaticFallbacks) []transcode.BenchmarkSampleWindow {
	if families.Rules == nil || families.Rules.HoldoutWindows != transcode.HoldoutWindowsFinalGrid || families.Size == nil || plan.VideoCodec != transcode.VideoCodecLibX265 {
		return nil
	}
	s := families.Size
	return transcode.GridVerificationWindows(req.SourceDuration, float64(s.FinalWindowSeconds), float64(s.FinalWindowEverySeconds), s.FinalWindowsMinimum, s.FinalWindowsMaximum)
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
	windows, _ := sizeFinalWindowsWithFresh(ec, duration, 0)
	return windows
}

// sizeFinalWindowsWithFresh also places up to fresh windows that the search
// never measured (see transcode.FreshValidationWindows).
func sizeFinalWindowsWithFresh(ec *ExecutionContext, duration float64, fresh int) (grid, extra []transcode.BenchmarkSampleWindow) {
	if getString(ec.Inputs, "mode") != "size" {
		return nil, nil
	}
	p, err := freezeModePolicy(ec)
	if err != nil || p == nil || p.Size == nil {
		return nil, nil
	}
	s := p.Size
	grid = transcode.PlanFinalValidationWindows(duration, float64(s.FinalWindowSeconds), float64(s.FinalWindowEverySeconds), s.FinalWindowsMinimum, s.FinalWindowsMaximum)
	if grid != nil && fresh > 0 {
		extra = transcode.FreshValidationWindows(grid, duration, float64(s.FinalWindowSeconds), fresh)
	}
	return grid, extra
}
