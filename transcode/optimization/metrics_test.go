package optimization

import (
	"math"
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

	t.Run("unknown metric type", func(t *testing.T) {
		scores := []SampleScore{
			{SampleIndex: 0, Score: 95.0, Valid: true},
		}
		agg := AggregateSampleScores(MetricType("psnr"), scores)
		if agg.Valid {
			t.Errorf("expected Valid = false for unknown metric type")
		}
		if agg.IneligibleReason != ReasonInvalidMetric {
			t.Errorf("expected reason %q, got %q", ReasonInvalidMetric, agg.IneligibleReason)
		}
	})

	t.Run("non-finite score NaN", func(t *testing.T) {
		scores := []SampleScore{
			{SampleIndex: 0, Score: math.NaN(), Valid: true},
		}
		agg := AggregateSampleScores(MetricTypeVMAF, scores)
		if agg.Valid {
			t.Errorf("expected Valid = false for NaN score")
		}
		if agg.IneligibleReason != ReasonScoreOutOfBounds {
			t.Errorf("expected reason %q, got %q", ReasonScoreOutOfBounds, agg.IneligibleReason)
		}
	})

	t.Run("non-finite score Inf", func(t *testing.T) {
		scores := []SampleScore{
			{SampleIndex: 0, Score: math.Inf(1), Valid: true},
		}
		agg := AggregateSampleScores(MetricTypeSSIM, scores)
		if agg.Valid {
			t.Errorf("expected Valid = false for Inf score")
		}
		if agg.IneligibleReason != ReasonScoreOutOfBounds {
			t.Errorf("expected reason %q, got %q", ReasonScoreOutOfBounds, agg.IneligibleReason)
		}
	})

	t.Run("vmaf score out of bounds", func(t *testing.T) {
		aggLow := AggregateSampleScores(MetricTypeVMAF, []SampleScore{{Score: -0.5, Valid: true}})
		if aggLow.Valid || aggLow.IneligibleReason != ReasonScoreOutOfBounds {
			t.Errorf("expected invalid for negative VMAF, got: %+v", aggLow)
		}
		aggHigh := AggregateSampleScores(MetricTypeVMAF, []SampleScore{{Score: 100.5, Valid: true}})
		if aggHigh.Valid || aggHigh.IneligibleReason != ReasonScoreOutOfBounds {
			t.Errorf("expected invalid for VMAF > 100, got: %+v", aggHigh)
		}
	})

	t.Run("ssim score out of bounds", func(t *testing.T) {
		aggLow := AggregateSampleScores(MetricTypeSSIM, []SampleScore{{Score: -0.01, Valid: true}})
		if aggLow.Valid || aggLow.IneligibleReason != ReasonScoreOutOfBounds {
			t.Errorf("expected invalid for negative SSIM, got: %+v", aggLow)
		}
		aggHigh := AggregateSampleScores(MetricTypeSSIM, []SampleScore{{Score: 1.05, Valid: true}})
		if aggHigh.Valid || aggHigh.IneligibleReason != ReasonScoreOutOfBounds {
			t.Errorf("expected invalid for SSIM > 1.0, got: %+v", aggHigh)
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
	})
}

func TestVMAFPolicy(t *testing.T) {
	policy := DefaultVMAFPolicy() // approved defaults: target 96.0, min 95.0, marginal 0.5
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
			{SampleIndex: 0, Score: 95.2, Valid: true},
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
			{SampleIndex: 0, Score: 94.5, Valid: true},
		})
		eval := policy.Evaluate(agg, sdrColor)
		if eval.Eligible || eval.TargetReached || eval.MinimumMet {
			t.Errorf("expected ineligible, target NOT reached, min NOT met: %+v", eval)
		}
		if eval.IneligibleReason != ReasonSampleBelowMinimum && eval.IneligibleReason != ReasonBelowMinimumQuality {
			t.Errorf("expected reason %q or %q, got %q", ReasonSampleBelowMinimum, ReasonBelowMinimumQuality, eval.IneligibleReason)
		}
	})

	// Regression test for required fix 1:
	// Mean score meets target, but one sample is below policy minimum (e.g. 94.0 < 95.0).
	// Must fail closed: candidate is marked ineligible with ReasonSampleBelowMinimum.
	t.Run("mean passes target but one sample below minimum fails closed", func(t *testing.T) {
		agg := AggregateSampleScores(MetricTypeVMAF, []SampleScore{
			{SampleIndex: 0, Score: 97.0, Valid: true},
			{SampleIndex: 1, Score: 94.0, Valid: true}, // Below min 95.0!
			{SampleIndex: 2, Score: 97.0, Valid: true},
		})
		if agg.MeanScore != 96.0 {
			t.Fatalf("MeanScore = %f, want 96.0", agg.MeanScore)
		}

		eval := policy.Evaluate(agg, sdrColor)
		if eval.Eligible {
			t.Errorf("expected Eligible = false when one sample is below minimum")
		}
		if eval.MinimumMet {
			t.Errorf("expected MinimumMet = false when one sample is below minimum")
		}
		if eval.TargetReached {
			t.Errorf("expected TargetReached = false when one sample is below minimum")
		}
		if eval.IneligibleReason != ReasonSampleBelowMinimum {
			t.Errorf("expected reason %q, got %q", ReasonSampleBelowMinimum, eval.IneligibleReason)
		}
	})
}

