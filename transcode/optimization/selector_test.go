package optimization

import (
	"testing"
)

func TestCandidateSelector_TargetReachedWithinTolerance(t *testing.T) {
	// Candidate A: VMAF 96.2, Size 1,000,000,000 bytes (~1000 MB)
	// Candidate B: VMAF 95.8, Size 800,000,000 bytes (~800 MB)
	// Target = 95.0, Min = 91.0, Tolerance = 0.5.
	// MaxScore = 96.2. Floor = 96.2 - 0.5 = 95.7.
	// Candidate B (95.8 >= 95.7) is within tolerance and smaller (800MB < 1000MB).
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
					{SampleIndex: 0, Score: 96.2, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					EstimatedTotalBytes: 1000000000,
					EstimatedTotalMB:    953.67,
					SavingsPercent:      20.0,
				},
			},
			{
				CandidateID:   "cand_b_eff",
				Profile:       "hevc_eff",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 95.8, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					EstimatedTotalBytes: 800000000,
					EstimatedTotalMB:    762.94,
					SavingsPercent:      36.0,
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
	// Candidate A: VMAF 97.0, Size 1,200,000,000 bytes
	// Candidate B: VMAF 95.2, Size 700,000,000 bytes
	// Target = 95.0, Min = 91.0, Tolerance = 0.5.
	// MaxScore = 97.0. Floor = 97.0 - 0.5 = 96.5.
	// Candidate B (95.2 < 96.5) is outside tolerance, so Candidate A is selected despite being larger.
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
					{SampleIndex: 0, Score: 97.0, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					EstimatedTotalBytes: 1200000000,
				},
			},
			{
				CandidateID:   "cand_b_too_low",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 95.2, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					EstimatedTotalBytes: 700000000,
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
	// Two candidates with identical size: 800MB. Candidate A has 96.0, Candidate B has 95.8.
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
					{SampleIndex: 0, Score: 95.8, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					EstimatedTotalBytes: 800000000,
				},
			},
			{
				CandidateID:   "cand_a",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 96.0, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					EstimatedTotalBytes: 800000000,
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
	// None reaches target (95.0), but Candidate A has 94.2 (1200MB) and Candidate B has 92.5 (700MB).
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
					{SampleIndex: 0, Score: 94.2, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					EstimatedTotalBytes: 1200000000,
				},
			},
			{
				CandidateID:   "cand_b_lower_qual_smaller",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 92.5, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					EstimatedTotalBytes: 700000000,
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
	// None reaches target (95.0), both meet minimum (91.0) with identical score 93.5.
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
					{SampleIndex: 0, Score: 93.5, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					EstimatedTotalBytes: 1000000000,
				},
			},
			{
				CandidateID:   "cand_b_smaller",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 93.5, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					EstimatedTotalBytes: 800000000,
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
	policy := DefaultVMAFPolicy()
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}

	in := SelectorInput{
		Policy: policy,
		Candidates: []CandidateInput{
			{
				CandidateID:   "cand_a",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 88.0, Valid: true},
				}),
			},
			{
				CandidateID:   "cand_b",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
					{SampleIndex: 0, Score: 90.0, Valid: true},
				}),
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
			},
			{
				CandidateID:   "cand_invalid_metric",
				EncodeSuccess: true,
				AggregateResult: MetricAggregate{
					Valid: false,
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
	policy := DefaultSSIMPolicy() // target 0.98, min 0.95, tolerance 0.005
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}

	in := SelectorInput{
		Policy: policy,
		Candidates: []CandidateInput{
			{
				CandidateID:   "ssim_hq",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeSSIM, []SampleScore{
					{SampleIndex: 0, Score: 0.988, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					EstimatedTotalBytes: 1500000000,
				},
			},
			{
				CandidateID:   "ssim_eff",
				EncodeSuccess: true,
				ColorInfo:     sdrColor,
				AggregateResult: AggregateSampleScores(MetricTypeSSIM, []SampleScore{
					{SampleIndex: 0, Score: 0.984, Valid: true},
				}),
				EstimatedOutput: EstimationResult{
					EstimatedTotalBytes: 1000000000,
				},
			},
		},
	}

	// MaxScore = 0.988. Floor = 0.988 - 0.005 = 0.983.
	// ssim_eff has 0.984 >= 0.983, so within tolerance, and smaller (1GB < 1.5GB).
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
