package transcodeworker

// PR2 worker-endpoint tests: the new versioned routes the Navigatorr
// HTTPExecutor depends on (doctor, capabilities, benchmarks). Transcode
// job routes remain covered by http_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestHTTP_DoctorSuccessAndShape(t *testing.T) {
	_, srv, _, _ := newHTTPTestSetup(t, "")
	rec := doRequest(t, srv, http.MethodGet, "/v1/doctor", "", "")
	// Doctor runs real environmental checks; on non-darwin CI it reports
	// 503 with ok=false rather than faking success.
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("doctor code=%d body=%s", rec.Code, rec.Body.String())
	}
	var res DoctorResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("doctor json: %v body=%s", err, rec.Body.String())
	}
	if rec.Code == http.StatusOK && !res.OK {
		t.Errorf("200 doctor must have ok=true: %+v", res)
	}
	if rec.Code == http.StatusServiceUnavailable && res.OK {
		t.Errorf("503 doctor must have ok=false: %+v", res)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("doctor content-type=%q", ct)
	}
	rec = doRequest(t, srv, http.MethodPost, "/v1/doctor", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST doctor: want 405, got %d", rec.Code)
	}
}

func TestHTTP_CapabilitiesShape(t *testing.T) {
	_, srv, _, _ := newHTTPTestSetup(t, "")
	rec := doRequest(t, srv, http.MethodGet, "/v1/capabilities", "", "")
	// Without an ffmpeg binary in CI, probe fails closed with 500 (never a
	// faked capability set). With ffmpeg present it returns the versioned
	// report. Both shapes are asserted loosely here; the executor-side
	// fingerprint/protocol checks are covered in transcode/http_test.go.
	if rec.Code != http.StatusOK && rec.Code != http.StatusInternalServerError {
		t.Fatalf("capabilities code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusOK {
		var caps transcode.WorkerCapabilities
		if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
			t.Fatalf("capabilities json: %v", err)
		}
		if caps.ProtocolVersion != transcode.WorkerProtocolVersion {
			t.Errorf("protocol_version=%d, want %d", caps.ProtocolVersion, transcode.WorkerProtocolVersion)
		}
		if caps.CapabilityFingerprint == "" {
			t.Errorf("capability_fingerprint must be set on success")
		}
		if err := transcode.VerifyCapabilityFingerprint(caps); err != nil {
			t.Errorf("fingerprint must verify: %v", err)
		}
	}
	rec = doRequest(t, srv, http.MethodPost, "/v1/capabilities", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST capabilities: want 405, got %d", rec.Code)
	}
}

