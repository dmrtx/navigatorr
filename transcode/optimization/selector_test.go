package optimization

import (
	"testing"
)

func TestCandidateSelector_TargetReachedWithinTolerance(t *testing.T) {
	// Candidate A: VMAF 96.8, Size 1,000,000,000 bytes (~1000 MB)
	// Candidate B: VMAF 96.4, Size 800,000,000 bytes (~800 MB)
	// Target = 96.0, Min = 95.0, Tolerance = 0.5.
	// MaxScore = 96.8. Floor = 96.8 - 0.5 = 96.3.
	// Candidate B (96.4 >= 96.3) is within tolerance and smaller (800MB < 1000MB).
	// Winner must be Candidate B!
	policy := DefaultVMAFPolicy()
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}

	in := SelectorInput{
		Policy: policy,
		Candidates: []CandidateInput{
			{
				CandidateID:   "cand_a_hq",
				Profile:       "hevc_hq",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 96.8, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  800000000,
					EstimatedTotalBytes:  1000000000,
					EstimatedTotalMB:     1000.0,
					SavingsPercent:       20.0,
				},
			},
			{
				CandidateID:   "cand_b_eff",
				Profile:       "hevc_eff",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 96.4, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  600000000,
					EstimatedTotalBytes:  800000000,
					EstimatedTotalMB:     800.0,
					SavingsPercent:       36.0,
				},
			},
		},
	}

	res := SelectCandidate(in)
	if res.Winner == nil {
		t.Fatalf("expected a winner, got nil")
	}
	if res.Winner.CandidateID != "cand_b_eff" {
		t.Errorf("Winner ID = %q, want %q", res.Winner.CandidateID, "cand_b_eff")
	}
	if res.DecisionReason != ReasonTargetReachedSmallestSize {
		t.Errorf("DecisionReason = %q, want %q", res.DecisionReason, ReasonTargetReachedSmallestSize)
	}
}

func TestCandidateSelector_TargetReachedOutsideTolerance(t *testing.T) {
	// Candidate A: VMAF 97.2, Size 1,200,000,000 bytes
	// Candidate B: VMAF 96.1, Size 700,000,000 bytes
	// Target = 96.0, Min = 95.0, Tolerance = 0.5.
	// MaxScore = 97.2. Floor = 97.2 - 0.5 = 96.7.
	// Candidate B (96.1 < 96.7) is outside tolerance, so Candidate A is selected despite being larger.
	policy := DefaultVMAFPolicy()
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}

	in := SelectorInput{
		Policy: policy,
		Candidates: []CandidateInput{
			{
				CandidateID:   "cand_a_hq",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 97.2, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  1000000000,
					EstimatedTotalBytes:  1200000000,
				},
			},
			{
				CandidateID:   "cand_b_too_low",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 96.1, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  500000000,
					EstimatedTotalBytes:  700000000,
				},
			},
		},
	}

	res := SelectCandidate(in)
	if res.Winner == nil {
		t.Fatalf("expected a winner, got nil")
	}
	if res.Winner.CandidateID != "cand_a_hq" {
		t.Errorf("Winner ID = %q, want %q", res.Winner.CandidateID, "cand_a_hq")
	}
	if res.DecisionReason != ReasonTargetReachedSmallestSize {
		t.Errorf("DecisionReason = %q, want %q", res.DecisionReason, ReasonTargetReachedSmallestSize)
	}
}

func TestCandidateSelector_TargetReachedSizeTieBreakQuality(t *testing.T) {
	// Two candidates with identical size: 800MB. Candidate A has 96.6, Candidate B has 96.2.
	// Candidate A wins on quality tie-break.
	policy := DefaultVMAFPolicy()
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}

	in := SelectorInput{
		Policy: policy,
		Candidates: []CandidateInput{
			{
				CandidateID:   "cand_b",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 96.2, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  600000000,
					EstimatedTotalBytes:  800000000,
				},
			},
			{
				CandidateID:   "cand_a",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 96.6, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  600000000,
					EstimatedTotalBytes:  800000000,
				},
			},
		},
	}

	res := SelectCandidate(in)
	if res.Winner == nil {
		t.Fatalf("expected a winner, got nil")
	}
	if res.Winner.CandidateID != "cand_a" {
		t.Errorf("Winner ID = %q, want %q", res.Winner.CandidateID, "cand_a")
	}
}

