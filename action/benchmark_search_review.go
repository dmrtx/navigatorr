package action

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

// searchReviewHistory bounds how many earlier rounds a review reads; only two
// comparable ones are needed, but rounds under a stricter goal are skipped.
const searchReviewHistory = 10

// SearchReviewRound is an earlier unsuccessful round shown at the review.
type SearchReviewRound struct {
	ActionID   string                  `json:"action_id"`
	Mode       string                  `json:"mode,omitempty"`
	Floor      *SearchReviewFloor      `json:"floor,omitempty"`
	Candidates []SearchReviewCandidate `json:"candidates,omitempty"`
}

// SearchReviewFloor is the VMAF floor a round was measured against.
type SearchReviewFloor struct {
	Minimum            float64  `json:"minimum,omitempty"`
	P5Minimum          *float64 `json:"p5_minimum,omitempty"`
	WorstWindowMinimum *float64 `json:"worst_window_minimum,omitempty"`
}

// SearchReviewCandidate is one setting an earlier round tried and its result.
type SearchReviewCandidate struct {
	ID                 string                          `json:"id"`
	VideoCodec         string                          `json:"video_codec,omitempty"`
	Quality            int                             `json:"quality"`
	Preset             string                          `json:"preset,omitempty"`
	Tune               string                          `json:"tune,omitempty"`
	AverageBitrateKbps int                             `json:"average_bitrate_kbps,omitempty"`
	Result             string                          `json:"result,omitempty"`
	VMAF               *transcode.BenchmarkVMAFSummary `json:"vmaf,omitempty"`
}

// searchReviewCandidates bounds each round's listed settings.
const searchReviewCandidates = 8

// SearchReview is the evidence behind a benchmark_search_review decision.
type SearchReview struct {
	Rounds []SearchReviewRound `json:"rounds"`
	// NewCandidates are the proposed settings no comparable round measured.
	NewCandidates []string `json:"new_candidates"`
}

// A new action/recipe must not silently restart an exhausted quality search.
// This gate only applies to new admissions, never tracking an accepted job.
// Only rounds whose quality floor and savings gate were no stricter than the
// proposal's count: a stricter goal's failure says nothing about a lenient
// one. When every proposed setting was already measured, another round would
// repeat that evidence, so only stopping is offered.
func (e *Engine) reviewBenchmarkSearch(ec *ExecutionContext) *StepResult {
	if getBool(ec.State, "benchmark_search_reviewed") || e.deps.Store == nil {
		return nil
	}
	var proposal transcode.BenchmarkRequest
	if raw, err := json.Marshal(ec.State["benchmark_request"]); err == nil {
		_ = json.Unmarshal(raw, &proposal)
	}
	review, err := e.searchReview(ec, &proposal)
	if err != nil {
		return &StepResult{Status: StepFailed, Error: fmt.Sprintf("checking previous benchmark outcomes: %v", err)}
	}
	if getBool(ec.State, "benchmark_search_review_required") {
		switch ec.Decision {
		case "run_once_after_review":
			ec.Decision = ""
			if review != nil && len(review.NewCandidates) == 0 {
				// Offered before every proposed setting was known to be
				// measured; repeating them is not authorized.
				break
			}
			ec.State["benchmark_search_reviewed"] = true
			delete(ec.State, "benchmark_search_review_required")
			return nil
		case "cancel", "reject":
			ec.Decision = ""
			return &StepResult{Status: StepCancelled, Outputs: map[string]any{"note": "Search stopped before submission; original unchanged."}}
		}
	}
	if review == nil {
		delete(ec.State, "benchmark_search_review_required")
		delete(ec.State, "benchmark_search_review")
		return nil
	}
	ids := make([]string, 0, len(review.Rounds))
	for _, round := range review.Rounds {
		ids = append(ids, round.ActionID)
	}
	ec.State["benchmark_search_review_required"] = true
	ec.State["previous_unsuccessful_benchmarks"] = ids
	ec.State["benchmark_search_review"] = review
	res := &StepResult{Status: StepWaitingDecision, WaitingCondition: "benchmark_search_review"}
	if len(review.NewCandidates) == 0 {
		res.WaitingReason = "Two earlier test rounds already tried every setting this test would use, with the same or a more lenient quality requirement, and none was acceptable. Running it again would repeat them, so it is not offered. The original is unchanged."
		res.WaitingOptions = []WaitingOption{{Decision: "cancel", Description: "Stop and keep the original."}}
		return res
	}
	res.WaitingReason = fmt.Sprintf("Two earlier test rounds found no acceptable setting for this file. This test would try %d %s not measured before (%s). Review the earlier results before testing again. The original is unchanged.", len(review.NewCandidates), plural(len(review.NewCandidates), "setting", "settings"), strings.Join(review.NewCandidates, ", "))
	res.WaitingOptions = []WaitingOption{
		{Decision: "cancel", Description: "Stop and keep the original."},
		{Decision: "run_once_after_review", Description: fmt.Sprintf("Run one more bounded sample test of the %d new %s with the same quality requirement. Only on the user's explicit instruction; never chosen automatically.", len(review.NewCandidates), plural(len(review.NewCandidates), "setting", "settings"))},
	}
	return res
}

