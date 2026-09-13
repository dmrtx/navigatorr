package transcode

import (
	"strings"
	"testing"
)

func validTestBenchmarkRequest() BenchmarkRequest {
	return BenchmarkRequest{
		ProtocolVersion: WorkerProtocolVersion,
		ID:              "bench-test-job-001",
		SourcePath:      "/media/movies/Test.Movie.mkv",
		SourceDuration:  7200.0,
		Metric:          "vmaf",
		Samples: []BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 100.0, DurationSeconds: 20.0, CenterSeconds: 110.0},
			{Index: 1, StartSeconds: 500.0, DurationSeconds: 20.0, CenterSeconds: 510.0},
			{Index: 2, StartSeconds: 1000.0, DurationSeconds: 20.0, CenterSeconds: 1010.0},
		},
		Candidates: []BenchmarkCandidate{
			{ID: "cand_q60", Quality: 60, VideoProfile: "main10", PixelFormat: "p010le"},
			{ID: "cand_q65", Quality: 65, VideoProfile: "main10", PixelFormat: "p010le"},
			{ID: "cand_q70", Quality: 70, VideoProfile: "main10", PixelFormat: "p010le"},
		},
	}
}

func TestValidateBenchmarkRequest_Valid(t *testing.T) {
	req := validTestBenchmarkRequest()
	if err := ValidateBenchmarkRequest(&req); err != nil {
		t.Fatalf("expected valid benchmark request, got error: %v", err)
	}

	// SSIM metric is also valid
	req.Metric = "ssim"
	if err := ValidateBenchmarkRequest(&req); err != nil {
		t.Fatalf("expected valid SSIM benchmark request, got error: %v", err)
	}
}

func TestValidateBenchmarkRequest_ProtocolMismatch(t *testing.T) {
	req := validTestBenchmarkRequest()
	req.ProtocolVersion = WorkerProtocolVersion + 1
	err := ValidateBenchmarkRequest(&req)
	if err == nil || !strings.Contains(err.Error(), "protocol_version") {
		t.Fatalf("expected protocol_version error, got: %v", err)
	}

	req.ProtocolVersion = 0
	err = ValidateBenchmarkRequest(&req)
	if err == nil || !strings.Contains(err.Error(), "protocol_version") {
		t.Fatalf("expected protocol_version error for 0, got: %v", err)
	}
}

func TestValidateBenchmarkRequest_InvalidJobIDs(t *testing.T) {
	invalidIDs := []string{
		"",
		"   ",
		"regular-job-123", // missing bench- prefix
		"job-bench-123",   // bench- not at start
		"bench-",          // empty suffix
		"bench-../etc",    // path traversal
		"bench-foo/bar",   // path separator
		"bench-foo\\bar",  // path separator
		"bench-job@123",   // invalid character
		"bench-job$123",   // shell special character
		"bench-" + strings.Repeat("a", MaxBenchmarkIDLength), // exceeds length
	}

	for _, id := range invalidIDs {
		req := validTestBenchmarkRequest()
		req.ID = id
		if err := ValidateBenchmarkRequest(&req); err == nil {
			t.Errorf("expected error for invalid ID %q, got nil", id)
		}
	}
}

func TestValidateBenchmarkRequest_SourceValidation(t *testing.T) {
	req := validTestBenchmarkRequest()
	req.SourcePath = ""
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Errorf("expected error for empty source path, got nil")
	}

	req.SourcePath = "/path/with/\x00/null"
	if err := ValidateBenchmarkRequest(&req); err == nil || !strings.Contains(err.Error(), "null byte") {
		t.Errorf("expected error for null byte in source path, got: %v", err)
	}
}

func TestValidateBenchmarkRequest_MetricValidation(t *testing.T) {
	invalidMetrics := []string{"", "psnr", "vmaf_hd", "ssim_plus", "arbitrary"}
	for _, m := range invalidMetrics {
		req := validTestBenchmarkRequest()
		req.Metric = m
		if err := ValidateBenchmarkRequest(&req); err == nil || !strings.Contains(err.Error(), "invalid metric") {
			t.Errorf("expected metric error for %q, got: %v", m, err)
		}
	}
}

