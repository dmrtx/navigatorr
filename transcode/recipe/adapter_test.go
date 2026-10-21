package recipe

import (
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestSamplePlanFollowsRecipeDefaults(t *testing.T) {
	cfg := transcode.SamplePlanConfig{
		Duration:          7200.0,
		SampleSeconds:     DefaultSampleSeconds,
		SampleCount:       DefaultSampleCount,
		Positions:         DefaultSamplingPositions,
		RelativePositions: true,
	}
	plan, err := transcode.PlanSamples(cfg)
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
