package transcode

import (
	"testing"

	"github.com/jakenesler/navigatorr/transcode/optimization"
)

func sizeV3TestRequest() BenchmarkRequest {
	req := validTestBenchmarkRequest()
	req.Mode, req.ModePolicyVersion, req.SearchBudgetSeconds = "size", optimization.ModePolicyV3, 600
	req.AudioMode = AudioModeCompactLossless
	req.Allow8BitTo10Bit = true
	req.Quality = &BenchmarkQualityConfig{VMAF: &BenchmarkQualityThresholds{Model: "v1_1080p_3h"}}
	return req
}

func TestValidateBenchmarkRequestSizeV3(t *testing.T) {
	req := sizeV3TestRequest()
	if err := ValidateBenchmarkRequest(&req); err != nil {
		t.Fatalf("size v3 request rejected: %v", err)
	}
	for name, mutate := range map[string]func(*BenchmarkRequest){
		"v3 quality mode":            func(r *BenchmarkRequest) { r.Mode = "quality" },
		"v3 compact mode":            func(r *BenchmarkRequest) { r.Mode = "compact" },
		"v2 size Main10 conversion":  func(r *BenchmarkRequest) { r.ModePolicyVersion, r.AudioMode = optimization.ModePolicyV2, "" },
		"v2 size lossless audio":     func(r *BenchmarkRequest) { r.ModePolicyVersion, r.Allow8BitTo10Bit = optimization.ModePolicyV2, false },
		"v3 size without its audio":  func(r *BenchmarkRequest) { r.AudioMode = "copy" },
		"profile benchmark lossless": func(r *BenchmarkRequest) { r.Mode, r.ModePolicyVersion, r.Allow8BitTo10Bit = "", "", false },
		"unknown policy version":     func(r *BenchmarkRequest) { r.ModePolicyVersion = "transcode-modes-v4" },
	} {
		r := sizeV3TestRequest()
		mutate(&r)
		if err := ValidateBenchmarkRequest(&r); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Frozen v2 size requests keep validating unchanged.
	v2 := sizeV3TestRequest()
	v2.ModePolicyVersion, v2.AudioMode, v2.Allow8BitTo10Bit = optimization.ModePolicyV2, "", false
	if err := ValidateBenchmarkRequest(&v2); err != nil {
		t.Fatalf("v2 size request rejected: %v", err)
	}
}

func TestModeMinimumSavingsV3(t *testing.T) {
	for _, version := range []string{"", "transcode-modes-v1", optimization.ModePolicyV2, optimization.ModePolicyV3} {
		if min, ok := optimization.ModeMinimumSavings("size", version); !ok || min != 15 {
			t.Fatalf("size %q: %d %v", version, min, ok)
		}
	}
	for _, mode := range []string{"quality", "x265_preserve", "compact"} {
		if _, ok := optimization.ModeMinimumSavings(mode, optimization.ModePolicyV3); ok {
			t.Fatalf("%s accepted under v3", mode)
		}
	}
	if !optimization.ModeSizeAllowedVersion("size", 100, 85, optimization.ModePolicyV3) || optimization.ModeSizeAllowedVersion("size", 100, 86, optimization.ModePolicyV3) {
		t.Fatal("v3 size gate is not 15% savings")
	}
}
