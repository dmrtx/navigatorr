package optimization

import (
	"testing"
)

func TestAggregateSampleScores(t *testing.T) {
	t.Run("empty scores", func(t *testing.T) {
		agg := AggregateSampleScores(MetricTypeVMAF, []SampleScore{})
		if agg.Valid {
			t.Errorf("expected Valid = false for empty scores")
		}
		if agg.IneligibleReason != ReasonNoValidSampleScores {
			t.Errorf("expected reason %q, got %q", ReasonNoValidSampleScores, agg.IneligibleReason)
		}
	})

	t.Run("incomplete sample scores with failure", func(t *testing.T) {
		scores := []SampleScore{
			{SampleIndex: 0, Score: 95.0, Valid: true},
			{SampleIndex: 1, Score: 0.0, Valid: false, Error: "decode error"},
			{SampleIndex: 2, Score: 96.0, Valid: true},
		}
		agg := AggregateSampleScores(MetricTypeVMAF, scores)
		if agg.Valid {
			t.Errorf("expected Valid = false when a sample is invalid")
		}
		if agg.IneligibleReason != ReasonIncompleteSampleScores {
			t.Errorf("expected reason %q, got %q", ReasonIncompleteSampleScores, agg.IneligibleReason)
		}
	})

	t.Run("all valid scores aggregation", func(t *testing.T) {
		scores := []SampleScore{
			{SampleIndex: 0, Score: 94.0, Valid: true},
			{SampleIndex: 1, Score: 96.0, Valid: true},
			{SampleIndex: 2, Score: 95.0, Valid: true},
		}
		agg := AggregateSampleScores(MetricTypeVMAF, scores)
		if !agg.Valid {
			t.Fatalf("expected Valid = true")
		}
		if agg.MeanScore != 95.0 {
			t.Errorf("MeanScore = %f, want 95.0", agg.MeanScore)
		}
		if agg.MinScore != 94.0 {
			t.Errorf("MinScore = %f, want 94.0", agg.MinScore)
		}
		if agg.MaxScore != 96.0 {
			t.Errorf("MaxScore = %f, want 96.0", agg.MaxScore)
		}
		// Harmonic mean of 94, 96, 95: 3 / (1/94 + 1/96 + 1/95) = 94.9930
		if agg.HarmonicMean <= 0 || agg.HarmonicMean > agg.MeanScore {
			t.Errorf("unexpected harmonic mean %f (should be <= mean %f)", agg.HarmonicMean, agg.MeanScore)
		}
	})
}

func TestVMAFPolicy(t *testing.T) {
	policy := DefaultVMAFPolicy() // target 95.0, min 91.0, marginal 0.5
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}

	t.Run("reaches target", func(t *testing.T) {
		agg := AggregateSampleScores(MetricTypeVMAF, []SampleScore{
			{SampleIndex: 0, Score: 96.5, Valid: true},
		})
		eval := policy.Evaluate(agg, sdrColor)
		if !eval.Eligible || !eval.TargetReached || !eval.MinimumMet {
			t.Errorf("expected eligible, target reached, min met: %+v", eval)
		}
		if eval.IneligibleReason != ReasonTargetReached {
			t.Errorf("expected reason %q, got %q", ReasonTargetReached, eval.IneligibleReason)
		}
	})

	t.Run("meets minimum but below target", func(t *testing.T) {
		agg := AggregateSampleScores(MetricTypeVMAF, []SampleScore{
			{SampleIndex: 0, Score: 93.0, Valid: true},
		})
		eval := policy.Evaluate(agg, sdrColor)
		if !eval.Eligible || eval.TargetReached || !eval.MinimumMet {
			t.Errorf("expected eligible, target NOT reached, min met: %+v", eval)
		}
		if eval.IneligibleReason != ReasonMinimumMet {
			t.Errorf("expected reason %q, got %q", ReasonMinimumMet, eval.IneligibleReason)
		}
	})

	t.Run("below minimum", func(t *testing.T) {
		agg := AggregateSampleScores(MetricTypeVMAF, []SampleScore{
			{SampleIndex: 0, Score: 89.5, Valid: true},
		})
		eval := policy.Evaluate(agg, sdrColor)
		if eval.Eligible || eval.TargetReached || eval.MinimumMet {
			t.Errorf("expected ineligible, target NOT reached, min NOT met: %+v", eval)
		}
		if eval.IneligibleReason != ReasonBelowMinimumQuality {
			t.Errorf("expected reason %q, got %q", ReasonBelowMinimumQuality, eval.IneligibleReason)
		}
	})
}

