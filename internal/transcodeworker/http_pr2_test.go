package transcodeworker

// PR2 worker-endpoint tests: the new versioned routes the Navigatorr
// HTTPExecutor depends on (doctor, capabilities). Transcode
// job routes remain covered by http_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

// The deployed server no longer calls the sample-test routes; an old caller
// gets a clear 410 for every method, never a panic.
func TestHTTP_RemovedBenchmarkRoutesAnswerGone(t *testing.T) {
	_, srv, _, _ := newHTTPTestSetup(t, "")
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/benchmarks", `{"protocol_version": 2, "id": "bench-old"}`},
		{http.MethodPost, "/v1/benchmarks", `{"protocol_version": `},
		{http.MethodGet, "/v1/benchmarks/bench-old", ""},
		{http.MethodPost, "/v1/benchmarks/bench-old/cancel", ""},
		{http.MethodGet, "/v1/benchmarks/bench-a/b", ""},
	} {
		rec := doRequest(t, srv, tc.method, tc.path, tc.body, "")
		if rec.Code != http.StatusGone {
			t.Errorf("%s %s: want 410, got %d (%s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("%s %s: must not set Location, got %q", tc.method, tc.path, loc)
		}
		if !strings.Contains(rec.Body.String(), "sample tests were removed") {
			t.Errorf("%s %s: body should say why, got %q", tc.method, tc.path, rec.Body.String())
		}
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