// searchReview returns nil when fewer than two comparable rounds exist.
func (e *Engine) searchReview(ec *ExecutionContext, proposal *transcode.BenchmarkRequest) (*SearchReview, error) {
	ids, err := e.deps.Store.UnsuccessfulBenchmarkHistory(getString(ec.State, "original_sha256"), ec.InstanceID, searchReviewHistory)
	if err != nil {
		return nil, err
	}
	review := &SearchReview{Rounds: []SearchReviewRound{}, NewCandidates: []string{}}
	tried := map[string]bool{}
	for _, id := range ids {
		if len(review.Rounds) == 2 {
			break
		}
		inst, err := e.deps.Store.GetActionInstance(id)
		if err != nil {
			return nil, err
		}
		if inst == nil {
			continue
		}
		var state struct {
			Request  transcode.BenchmarkRequest                    `json:"benchmark_request"`
			Decision *transcode.BenchmarkDecision                  `json:"benchmark_decision"`
			Quality  []transcode.BenchmarkCandidateQualityEvidence `json:"benchmark_quality"`
		}
		if json.Unmarshal([]byte(inst.StateJSON), &state) != nil || !roundComparable(&state.Request, proposal) {
			continue
		}
		var inputs map[string]any
		_ = json.Unmarshal([]byte(inst.InputsJSON), &inputs)
		round := SearchReviewRound{ActionID: id, Mode: getString(inputs, "mode"), Floor: reviewFloor(state.Request.Quality)}
		if state.Decision != nil {
			transcode.AttachVMAFEvidence(state.Decision, state.Quality)
			byID := map[string]transcode.BenchmarkCandidate{}
			for _, c := range state.Request.Candidates {
				byID[c.ID] = c
			}
			for _, ev := range state.Decision.Evaluations {
				c, ok := byID[ev.CandidateID]
				if !ok {
					c = transcode.BenchmarkCandidate{ID: ev.CandidateID, VideoCodec: ev.VideoCodec, Quality: ev.Quality, Preset: ev.Preset, Tune: ev.Tune, AverageBitrateKbps: ev.AverageBitrateKbps, VideoProfile: ev.VideoProfile, PixelFormat: ev.PixelFormat}
				}
				if len(round.Candidates) < searchReviewCandidates {
					round.Candidates = append(round.Candidates, SearchReviewCandidate{ID: c.ID, VideoCodec: transcode.BenchmarkCandidateVideoCodec(c), Quality: c.Quality, Preset: c.Preset, Tune: c.Tune, AverageBitrateKbps: c.AverageBitrateKbps, Result: ev.EvaluationReason, VMAF: ev.VMAF})
				}
				if candidateMeasured(ev) {
					tried[candidateIdentity(c)] = true
				}
			}
		}
		review.Rounds = append(review.Rounds, round)
	}
	if len(review.Rounds) < 2 {
		return nil, nil
	}
	for _, c := range proposal.Candidates {
		if !tried[candidateIdentity(c)] {
			review.NewCandidates = append(review.NewCandidates, c.ID)
		}
	}
	return review, nil
}

