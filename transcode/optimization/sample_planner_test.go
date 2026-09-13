package optimization

import (
	"math"
	"testing"
)

func TestSamplePlanner_SingleSample(t *testing.T) {
	tests := []struct {
		name          string
		duration      float64
		sampleSeconds float64
		wantStart     float64
		wantEnd       float64
		wantDur       float64
		wantCenter    float64
	}{
		{
			name:          "single sample 60s in 120s video",
			duration:      120.0,
			sampleSeconds: 60.0,
			wantStart:     30.0,
			wantEnd:       90.0,
			wantDur:       60.0,
			wantCenter:    60.0,
		},
		{
			name:          "single sample 60s in 3600s video",
			duration:      3600.0,
			sampleSeconds: 60.0,
			wantStart:     1770.0,
			wantEnd:       1830.0,
			wantDur:       60.0,
			wantCenter:    1800.0,
		},
		{
			name:          "single sample 30s in 200s video",
			duration:      200.0,
			sampleSeconds: 30.0,
			wantStart:     85.0,
			wantEnd:       115.0,
			wantDur:       30.0,
			wantCenter:    100.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := PlanSamples(SamplePlanConfig{
				Duration:      tt.duration,
				SampleSeconds: tt.sampleSeconds,
				SampleCount:   1,
			})
			if err != nil {
				t.Fatalf("PlanSamples error: %v", err)
			}
			if plan.ActualSampleCount != 1 {
				t.Fatalf("expected 1 sample, got %d", plan.ActualSampleCount)
			}
			s := plan.Samples[0]
			if s.StartSeconds != tt.wantStart {
				t.Errorf("StartSeconds = %f, want %f", s.StartSeconds, tt.wantStart)
			}
			if s.EndSeconds != tt.wantEnd {
				t.Errorf("EndSeconds = %f, want %f", s.EndSeconds, tt.wantEnd)
			}
			if s.DurationSeconds != tt.wantDur {
				t.Errorf("DurationSeconds = %f, want %f", s.DurationSeconds, tt.wantDur)
			}
			if s.CenterSeconds != tt.wantCenter {
				t.Errorf("CenterSeconds = %f, want %f", s.CenterSeconds, tt.wantCenter)
			}
			if plan.IsFullDuration {
				t.Errorf("expected IsFullDuration to be false")
			}
		})
	}
}

func TestSamplePlanner_ShortVideoFullDuration(t *testing.T) {
	tests := []struct {
		name          string
		duration      float64
		sampleSeconds float64
		sampleCount   int
	}{
		{
			name:          "duration less than sample_seconds (45s vs 60s)",
			duration:      45.0,
			sampleSeconds: 60.0,
			sampleCount:   1,
		},
		{
			name:          "duration equal to sample_seconds (60s vs 60s)",
			duration:      60.0,
			sampleSeconds: 60.0,
			sampleCount:   1,
		},
		{
			name:          "multi-sample requested on short video",
			duration:      35.5,
			sampleSeconds: 60.0,
			sampleCount:   3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := PlanSamples(SamplePlanConfig{
				Duration:      tt.duration,
				SampleSeconds: tt.sampleSeconds,
				SampleCount:   tt.sampleCount,
			})
			if err != nil {
				t.Fatalf("PlanSamples error: %v", err)
			}
			if !plan.IsFullDuration {
				t.Errorf("expected IsFullDuration = true")
			}
			if plan.ActualSampleCount != 1 {
				t.Errorf("expected 1 sample, got %d", plan.ActualSampleCount)
			}
			if plan.TotalSampleDuration != tt.duration {
				t.Errorf("TotalSampleDuration = %f, want %f", plan.TotalSampleDuration, tt.duration)
			}
			if plan.CoverageRatio != 1.0 {
				t.Errorf("CoverageRatio = %f, want 1.0", plan.CoverageRatio)
			}
			if tt.sampleCount > 1 && !plan.ReducedSampleCount {
				t.Errorf("expected ReducedSampleCount = true when count was %d", tt.sampleCount)
			}
			s := plan.Samples[0]
			if s.StartSeconds != 0.0 {
				t.Errorf("StartSeconds = %f, want 0.0", s.StartSeconds)
			}
			if s.EndSeconds != tt.duration {
				t.Errorf("EndSeconds = %f, want %f", s.EndSeconds, tt.duration)
			}
		})
	}
}