func TestCandidateSelector_NoneReachesTargetSomeMeetMinimum(t *testing.T) {
	// Target = 96.0, Min = 95.0.
	// Neither reaches target, but Candidate A has 95.7 (1200MB) and Candidate B has 95.2 (700MB).
	// When none reaches target, must choose highest quality!
	policy := DefaultVMAFPolicy()
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}

	in := SelectorInput{
		Policy: policy,
		Candidates: []CandidateInput{
			{
				CandidateID:   "cand_a_higher_qual",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 95.7, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  1000000000,
					EstimatedTotalBytes:  1200000000,
				},
			},
			{
				CandidateID:   "cand_b_lower_qual_smaller",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 95.2, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  500000000,
					EstimatedTotalBytes:  700000000,
				},
			},
		},
	}

	res := SelectCandidate(in)
	if res.Winner == nil {
		t.Fatalf("expected a winner, got nil")
	}
	if res.Winner.CandidateID != "cand_a_higher_qual" {
		t.Errorf("Winner ID = %q, want %q", res.Winner.CandidateID, "cand_a_higher_qual")
	}
	if res.DecisionReason != ReasonMinimumMetHighestQuality {
		t.Errorf("DecisionReason = %q, want %q", res.DecisionReason, ReasonMinimumMetHighestQuality)
	}
}

func TestCandidateSelector_MinimumMetSizeTieBreak(t *testing.T) {
	// Target = 96.0, Min = 95.0.
	// None reaches target, both meet minimum with identical score 95.4.
	// Candidate B has smaller size (800MB vs 1000MB) -> Candidate B wins on size tie-break!
	policy := DefaultVMAFPolicy()
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}

	in := SelectorInput{
		Policy: policy,
		Candidates: []CandidateInput{
			{
				CandidateID:   "cand_a_larger",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 95.4, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  800000000,
					EstimatedTotalBytes:  1000000000,
				},
			},
			{
				CandidateID:   "cand_b_smaller",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 95.4, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  600000000,
					EstimatedTotalBytes:  800000000,
				},
			},
		},
	}

	res := SelectCandidate(in)
	if res.Winner == nil {
		t.Fatalf("expected a winner, got nil")
	}
	if res.Winner.CandidateID != "cand_b_smaller" {
		t.Errorf("Winner ID = %q, want %q", res.Winner.CandidateID, "cand_b_smaller")
	}
	if res.DecisionReason != ReasonMinimumMetHighestQuality {
		t.Errorf("DecisionReason = %q, want %q", res.DecisionReason, ReasonMinimumMetHighestQuality)
	}
}

func TestCandidateSelector_NoCandidateMeetsMinimum(t *testing.T) {
	policy := DefaultVMAFPolicy() // Min = 95.0
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}

	in := SelectorInput{
		Policy: policy,
		Candidates: []CandidateInput{
			{
				CandidateID:   "cand_a",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 93.0, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  800000000,
					EstimatedTotalBytes:  1000000000,
				},
			},
			{
				CandidateID:   "cand_b",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 94.0, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  600000000,
					EstimatedTotalBytes:  800000000,
				},
			},
		},
	}

	res := SelectCandidate(in)
	if res.Winner != nil {
		t.Errorf("expected no winner, got %+v", res.Winner)
	}
	if res.DecisionReason != ReasonNoCandidateMetMinimumQuality {
		t.Errorf("DecisionReason = %q, want %q", res.DecisionReason, ReasonNoCandidateMetMinimumQuality)
	}
}

// Regression test for Fix 1:
// Candidate mean score passes target (e.g. 96.2), but one sample is below policy minimum (e.g. 94.0 < 95.0).
// Candidate must be disqualified, and if no other candidates qualify, selector returns no winner!
func TestCandidateSelector_PerSampleQualityGateFailsCandidate(t *testing.T) {
	policy := DefaultVMAFPolicy() // Target = 96.0, Min = 95.0
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}

	in := SelectorInput{
		Policy: policy,
		Candidates: []CandidateInput{
			{
				CandidateID:   "cand_mean_passes_sample_fails",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 97.4, Valid: true},
					{SampleIndex: 1, Score: 94.0, Valid: true}, // Below minimum 95.0!
					{SampleIndex: 2, Score: 97.2, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  500000000,
					EstimatedTotalBytes:  600000000,
				},
			},
		},
	}

	res := SelectCandidate(in)
	if res.Winner != nil {
		t.Errorf("expected no winner because sample failed minimum, got: %+v", res.Winner)
	}
	if len(res.AllEvaluated) != 1 {
		t.Fatalf("expected 1 evaluated candidate")
	}
	if res.AllEvaluated[0].Eligible {
		t.Errorf("expected candidate to be ineligible")
	}
	if res.AllEvaluated[0].EvaluationReason != ReasonSampleBelowMinimum {
		t.Errorf("EvaluationReason = %q, want %q", res.AllEvaluated[0].EvaluationReason, ReasonSampleBelowMinimum)
	}
}