// roundComparable reports whether a failure under prior implies the proposal
// would fail too: each prior limit is no stricter than the proposal's. An
// unset limit is the most lenient value.
func roundComparable(prior, proposal *transcode.BenchmarkRequest) bool {
	if prior.MinSavingsPercent > proposal.MinSavingsPercent {
		return false
	}
	// A different audio plan, compact transform, metric, sample set or goal
	// policy measures a different output; its verdict is not evidence here.
	if prior.AudioMode != proposal.AudioMode || prior.Metric != proposal.Metric || prior.ModePolicyVersion != proposal.ModePolicyVersion || !sameJSON(prior.Compact, proposal.Compact) || (len(prior.Samples) > 0 || len(proposal.Samples) > 0) && !sameJSON(prior.Samples, proposal.Samples) {
		return false
	}
	pq, nq := prior.Quality, proposal.Quality
	if pq == nil {
		return true
	}
	if nq == nil {
		nq = &transcode.BenchmarkQualityConfig{}
	}
	for _, pair := range [][2]*transcode.BenchmarkQualityThresholds{{pq.VMAF, nq.VMAF}, {pq.SSIM, nq.SSIM}} {
		p, n := pair[0], pair[1]
		if p == nil {
			continue
		}
		if n == nil {
			n = &transcode.BenchmarkQualityThresholds{}
		}
		if p.Minimum > n.Minimum || floorValue(p.P5Minimum, p.GuardrailEnforcement) > floorValue(n.P5Minimum, n.GuardrailEnforcement) || floorValue(p.WorstWindowMinimum, p.GuardrailEnforcement) > floorValue(n.WorstWindowMinimum, n.GuardrailEnforcement) {
			return false
		}
	}
	return bandingLimit(pq.Banding, func(b *transcode.BenchmarkBandingConfig) *float64 { return b.MaxMean }) >= bandingLimit(nq.Banding, func(b *transcode.BenchmarkBandingConfig) *float64 { return b.MaxMean }) &&
		bandingLimit(pq.Banding, func(b *transcode.BenchmarkBandingConfig) *float64 { return b.MaxPeak }) >= bandingLimit(nq.Banding, func(b *transcode.BenchmarkBandingConfig) *float64 { return b.MaxPeak })
}

// floorValue treats an observe-only guardrail as no limit.
func floorValue(limit *float64, enforcement string) float64 {
	if limit == nil || enforcement != "reject" {
		return 0
	}
	return *limit
}

func bandingLimit(b *transcode.BenchmarkBandingConfig, limit func(*transcode.BenchmarkBandingConfig) *float64) float64 {
	if b == nil || !b.Enabled || b.Enforcement != "reject" || limit(b) == nil {
		return math.Inf(1)
	}
	return *limit(b)
}

func reviewFloor(q *transcode.BenchmarkQualityConfig) *SearchReviewFloor {
	if q == nil || q.VMAF == nil {
		return nil
	}
	return &SearchReviewFloor{Minimum: q.VMAF.Minimum, P5Minimum: q.VMAF.P5Minimum, WorstWindowMinimum: q.VMAF.WorstWindowMinimum}
}

// measuredVerdicts are evaluation reasons that are a quality or savings
// result for the setting, including a skip inferred from a failed
// higher-quality or smaller sibling. Encode, metric and estimate failures are
// technical and never count as the setting having been tried.
var measuredVerdicts = map[string]bool{
	"sample_below_minimum_quality":             true,
	"below_minimum_quality":                    true,
	"insufficient_savings":                     true,
	"insufficient_savings_before_metrics":      true,
	"skipped_quality_dominated":                true,
	"skipped_savings_dominated":                true,
	"quality_vmaf_mean_below_minimum":          true,
	"quality_vmaf_sample_below_minimum":        true,
	"quality_vmaf_p5_below_minimum":            true,
	"quality_vmaf_worst_window_below_minimum":  true,
	"quality_vmaf_frames_below_limit_exceeded": true,
	"quality_vmaf_selection_margin_not_met":    true,
	"quality_ssim_mean_below_minimum":          true,
	"quality_cambi_mean_above_maximum":         true,
	"quality_cambi_peak_above_maximum":         true,
}

func candidateMeasured(ev transcode.BenchmarkCandidateEvaluation) bool {
	return measuredVerdicts[ev.EvaluationReason]
}

// candidateIdentity ignores the candidate ID, which differs between rounds.
func candidateIdentity(c transcode.BenchmarkCandidate) string {
	c.ID = ""
	c.VideoCodec = transcode.BenchmarkCandidateVideoCodec(c)
	raw, _ := json.Marshal(c)
	return string(raw)
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