func TestSamplePlanner_AvoidOverlapsAndReduction(t *testing.T) {
	// 3 samples of 60s requested on 100s video -> 3*60 = 180s > 100s.
	// Can only fit floor(100/60) = 1 sample of 60s.
	plan, err := PlanSamples(SamplePlanConfig{
		Duration:      100.0,
		SampleSeconds: 60.0,
		SampleCount:   3,
	})
	if err != nil {
		t.Fatalf("PlanSamples error: %v", err)
	}

	if !plan.ReducedSampleCount {
		t.Errorf("expected ReducedSampleCount = true")
	}
	if plan.ActualSampleCount != 1 {
		t.Errorf("expected ActualSampleCount = 1, got %d", plan.ActualSampleCount)
	}
	if plan.Reason != ReasonReducedSampleCount {
		t.Errorf("expected Reason %q, got %q", ReasonReducedSampleCount, plan.Reason)
	}

	s := plan.Samples[0]
	if s.DurationSeconds != 60.0 {
		t.Errorf("sample duration = %f, want 60.0", s.DurationSeconds)
	}
	if s.StartSeconds < 0 || s.EndSeconds > 100.0 {
		t.Errorf("sample out of bounds: [%f, %f]", s.StartSeconds, s.EndSeconds)
	}
}

func TestSamplePlanner_MultiSampleNonOverlapping(t *testing.T) {
	// 3 samples of 60s on 3600s video
	plan, err := PlanSamples(SamplePlanConfig{
		Duration:      3600.0,
		SampleSeconds: 60.0,
		SampleCount:   3,
	})
	if err != nil {
		t.Fatalf("PlanSamples error: %v", err)
	}

	if plan.ActualSampleCount != 3 {
		t.Fatalf("expected 3 samples, got %d", plan.ActualSampleCount)
	}
	if plan.ReducedSampleCount {
		t.Errorf("did not expect ReducedSampleCount to be true")
	}

	// Verify no overlaps and all samples are 60s
	var prevEnd float64
	for i, s := range plan.Samples {
		if s.Index != i {
			t.Errorf("sample index = %d, want %d", s.Index, i)
		}
		if s.DurationSeconds != 60.0 {
			t.Errorf("sample %d duration = %f, want 60.0", i, s.DurationSeconds)
		}
		if s.StartSeconds < prevEnd {
			t.Errorf("sample %d overlaps with previous sample: start=%f, prevEnd=%f", i, s.StartSeconds, prevEnd)
		}
		if s.StartSeconds < 0 || s.EndSeconds > 3600.0 {
			t.Errorf("sample %d out of bounds: [%f, %f]", i, s.StartSeconds, s.EndSeconds)
		}
		prevEnd = s.EndSeconds
	}
}