func TestSSIMPolicy(t *testing.T) {
	policy := DefaultSSIMPolicy() // target 0.98, min 0.95, marginal 0.005
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}

	t.Run("reaches target", func(t *testing.T) {
		agg := AggregateSampleScores(MetricTypeSSIM, []SampleScore{
			{SampleIndex: 0, Score: 0.985, Valid: true},
		})
		eval := policy.Evaluate(agg, sdrColor)
		if !eval.Eligible || !eval.TargetReached || !eval.MinimumMet {
			t.Errorf("expected eligible, target reached, min met: %+v", eval)
		}
		if eval.IneligibleReason != ReasonTargetReached {
			t.Errorf("expected reason %q, got %q", ReasonTargetReached, eval.IneligibleReason)
		}
	})

	t.Run("meets minimum but below target", func(t *testing.T) {
		agg := AggregateSampleScores(MetricTypeSSIM, []SampleScore{
			{SampleIndex: 0, Score: 0.965, Valid: true},
		})
		eval := policy.Evaluate(agg, sdrColor)
		if !eval.Eligible || eval.TargetReached || !eval.MinimumMet {
			t.Errorf("expected eligible, target NOT reached, min met: %+v", eval)
		}
		if eval.IneligibleReason != ReasonMinimumMet {
			t.Errorf("expected reason %q, got %q", ReasonMinimumMet, eval.IneligibleReason)
		}
	})

	t.Run("below minimum", func(t *testing.T) {
		agg := AggregateSampleScores(MetricTypeSSIM, []SampleScore{
			{SampleIndex: 0, Score: 0.930, Valid: true},
		})
		eval := policy.Evaluate(agg, sdrColor)
		if eval.Eligible || eval.TargetReached || eval.MinimumMet {
			t.Errorf("expected ineligible, target NOT reached, min NOT met: %+v", eval)
		}
		if eval.IneligibleReason != ReasonBelowMinimumQuality {
			t.Errorf("expected reason %q, got %q", ReasonBelowMinimumQuality, eval.IneligibleReason)
		}
	})
}

func TestNoThresholdConversion(t *testing.T) {
	// SSIM metric evaluated with VMAF policy must be rejected cleanly without conversion
	vmafPolicy := DefaultVMAFPolicy()
	ssimAgg := AggregateSampleScores(MetricTypeSSIM, []SampleScore{
		{SampleIndex: 0, Score: 0.99, Valid: true},
	})
	eval := vmafPolicy.Evaluate(ssimAgg, ColorInfo{})
	if eval.Eligible {
		t.Errorf("expected SSIM aggregate to be ineligible under VMAF policy")
	}
	if eval.IneligibleReason != ReasonInvalidMetric {
		t.Errorf("expected reason %q, got %q", ReasonInvalidMetric, eval.IneligibleReason)
	}

	// VMAF metric evaluated with SSIM policy must also be rejected
	ssimPolicy := DefaultSSIMPolicy()
	vmafAgg := AggregateSampleScores(MetricTypeVMAF, []SampleScore{
		{SampleIndex: 0, Score: 97.0, Valid: true},
	})
	eval2 := ssimPolicy.Evaluate(vmafAgg, ColorInfo{})
	if eval2.Eligible {
		t.Errorf("expected VMAF aggregate to be ineligible under SSIM policy")
	}
	if eval2.IneligibleReason != ReasonInvalidMetric {
		t.Errorf("expected reason %q, got %q", ReasonInvalidMetric, eval2.IneligibleReason)
	}
}

func TestHDRExplicitIneligibility(t *testing.T) {
	policy := DefaultVMAFPolicy()
	agg := AggregateSampleScores(MetricTypeVMAF, []SampleScore{
		{SampleIndex: 0, Score: 98.0, Valid: true},
	})

	hdrCases := []struct {
		name  string
		color ColorInfo
	}{
		{name: "hdr format flag", color: ColorInfo{HDRFormat: "hdr10"}},
		{name: "smpte2084 PQ transfer", color: ColorInfo{ColorTransfer: "smpte2084"}},
		{name: "arib-std-b67 HLG transfer", color: ColorInfo{ColorTransfer: "arib-std-b67"}},
		{name: "bt2020 primaries", color: ColorInfo{ColorPrimaries: "bt2020"}},
		{name: "bt2020nc color space", color: ColorInfo{ColorSpace: "bt2020nc"}},
	}

	for _, tc := range hdrCases {
		t.Run(tc.name, func(t *testing.T) {
			if !IsHDR(tc.color) {
				t.Fatalf("IsHDR should be true for %+v", tc.color)
			}
			eval := policy.Evaluate(agg, tc.color)
			if eval.Eligible {
				t.Errorf("HDR color should be ineligible for SDR policy: %+v", eval)
			}
			if eval.IneligibleReason != ReasonHDRIneligibleForSDRScoring {
				t.Errorf("expected reason %q, got %q", ReasonHDRIneligibleForSDRScoring, eval.IneligibleReason)
			}
		})
	}
}

func TestValidatePolicy(t *testing.T) {
	if err := ValidatePolicy(nil); err == nil {
		t.Errorf("expected error for nil policy")
	}

	badThresholds := NewVMAFPolicy(90.0, 95.0, 0.5)
	if err := ValidatePolicy(badThresholds); err == nil {
		t.Errorf("expected error when Min > Target")
	}

	badTolerance := NewVMAFPolicy(95.0, 91.0, -0.1)
	if err := ValidatePolicy(badTolerance); err == nil {
		t.Errorf("expected error when MarginalTolerance < 0")
	}

	goodPolicy := DefaultVMAFPolicy()
	if err := ValidatePolicy(goodPolicy); err != nil {
		t.Errorf("unexpected error for valid policy: %v", err)
	}
}
