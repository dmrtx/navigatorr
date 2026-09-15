package transcode

import (
	"testing"
)

func TestValidateBenchmarkRequest_Concurrency(t *testing.T) {
	base := func() BenchmarkRequest {
		req := validTestBenchmarkRequest()
		req.Adaptive = nil
		return req
	}

	t.Run("nil concurrency is valid", func(t *testing.T) {
		req := base()
		req.Concurrency = nil
		if err := ValidateBenchmarkRequest(&req); err != nil {
			t.Fatalf("expected nil concurrency to validate, got: %v", err)
		}
	})

	t.Run("zero values select defaults", func(t *testing.T) {
		req := base()
		req.Concurrency = &BenchmarkConcurrencyConfig{}
		if err := ValidateBenchmarkRequest(&req); err != nil {
			t.Fatalf("expected zero concurrency to validate, got: %v", err)
		}
		if req.Concurrency.EncodeConcurrency != DefaultEncodeConcurrency ||
			req.Concurrency.MetricConcurrency != DefaultMetricConcurrency {
			t.Errorf("expected defaults (%d,%d), got (%d,%d)",
				DefaultEncodeConcurrency, DefaultMetricConcurrency,
				req.Concurrency.EncodeConcurrency, req.Concurrency.MetricConcurrency)
		}
	})

	t.Run("explicit values within bound are valid", func(t *testing.T) {
		req := base()
		req.Concurrency = &BenchmarkConcurrencyConfig{EncodeConcurrency: 1, MetricConcurrency: 4}
		if err := ValidateBenchmarkRequest(&req); err != nil {
			t.Fatalf("expected valid concurrency, got: %v", err)
		}
	})

	t.Run("encode over max is rejected", func(t *testing.T) {
		req := base()
		req.Concurrency = &BenchmarkConcurrencyConfig{EncodeConcurrency: MaxBenchmarkConcurrency + 1, MetricConcurrency: 2}
		if err := ValidateBenchmarkRequest(&req); err == nil {
			t.Fatalf("expected error for encode concurrency over max, got nil")
		}
	})

	t.Run("metric over max is rejected", func(t *testing.T) {
		req := base()
		req.Concurrency = &BenchmarkConcurrencyConfig{EncodeConcurrency: 2, MetricConcurrency: MaxBenchmarkConcurrency + 1}
		if err := ValidateBenchmarkRequest(&req); err == nil {
			t.Fatalf("expected error for metric concurrency over max, got nil")
		}
	})

	t.Run("negative is rejected", func(t *testing.T) {
		req := base()
		req.Concurrency = &BenchmarkConcurrencyConfig{EncodeConcurrency: -1, MetricConcurrency: 2}
		if err := ValidateBenchmarkRequest(&req); err == nil {
			t.Fatalf("expected error for negative encode concurrency, got nil")
		}
	})
}
