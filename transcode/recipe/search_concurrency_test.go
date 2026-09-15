package recipe

import (
	"testing"
)

func validConcurrencyTestPolicy() *OptimizationPolicy {
	vmafTol := 0.5
	return &OptimizationPolicy{
		Enabled: true,
		Sampling: &SamplingPolicy{
			Strategy:      "distributed",
			SampleCount:   3,
			SampleSeconds: 15.0,
			Positions:     []float64{0.25, 0.5, 0.75},
		},
		Quality: &QualityPolicy{
			PreferredMetric: "vmaf",
			VMAF:            &MetricTarget{Target: 96.0, Minimum: 95.0, MarginalTolerance: &vmafTol},
		},
		Search: &SearchPolicy{
			MaxCandidates: 3,
			QualityValues: []int{60, 65, 70},
		},
	}
}

func TestSearchPolicy_ConcurrencyValidation(t *testing.T) {
	t.Run("zero concurrency is valid", func(t *testing.T) {
		opt := validConcurrencyTestPolicy()
		if err := ValidateOptimizationPolicy("test", opt); err != nil {
			t.Fatalf("expected zero concurrency to validate, got: %v", err)
		}
	})

	t.Run("explicit concurrency within bound is valid", func(t *testing.T) {
		opt := validConcurrencyTestPolicy()
		opt.Search.EncodeConcurrency = 1
		opt.Search.MetricConcurrency = 4
		if err := ValidateOptimizationPolicy("test", opt); err != nil {
			t.Fatalf("expected valid concurrency, got: %v", err)
		}
	})

	t.Run("encode concurrency over bound is rejected", func(t *testing.T) {
		opt := validConcurrencyTestPolicy()
		opt.Search.EncodeConcurrency = 5
		if err := ValidateOptimizationPolicy("test", opt); err == nil {
			t.Fatalf("expected error for encode concurrency over bound, got nil")
		}
	})

	t.Run("metric concurrency over bound is rejected", func(t *testing.T) {
		opt := validConcurrencyTestPolicy()
		opt.Search.MetricConcurrency = 9
		if err := ValidateOptimizationPolicy("test", opt); err == nil {
			t.Fatalf("expected error for metric concurrency over bound, got nil")
		}
	})

	t.Run("negative concurrency is rejected", func(t *testing.T) {
		opt := validConcurrencyTestPolicy()
		opt.Search.EncodeConcurrency = -1
		if err := ValidateOptimizationPolicy("test", opt); err == nil {
			t.Fatalf("expected error for negative encode concurrency, got nil")
		}
	})

	t.Run("clone preserves concurrency", func(t *testing.T) {
		opt := validConcurrencyTestPolicy()
		opt.Search.EncodeConcurrency = 2
		opt.Search.MetricConcurrency = 3
		cloned := opt.Clone()
		if cloned.Search.EncodeConcurrency != 2 || cloned.Search.MetricConcurrency != 3 {
			t.Errorf("clone lost concurrency settings: %+v", cloned.Search)
		}
		cloned.Search.EncodeConcurrency = 99
		if opt.Search.EncodeConcurrency == 99 {
			t.Errorf("clone aliases original concurrency fields")
		}
	})
}