func TestSSIMPolicy(t *testing.T) {
	policy := DefaultSSIMPolicy() // approved defaults: target 0.99, min 0.98, marginal 0.005
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}

	t.Run("reaches target", func(t *testing.T) {
		agg := AggregateSampleScores(MetricTypeSSIM, []SampleScore{
			{SampleIndex: 0, Score: 0.992, Valid: true},
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
			{SampleIndex: 0, Score: 0.985, Valid: true},
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
			{SampleIndex: 0, Score: 0.970, Valid: true},
		})
		eval := policy.Evaluate(agg, sdrColor)
		if eval.Eligible || eval.TargetReached || eval.MinimumMet {
			t.Errorf("expected ineligible, target NOT reached, min NOT met: %+v", eval)
		}
		if eval.IneligibleReason != ReasonSampleBelowMinimum && eval.IneligibleReason != ReasonBelowMinimumQuality {
			t.Errorf("expected reason %q or %q, got %q", ReasonSampleBelowMinimum, ReasonBelowMinimumQuality, eval.IneligibleReason)
		}
	})

	// Regression test for SSIM per-sample quality gate
	t.Run("ssim mean passes but one sample below minimum fails closed", func(t *testing.T) {
		agg := AggregateSampleScores(MetricTypeSSIM, []SampleScore{
			{SampleIndex: 0, Score: 0.995, Valid: true},
			{SampleIndex: 1, Score: 0.975, Valid: true}, // Below min 0.98!
			{SampleIndex: 2, Score: 0.995, Valid: true},
		})
		eval := policy.Evaluate(agg, sdrColor)
		if eval.Eligible || eval.MinimumMet || eval.TargetReached {
			t.Errorf("expected candidate to be ineligible due to failing sample: %+v", eval)
		}
		if eval.IneligibleReason != ReasonSampleBelowMinimum {
			t.Errorf("expected reason %q, got %q", ReasonSampleBelowMinimum, eval.IneligibleReason)
		}
	})
}

