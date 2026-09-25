package transcode

import "testing"

func TestBenchmarkQualityModelAndGuardrailValidation(t *testing.T) {
	newRequest := func() BenchmarkRequest {
		req := validTestBenchmarkRequest()
		req.Quality = &BenchmarkQualityConfig{VMAF: &BenchmarkQualityThresholds{Target: 96, Minimum: 95, Model: "v1_1080p_3h"}}
		return req
	}
	req := newRequest()
	if err := ValidateBenchmarkRequest(&req); err != nil {
		t.Fatal(err)
	}
	if req.Quality.VMAF.WorstWindowSeconds != 5 || req.Quality.FinalValidation == nil || req.Quality.FinalValidation.Mode != "sampled" {
		t.Fatalf("missing resolved quality defaults: %+v", req.Quality)
	}
	req = newRequest()
	req.Quality.VMAF.Model = "unknown"
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Fatal("accepted unknown model")
	}
	req = newRequest()
	bad := 101.0
	req.Quality.VMAF.P5Minimum = &bad
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Fatal("accepted out-of-range p5 threshold")
	}
	req = newRequest()
	limit := 3
	req.Quality.VMAF.MaxFramesBelowThreshold = &limit
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Fatal("accepted frame-count limit without frame threshold")
	}
	req = newRequest()
	req.Quality.Banding = &BenchmarkBandingConfig{Enabled: true, Metric: "cambi", Mode: "full_ref"}
	if err := ValidateBenchmarkRequest(&req); err != nil || req.Quality.Banding.Enforcement != "observe" {
		t.Fatalf("CAMBI observe default failed: %v %+v", err, req.Quality.Banding)
	}
	req = validTestBenchmarkRequest()
	req.Metric = "ssim"
	req.Quality = &BenchmarkQualityConfig{SSIM: &BenchmarkQualityThresholds{Target: 0.99, Minimum: 0.98}, Banding: &BenchmarkBandingConfig{Enabled: true, Metric: "cambi", Mode: "full_ref"}}
	if err := ValidateBenchmarkRequest(&req); err != nil || req.Quality.FinalValidation == nil {
		t.Fatalf("CAMBI-only sampled validation default failed: %v %+v", err, req.Quality)
	}
	req.Metric = "vmaf"
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Fatal("accepted final VMAF validation without an explicit model")
	}
	req = newRequest()
	req.Quality.Banding = &BenchmarkBandingConfig{Enabled: true, Metric: "cambi", Mode: "full_ref", Enforcement: "reject"}
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Fatal("accepted CAMBI rejection without calibrated limit")
	}
}
