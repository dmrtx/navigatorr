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
	}
}

func TestValidateBenchmarkJobID(t *testing.T) {
	validIDs := []string{
		"bench-1",
		"bench-job123",
		"bench-my_benchmark-v1.0",
		"bench-ABCDEF123456",
		"bench-" + strings.Repeat("a", MaxBenchmarkIDLength-6),
	}
	for _, id := range validIDs {
		if err := ValidateBenchmarkJobID(id); err != nil {
			t.Errorf("expected %q to be valid job ID, got: %v", id, err)
		}
	}

	invalidIDs := []string{
		"",
		"   ",
		"regular-job-123",   // missing bench- prefix
		"job-bench-123",     // bench- not at start
		"bench-",            // length 6 < 7
		"bench-..",          // path traversal
		"bench-../etc",      // path traversal
		"bench-foo/bar",     // path separator
		"bench-foo\\bar",    // path separator
		"bench-job:123",     // illegal colon
		"bench-job\x00123",  // null byte
		"bench-foo%2fbar",   // url encoded slash
		"bench-foo%5cbar",   // url encoded backslash
		"bench-.hidden",     // dot immediately following bench-
		"bench-_underscore", // underscore immediately following bench-
		"bench--dash",       // dash immediately following bench-
		"bench-samples",     // reserved name
		"bench-scratch",     // reserved name
		"bench-lock",        // reserved name
		"bench-con",         // reserved DOS name
		"bench-prn",         // reserved DOS name
		"bench-aux",         // reserved DOS name
		"bench-nul",         // reserved DOS name
		"bench-com1",        // reserved DOS name
		"bench-job@123",     // invalid character
		"bench-job$123",     // shell special character
		"bench-" + strings.Repeat("a", MaxBenchmarkIDLength-5), // 129 chars exceeds MaxBenchmarkIDLength
	}

	for _, id := range invalidIDs {
		if err := ValidateBenchmarkJobID(id); err == nil {
			t.Errorf("expected error for invalid ID %q, got nil", id)
		}
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

	// Changing a window changes digest
	req2.Samples[0].StartSeconds = 150
	d3, _ := DigestBenchmarkRequest(&req2)
	if d1 == d3 {
		t.Fatalf("expected different digest when a sample window changes")
	}

	// Changing source content identity changes the request digest, so the
	// final gate is bound to the source bytes it was planned for.
	req3 := validTestBenchmarkRequest()
	req3.SourceSHA256 = strings.Repeat("a", 64)
	d4, err := DigestBenchmarkRequest(&req3)
	if err != nil {
		t.Fatalf("DigestBenchmarkRequest failed: %v", err)
	}
	req4 := validTestBenchmarkRequest()
	req4.SourceSHA256 = strings.Repeat("b", 64)
	d5, err := DigestBenchmarkRequest(&req4)
	if err != nil {
		t.Fatalf("DigestBenchmarkRequest failed: %v", err)
	}
	if d1 == d4 || d4 == d5 {
		t.Fatalf("source SHA-256 must alter the request digest (%s / %s / %s)", d1, d4, d5)
	}
}
