package optimization

import (
	"math"
	"sort"
)

// CandidateInput represents one candidate transcode variant evaluated for selection.
type CandidateInput struct {
	VideoCodec      string           `json:"video_codec,omitempty"`
	MeasuredSeconds *float64         `json:"measured_seconds,omitempty"`
	CandidateID     string           `json:"candidate_id"`
	Profile         string           `json:"profile,omitempty"`
	EncodeSuccess   bool             `json:"encode_success"`
	AggregateResult MetricAggregate  `json:"aggregate_result"`
	ColorInfo       ColorInfo        `json:"color_info"`
	EstimatedOutput EstimationResult `json:"estimated_output"`
}

// SelectorInput encapsulates the inputs provided to the CandidateSelector.
type SelectorInput struct {
	// Policy is the quality policy (e.g. VMAFPolicy or SSIMPolicy) used to evaluate candidates.
	Policy      QualityPolicy `json:"-"`
	Mode        string        `json:"mode,omitempty"`
	SourceBytes int64         `json:"source_bytes,omitempty"`
	// Candidates is the set of candidate transcode profiles/configurations tested.
	Candidates []CandidateInput `json:"candidates"`
}

// EvaluatedCandidate captures the post-evaluation metrics and eligibility status of a candidate.
type EvaluatedCandidate struct {
	VideoCodec       string   `json:"video_codec,omitempty"`
	MeasuredSeconds  *float64 `json:"measured_seconds,omitempty"`
	CandidateID      string   `json:"candidate_id"`
	Profile          string   `json:"profile,omitempty"`
	Score            float64  `json:"score"`
	EstimatedBytes   int64    `json:"estimated_bytes"`
	EstimatedMB      float64  `json:"estimated_mb"`
	SavingsPercent   float64  `json:"savings_percent"`
	Eligible         bool     `json:"eligible"`
	TargetReached    bool     `json:"target_reached"`
	MinimumMet       bool     `json:"minimum_met"`
	EvaluationReason string   `json:"evaluation_reason"`
	Uncertainties    []string `json:"uncertainties,omitempty"`
}

// SelectionResult represents the explainable, deterministic output of candidate selection.
type SelectionResult struct {
	Selection      *SelectionExplanation `json:"selection,omitempty"`
	Winner         *EvaluatedCandidate   `json:"winner,omitempty"`
	DecisionReason string                `json:"decision_reason"`
	AllEvaluated   []EvaluatedCandidate  `json:"all_evaluated"`
}

// SelectionExplanation exposes the existing quality-versus-size policy.
// Excluded candidates remain eligible; they are outside the final size contest.
type SelectionExplanation struct {
	BestScore     float64  `json:"best_score"`
	Tolerance     float64  `json:"marginal_tolerance"`
	ScoreFloor    float64  `json:"score_floor"`
	OutsideMargin []string `json:"outside_quality_margin,omitempty"`
}

func ExplainTargetSelection(candidates []EvaluatedCandidate, tolerance float64) *SelectionExplanation {
	var out *SelectionExplanation
	for _, c := range candidates {
		if c.Eligible && c.TargetReached && (out == nil || c.Score > out.BestScore) {
			out = &SelectionExplanation{BestScore: c.Score, Tolerance: tolerance, ScoreFloor: c.Score - tolerance}
		}
	}
	if out != nil {
		for _, c := range candidates {
			if c.Eligible && c.TargetReached && c.Score < out.ScoreFloor {
				out.OutsideMargin = append(out.OutsideMargin, c.CandidateID)
			}
		}
	}
	return out
}