// Regression test for Fix 3:
// Unusable estimate must never win simply because its size appears smallest (0 bytes)!
func TestCandidateSelector_UnusableEstimateRejected(t *testing.T) {
	policy := DefaultVMAFPolicy()
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}

	in := SelectorInput{
		Policy: policy,
		Candidates: []CandidateInput{
			{
				// High quality candidate, but unusable estimate (0 video bytes)
				CandidateID:   "cand_zero_size_unusable",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 98.0, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: false,
					UnusableReason:       ReasonInvalidVideoEstimate,
					EstimatedVideoBytes:  0,
					EstimatedTotalBytes:  0,
				},
			},
			{
				// Valid candidate with valid estimate
				CandidateID:   "cand_valid",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 96.5, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  600000000,
					EstimatedTotalBytes:  800000000,
				},
			},
		},
	}

	res := SelectCandidate(in)
	if res.Winner == nil {
		t.Fatalf("expected cand_valid to win, got nil")
	}
	if res.Winner.CandidateID != "cand_valid" {
		t.Errorf("Winner ID = %q, want %q (unusable estimate must never win)", res.Winner.CandidateID, "cand_valid")
	}

	// Verify the zero-size candidate was evaluated as ineligible
	for _, ec := range res.AllEvaluated {
		if ec.CandidateID == "cand_zero_size_unusable" {
			if ec.Eligible {
				t.Errorf("unusable candidate must not be eligible")
			}
			if ec.EvaluationReason != ReasonInvalidVideoEstimate {
				t.Errorf("expected evaluation reason %q, got %q", ReasonInvalidVideoEstimate, ec.EvaluationReason)
			}
		}
	}
}

func TestCandidateSelector_InvalidCandidatesAndHDR(t *testing.T) {
	policy := DefaultVMAFPolicy()

	in := SelectorInput{
		Policy: policy,
		Candidates: []CandidateInput{
			{
				CandidateID:   "cand_failed_encode",
				EncodeSuccess: false,
			},
			{
				CandidateID:   "cand_hdr_ineligible",
				EncodeSuccess: true,
				ColorInfo:     ColorInfo{ColorTransfer: "smpte2084"},
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 98.0, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  500000000,
					EstimatedTotalBytes:  600000000,
				},
			},
			{
				CandidateID:   "cand_invalid_metric",
				EncodeSuccess: true,
				AggregateResult: MetricAggregate{
					Valid: false,
				},
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  500000000,
					EstimatedTotalBytes:  600000000,
				},
			},
		},
	}

	res := SelectCandidate(in)
	if res.Winner != nil {
		t.Errorf("expected no winner, got %+v", res.Winner)
	}
	if res.DecisionReason != ReasonAllCandidatesInvalid {
		t.Errorf("DecisionReason = %q, want %q", res.DecisionReason, ReasonAllCandidatesInvalid)
	}
}

func TestCandidateSelector_NilPolicy(t *testing.T) {
	in := SelectorInput{
		Policy: nil,
		Candidates: []CandidateInput{
			{CandidateID: "c1", EncodeSuccess: true},
		},
	}

	res := SelectCandidate(in)
	if res.Winner != nil {
		t.Errorf("expected no winner for nil policy")
	}
	if res.DecisionReason != ReasonNoValidMetric {
		t.Errorf("DecisionReason = %q, want %q", res.DecisionReason, ReasonNoValidMetric)
	}
}

func TestCandidateSelector_SSIMPolicy(t *testing.T) {
	policy := DefaultSSIMPolicy() // target 0.99, min 0.98, tolerance 0.005
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}

	in := SelectorInput{
		Policy: policy,
		Candidates: []CandidateInput{
			{
				CandidateID:   "ssim_hq",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeSSIM, []SampleScore{
					{SampleIndex: 0, Score: 0.994, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  1200000000,
					EstimatedTotalBytes:  1500000000,
				},
			},
			{
				CandidateID:   "ssim_eff",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeSSIM, []SampleScore{
					{SampleIndex: 0, Score: 0.991, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					SuitableForSelection: true,
					EstimatedVideoBytes:  800000000,
					EstimatedTotalBytes:  1000000000,
				},
			},
		},
	}

	// MaxScore = 0.994. Floor = 0.994 - 0.005 = 0.989.
	// ssim_eff has 0.991 >= 0.989, so within tolerance, and smaller (1GB < 1.5GB).
	res := SelectCandidate(in)
	if res.Winner == nil {
		t.Fatalf("expected a winner, got nil")
	}
	if res.Winner.CandidateID != "ssim_eff" {
		t.Errorf("Winner ID = %q, want %q", res.Winner.CandidateID, "ssim_eff")
	}
	if res.DecisionReason != ReasonTargetReachedSmallestSize {
		t.Errorf("DecisionReason = %q, want %q", res.DecisionReason, ReasonTargetReachedSmallestSize)
	}
}
