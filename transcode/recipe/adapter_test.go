package recipe

import (
	"testing"

	"github.com/jakenesler/navigatorr/transcode/optimization"
)

func TestOptimizationPackage_AlignmentWithRecipeDefaults(t *testing.T) {
	// VMAF alignment
	vmaf := optimization.DefaultVMAFPolicy()
	if vmaf.Target != DefaultVMAFTarget {
		t.Errorf("VMAF target mismatch: recipe=%v, opt=%v", DefaultVMAFTarget, vmaf.Target)
	}
	if vmaf.Min != DefaultVMAFMinimum {
		t.Errorf("VMAF minimum mismatch: recipe=%v, opt=%v", DefaultVMAFMinimum, vmaf.Min)
	}
	if vmaf.MarginalTolerance != DefaultVMAFMarginalTolerance {
		t.Errorf("VMAF tolerance mismatch: recipe=%v, opt=%v", DefaultVMAFMarginalTolerance, vmaf.MarginalTolerance)
	}

	// SSIM alignment
	ssim := optimization.DefaultSSIMPolicy()
	if ssim.Target != DefaultSSIMTarget {
		t.Errorf("SSIM target mismatch: recipe=%v, opt=%v", DefaultSSIMTarget, ssim.Target)
	}
	if ssim.Min != DefaultSSIMMinimum {
		t.Errorf("SSIM minimum mismatch: recipe=%v, opt=%v", DefaultSSIMMinimum, ssim.Min)
	}
	if ssim.MarginalTolerance != DefaultSSIMMarginalTolerance {
		t.Errorf("SSIM tolerance mismatch: recipe=%v, opt=%v", DefaultSSIMMarginalTolerance, ssim.MarginalTolerance)
	}

	// Sampling configuration compatibility
	cfg := optimization.SamplePlanConfig{
		Duration:          7200.0,
		SampleSeconds:     DefaultSampleSeconds,
		SampleCount:       DefaultSampleCount,
		Positions:         DefaultSamplingPositions,
		RelativePositions: true,
	}
	plan, err := optimization.PlanSamples(cfg)
	if err != nil {
		t.Fatalf("PlanSamples with recipe defaults failed: %v", err)
	}
	if plan.ActualSampleCount != 3 {
		t.Errorf("expected 3 samples, got %d", plan.ActualSampleCount)
	}
	if plan.SampleSeconds != 20.0 {
		t.Errorf("expected sample seconds 20.0, got %v", plan.SampleSeconds)
	}
}
