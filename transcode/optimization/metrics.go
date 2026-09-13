package optimization

import (
	"fmt"
	"math"
)

// SampleScore holds the metric score for an individual sample segment.
type SampleScore struct {
	SampleIndex int     `json:"sample_index"`
	Score       float64 `json:"score"`
	Valid       bool    `json:"valid"`
	Error       string  `json:"error,omitempty"`
}

// MetricAggregate aggregates per-sample metric scores deterministically.
type MetricAggregate struct {
	MetricType       MetricType    `json:"metric_type"`
	SampleScores     []SampleScore `json:"sample_scores"`
	Valid            bool          `json:"valid"`
	MeanScore        float64       `json:"mean_score"`
	MinScore         float64       `json:"min_score"`
	MaxScore         float64       `json:"max_score"`
	IneligibleReason string        `json:"ineligible_reason,omitempty"`
}

// AggregateSampleScores computes deterministic aggregate metrics over sample scores.
// If any sample score is invalid or the score list is empty, Valid is marked false.
func AggregateSampleScores(metric MetricType, scores []SampleScore) MetricAggregate {
	agg := MetricAggregate{
		MetricType:   metric,
		SampleScores: scores,
	}

	if len(scores) == 0 {
		agg.Valid = false
		agg.IneligibleReason = ReasonNoValidSampleScores
		return agg
	}

	var sum float64
	minScore := math.MaxFloat64
	maxScore := -math.MaxFloat64
	allValid := true

	for _, s := range scores {
		if !s.Valid {
			allValid = false
			continue
		}
		sum += s.Score
		if s.Score < minScore {
			minScore = s.Score
		}
		if s.Score > maxScore {
			maxScore = s.Score
		}
	}

	if !allValid {
		agg.Valid = false
		agg.IneligibleReason = ReasonIncompleteSampleScores
		return agg
	}

	n := float64(len(scores))
	agg.Valid = true
	agg.MeanScore = round4(sum / n)
	agg.MinScore = round4(minScore)
	agg.MaxScore = round4(maxScore)

	return agg
}

// PolicyEvaluation represents the outcome of evaluating a candidate's metric results against a quality policy.
type PolicyEvaluation struct {
	MetricType       MetricType `json:"metric_type"`
	TargetScore      float64    `json:"target_score"`
	MinScore         float64    `json:"min_score"`
	CandidateScore   float64    `json:"candidate_score"`
	Eligible         bool       `json:"eligible"`
	TargetReached    bool       `json:"target_reached"`
	MinimumMet       bool       `json:"minimum_met"`
	IneligibleReason string     `json:"ineligible_reason,omitempty"`
}

// QualityPolicy defines the contract for metric-specific quality policies.
// Implementations MUST NOT convert thresholds across metric types (e.g. VMAF and SSIM remain separate).
type QualityPolicy interface {
	Metric() MetricType
	TargetScore() float64
	MinScore() float64
	Tolerance() float64
	Evaluate(agg MetricAggregate, color ColorInfo) PolicyEvaluation
}

// VMAFPolicy enforces quality thresholds on the VMAF scale (0 - 100).
type VMAFPolicy struct {
	Target            float64 `json:"target"`
	Min               float64 `json:"min"`
	MarginalTolerance float64 `json:"marginal_tolerance"`
}

// NewVMAFPolicy creates a VMAF policy with explicit target, minimum, and marginal tolerance.
func NewVMAFPolicy(target, min, marginalTolerance float64) *VMAFPolicy {
	return &VMAFPolicy{
		Target:            target,
		Min:               min,
		MarginalTolerance: marginalTolerance,
	}
}

// DefaultVMAFPolicy returns the approved standard SDR VMAF policy (target 96.0, min 95.0, marginal tolerance 0.5).
func DefaultVMAFPolicy() *VMAFPolicy {
	return NewVMAFPolicy(96.0, 95.0, 0.5)
}

func (p *VMAFPolicy) Metric() MetricType {
	return MetricTypeVMAF
}

func (p *VMAFPolicy) TargetScore() float64 {
	return p.Target
}

func (p *VMAFPolicy) MinScore() float64 {
	return p.Min
}

func (p *VMAFPolicy) Tolerance() float64 {
	return p.MarginalTolerance
}

func (p *VMAFPolicy) Evaluate(agg MetricAggregate, color ColorInfo) PolicyEvaluation {
	eval := PolicyEvaluation{
		MetricType:  p.Metric(),
		TargetScore: p.Target,
		MinScore:    p.Min,
	}

	if err := ValidatePolicy(p); err != nil {
		eval.IneligibleReason = ReasonInvalidMetric
		return eval
	}

	if agg.MetricType != p.Metric() {
		eval.IneligibleReason = ReasonInvalidMetric
		return eval
	}

	if !agg.Valid {
		if agg.IneligibleReason != "" {
			eval.IneligibleReason = agg.IneligibleReason
		} else {
			eval.IneligibleReason = ReasonNoValidMetric
		}
		return eval
	}

	// SDR scoring rule: Non-SDR/HDR media is ineligible for automatic SDR VMAF scoring.
	if IsHDR(color) {
		eval.IneligibleReason = ReasonHDRIneligibleForSDRScoring
		return eval
	}

	eval.CandidateScore = agg.MeanScore

	// Per-sample quality gate: ANY valid sample below the policy minimum makes the candidate ineligible.
	for _, s := range agg.SampleScores {
		if s.Valid && s.Score < p.Min {
			eval.IneligibleReason = ReasonSampleBelowMinimum
			eval.Eligible = false
			eval.MinimumMet = false
			eval.TargetReached = false
			return eval
		}
	}

	if eval.CandidateScore < p.Min {
		eval.IneligibleReason = ReasonBelowMinimumQuality
		return eval
	}

	eval.Eligible = true
	eval.MinimumMet = true

	if eval.CandidateScore >= p.Target {
		eval.TargetReached = true
		eval.IneligibleReason = ReasonTargetReached
	} else {
		eval.IneligibleReason = ReasonMinimumMet
	}

	return eval
}