func TestNoThresholdConversion(t *testing.T) {
	// SSIM metric evaluated with VMAF policy must be rejected cleanly without conversion
	vmafPolicy := DefaultVMAFPolicy()
	ssimAgg := AggregateSampleScores(MetricTypeSSIM, []SampleScore{
		{SampleIndex: 0, Score: 0.995, Valid: true},
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
	// Untyped nil
	if err := ValidatePolicy(nil); err == nil {
		t.Errorf("expected error for untyped nil policy")
	}

	// Typed nil *VMAFPolicy must fail safely without panic
	var typedNilVMAF *VMAFPolicy = nil
	if err := ValidatePolicy(typedNilVMAF); err == nil {
		t.Errorf("expected error for typed-nil VMAF policy")
	}
	evalNilVMAF := typedNilVMAF.Evaluate(MetricAggregate{}, ColorInfo{})
	if evalNilVMAF.Eligible {
		t.Errorf("expected ineligible for typed-nil VMAF evaluate")
	}

	// Typed nil *SSIMPolicy must fail safely without panic
	var typedNilSSIM *SSIMPolicy = nil
	if err := ValidatePolicy(typedNilSSIM); err == nil {
		t.Errorf("expected error for typed-nil SSIM policy")
	}
	evalNilSSIM := typedNilSSIM.Evaluate(MetricAggregate{}, ColorInfo{})
	if evalNilSSIM.Eligible {
		t.Errorf("expected ineligible for typed-nil SSIM evaluate")
	}

	// Min > Target
	badThresholds := NewVMAFPolicy(90.0, 95.0, 0.5)
	if err := ValidatePolicy(badThresholds); err == nil {
		t.Errorf("expected error when Min > Target")
	}

	// Negative tolerance
	badTolerance := NewVMAFPolicy(96.0, 95.0, -0.1)
	if err := ValidatePolicy(badTolerance); err == nil {
		t.Errorf("expected error when MarginalTolerance < 0")
	}

	// Tolerance out of bounds for VMAF (> 100)
	tolOver100VMAF := NewVMAFPolicy(96.0, 95.0, 105.0)
	if err := ValidatePolicy(tolOver100VMAF); err == nil {
		t.Errorf("expected error when VMAF tolerance > 100")
	}

	// Tolerance out of bounds for SSIM (> 1.0)
	tolOver1SSIM := NewSSIMPolicy(0.99, 0.98, 1.2)
	if err := ValidatePolicy(tolOver1SSIM); err == nil {
		t.Errorf("expected error when SSIM tolerance > 1.0")
	}

	// Non-finite values
	nanPolicy := NewVMAFPolicy(math.NaN(), 95.0, 0.5)
	if err := ValidatePolicy(nanPolicy); err == nil {
		t.Errorf("expected error for NaN target")
	}
	infPolicy := NewVMAFPolicy(96.0, math.Inf(1), 0.5)
	if err := ValidatePolicy(infPolicy); err == nil {
		t.Errorf("expected error for Inf min")
	}

	// VMAF out of bounds (< 0 or > 100)
	vmafNegative := NewVMAFPolicy(96.0, -1.0, 0.5)
	if err := ValidatePolicy(vmafNegative); err == nil {
		t.Errorf("expected error for VMAF min < 0")
	}
	vmafOver100 := NewVMAFPolicy(105.0, 95.0, 0.5)
	if err := ValidatePolicy(vmafOver100); err == nil {
		t.Errorf("expected error for VMAF target > 100")
	}

	// SSIM out of bounds (< 0 or > 1)
	ssimNegative := NewSSIMPolicy(0.99, -0.05, 0.005)
	if err := ValidatePolicy(ssimNegative); err == nil {
		t.Errorf("expected error for SSIM min < 0")
	}
	ssimOver1 := NewSSIMPolicy(1.05, 0.98, 0.005)
	if err := ValidatePolicy(ssimOver1); err == nil {
		t.Errorf("expected error for SSIM target > 1.0")
	}

	// Valid policies
	if err := ValidatePolicy(DefaultVMAFPolicy()); err != nil {
		t.Errorf("unexpected error for default VMAF policy: %v", err)
	}
	if err := ValidatePolicy(DefaultSSIMPolicy()); err != nil {
		t.Errorf("unexpected error for default SSIM policy: %v", err)
	}
}
