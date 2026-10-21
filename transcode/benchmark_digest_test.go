package transcode

import (
	"encoding/json"
	"testing"
)

// finalGateRequestFixture is a request shaped like the ones the final quality
// gate freezes into an action's state: every field the planner sets, none of
// the search fields.
func finalGateRequestFixture() *BenchmarkRequest {
	f := func(v float64) *float64 { return &v }
	n := 5
	return &BenchmarkRequest{
		ProtocolVersion: WorkerProtocolVersion,
		ID:              "bench-final-0001",
		SourcePath:      "/media/example/source.mkv",
		SourceDuration:  5400.5,
		Metric:          "both",
		Samples: []BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 120, DurationSeconds: 10, CenterSeconds: 125},
			{Index: 1, StartSeconds: 2700.25, DurationSeconds: 10, CenterSeconds: 2705.25},
		},
		Quality: &BenchmarkQualityConfig{
			PreferredMetric: "vmaf",
			VMAF: &BenchmarkQualityThresholds{
				Target: 96, Minimum: 95, MarginalTolerance: f(0.5), Model: "vmaf_v0.6.1",
				GuardrailEnforcement: "enforce", P5Minimum: f(92), WorstWindowMinimum: f(93),
				WorstWindowSeconds: 10, FrameThreshold: f(80), MaxFramesBelowThreshold: &n,
			},
			SSIM:            &BenchmarkQualityThresholds{Target: 0.98, Minimum: 0.97},
			Banding:         &BenchmarkBandingConfig{Enabled: true, Metric: "cambi", Mode: "full_ref", Enforcement: "enforce", MaxMean: f(0.5), MaxPeak: f(1.5)},
			FinalValidation: &BenchmarkFinalValidationConfig{Mode: "sampled"},
		},
		SourceSHA256:      "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Mode:              "size",
		ModePolicyVersion: "transcode-modes-v3",
	}
}

// The deployed server stores this digest beside the request it froze and
// compares it again before the final gate runs, so the request's JSON must
// not change when search-only fields leave the struct.
func TestBenchmarkRequestDigestIsStable(t *testing.T) {
	const want = "sha256:bbb1fc99d23a17180bdadb81b51d81fec7baee90e9b0d812f7acc920bbed2462"
	got, err := DigestBenchmarkRequest(finalGateRequestFixture())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		raw, _ := json.Marshal(finalGateRequestFixture())
		t.Fatalf("digest = %s, want %s\nrequest: %s", got, want, raw)
	}
}
