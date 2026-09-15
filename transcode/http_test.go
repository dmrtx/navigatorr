package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newClosedListener binds an ephemeral loopback port and closes it, so the
// returned address is guaranteed (barring a race) to refuse connections.
func newClosedListener(t *testing.T) (string, error) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr, nil
}

func testHTTPMappings() []PathMapping {
	return []PathMapping{{Local: "/local/media", Remote: "/Volumes/media"}}
}

func newTestHTTPExecutor(t *testing.T, srv *httptest.Server) *HTTPExecutor {
	t.Helper()
	exec, err := NewHTTPExecutor(HTTPConfig{
		BaseURL:        srv.URL,
		RequestTimeout: 5 * time.Second,
		SubmitTimeout:  5 * time.Second,
		PathMappings:   testHTTPMappings(),
	})
	if err != nil {
		t.Fatalf("NewHTTPExecutor: %v", err)
	}
	return exec
}

func validTestCaps(t *testing.T) WorkerCapabilities {
	t.Helper()
	caps := WorkerCapabilities{
		ProtocolVersion: WorkerProtocolVersion,
		WorkerVersion:   "1.0.0",
		BuildGitCommit:  "abcdef0",
		FFmpegVersion:   "7.1",
		Encoders:        map[string]bool{"hevc_videotoolbox": true},
		Filters:         map[string]bool{"scale": true},
		EncoderDetails: map[string]EncoderCapabilities{
			"hevc_videotoolbox": {Encoder: "hevc_videotoolbox", Available: true, Profiles: []string{"main"}, PixelFormats: []string{"nv12"}, Options: []string{"profile"}},
		},
	}
	fp, err := ComputeCapabilityFingerprint(caps)
	if err != nil {
		t.Fatal(err)
	}
	caps.CapabilityFingerprint = fp
	return caps
}

func TestHTTPExecutor_Doctor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/doctor" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"ok": true}`)
	}))
	defer srv.Close()

	if err := newTestHTTPExecutor(t, srv).Doctor(context.Background()); err != nil {
		t.Fatalf("Doctor: %v", err)
	}

	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, `{"ok": false, "error": "ffmpeg not accessible"}`)
	}))
	defer failSrv.Close()
	if err := newTestHTTPExecutor(t, failSrv).Doctor(context.Background()); err == nil || !strings.Contains(err.Error(), "ffmpeg not accessible") {
		t.Fatalf("expected doctor failure, got %v", err)
	}
}

func TestHTTPExecutor_Capabilities(t *testing.T) {
	caps := validTestCaps(t)
	body, _ := json.Marshal(caps)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/capabilities" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer srv.Close()

	got, err := newTestHTTPExecutor(t, srv).Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if got.Fingerprint() != caps.Fingerprint() {
		t.Errorf("fingerprint mismatch")
	}

	tampered := caps
	tampered.CapabilityFingerprint = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	tamperedBody, _ := json.Marshal(tampered)
	tamperSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(tamperedBody)
	}))
	defer tamperSrv.Close()
	if _, err := newTestHTTPExecutor(t, tamperSrv).Capabilities(context.Background()); err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("expected fingerprint mismatch, got %v", err)
	}

	protoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"protocol_version": 99, "capability_fingerprint": "x"}`)
	}))
	defer protoSrv.Close()
	if _, err := newTestHTTPExecutor(t, protoSrv).Capabilities(context.Background()); err == nil || !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("expected protocol mismatch, got %v", err)
	}
}

func TestHTTPExecutor_SubmitStatusCancel(t *testing.T) {
	var gotSubmit map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs":
			_ = json.NewDecoder(r.Body).Decode(&gotSubmit)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintln(w, `{"id": "job-1", "status": "queued"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job-1":
			fmt.Fprintln(w, `{"id": "job-1", "status": "running", "progress": 42.5, "candidate_path": "/Volumes/media/out.mkv"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs/job-1/cancel":
			fmt.Fprintln(w, `{"id": "job-1", "status": "cancelled"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintln(w, `{"error": "not found"}`)
		}
	}))
	defer srv.Close()
	exec := newTestHTTPExecutor(t, srv)
	ctx := context.Background()

	job, err := exec.Submit(ctx, Request{ID: "job-1", SourcePath: "/local/media/a.mkv", CandidatePath: "/local/media/b.mkv", Profile: "hevc-vt"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if job.ID != "job-1" {
		t.Errorf("job id = %q", job.ID)
	}
	// Path mapping parity: daemon must observe REMOTE paths.
	if gotSubmit["source_path"] != "/Volumes/media/a.mkv" || gotSubmit["candidate_path"] != "/Volumes/media/b.mkv" {
		t.Errorf("daemon saw untranslated paths: %v", gotSubmit)
	}

	st, err := exec.Status(ctx, "job-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Status != StatusRunning || st.Progress != 42.5 {
		t.Errorf("unexpected status: %+v", st)
	}
	// Reverse translation parity: remote -> local.
	if st.CandidatePath != "/local/media/out.mkv" {
		t.Errorf("candidate not translated back: %q", st.CandidatePath)
	}

	if err := exec.Cancel(ctx, "job-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}

func TestHTTPExecutor_BearerAuth(t *testing.T) {
	const token = "s3cret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintln(w, `{"error": "unauthorized"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"ok": true}`)
	}))
	defer srv.Close()

	exec, _ := NewHTTPExecutor(HTTPConfig{BaseURL: srv.URL, Token: token, RequestTimeout: 5 * time.Second, SubmitTimeout: 5 * time.Second, PathMappings: testHTTPMappings()})
	if err := exec.Doctor(context.Background()); err != nil {
		t.Fatalf("authed doctor: %v", err)
	}

	bare, _ := NewHTTPExecutor(HTTPConfig{BaseURL: srv.URL, RequestTimeout: 5 * time.Second, SubmitTimeout: 5 * time.Second, PathMappings: testHTTPMappings()})
	err := bare.Doctor(context.Background())
	var herr *HTTPError
	if err == nil {
		t.Fatal("expected 401 without token")
	}
	if !errorAsHTTP(err, &herr) || herr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected typed 401, got %v", err)
	}
}