// SelectCandidate implements deterministic quality/size trade-off candidate selection:
//  1. Rejects invalid, failed, or below-minimum candidates.
//  2. Among target-reaching candidates, chooses the candidate with the smallest estimated output
//     within the policy's marginal-quality tolerance relative to the highest-quality target-reaching candidate.
//  3. If none reaches target but some meet minimum quality, chooses the candidate with the highest quality
//     (using smaller estimated output as a tie-breaker).
//  4. If no candidate meets minimum quality or is valid, returns no winner with a deterministic reason code.
func SelectCandidate(in SelectorInput) SelectionResult {
	res := SelectionResult{
		AllEvaluated: make([]EvaluatedCandidate, 0, len(in.Candidates)),
	}

	if len(in.Candidates) == 0 {
		res.DecisionReason = ReasonNoCandidatesProvided
		return res
	}

	if err := ValidatePolicy(in.Policy); err != nil {
		for _, c := range in.Candidates {
			res.AllEvaluated = append(res.AllEvaluated, EvaluatedCandidate{
				CandidateID:      c.CandidateID,
				Profile:          c.Profile,
				EstimatedBytes:   c.EstimatedOutput.EstimatedTotalBytes,
				EstimatedMB:      c.EstimatedOutput.EstimatedTotalMB,
				SavingsPercent:   c.EstimatedOutput.SavingsPercent,
				Eligible:         false,
				EvaluationReason: ReasonNoValidMetric,
				Uncertainties:    c.EstimatedOutput.Uncertainties,
			})
		}
		res.DecisionReason = ReasonNoValidMetric
		return res
	}

	var targetReaching []EvaluatedCandidate
	var minimumMeeting []EvaluatedCandidate
	allBelowMin := true

	for _, c := range in.Candidates {
		ec := EvaluatedCandidate{
			VideoCodec: c.VideoCodec, MeasuredSeconds: c.MeasuredSeconds,
			CandidateID:    c.CandidateID,
			Profile:        c.Profile,
			EstimatedBytes: c.EstimatedOutput.EstimatedTotalBytes,
			EstimatedMB:    c.EstimatedOutput.EstimatedTotalMB,
			SavingsPercent: c.EstimatedOutput.SavingsPercent,
			Uncertainties:  c.EstimatedOutput.Uncertainties,
		}

		if !c.EncodeSuccess {
			ec.Eligible = false
			ec.EvaluationReason = ReasonCandidateEncodeFailed
			allBelowMin = false
			res.AllEvaluated = append(res.AllEvaluated, ec)
			continue
		}

		// Estimate usability gate: candidates with invalid/missing video or nonpositive bytes must never win.
		if !c.EstimatedOutput.SuitableForSelection || c.EstimatedOutput.EstimatedVideoBytes <= 0 || c.EstimatedOutput.EstimatedTotalBytes <= 0 {
			ec.Eligible = false
			ec.TargetReached = false
			ec.MinimumMet = false
			if c.EstimatedOutput.UnusableReason != "" {
				ec.EvaluationReason = c.EstimatedOutput.UnusableReason
			} else {
				ec.EvaluationReason = ReasonUnusableEstimate
			}
			allBelowMin = false
			res.AllEvaluated = append(res.AllEvaluated, ec)
			continue
		}

		eval := in.Policy.Evaluate(c.AggregateResult, c.ColorInfo)
		ec.Score = eval.CandidateScore
		ec.Eligible = eval.Eligible
		ec.TargetReached = eval.TargetReached
		ec.MinimumMet = eval.MinimumMet
		ec.EvaluationReason = eval.IneligibleReason

		if eval.IneligibleReason != ReasonBelowMinimumQuality && eval.IneligibleReason != ReasonSampleBelowMinimum {
			allBelowMin = false
		}

		if ec.Eligible {
			if ec.TargetReached {
				targetReaching = append(targetReaching, ec)
			} else if ec.MinimumMet {
				minimumMeeting = append(minimumMeeting, ec)
			}
		}

		res.AllEvaluated = append(res.AllEvaluated, ec)
	}

	if in.Mode != "" {
		return SelectMeasuredCandidates(res.AllEvaluated, in.Mode, in.Policy.Tolerance(), in.SourceBytes)
	}
	// Case 1: Target-reaching candidates exist
	if len(targetReaching) > 0 {
		res.Selection = ExplainTargetSelection(targetReaching, in.Policy.Tolerance())
		scoreFloor := res.Selection.ScoreFloor

		var qualified []EvaluatedCandidate
		for _, c := range targetReaching {
			if c.Score >= scoreFloor {
				qualified = append(qualified, c)
			}
		}

		// Choose smallest estimated output within tolerance; tie-break on higher score, then ID
		sort.Slice(qualified, func(i, j int) bool {
			if qualified[i].EstimatedBytes != qualified[j].EstimatedBytes {
				return qualified[i].EstimatedBytes < qualified[j].EstimatedBytes
			}
			if qualified[i].Score != qualified[j].Score {
				return qualified[i].Score > qualified[j].Score
			}
			return qualified[i].CandidateID < qualified[j].CandidateID
		})

		winner := qualified[0]
		res.Winner = &winner
		res.DecisionReason = ReasonTargetReachedSmallestSize
		return res
	}

	// Case 2: No candidate reached target, but some met minimum quality
	if len(minimumMeeting) > 0 {
		// Choose candidate with highest quality score; tie-break on smaller size, then ID
		sort.Slice(minimumMeeting, func(i, j int) bool {
			if math.Abs(minimumMeeting[i].Score-minimumMeeting[j].Score) > 1e-6 {
				return minimumMeeting[i].Score > minimumMeeting[j].Score
			}
			if minimumMeeting[i].EstimatedBytes != minimumMeeting[j].EstimatedBytes {
				return minimumMeeting[i].EstimatedBytes < minimumMeeting[j].EstimatedBytes
			}
			return minimumMeeting[i].CandidateID < minimumMeeting[j].CandidateID
		})

		winner := minimumMeeting[0]
		res.Winner = &winner
		res.DecisionReason = ReasonMinimumMetHighestQuality
		return res
	}

	// Case 3: No valid candidates met minimum quality
	res.Winner = nil
	if allBelowMin && len(in.Candidates) > 0 {
		res.DecisionReason = ReasonNoCandidateMetMinimumQuality
	} else {
		res.DecisionReason = ReasonAllCandidatesInvalid
	}

	return res
}

