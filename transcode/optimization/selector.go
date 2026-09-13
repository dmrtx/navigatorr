package optimization

import (
	"math"
	"sort"
)

// CandidateInput represents one candidate transcode variant evaluated for selection.
type CandidateInput struct {
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
	Policy QualityPolicy `json:"-"`
	// Candidates is the set of candidate transcode profiles/configurations tested.
	Candidates []CandidateInput `json:"candidates"`
}

// EvaluatedCandidate captures the post-evaluation metrics and eligibility status of a candidate.
type EvaluatedCandidate struct {
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
	Winner         *EvaluatedCandidate  `json:"winner,omitempty"`
	DecisionReason string               `json:"decision_reason"`
	AllEvaluated   []EvaluatedCandidate `json:"all_evaluated"`
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

	// Case 1: Target-reaching candidates exist
	if len(targetReaching) > 0 {
		// Find highest quality score among target-reaching candidates
		var maxScore float64 = -math.MaxFloat64
		for _, c := range targetReaching {
			if c.Score > maxScore {
				maxScore = c.Score
			}
		}

		// Filter candidates within marginal-quality tolerance of maxScore
		tolerance := in.Policy.Tolerance()
		scoreFloor := maxScore - tolerance

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