func errorAsHTTP(err error, target **HTTPError) bool {
	for err != nil {
		if h, ok := err.(*HTTPError); ok {
			*target = h
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestHTTPExecutor_Non2xxMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/jobs":
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintln(w, `{"id": "job-busy", "status": "", "error": "worker busy: maximum parallel jobs (1) reached"}`)
		case "/v1/jobs/job-1":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintln(w, `{"error": "job not found"}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(w, `{"error": "boom"}`)
		}
	}))
	defer srv.Close()
	exec := newTestHTTPExecutor(t, srv)
	ctx := context.Background()

	_, err := exec.Submit(ctx, Request{ID: "job-busy", SourcePath: "/local/media/a.mkv", CandidatePath: "/local/media/b.mkv"})
	if err == nil || !strings.Contains(err.Error(), "worker busy") {
		t.Fatalf("busy must preserve classifier substring, got %v", err)
	}
	var herr *HTTPError
	if !errorAsHTTP(err, &herr) || herr.StatusCode != http.StatusConflict {
		t.Fatalf("busy must be typed 409, got %v", err)
	}

	_, err = exec.Status(ctx, "job-1")
	if !errorAsHTTP(err, &herr) || herr.StatusCode != http.StatusNotFound {
		t.Fatalf("expected typed 404, got %v", err)
	}
}

func TestHTTPExecutor_BoundedMalformedResponses(t *testing.T) {
	garbageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `this is not json{{{`)
	}))
	defer garbageSrv.Close()
	if _, err := newTestHTTPExecutor(t, garbageSrv).Status(context.Background(), "job-1"); err == nil || !strings.Contains(err.Error(), "parsing status response") {
		t.Fatalf("malformed JSON must be a bounded parse error, got %v", err)
	}

	huge := strings.Repeat("e", maxHTTPResponseBytes+1024)
	hugeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": huge})
	}))
	defer hugeSrv.Close()
	err := newTestHTTPExecutor(t, hugeSrv).Doctor(context.Background())
	if err == nil {
		t.Fatal("oversized body must fail closed")
	}
	// The bounded-body guard must hold end to end (unwrap: transport
	// wrappers preserve the typed cause for errors.As callers).
	var herr *HTTPError
	if errorAsHTTP(err, &herr) {
		if len(herr.Message) > maxHTTPErrorLen {
			t.Errorf("error message not bounded: %d chars", len(herr.Message))
		}
	} else if len(err.Error()) > maxHTTPResponseBytes {
		t.Errorf("untyped error must still be bounded, got %d chars", len(err.Error()))
	}
}

func TestHTTPExecutor_NoRedirectFollowing(t *testing.T) {
	var followed atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/v1/jobs/job-1-final")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	mux.HandleFunc("/v1/jobs/job-1-final", func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		fmt.Fprintln(w, `{"id": "job-1", "status": "completed"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := newTestHTTPExecutor(t, srv).Status(context.Background(), "job-1")
	if err == nil {
		t.Fatal("redirect must fail closed, got nil error")
	}
	var herr *HTTPError
	if !errorAsHTTP(err, &herr) || herr.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("expected typed 301 (no follow), got %v", err)
	}
	if followed.Load() != 0 {
		t.Errorf("redirect target must never be fetched (followed=%d)", followed.Load())
	}
}

func TestHTTPExecutor_SubmitTimeoutIsUncertainSingleAttempt(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintln(w, `{"id": "job-slow", "status": "queued"}`)
	}))
	defer srv.Close()

	exec, _ := NewHTTPExecutor(HTTPConfig{
		BaseURL: srv.URL, RequestTimeout: time.Second, SubmitTimeout: 100 * time.Millisecond, PathMappings: testHTTPMappings(),
	})
	_, err := exec.Submit(context.Background(), Request{ID: "job-slow", SourcePath: "/local/media/a.mkv", CandidatePath: "/local/media/b.mkv"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !IsTransportUncertain(err) {
		t.Fatalf("timeout must be *UncertainError (UNKNOWN, not encode failure), got %T: %v", err, err)
	}
	// Give the slow handler a chance to record any illicit retry, then assert
	// exactly one transport attempt was observed.
	time.Sleep(700 * time.Millisecond)
	if got := attempts.Load(); got != 1 {
		t.Errorf("submit must never auto-retry at transport layer: attempts=%d, want 1", got)
	}
}

func TestHTTPExecutor_ConnectionFailureIsUncertain(t *testing.T) {
	// Bind then close to get a guaranteed-refused local port.
	lis, err := newClosedListener(t)
	if err != nil {
		t.Skipf("no local port: %v", err)
	}
	exec, _ := NewHTTPExecutor(HTTPConfig{BaseURL: "http://" + lis, RequestTimeout: 2 * time.Second, SubmitTimeout: 2 * time.Second, PathMappings: testHTTPMappings()})
	_, err = exec.Submit(context.Background(), Request{ID: "job-x", SourcePath: "/local/media/a.mkv", CandidatePath: "/local/media/b.mkv"})
	if err == nil || !IsTransportUncertain(err) {
		t.Fatalf("connection failure must be uncertain, got %v", err)
	}
	var ue *UncertainError
	if ok := errorAsUncertain(err, &ue); !ok || ue.JobID != "job-x" || ue.Op != "submit" {
		t.Fatalf("uncertain error must carry op/job context, got %+v", ue)
	}
}

func errorAsUncertain(err error, target **UncertainError) bool {
	for err != nil {
		if u, ok := err.(*UncertainError); ok {
			*target = u
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestHTTPExecutor_PathMappingParity(t *testing.T) {
	exec, _ := NewHTTPExecutor(HTTPConfig{
		BaseURL: "http://127.0.0.1:8097", PathMappings: []PathMapping{
			{Local: "/media", Remote: "/Volumes/media"},
			{Local: "/media/Anime/Special", Remote: "/Volumes/fast-storage/Special"},
		},
	})
	got, err := exec.TranslateLocalToRemote("/media/Anime/Special/OVA.mkv")
	if err != nil || got != "/Volumes/fast-storage/Special/OVA.mkv" {
		t.Errorf("longest-prefix: got %q err %v", got, err)
	}
	if _, err := exec.TranslateLocalToRemote("/media2/test.mkv"); err == nil {
		t.Error("boundary safety: /media2 must not match /media")
	}
	back, err := exec.TranslateRemoteToLocal("/Volumes/media/Movies/T.mkv")
	if err != nil || back != "/media/Movies/T.mkv" {
		t.Errorf("reverse: got %q err %v", back, err)
	}
	if _, err := exec.TranslateRemoteToLocal("/Volumes/media_backup/f.mkv"); err == nil {
		t.Error("reverse boundary safety violated")
	}
	empty, _ := NewHTTPExecutor(HTTPConfig{BaseURL: "http://127.0.0.1:8097"})
	if _, err := empty.TranslateLocalToRemote("/media/a.mkv"); err == nil {
		t.Error("empty mappings must fail closed")
	}
}

func TestHTTPExecutor_TokenFileAndValidation(t *testing.T) {
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokPath, []byte("  file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	exec, err := NewHTTPExecutor(HTTPConfig{BaseURL: "http://127.0.0.1:8097", TokenFile: tokPath, PathMappings: testHTTPMappings()})
	if err != nil {
		t.Fatal(err)
	}
	if exec.token != "file-secret" {
		t.Errorf("token file not trimmed/loaded: %q", exec.token)
	}
	if _, err := NewHTTPExecutor(HTTPConfig{}); err == nil {
		t.Error("empty base_url must fail")
	}
	if _, err := NewHTTPExecutor(HTTPConfig{BaseURL: "ftp://host/x"}); err == nil {
		t.Error("non-http scheme must fail closed")
	}
	if _, err := NewHTTPExecutor(HTTPConfig{BaseURL: "http://127.0.0.1:8097", TokenFile: filepath.Join(dir, "missing")}); err == nil {
		t.Error("missing token file must fail")
	}
}

func TestHTTPExecutor_BenchmarkRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/benchmarks":
			var req BenchmarkRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.SourcePath != "/Volumes/media/source.mkv" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error": "unexpected source %s"}`, req.SourcePath)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintln(w, `{"protocol_version": 1, "id": "bench-test-123", "status": "queued"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/benchmarks/bench-test-123":
			fmt.Fprintln(w, `{"protocol_version": 1, "id": "bench-test-123", "status": "running", "source_path": "/Volumes/media/source.mkv", "progress": 50.0}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/benchmarks/bench-test-123/cancel":
			fmt.Fprintln(w, `{"protocol_version": 1, "id": "bench-test-123", "status": "cancelled"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintln(w, `{"error": "not found"}`)
		}
	}))
	defer srv.Close()
	exec := newTestHTTPExecutor(t, srv)
	ctx := context.Background()

	job, err := exec.BenchmarkSubmit(ctx, BenchmarkRequest{
		ProtocolVersion: WorkerProtocolVersion, ID: "bench-test-123", SourcePath: "/local/media/source.mkv",
		Metric: "vmaf", Samples: []BenchmarkSampleWindow{{Index: 0, StartSeconds: 1, DurationSeconds: 5}},
		Candidates: []BenchmarkCandidate{{ID: "c1", Quality: 65}},
	})
	if err != nil || job.ID != "bench-test-123" {
		t.Fatalf("BenchmarkSubmit: job=%+v err=%v", job, err)
	}
	st, err := exec.BenchmarkStatus(ctx, "bench-test-123")
	if err != nil || st.Status != "running" {
		t.Fatalf("BenchmarkStatus: %+v err=%v", st, err)
	}
	if st.SourcePath != "/local/media/source.mkv" {
		t.Errorf("benchmark source not translated back: %q", st.SourcePath)
	}
	if err := exec.BenchmarkCancel(ctx, "bench-test-123"); err != nil {
		t.Fatalf("BenchmarkCancel: %v", err)
	}
	if _, err := exec.BenchmarkStatus(ctx, "not-a-bench-id"); err == nil {
		t.Error("invalid benchmark id must fail closed")
	}
}

// singleShotServer serves one canned status/body per request while counting
// attempts. The handler runs before counting so a test can assert the
// executor performed exactly one transport attempt (no hidden retry).
func singleShotServer(t *testing.T, attempts *atomic.Int32, code int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
}

func submitTestRequest() Request {
	return Request{ID: "job-1", SourcePath: "/local/media/a.mkv", CandidatePath: "/local/media/b.mkv", Profile: "hevc-vt"}
}

func benchmarkTestRequest() BenchmarkRequest {
	return BenchmarkRequest{
		ProtocolVersion: WorkerProtocolVersion, ID: "bench-test-123", SourcePath: "/local/media/source.mkv",
		Metric: "vmaf", Samples: []BenchmarkSampleWindow{{Index: 0, StartSeconds: 1, DurationSeconds: 5}},
		Candidates: []BenchmarkCandidate{{ID: "c1", Quality: 65}},
	}
}

func TestHTTPExecutor_SubmitTruncated2xxIsUncertainSingleAttempt(t *testing.T) {
	var attempts atomic.Int32
	// 201 with a truncated JSON acknowledgement: the worker may have queued
	// the job while the ack was lost. Must be UNKNOWN, exactly once.
	srv := singleShotServer(t, &attempts, http.StatusCreated, `{"id": "job-1", "status": "que`)
	defer srv.Close()

	_, err := newTestHTTPExecutor(t, srv).Submit(context.Background(), submitTestRequest())
	if err == nil || !IsTransportUncertain(err) {
		t.Fatalf("truncated submit ack must be *UncertainError, got %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("submit must attempt exactly once, got %d", got)
	}
}

func TestHTTPExecutor_SubmitConnectionResetMidBodyIsUncertain(t *testing.T) {
	var attempts atomic.Int32
	// True transport truncation: 201 headers promise 1000 bytes, the server
	// delivers a fragment then drops the connection.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("server does not support hijacking")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		fragment := `{"id": "job-1", "status": "que`
		_, _ = buf.WriteString("HTTP/1.1 201 Created\r\nContent-Type: application/json\r\nContent-Length: 1000\r\nConnection: close\r\n\r\n" + fragment)
		_ = buf.Flush()
	}))
	defer srv.Close()

	_, err := newTestHTTPExecutor(t, srv).Submit(context.Background(), submitTestRequest())
	if err == nil || !IsTransportUncertain(err) {
		t.Fatalf("reset mid-body submit must be *UncertainError, got %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("submit must attempt exactly once, got %d", got)
	}
}

func TestHTTPExecutor_SubmitAmbiguousAckIsUncertain(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty object", `{}`},
		{"mismatched id", `{"id": "job-other", "status": "queued"}`},
		{"empty status", `{"id": "job-1", "status": ""}`},
		{"unknown status", `{"id": "job-1", "status": "bogus"}`},
		{"error field on 2xx", `{"id": "job-1", "status": "queued", "error": "worker busy: maximum parallel jobs (1) reached"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := singleShotServer(t, &attempts, http.StatusCreated, tc.body)
			defer srv.Close()

			_, err := newTestHTTPExecutor(t, srv).Submit(context.Background(), submitTestRequest())
			if err == nil || !IsTransportUncertain(err) {
				t.Fatalf("ambiguous submit ack %s must be *UncertainError, got %v", tc.body, err)
			}
			if got := attempts.Load(); got != 1 {
				t.Errorf("submit must attempt exactly once, got %d", got)
			}
		})
	}
}

func TestHTTPExecutor_CancelTruncated2xxIsUncertainSingleAttempt(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"truncated json", `{"id": "job-1", "status": "cancell`},
		{"empty object", `{}`},
		{"mismatched id", `{"id": "job-other", "status": "cancelled"}`},
		{"wrong status", `{"id": "job-1", "status": "running"}`},
		{"error field on 2xx", `{"id": "job-1", "status": "cancelled", "error": "boom"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := singleShotServer(t, &attempts, http.StatusOK, tc.body)
			defer srv.Close()

			err := newTestHTTPExecutor(t, srv).Cancel(context.Background(), "job-1")
			if err == nil || !IsTransportUncertain(err) {
				t.Fatalf("ambiguous cancel ack %s must be *UncertainError (never false success), got %v", tc.body, err)
			}
			if got := attempts.Load(); got != 1 {
				t.Errorf("cancel must attempt exactly once, got %d", got)
			}
		})
	}
}

func TestHTTPExecutor_BenchmarkSubmitAmbiguous2xxIsUncertain(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"truncated json", `{"protocol_version": 1, "id": "bench-test-123", "status": "que`},
		{"mismatched id", `{"protocol_version": 1, "id": "bench-other", "status": "queued"}`},
		{"empty status", `{"protocol_version": 1, "id": "bench-test-123", "status": ""}`},
		{"protocol mismatch on 2xx", `{"protocol_version": 99, "id": "bench-test-123", "status": "queued"}`},
		{"error field on 2xx", `{"protocol_version": 1, "id": "bench-test-123", "status": "queued", "error": "worker busy: maximum parallel jobs (1) reached"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := singleShotServer(t, &attempts, http.StatusCreated, tc.body)
			defer srv.Close()

			_, err := newTestHTTPExecutor(t, srv).BenchmarkSubmit(context.Background(), benchmarkTestRequest())
			if err == nil || !IsTransportUncertain(err) {
				t.Fatalf("ambiguous benchmark submit ack %s must be *UncertainError, got %v", tc.body, err)
			}
			if got := attempts.Load(); got != 1 {
				t.Errorf("benchmark submit must attempt exactly once, got %d", got)
			}
		})
	}
}

func TestHTTPExecutor_BenchmarkCancelAmbiguous2xxIsUncertain(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"truncated json", `{"protocol_version": 1, "id": "bench-test-123", "status": "cancell`},
		{"mismatched id", `{"protocol_version": 1, "id": "bench-other", "status": "cancelled"}`},
		{"wrong status", `{"protocol_version": 1, "id": "bench-test-123", "status": "running"}`},
		{"protocol mismatch on 2xx", `{"protocol_version": 99, "id": "bench-test-123", "status": "cancelled"}`},
		{"error field on 2xx", `{"protocol_version": 1, "id": "bench-test-123", "status": "cancelled", "error": "boom"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := singleShotServer(t, &attempts, http.StatusOK, tc.body)
			defer srv.Close()

			err := newTestHTTPExecutor(t, srv).BenchmarkCancel(context.Background(), "bench-test-123")
			if err == nil || !IsTransportUncertain(err) {
				t.Fatalf("ambiguous benchmark cancel ack %s must be *UncertainError, got %v", tc.body, err)
			}
			if got := attempts.Load(); got != 1 {
				t.Errorf("benchmark cancel must attempt exactly once, got %d", got)
			}
		})
	}
}

func TestHTTPExecutor_MutatingNon2xxStaysDefinitive(t *testing.T) {
	// A complete 4xx/5xx rejection is definitive and must NOT become
	// UncertainError — even with a malformed body.
	var attempts atomic.Int32
	srv := singleShotServer(t, &attempts, http.StatusConflict, `not json{{{`)
	defer srv.Close()
	exec := newTestHTTPExecutor(t, srv)
	ctx := context.Background()

	_, err := exec.Submit(ctx, submitTestRequest())
	var herr *HTTPError
	if !errorAsHTTP(err, &herr) || herr.StatusCode != http.StatusConflict {
		t.Fatalf("garbage 409 must stay typed *HTTPError 409, got %v", err)
	}
	if IsTransportUncertain(err) {
		t.Errorf("definitive 409 must NOT be uncertain: %v", err)
	}

	if err := exec.Cancel(ctx, "job-1"); !errorAsHTTP(err, &herr) || IsTransportUncertain(err) {
		t.Fatalf("garbage 409 cancel must stay definitive *HTTPError, got %v", err)
	}

	_, err = exec.BenchmarkSubmit(ctx, benchmarkTestRequest())
	if !errorAsHTTP(err, &herr) || IsTransportUncertain(err) {
		t.Fatalf("garbage 409 benchmark submit must stay definitive *HTTPError, got %v", err)
	}
	if err := exec.BenchmarkCancel(ctx, "bench-test-123"); !errorAsHTTP(err, &herr) || IsTransportUncertain(err) {
		t.Fatalf("garbage 409 benchmark cancel must stay definitive *HTTPError, got %v", err)
	}
	if got := attempts.Load(); got != 4 {
		t.Errorf("expected exactly 4 attempts (one per op), got %d", got)
	}
}

func TestHTTPExecutor_MutatingRedirectStaysDefinitive(t *testing.T) {
	// A 3xx on a mutating POST is a definitive refusal (redirects are never
	// followed): typed *HTTPError, non-uncertain, target never fetched.
	var attempts, followed atomic.Int32
	mux := http.NewServeMux()
	redirect := func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Location", "/v1/followed")
		w.WriteHeader(http.StatusMovedPermanently)
	}
	mux.HandleFunc("/v1/jobs", redirect)
	mux.HandleFunc("/v1/jobs/job-1/cancel", redirect)
	mux.HandleFunc("/v1/benchmarks", redirect)
	mux.HandleFunc("/v1/benchmarks/bench-test-123/cancel", redirect)
	mux.HandleFunc("/v1/followed", func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		fmt.Fprintln(w, `{"id": "job-1", "status": "queued"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	exec := newTestHTTPExecutor(t, srv)
	ctx := context.Background()

	_, err := exec.Submit(ctx, submitTestRequest())
	var herr *HTTPError
	if !errorAsHTTP(err, &herr) || herr.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("submit 301 must stay typed *HTTPError 301, got %v", err)
	}
	if IsTransportUncertain(err) {
		t.Errorf("definitive 301 must NOT be uncertain: %v", err)
	}
	if err := exec.Cancel(ctx, "job-1"); !errorAsHTTP(err, &herr) || IsTransportUncertain(err) {
		t.Fatalf("cancel 301 must stay definitive *HTTPError, got %v", err)
	}
	if _, err := exec.BenchmarkSubmit(ctx, benchmarkTestRequest()); !errorAsHTTP(err, &herr) || IsTransportUncertain(err) {
		t.Fatalf("benchmark submit 301 must stay definitive *HTTPError, got %v", err)
	}
	if err := exec.BenchmarkCancel(ctx, "bench-test-123"); !errorAsHTTP(err, &herr) || IsTransportUncertain(err) {
		t.Fatalf("benchmark cancel 301 must stay definitive *HTTPError, got %v", err)
	}
	if got := attempts.Load(); got != 4 {
		t.Errorf("expected exactly 4 attempts (one per op), got %d", got)
	}
	if got := followed.Load(); got != 0 {
		t.Errorf("redirect target must never be fetched (followed=%d)", got)
	}
}

func TestHTTPExecutor_MutatingOversized4xxStaysDefinitive(t *testing.T) {
	// An oversized non-2xx body is still a definitive rejection: the status
	// line itself is complete. Typed *HTTPError, non-uncertain.
	var attempts atomic.Int32
	huge := `{"error": "` + strings.Repeat("e", maxHTTPResponseBytes+1024) + `"}`
	srv := singleShotServer(t, &attempts, http.StatusRequestEntityTooLarge, huge)
	defer srv.Close()
	exec := newTestHTTPExecutor(t, srv)
	ctx := context.Background()

	_, err := exec.Submit(ctx, submitTestRequest())
	var herr *HTTPError
	if !errorAsHTTP(err, &herr) || herr.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized 413 must stay typed *HTTPError 413, got %v", err)
	}
	if IsTransportUncertain(err) {
		t.Errorf("definitive 413 must NOT be uncertain: %v", err)
	}
	if len(herr.Message) > maxHTTPErrorLen {
		t.Errorf("definitive error message must stay bounded, got %d chars", len(herr.Message))
	}
	if err := exec.Cancel(ctx, "job-1"); !errorAsHTTP(err, &herr) || IsTransportUncertain(err) {
		t.Fatalf("oversized 413 cancel must stay definitive *HTTPError, got %v", err)
	}
	if _, err := exec.BenchmarkSubmit(ctx, benchmarkTestRequest()); !errorAsHTTP(err, &herr) || IsTransportUncertain(err) {
		t.Fatalf("oversized 413 benchmark submit must stay definitive *HTTPError, got %v", err)
	}
	if err := exec.BenchmarkCancel(ctx, "bench-test-123"); !errorAsHTTP(err, &herr) || IsTransportUncertain(err) {
		t.Fatalf("oversized 413 benchmark cancel must stay definitive *HTTPError, got %v", err)
	}
	if got := attempts.Load(); got != 4 {
		t.Errorf("expected exactly 4 attempts (one per op), got %d", got)
	}
}

func TestHTTPExecutor_BenchmarkProtocolMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"protocol_version": 99, "id": "bench-test-123", "status": "queued"}`)
	}))
	defer srv.Close()
	_, err := newTestHTTPExecutor(t, srv).BenchmarkSubmit(context.Background(), BenchmarkRequest{
		ProtocolVersion: WorkerProtocolVersion, ID: "bench-test-123", SourcePath: "/local/media/source.mkv",
		Metric: "vmaf", Samples: []BenchmarkSampleWindow{{Index: 0, StartSeconds: 1, DurationSeconds: 5}},
		Candidates: []BenchmarkCandidate{{ID: "c1", Quality: 65}},
	})
	// A 2xx protocol mismatch on a mutating POST is UNKNOWN (the benchmark
	// may already exist), not a definitive rejection.
	if err == nil || !IsTransportUncertain(err) {
		t.Fatalf("2xx protocol mismatch must be *UncertainError, got %v", err)
	}
	if !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("uncertain error must preserve protocol context, got %v", err)
	}
}