// SSIMPolicy enforces quality thresholds on the SSIM scale (0.0 - 1.0).
type SSIMPolicy struct {
	Target            float64 `json:"target"`
	Min               float64 `json:"min"`
	MarginalTolerance float64 `json:"marginal_tolerance"`
}

// NewSSIMPolicy creates an SSIM policy with explicit target, minimum, and marginal tolerance.
func NewSSIMPolicy(target, min, marginalTolerance float64) *SSIMPolicy {
	return &SSIMPolicy{
		Target:            target,
		Min:               min,
		MarginalTolerance: marginalTolerance,
	}
}

// DefaultSSIMPolicy returns the approved standard SSIM policy (target 0.99, min 0.98, marginal tolerance 0.005).
func DefaultSSIMPolicy() *SSIMPolicy {
	return NewSSIMPolicy(0.99, 0.98, 0.005)
}

func (p *SSIMPolicy) Metric() MetricType {
	return MetricTypeSSIM
}

func (p *SSIMPolicy) TargetScore() float64 {
	return p.Target
}

func (p *SSIMPolicy) MinScore() float64 {
	return p.Min
}

func (p *SSIMPolicy) Tolerance() float64 {
	return p.MarginalTolerance
}

func (p *SSIMPolicy) Evaluate(agg MetricAggregate, color ColorInfo) PolicyEvaluation {
	eval := PolicyEvaluation{
		MetricType:  p.Metric(),
		TargetScore: p.Target,
		MinScore:    p.Min,
	}

	if err := ValidatePolicy(p); err != nil {
		eval.IneligibleReason = ReasonInvalidMetric
		return eval
	}

	if agg.MetricType != p.Metric() {
		eval.IneligibleReason = ReasonInvalidMetric
		return eval
	}

	if !agg.Valid {
		if agg.IneligibleReason != "" {
			eval.IneligibleReason = agg.IneligibleReason
		} else {
			eval.IneligibleReason = ReasonNoValidMetric
		}
		return eval
	}

	// SDR scoring rule: Non-SDR/HDR media is ineligible for automatic SDR SSIM scoring.
	if IsHDR(color) {
		eval.IneligibleReason = ReasonHDRIneligibleForSDRScoring
		return eval
	}

	eval.CandidateScore = agg.MeanScore

	// Per-sample quality gate: ANY valid sample below the policy minimum makes the candidate ineligible.
	for _, s := range agg.SampleScores {
		if s.Valid && s.Score < p.Min {
			eval.IneligibleReason = ReasonSampleBelowMinimum
			eval.Eligible = false
			eval.MinimumMet = false
			eval.TargetReached = false
			return eval
		}
	}

	if eval.CandidateScore < p.Min {
		eval.IneligibleReason = ReasonBelowMinimumQuality
		return eval
	}

	eval.Eligible = true
	eval.MinimumMet = true

	if eval.CandidateScore >= p.Target {
		eval.TargetReached = true
		eval.IneligibleReason = ReasonTargetReached
	} else {
		eval.IneligibleReason = ReasonMinimumMet
	}

	return eval
}

// ValidatePolicy ensures that a quality policy is non-nil, finite, and within valid metric bounds.
func ValidatePolicy(p QualityPolicy) error {
	if p == nil {
		return fmt.Errorf("%s: quality policy is required", ReasonMetricMissing)
	}

	target := p.TargetScore()
	min := p.MinScore()
	tol := p.Tolerance()

	if !isFinite(target) || !isFinite(min) || !isFinite(tol) {
		return fmt.Errorf("invalid policy: thresholds and tolerance must be finite numbers")
	}

	if tol < 0 {
		return fmt.Errorf("invalid policy: marginal tolerance %f cannot be negative", tol)
	}

	if min > target {
		return fmt.Errorf("invalid policy: minimum score %f cannot exceed target score %f", min, target)
	}

	switch p.Metric() {
	case MetricTypeVMAF:
		if min < 0.0 || target > 100.0 {
			return fmt.Errorf("invalid VMAF policy: thresholds must be within [0, 100], got min %f, target %f", min, target)
		}
	case MetricTypeSSIM:
		if min < 0.0 || target > 1.0 {
			return fmt.Errorf("invalid SSIM policy: thresholds must be within [0, 1], got min %f, target %f", min, target)
		}
	default:
		return fmt.Errorf("%s: unknown metric type %s", ReasonInvalidMetric, p.Metric())
	}

	return nil
}