func benchmarkRequestFor(t *testing.T, id, source string) string {
	t.Helper()
	b, err := json.Marshal(transcode.BenchmarkRequest{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              id,
		SourcePath:      source,
		Metric:          "vmaf",
		Samples:         []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 1, DurationSeconds: 5}},
		Candidates:      []transcode.BenchmarkCandidate{{ID: "c1", Quality: 65}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHTTP_BenchmarkSubmitStatusCancelWiring(t *testing.T) {
	// Capacity 0 would reject everything; use a wide-open worker and a
	// selfExe that can never spawn (submit then fails 500 without hanging).
	tempDir := t.TempDir()
	sourceFile := filepath.Join(tempDir, "source.mkv")
	if err := os.WriteFile(sourceFile, []byte("dummy"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &WorkerConfig{StateDir: filepath.Join(tempDir, "jobs"), AllowedRoots: []string{tempDir}, MaxParallelJobs: 8, Quality: 65}
	worker := NewWorker(cfg)
	srv := NewServer(worker, filepath.Join(tempDir, "nonexistent-transcode-binary"), "", "")

	// Unknown fields rejected (no raw-arg smuggling surface).
	badUnknown := `{"protocol_version": 2, "id": "bench-unknown-fields", "source_path": "` + sourceFile + `", "metric": "vmaf", "samples": [{"index": 0, "start_seconds": 1, "duration_seconds": 5}], "candidates": [{"id": "c1", "quality": 65}], "ffmpeg_args": ["-crf", "20"]}`
	rec := doRequest(t, srv, http.MethodPost, "/v1/benchmarks", badUnknown, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown fields: want 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Malformed JSON -> 400 envelope.
	rec = doRequest(t, srv, http.MethodPost, "/v1/benchmarks", `{"protocol_version": `, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed: want 400, got %d", rec.Code)
	}

	// Invalid benchmark id (transcode-shaped) fails closed.
	rec = doRequest(t, srv, http.MethodPost, "/v1/benchmarks", benchmarkRequestFor(t, "job-not-bench", sourceFile), "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-bench id: want 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Unknown benchmark GET/cancel -> 404.
	rec = doRequest(t, srv, http.MethodGet, "/v1/benchmarks/bench-does-not-exist", "", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown bench GET: want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, srv, http.MethodPost, "/v1/benchmarks/bench-does-not-exist/cancel", "", "")
	// Cancel is reconciling: unknown id -> 404 (worker has no record).
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown bench cancel: want 404, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Seed a terminal benchmark record directly and verify idempotent
	// re-submit returns 200 terminal (no spawn) plus GET wiring.
	seedDir := filepath.Join(tempDir, "jobs", "bench-seeded-1")
	req := transcode.BenchmarkRequest{
		ProtocolVersion: transcode.WorkerProtocolVersion, ID: "bench-seeded-1", SourcePath: sourceFile,
		Metric: "vmaf", Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 1, DurationSeconds: 5}},
		Candidates: []transcode.BenchmarkCandidate{{ID: "c1", Quality: 65}},
	}
	digest, err := transcode.DigestBenchmarkRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	seed := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion, ID: "bench-seeded-1", Status: "completed",
		Source: sourceFile, Metric: "vmaf", Samples: req.Samples, Candidates: req.Candidates,
		RequestDigest: digest, Attempt: 1, RunToken: "run-seed",
		CreatedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(),
	}
	if err := SaveBenchmarkAtomic(filepath.Join(seedDir, "benchmark.json"), seed); err != nil {
		t.Fatal(err)
	}
	rec = doRequest(t, srv, http.MethodPost, "/v1/benchmarks", benchmarkRequestFor(t, "bench-seeded-1", sourceFile), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("idempotent terminal resubmit: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resubmit transcode.BenchmarkSubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resubmit); err != nil {
		t.Fatalf("resubmit json: %v", err)
	}
	if resubmit.Status != "completed" || resubmit.ProtocolVersion != transcode.WorkerProtocolVersion {
		t.Errorf("unexpected resubmit: %+v", resubmit)
	}
	rec = doRequest(t, srv, http.MethodGet, "/v1/benchmarks/bench-seeded-1", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("seeded GET: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var bst transcode.BenchmarkStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &bst); err != nil {
		t.Fatalf("bench status json: %v", err)
	}
	if bst.ProtocolVersion != transcode.WorkerProtocolVersion || bst.ID != "bench-seeded-1" || bst.Status != "completed" {
		t.Errorf("unexpected bench status: %+v", bst)
	}
}

func TestHTTP_BenchmarkTraversalNeverRedirects(t *testing.T) {
	_, srv, _, _ := newHTTPTestSetup(t, "")
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/benchmarks/../evil"},
		{http.MethodPost, "/v1/benchmarks/../evil/cancel"},
		{http.MethodGet, "/v1/benchmarks/a%2Fb"},
		{http.MethodGet, "/v1/benchmarks/%2e%2e"},
		{http.MethodGet, "/v1/benchmarks/job-plain"},
	} {
		rec := doRequest(t, srv, tc.method, tc.path, "", "")
		if rec.Code >= 300 && rec.Code < 400 {
			t.Errorf("%s %s: must never redirect, got %d", tc.method, tc.path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("%s %s: must not set Location, got %q", tc.method, tc.path, loc)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: want 400, got %d (%s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	// Deep non-dot path 404s safely without redirect.
	rec := doRequest(t, srv, http.MethodGet, "/v1/benchmarks/bench-a/b", "", "")
	if rec.Code >= 300 && rec.Code < 400 {
		t.Errorf("deep bench path must never redirect, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("deep bench path must not set Location, got %q", loc)
	}
}

func TestHTTP_DoctorCapabilitiesRequireAuth(t *testing.T) {
	_, srv, _, _ := newHTTPTestSetup(t, "s3cret-token")
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/doctor"},
		{http.MethodGet, "/v1/capabilities"},
		{http.MethodPost, "/v1/benchmarks"},
		{http.MethodGet, "/v1/benchmarks/bench-x"},
		{http.MethodPost, "/v1/benchmarks/bench-x/cancel"},
	} {
		rec := doRequest(t, srv, tc.method, tc.path, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: want 401, got %d", tc.method, tc.path, rec.Code)
		}
	}
	// Health/ready/doctor/capabilities all gated when a token is set.
	rec := doRequest(t, srv, http.MethodGet, "/v1/doctor", "", "s3cret-token")
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Errorf("authed doctor: want 200/503, got %d (%s)", rec.Code, rec.Body.String())
	}
	_ = httptest.NewRecorder()
}
