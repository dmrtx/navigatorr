package transcode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSearchExtensionValidate(t *testing.T) {
	cases := []struct {
		name string
		ext  *SearchExtension
		base int
		ok   bool
	}{
		{"absent is fixed", nil, 600, true},
		{"typical", &SearchExtension{MaxSeconds: 1800, SteadySeconds: 180}, 600, true},
		{"ceiling", &SearchExtension{MaxSeconds: MaxSearchBudgetSeconds, SteadySeconds: MaxSearchSteadySeconds}, 600, true},
		{"smallest window", &SearchExtension{MaxSeconds: 601, SteadySeconds: MinSearchSteadySeconds}, 600, true},
		{"cap equals base", &SearchExtension{MaxSeconds: 600, SteadySeconds: 180}, 600, false},
		{"cap below base", &SearchExtension{MaxSeconds: 300, SteadySeconds: 180}, 600, false},
		{"cap over ceiling", &SearchExtension{MaxSeconds: MaxSearchBudgetSeconds + 1, SteadySeconds: 180}, 600, false},
		{"window too short", &SearchExtension{MaxSeconds: 1800, SteadySeconds: MinSearchSteadySeconds - 1}, 600, false},
		{"window too long", &SearchExtension{MaxSeconds: 1800, SteadySeconds: MaxSearchSteadySeconds + 1}, 600, false},
		{"no base to extend", &SearchExtension{MaxSeconds: 1800, SteadySeconds: 180}, 0, false},
	}
	for _, c := range cases {
		if err := c.ext.Validate(c.base); (err == nil) != c.ok {
			t.Errorf("%s: err=%v want ok=%v", c.name, err, c.ok)
		}
	}
	var none *SearchExtension
	if none.Clone() != nil {
		t.Fatal("nil clone")
	}
	orig := &SearchExtension{MaxSeconds: 1800, SteadySeconds: 180}
	clone := orig.Clone()
	clone.MaxSeconds = 1
	if orig.MaxSeconds != 1800 {
		t.Fatal("clone shares storage")
	}
}

func TestBenchmarkRequestSearchExtension(t *testing.T) {
	req := validTestBenchmarkRequest()
	req.Mode, req.SearchBudgetSeconds = "size", 600

	req.SearchExtension = &SearchExtension{MaxSeconds: 1800, SteadySeconds: 180}
	if err := ValidateBenchmarkRequest(&req); err != nil {
		t.Fatalf("valid extension rejected: %v", err)
	}
	req.SearchExtension = &SearchExtension{MaxSeconds: 500, SteadySeconds: 180}
	if err := ValidateBenchmarkRequest(&req); err == nil || !strings.Contains(err.Error(), "max_seconds") {
		t.Fatalf("cap below the base accepted: %v", err)
	}
	req.SearchExtension = &SearchExtension{MaxSeconds: 1800, SteadySeconds: 1}
	if err := ValidateBenchmarkRequest(&req); err == nil || !strings.Contains(err.Error(), "steady_seconds") {
		t.Fatalf("tiny steady window accepted: %v", err)
	}

	// Only mode searches have a budget to extend.
	profile := validTestBenchmarkRequest()
	profile.SearchExtension = &SearchExtension{MaxSeconds: 1800, SteadySeconds: 180}
	if err := ValidateBenchmarkRequest(&profile); err == nil || !strings.Contains(err.Error(), "search extension") {
		t.Fatalf("profile benchmark accepted an extension: %v", err)
	}
}

// An older worker rejects unknown fields, so a request without an extension
// must not mention it, and a status without busy time must stay as before.
func TestSearchExtensionWireShape(t *testing.T) {
	req := validTestBenchmarkRequest()
	req.Mode, req.SearchBudgetSeconds = "size", 600
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "search_extension") {
		t.Fatalf("request without an extension mentions it: %s", raw)
	}
	req.SearchExtension = &SearchExtension{MaxSeconds: 1800, SteadySeconds: 180}
	raw, _ = json.Marshal(req)
	if !strings.Contains(string(raw), `"search_extension":{"max_seconds":1800,"steady_seconds":180}`) {
		t.Fatalf("extension not on the wire: %s", raw)
	}
	status, _ := json.Marshal(BenchmarkStatus{})
	if strings.Contains(string(status), "search_worker_busy") || strings.Contains(string(status), "search_extension") {
		t.Fatalf("idle status carries budget fields: %s", status)
	}
}

func TestSearchBudgetExtensionChangesFingerprint(t *testing.T) {
	caps := WorkerCapabilities{ProtocolVersion: WorkerProtocolVersion, FFmpegVersion: "9.0.1", Encoders: map[string]bool{}, Filters: map[string]bool{}}
	before, err := ComputeCapabilityFingerprint(caps)
	if err != nil {
		t.Fatal(err)
	}
	caps.SearchBudgetExtension = true
	after, err := ComputeCapabilityFingerprint(caps)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("a worker that accepts search_extension must fingerprint differently from one that does not")
	}
}
