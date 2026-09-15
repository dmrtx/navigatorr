package transcodeworker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newHTTPTestSetup builds a Worker + Server backed by temp dirs. selfExe is
// os.Args[0] but tests avoid spawning by using validation failures, busy
// slots, or pre-seeded job records.
func newHTTPTestSetup(t *testing.T, token string) (*Worker, *Server, string, string) {
	t.Helper()
	tempDir := t.TempDir()
	sourceFile := filepath.Join(tempDir, "source.mkv")
	if err := os.WriteFile(sourceFile, []byte("dummy-media"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &WorkerConfig{
		StateDir:        filepath.Join(tempDir, "jobs"),
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: 1,
		Quality:         65,
	}
	worker := NewWorker(cfg)
	srv := NewServer(worker, os.Args[0], "", token)
	return worker, srv, tempDir, sourceFile
}

func doRequest(t *testing.T, srv *Server, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHTTP_HealthAndReadySuccess(t *testing.T) {
	_, srv, _, _ := newHTTPTestSetup(t, "")
	rec := doRequest(t, srv, http.MethodGet, "/v1/health", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health code=%d body=%s", rec.Code, rec.Body.String())
	}
	var health map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("health json: %v", err)
	}
	if health["ok"] != true {
		t.Errorf("health ok != true: %v", health)
	}

	rec = doRequest(t, srv, http.MethodGet, "/v1/ready", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ready code=%d body=%s", rec.Code, rec.Body.String())
	}
	var ready map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &ready); err != nil {
		t.Fatalf("ready json: %v", err)
	}
	if ready["ready"] != true {
		t.Errorf("ready != true: %v", ready)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("ready content-type=%q, want application/json", ct)
	}
}

func TestHTTP_MalformedJSON(t *testing.T) {
	_, srv, _, _ := newHTTPTestSetup(t, "")
	rec := doRequest(t, srv, http.MethodPost, "/v1/jobs", `{"id": "job-1", "source_path": `, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed JSON, got %d (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error envelope json: %v", err)
	}
	if body["error"] == "" {
		t.Errorf("expected error field, got %v", body)
	}
}

func TestHTTP_InvalidJobIDsFailClosed(t *testing.T) {
	_, srv, tempDir, sourceFile := newHTTPTestSetup(t, "")
	cand := filepath.Join(tempDir, "cand.mkv")

	invalidIDs := []string{
		"",
		"   ",
		"../evil",
		"a/b",
		`a\b`,
		"job..traversal",
		"bench-123",
		"job with spaces",
		"job@evil",
		".",
		"..",
		strings.Repeat("a", MaxTranscodeJobIDLength+1),
	}
	for _, id := range invalidIDs {
		payload, _ := json.Marshal(map[string]string{
			"id":             id,
			"source_path":    sourceFile,
			"candidate_path": cand,
		})
		rec := doRequest(t, srv, http.MethodPost, "/v1/jobs", string(payload), "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST id=%q: expected 400, got %d (%s)", id, rec.Code, rec.Body.String())
		}
	}

	// Path-param traversal must fail closed with 400 and MUST NOT redirect
	// (ServeMux path cleaning previously turned /v1/jobs/../evil into a 307
	// to /v1/evil before validation; the outer rejectTraversal guard now
	// rejects before the mux).
	for _, path := range []string{
		"/v1/jobs/../evil",
		"/v1/jobs/./evil",
		"/v1/jobs/a%2Fb",
		"/v1/jobs/a%2fb",
		"/v1/jobs/bench-123",
		"/v1/jobs/%2e%2e",
	} {
		rec := doRequest(t, srv, http.MethodGet, path, "", "")
		if rec.Code >= 300 && rec.Code < 400 {
			t.Errorf("GET %s: must never redirect, got %d (Location=%q)", path, rec.Code, rec.Header().Get("Location"))
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("GET %s: must not set Location header, got %q", path, loc)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s: expected 400, got %d (%s)", path, rec.Code, rec.Body.String())
		}
	}

	// Cancel with invalid ID fails closed with 400 and no redirect.
	rec := doRequest(t, srv, http.MethodPost, "/v1/jobs/../evil/cancel", "", "")
	if rec.Code >= 300 && rec.Code < 400 {
		t.Errorf("cancel traversal: must never redirect, got %d (Location=%q)", rec.Code, rec.Header().Get("Location"))
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("cancel traversal: must not set Location header, got %q", loc)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("cancel traversal: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestHTTP_TraversalNeverRedirects(t *testing.T) {
	_, srv, _, _ := newHTTPTestSetup(t, "")
	cases := []struct {
		method string
		path   string
		// want is the exact expected code; 0 means "400 preferred,
		// 404 acceptable, but never 3xx and never with a Location header".
		want int
	}{
		{http.MethodGet, "/v1/jobs/../evil", http.StatusBadRequest},
		{http.MethodPost, "/v1/jobs/../evil/cancel", http.StatusBadRequest},
		{http.MethodGet, "/v1/jobs/./evil", http.StatusBadRequest},
		{http.MethodGet, "/v1/jobs/a%2Fb", http.StatusBadRequest},
		{http.MethodGet, "/v1/jobs/a%2fb", http.StatusBadRequest},
		{http.MethodGet, "/v1/jobs/a%5Cb", http.StatusBadRequest},
		{http.MethodGet, "/v1/jobs/%2e%2e", http.StatusBadRequest},
		{http.MethodGet, "/v1/jobs/%2E%2E/evil", http.StatusBadRequest},
		{http.MethodPost, "/v1/jobs/%2e%2e/cancel", http.StatusBadRequest},
		// Deep non-dot path: no redirect risk; inner handler 404s safely.
		{http.MethodGet, "/v1/jobs/a/b", 0},
	}
	for _, tc := range cases {
		rec := doRequest(t, srv, tc.method, tc.path, "", "")
		if rec.Code >= 300 && rec.Code < 400 {
			t.Errorf("%s %s: must never redirect, got %d (Location=%q)", tc.method, tc.path, rec.Code, rec.Header().Get("Location"))
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("%s %s: must not set Location header, got %q", tc.method, tc.path, loc)
		}
		if tc.want != 0 && rec.Code != tc.want {
			t.Errorf("%s %s: expected %d, got %d (%s)", tc.method, tc.path, tc.want, rec.Code, rec.Body.String())
		}
		if tc.want == 0 && rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: expected 400 or safely-rejected 404, got %d (%s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestHTTP_AuthBeforeTraversal(t *testing.T) {
	_, srv, _, _ := newHTTPTestSetup(t, "s3cret-token")

	// Unauthenticated traversal must be rejected by auth first: 401, never a
	// redirect and never a 400 that would leak validation before auth.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/jobs/../evil"},
		{http.MethodPost, "/v1/jobs/../evil/cancel"},
		{http.MethodGet, "/v1/jobs/a%2Fb"},
	} {
		rec := doRequest(t, srv, tc.method, tc.path, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: expected 401, got %d (%s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if rec.Code >= 300 && rec.Code < 400 {
			t.Errorf("%s %s without token: must never redirect, got %d", tc.method, tc.path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("%s %s without token: must not set Location, got %q", tc.method, tc.path, loc)
		}
	}

	// Authenticated traversal reaches validation: 400 with no redirect.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/jobs/../evil"},
		{http.MethodPost, "/v1/jobs/../evil/cancel"},
		{http.MethodGet, "/v1/jobs/a%2Fb"},
	} {
		rec := doRequest(t, srv, tc.method, tc.path, "", "s3cret-token")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s with token: expected 400, got %d (%s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if rec.Code >= 300 && rec.Code < 400 {
			t.Errorf("%s %s with token: must never redirect, got %d", tc.method, tc.path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("%s %s with token: must not set Location, got %q", tc.method, tc.path, loc)
		}
	}
}

func TestHTTP_ErrorMessagesBounded(t *testing.T) {
	_, srv, tempDir, sourceFile := newHTTPTestSetup(t, "")
	hugeID := strings.Repeat("a", 10000)
	payload, _ := json.Marshal(map[string]string{
		"id":             hugeID,
		"source_path":    sourceFile,
		"candidate_path": filepath.Join(tempDir, "cand.mkv"),
	})
	rec := doRequest(t, srv, http.MethodPost, "/v1/jobs", string(payload), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized ID, got %d", rec.Code)
	}
	if len(rec.Body.Bytes()) > 4096 {
		t.Errorf("error response must be bounded, got %d bytes", len(rec.Body.Bytes()))
	}
	var envelope map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("error envelope json: %v", err)
	}
	if len(envelope["error"]) > maxHTTPErrorLen {
		t.Errorf("error field must be capped at %d chars, got %d", maxHTTPErrorLen, len(envelope["error"]))
	}

	// Direct unit check: short messages pass through untouched.
	if got := boundHTTPErrorMessage("short"); got != "short" {
		t.Errorf("short message must be untouched, got %q", got)
	}
	if got := boundHTTPErrorMessage(strings.Repeat("b", maxHTTPErrorLen+100)); len(got) > maxHTTPErrorLen || !strings.HasSuffix(got, "...") {
		t.Errorf("oversized message must be truncated with ellipsis, got len %d", len(got))
	}
}

func TestHTTP_ReadyVerifiesWritable(t *testing.T) {
	_, srv, tempDir, _ := newHTTPTestSetup(t, "")

	// Success case leaves no probe files behind.
	rec := doRequest(t, srv, http.MethodGet, "/v1/ready", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ready code=%d body=%s", rec.Code, rec.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(tempDir, "jobs"))
	if err != nil {
		t.Fatalf("reading state dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ready-") {
			t.Errorf("readiness probe file %q must be removed", e.Name())
		}
	}

	// Deterministic unwritable case without chmod assumptions: StateDir
	// pointing at an existing regular file makes MkdirAll fail -> 503.
	blocker := filepath.Join(tempDir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	badCfg := &WorkerConfig{
		StateDir:        blocker,
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: 1,
	}
	badSrv := NewServer(NewWorker(badCfg), os.Args[0], "", "")
	badRec := httptest.NewRecorder()
	badSrv.Handler().ServeHTTP(badRec, httptest.NewRequest(http.MethodGet, "/v1/ready", nil))
	if badRec.Code != http.StatusServiceUnavailable {
		t.Errorf("file-as-state-dir: expected 503, got %d (%s)", badRec.Code, badRec.Body.String())
	}
}

func TestHTTP_PathTraversalOutsideRootsFailsClosed(t *testing.T) {
	_, srv, tempDir, _ := newHTTPTestSetup(t, "")
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.mkv")
	_ = os.WriteFile(outsideFile, []byte("outside"), 0644)

	payload, _ := json.Marshal(map[string]string{
		"id":             "job-outside",
		"source_path":    outsideFile,
		"candidate_path": filepath.Join(tempDir, "cand.mkv"),
	})
	rec := doRequest(t, srv, http.MethodPost, "/v1/jobs", string(payload), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for source outside roots, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "outside allowed roots") {
		t.Errorf("expected outside-roots message, got %s", rec.Body.String())
	}
}

func TestHTTP_UnknownJob404(t *testing.T) {
	_, srv, _, _ := newHTTPTestSetup(t, "")
	rec := doRequest(t, srv, http.MethodGet, "/v1/jobs/does-not-exist-123", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown job, got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, srv, http.MethodPost, "/v1/jobs/does-not-exist-123/cancel", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown cancel, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestHTTP_BearerAuth(t *testing.T) {
	_, srv, _, _ := newHTTPTestSetup(t, "s3cret-token")

	// No header -> 401 on all routes.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/health"},
		{http.MethodGet, "/v1/ready"},
		{http.MethodGet, "/v1/jobs/some-id"},
		{http.MethodPost, "/v1/jobs"},
		{http.MethodPost, "/v1/jobs/some-id/cancel"},
	} {
		rec := doRequest(t, srv, tc.method, tc.path, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: expected 401, got %d", tc.method, tc.path, rec.Code)
		}
	}

	// Wrong token -> 401.
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: expected 401, got %d", rec.Code)
	}

	// Malformed scheme -> 401.
	req = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Token s3cret-token")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("malformed scheme: expected 401, got %d", rec.Code)
	}

	// Correct token -> 200.
	rec = doRequest(t, srv, http.MethodGet, "/v1/health", "", "s3cret-token")
	if rec.Code != http.StatusOK {
		t.Errorf("correct token: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	// No-auth server (loopback default) allows requests without headers.
	_, openSrv, _, _ := newHTTPTestSetup(t, "")
	rec = doRequest(t, openSrv, http.MethodGet, "/v1/health", "", "")
	if rec.Code != http.StatusOK {
		t.Errorf("open server health: expected 200, got %d", rec.Code)
	}
}

func TestHTTP_SubmitStatusCancelWiring(t *testing.T) {
	_, srv, tempDir, sourceFile := newHTTPTestSetup(t, "")

	// Seed a running job directly (no ffmpeg needed). PID = test process (alive).
	jobDir := filepath.Join(tempDir, "jobs", "job-wired-1")
	cand := filepath.Join(tempDir, "wired-cand.mkv")
	seed := &JobRecord{
		ID:        "job-wired-1",
		Status:    "running",
		PID:       os.Getpid(),
		Source:    sourceFile,
		Candidate: cand,
		CreatedAt: time.Now().UTC(),
		StartedAt: time.Now().UTC(),
	}
	if err := SaveJobAtomic(filepath.Join(jobDir, "job.json"), seed); err != nil {
		t.Fatal(err)
	}

	// GET status wiring -> 200 running.
	rec := doRequest(t, srv, http.MethodGet, "/v1/jobs/job-wired-1", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status code=%d body=%s", rec.Code, rec.Body.String())
	}
	var st JobStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("status json: %v", err)
	}
	if st.ID != "job-wired-1" || st.Status != "running" {
		t.Errorf("unexpected status: %+v", st)
	}

	// Idempotent POST with same ID reuses the running job without spawning.
	payload, _ := json.Marshal(SubmitRequest{
		ID:            "job-wired-1",
		SourcePath:    sourceFile,
		CandidatePath: cand,
	})
	rec = doRequest(t, srv, http.MethodPost, "/v1/jobs", string(payload), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("idempotent submit code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Invalid plan is rejected 400 without spawning (fail closed, digest checks).
	badPayload, _ := json.Marshal(map[string]any{
		"id":             "job-bad-plan",
		"source_path":    sourceFile,
		"candidate_path": filepath.Join(tempDir, "bad-cand.mkv"),
		"plan": map[string]any{
			"container": "mp4",
		},
	})
	rec = doRequest(t, srv, http.MethodPost, "/v1/jobs", string(badPayload), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad plan: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(tempDir, "jobs", "job-bad-plan")); !os.IsNotExist(err) {
		t.Errorf("invalid plan must not create a job directory")
	}

	// PR3 durable queue wiring: MaxParallelJobs=1 already occupied by
	// job-wired-1, so a fresh valid submit persists as queued (201), not 409.
	busyPayload, _ := json.Marshal(SubmitRequest{
		ID:            "job-wired-2",
		SourcePath:    sourceFile,
		CandidatePath: filepath.Join(tempDir, "wired-cand-2.mkv"),
	})
	rec = doRequest(t, srv, http.MethodPost, "/v1/jobs", string(busyPayload), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("durable queue submit: expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	var queued SubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &queued); err != nil {
		t.Fatalf("queued json: %v", err)
	}
	if queued.Status != "queued" {
		t.Fatalf("expected queued status, got %+v", queued)
	}

	// Cancel wiring -> 200 cancelled. Use a non-alive PID so Cancel does not
	// signal the test process; Status would otherwise mark it failed first.
	cancelDir := filepath.Join(tempDir, "jobs", "job-wired-cancel")
	cancelCand := filepath.Join(tempDir, "wired-cancel-cand.mkv")
	_ = os.WriteFile(cancelCand, []byte("partial"), 0644)
	cancelSeed := &JobRecord{
		ID:        "job-wired-cancel",
		Status:    "queued",
		PID:       999999,
		Source:    sourceFile,
		Candidate: cancelCand,
		CreatedAt: time.Now().UTC(),
	}
	if err := SaveJobAtomic(filepath.Join(cancelDir, "job.json"), cancelSeed); err != nil {
		t.Fatal(err)
	}
	rec = doRequest(t, srv, http.MethodPost, "/v1/jobs/job-wired-cancel/cancel", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel code=%d body=%s", rec.Code, rec.Body.String())
	}
	var cancelled JobStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cancelled); err != nil {
		t.Fatalf("cancel json: %v", err)
	}
	if cancelled.Status != "cancelled" {
		t.Errorf("expected cancelled, got %+v", cancelled)
	}
	// Follow-up status reflects cancellation.
	rec = doRequest(t, srv, http.MethodGet, "/v1/jobs/job-wired-cancel", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("post-cancel status code=%d", rec.Code)
	}
	var after JobStatusResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &after)
	if after.Status != "cancelled" {
		t.Errorf("expected cancelled after cancel, got %q", after.Status)
	}
	if _, err := os.Stat(cancelCand); !os.IsNotExist(err) {
		t.Errorf("cancel must clean the incomplete candidate")
	}
}

func TestHTTP_SafeBind(t *testing.T) {
	if !IsLoopbackBind("127.0.0.1:8097") {
		t.Errorf("127.0.0.1 should be loopback")
	}
	if !IsLoopbackBind("[::1]:8097") {
		t.Errorf("::1 should be loopback")
	}
	if !IsLoopbackBind("localhost:8097") {
		t.Errorf("localhost should be loopback")
	}
	for _, addr := range []string{"0.0.0.0:8097", ":8097", "192.0.2.10:8097", "example.com:8097"} {
		if IsLoopbackBind(addr) {
			t.Errorf("%q should NOT be loopback", addr)
		}
	}
	if err := ValidateServeAddr("0.0.0.0:8097", ""); err == nil {
		t.Errorf("non-loopback without token must fail closed")
	}
	if err := ValidateServeAddr("0.0.0.0:8097", "tok"); err != nil {
		t.Errorf("non-loopback with token should pass: %v", err)
	}
	if err := ValidateServeAddr("127.0.0.1:8097", ""); err != nil {
		t.Errorf("loopback without token should pass: %v", err)
	}
}

func TestHTTP_TokenFile(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token.txt")
	if err := os.WriteFile(tokenPath, []byte("  file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveHTTPToken("", tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != "file-secret" {
		t.Errorf("got %q, want file-secret", got)
	}
	// Explicit token wins over file.
	got, err = ResolveHTTPToken("explicit", tokenPath)
	if err != nil || got != "explicit" {
		t.Errorf("explicit should win: got %q err %v", got, err)
	}
	if _, err := ResolveHTTPToken("", filepath.Join(dir, "missing")); err == nil {
		t.Errorf("missing token file must error")
	}
}