func TestSamplePlanner_ExplicitPositions(t *testing.T) {
	t.Run("explicit seconds without overlap", func(t *testing.T) {
		plan, err := PlanSamples(SamplePlanConfig{
			Duration:      600.0,
			SampleSeconds: 60.0,
			Positions:     []float64{100.0, 300.0, 500.0},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if plan.ActualSampleCount != 3 {
			t.Fatalf("expected 3 samples, got %d", plan.ActualSampleCount)
		}
		if plan.Samples[0].CenterSeconds != 100.0 {
			t.Errorf("sample 0 center = %f, want 100.0", plan.Samples[0].CenterSeconds)
		}
		if plan.Samples[1].CenterSeconds != 300.0 {
			t.Errorf("sample 1 center = %f, want 300.0", plan.Samples[1].CenterSeconds)
		}
		if plan.Samples[2].CenterSeconds != 500.0 {
			t.Errorf("sample 2 center = %f, want 500.0", plan.Samples[2].CenterSeconds)
		}
	})

	t.Run("explicit relative fractions", func(t *testing.T) {
		plan, err := PlanSamples(SamplePlanConfig{
			Duration:          1000.0,
			SampleSeconds:     50.0,
			RelativePositions: true,
			Positions:         []float64{0.2, 0.5, 0.8},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if plan.ActualSampleCount != 3 {
			t.Fatalf("expected 3 samples, got %d", plan.ActualSampleCount)
		}
		if plan.Samples[0].CenterSeconds != 200.0 {
			t.Errorf("sample 0 center = %f, want 200.0", plan.Samples[0].CenterSeconds)
		}
		if plan.Samples[1].CenterSeconds != 500.0 {
			t.Errorf("sample 1 center = %f, want 500.0", plan.Samples[1].CenterSeconds)
		}
		if plan.Samples[2].CenterSeconds != 800.0 {
			t.Errorf("sample 2 center = %f, want 800.0", plan.Samples[2].CenterSeconds)
		}
	})

	t.Run("clamping at boundaries", func(t *testing.T) {
		// center at 10 on 60s sample -> desired [-20, 40] clamped to [0, 60]
		// center at 590 on 600s duration -> desired [560, 620] clamped to [540, 600]
		plan, err := PlanSamples(SamplePlanConfig{
			Duration:      600.0,
			SampleSeconds: 60.0,
			Positions:     []float64{10.0, 590.0},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if plan.ActualSampleCount != 2 {
			t.Fatalf("expected 2 samples, got %d", plan.ActualSampleCount)
		}
		if plan.Samples[0].StartSeconds != 0.0 || plan.Samples[0].EndSeconds != 60.0 {
			t.Errorf("sample 0 bounds = [%f, %f], want [0.0, 60.0]", plan.Samples[0].StartSeconds, plan.Samples[0].EndSeconds)
		}
		if plan.Samples[1].StartSeconds != 540.0 || plan.Samples[1].EndSeconds != 600.0 {
			t.Errorf("sample 1 bounds = [%f, %f], want [540.0, 600.0]", plan.Samples[1].StartSeconds, plan.Samples[1].EndSeconds)
		}
	})

	t.Run("dropping overlapping positions", func(t *testing.T) {
		// positions 100, 120, 300 with 60s duration.
		// window 0: [70, 130]
		// window 1 (120): desired [90, 150] overlaps with [70, 130] -> dropped!
		// window 2 (300): [270, 330] does not overlap -> kept!
		plan, err := PlanSamples(SamplePlanConfig{
			Duration:      600.0,
			SampleSeconds: 60.0,
			Positions:     []float64{100.0, 120.0, 300.0},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if plan.ActualSampleCount != 2 {
			t.Fatalf("expected 2 samples after dropping overlap, got %d", plan.ActualSampleCount)
		}
		if !plan.ReducedSampleCount {
			t.Errorf("expected ReducedSampleCount = true")
		}
		if plan.Samples[0].CenterSeconds != 100.0 || plan.Samples[1].CenterSeconds != 300.0 {
			t.Errorf("unexpected centers: %f, %f", plan.Samples[0].CenterSeconds, plan.Samples[1].CenterSeconds)
		}
	})
}

func TestSamplePlanner_InvalidInputs(t *testing.T) {
	_, err := PlanSamples(SamplePlanConfig{
		Duration:      0,
		SampleSeconds: 60.0,
	})
	if err == nil {
		t.Errorf("expected error for 0 duration")
	}

	_, err = PlanSamples(SamplePlanConfig{
		Duration:      -10.0,
		SampleSeconds: 60.0,
	})
	if err == nil {
		t.Errorf("expected error for negative duration")
	}

	_, err = PlanSamples(SamplePlanConfig{
		Duration:      100.0,
		SampleSeconds: 0,
	})
	if err == nil {
		t.Errorf("expected error for 0 sample seconds")
	}

	_, err = PlanSamples(SamplePlanConfig{
		Duration:      100.0,
		SampleSeconds: -5.0,
	})
	if err == nil {
		t.Errorf("expected error for negative sample seconds")
	}

	// Contradictory sample_count vs positions count
	_, err = PlanSamples(SamplePlanConfig{
		Duration:      100.0,
		SampleSeconds: 10.0,
		SampleCount:   3,
		Positions:     []float64{20.0, 50.0}, // only 2 positions!
	})
	if err == nil {
		t.Errorf("expected error for contradictory sample_count vs positions")
	}

	// Out of bounds relative positions
	_, err = PlanSamples(SamplePlanConfig{
		Duration:          100.0,
		SampleSeconds:     10.0,
		RelativePositions: true,
		Positions:         []float64{0.2, 1.2}, // 1.2 > 1.0
	})
	if err == nil {
		t.Errorf("expected error for relative position > 1.0")
	}

	_, err = PlanSamples(SamplePlanConfig{
		Duration:          100.0,
		SampleSeconds:     10.0,
		RelativePositions: true,
		Positions:         []float64{-0.1, 0.5},
	})
	if err == nil {
		t.Errorf("expected error for relative position < 0.0")
	}

	// Out of bounds absolute positions
	_, err = PlanSamples(SamplePlanConfig{
		Duration:      100.0,
		SampleSeconds: 10.0,
		Positions:     []float64{20.0, 150.0}, // 150 > 100
	})
	if err == nil {
		t.Errorf("expected error for absolute position > duration")
	}

	// Non-finite position
	_, err = PlanSamples(SamplePlanConfig{
		Duration:      100.0,
		SampleSeconds: 10.0,
		Positions:     []float64{20.0, math.NaN()},
	})
	if err == nil {
		t.Errorf("expected error for NaN position")
	}
}