func TestValidateBenchmarkRequest_SampleWindowValidation(t *testing.T) {
	// Empty samples
	req := validTestBenchmarkRequest()
	req.Samples = nil
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Errorf("expected error for nil samples")
	}

	// Negative index
	req = validTestBenchmarkRequest()
	req.Samples[0].Index = -1
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Errorf("expected error for negative sample index")
	}

	// Duplicate index
	req = validTestBenchmarkRequest()
	req.Samples[1].Index = 0
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Errorf("expected error for duplicate sample index")
	}

	// Negative start seconds
	req = validTestBenchmarkRequest()
	req.Samples[0].StartSeconds = -5.0
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Errorf("expected error for negative start seconds")
	}

	// Zero or negative duration
	req = validTestBenchmarkRequest()
	req.Samples[0].DurationSeconds = 0.0
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Errorf("expected error for zero duration seconds")
	}

	// End time exceeds source duration
	req = validTestBenchmarkRequest()
	req.SourceDuration = 100.0
	req.Samples[0].StartSeconds = 90.0
	req.Samples[0].DurationSeconds = 20.0 // 110 > 100
	if err := ValidateBenchmarkRequest(&req); err == nil || !strings.Contains(err.Error(), "exceeds source duration") {
		t.Errorf("expected error for sample exceeding duration, got: %v", err)
	}

	// Exceeds max samples limit
	req = validTestBenchmarkRequest()
	req.Samples = make([]BenchmarkSampleWindow, MaxBenchmarkSamples+1)
	for i := range req.Samples {
		req.Samples[i] = BenchmarkSampleWindow{Index: i, StartSeconds: float64(i * 10), DurationSeconds: 5.0}
	}
	if err := ValidateBenchmarkRequest(&req); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("expected error for exceeding MaxBenchmarkSamples, got: %v", err)
	}
}

func TestValidateBenchmarkRequest_CandidateValidation(t *testing.T) {
	// Empty candidates
	req := validTestBenchmarkRequest()
	req.Candidates = nil
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Errorf("expected error for nil candidates")
	}

	// Quality out of range
	req = validTestBenchmarkRequest()
	req.Candidates[0].Quality = 0
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Errorf("expected error for quality 0")
	}
	req.Candidates[0].Quality = 101
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Errorf("expected error for quality 101")
	}

	// Duplicate candidate ID
	req = validTestBenchmarkRequest()
	req.Candidates[1].ID = req.Candidates[0].ID
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Errorf("expected error for duplicate candidate ID")
	}

	// Duplicate quality
	req = validTestBenchmarkRequest()
	req.Candidates[1].Quality = req.Candidates[0].Quality
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Errorf("expected error for duplicate candidate quality")
	}

	// Invalid candidate ID characters
	req = validTestBenchmarkRequest()
	req.Candidates[0].ID = "cand;rm -rf"
	if err := ValidateBenchmarkRequest(&req); err == nil {
		t.Errorf("expected error for malicious candidate ID")
	}
}

func TestDigestBenchmarkRequest_Determinism(t *testing.T) {
	req1 := validTestBenchmarkRequest()
	req2 := validTestBenchmarkRequest()

	d1, err := DigestBenchmarkRequest(&req1)
	if err != nil {
		t.Fatalf("DigestBenchmarkRequest failed: %v", err)
	}
	d2, err := DigestBenchmarkRequest(&req2)
	if err != nil {
		t.Fatalf("DigestBenchmarkRequest failed: %v", err)
	}

	if d1 != d2 {
		t.Fatalf("digests differ for identical requests: %s != %s", d1, d2)
	}

	// Changing candidate quality changes digest
	req2.Candidates[0].Quality = 55
	d3, _ := DigestBenchmarkRequest(&req2)
	if d1 == d3 {
		t.Fatalf("expected different digest when candidate quality changes")
	}
}