// SelectMeasuredCandidates selects complete, already evaluated plans. Codec/rate
// numbers never participate in ranking. The 2% equivalence band is relative to
// the smallest qualifying estimate, keeping the ordering transitive and stable.
func SelectMeasuredCandidates(candidates []EvaluatedCandidate, mode string, tolerance float64, sourceBytes int64) SelectionResult {
	r := SelectionResult{AllEvaluated: append([]EvaluatedCandidate(nil), candidates...), DecisionReason: ReasonAllCandidatesInvalid}
	if mode != "size" && mode != "quality" && mode != "x265_preserve" {
		r.DecisionReason = "invalid_mode"
		return r
	}
	if math.IsNaN(tolerance) || math.IsInf(tolerance, 0) || tolerance < 0 {
		r.DecisionReason = "invalid_tolerance"
		return r
	}
	var eligible []EvaluatedCandidate
	for i := range r.AllEvaluated {
		c := &r.AllEvaluated[i]
		if !c.Eligible || !c.MinimumMet || c.EstimatedBytes <= 0 || math.IsNaN(c.Score) || math.IsInf(c.Score, 0) {
			c.Eligible = false
			continue
		}
		if mode == "x265_preserve" && c.VideoCodec != "libx265" {
			c.Eligible = false
			c.EvaluationReason = "encoder_not_allowed"
			continue
		}
		if sourceBytes <= 0 {
			c.Eligible = false
			c.EvaluationReason = "source_size_unknown"
			continue
		}
		// Compare unrounded bytes; avoid integer multiplication overflow.
		if !ModeSizeAllowed(mode, sourceBytes, c.EstimatedBytes) {
			c.Eligible = false
			c.EvaluationReason = "insufficient_savings"
			continue
		}
		eligible = append(eligible, *c)
	}
	if len(eligible) == 0 {
		return r
	}
	if mode != "size" {
		best := eligible[0].Score
		for _, c := range eligible {
			if c.Score > best {
				best = c.Score
			}
		}
		r.Selection = &SelectionExplanation{BestScore: best, Tolerance: tolerance, ScoreFloor: best - tolerance}
		filtered := eligible[:0]
		for _, c := range eligible {
			if c.Score >= best-tolerance {
				filtered = append(filtered, c)
			} else {
				r.Selection.OutsideMargin = append(r.Selection.OutsideMargin, c.CandidateID)
			}
		}
		eligible = filtered
	}
	smallest := eligible[0].EstimatedBytes
	for _, c := range eligible {
		if c.EstimatedBytes < smallest {
			smallest = c.EstimatedBytes
		}
	}
	var finalists []EvaluatedCandidate
	for _, c := range eligible {
		if float64(c.EstimatedBytes) <= float64(smallest)*1.02 {
			finalists = append(finalists, c)
		}
	}
	sort.Slice(finalists, func(i, j int) bool {
		a, b := finalists[i], finalists[j]
		knownA := a.MeasuredSeconds != nil && *a.MeasuredSeconds > 0 && !math.IsNaN(*a.MeasuredSeconds) && !math.IsInf(*a.MeasuredSeconds, 0)
		knownB := b.MeasuredSeconds != nil && *b.MeasuredSeconds > 0 && !math.IsNaN(*b.MeasuredSeconds) && !math.IsInf(*b.MeasuredSeconds, 0)
		if knownA != knownB {
			return knownA
		}
		if knownA && *a.MeasuredSeconds != *b.MeasuredSeconds {
			return *a.MeasuredSeconds < *b.MeasuredSeconds
		}
		return a.CandidateID < b.CandidateID
	})
	winner := finalists[0]
	r.Winner = &winner
	r.DecisionReason = "measured_plan_selected"
	return r
}

// ModeSizeAllowed compares fixed mode policies with exact integer bytes. Splitting
// the quotient and remainder avoids overflow even at MaxInt64.
func ModeSizeAllowed(mode string, source, candidate int64) bool {
	if source <= 0 || candidate <= 0 {
		return false
	}
	if mode == "x265_preserve" {
		return candidate <= source
	}
	if mode != "size" && mode != "quality" {
		return false
	}
	maximum := (source/100)*85 + ((source%100)*85)/100
	return candidate <= maximum
}
