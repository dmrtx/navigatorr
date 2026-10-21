package transcodeworker

// Protocol hardening tests: typed not-found, the structured error envelope
// (code/retryable/Retry-After), cancel on finished jobs, and request logging.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

type protocolEnvelope struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Error     string `json:"error"`
	Code      string `json:"code"`
	Retryable *bool  `json:"retryable"`
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) protocolEnvelope {
	t.Helper()
	var env protocolEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding envelope: %v (%s)", err, rec.Body.String())
	}
	return env
}

func assertEnvelope(t *testing.T, rec *httptest.ResponseRecorder, status int, code string, retryable bool) protocolEnvelope {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status=%d want %d (%s)", rec.Code, status, rec.Body.String())
	}
	env := decodeEnvelope(t, rec)
	if env.Code != code {
		t.Fatalf("code=%q want %q (%s)", env.Code, code, rec.Body.String())
	}
	if env.Retryable == nil || *env.Retryable != retryable {
		t.Fatalf("retryable=%v want %v (%s)", env.Retryable, retryable, rec.Body.String())
	}
	return env
}

func TestProtocol_MissingJobIsNotFound(t *testing.T) {
	w, tempDir, _ := newPR3Worker(t, 1, newStubSpawn())
	srv := NewServer(w, "test-exe", "", "")

	env := assertEnvelope(t, doRequest(t, srv, http.MethodGet, "/v1/jobs/job-missing-1", "", ""), http.StatusNotFound, transcode.WorkerErrorNotFound, false)
	if env.Error != "job not found" {
		t.Fatalf("missing job keeps its historical message, got %q", env.Error)
	}
	assertEnvelope(t, doRequest(t, srv, http.MethodPost, "/v1/jobs/job-missing-1/cancel", "", ""), http.StatusNotFound, transcode.WorkerErrorNotFound, false)
	if _, err := os.Stat(filepath.Join(tempDir, "jobs", "job-missing-1")); !os.IsNotExist(err) {
		t.Fatalf("cancel of an unknown job must not create its directory: %v", err)
	}
}

// A job.json that exists but cannot be read is not a missing job. Answering
// 404 would make Navigatorr's reconciler resubmit a job that may be running.
func TestProtocol_UnreadableJobIsServerError(t *testing.T) {
	w, tempDir, _ := newPR3Worker(t, 1, newStubSpawn())
	srv := NewServer(w, "test-exe", "", "")

	// job.json as a directory: a read error that is not fs.ErrNotExist and
	// does not depend on file permissions (tests may run as root).
	if err := os.MkdirAll(filepath.Join(tempDir, "jobs", "job-unreadable-1", "job.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	cancelWriteFile(t, filepath.Join(tempDir, "jobs", "job-corrupt-2", "job.json"), "{not json")

	for _, id := range []string{"job-unreadable-1", "job-corrupt-2"} {
		env := assertEnvelope(t, doRequest(t, srv, http.MethodGet, "/v1/jobs/"+id, "", ""), http.StatusInternalServerError, transcode.WorkerErrorInternal, true)
		if strings.Contains(env.Error, "job not found") || !strings.Contains(env.Error, "reading job state") {
			t.Fatalf("%s: a read failure must not read as a missing job: %q", id, env.Error)
		}
		assertEnvelope(t, doRequest(t, srv, http.MethodPost, "/v1/jobs/"+id+"/cancel", "", ""), http.StatusInternalServerError, transcode.WorkerErrorInternal, true)
	}
}

func TestProtocol_PermissionDeniedJobIsServerError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of mode")
	}
	w, tempDir, _ := newPR3Worker(t, 1, newStubSpawn())
	srv := NewServer(w, "test-exe", "", "")
	path := filepath.Join(tempDir, "jobs", "job-locked-1", "job.json")
	cancelWriteFile(t, path, `{"id":"job-locked-1","status":"running"}`)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	assertEnvelope(t, doRequest(t, srv, http.MethodGet, "/v1/jobs/job-locked-1", "", ""), http.StatusInternalServerError, transcode.WorkerErrorInternal, true)
}

func TestProtocol_NotFoundIsTypedNotTextual(t *testing.T) {
	// The old substring match turned any "reading job file" error into a 404.
	if isNotFoundError(errors.New("reading job file /state/job-x/job.json: permission denied")) {
		t.Fatal("a textual read failure must not classify as not found")
	}
	if !isNotFoundError(fmt.Errorf("status: %w", ErrJobNotFound)) {
		t.Fatal("a wrapped ErrJobNotFound must classify as not found")
	}
}

func TestProtocol_SubmitErrorClassification(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{&IdempotencyConflictError{Key: "k", ExistingJobID: "job-a"}, http.StatusConflict, transcode.WorkerErrorIdempotencyConflict},
		{errors.New("worker busy: max parallel jobs reached"), http.StatusTooManyRequests, transcode.WorkerErrorBusy},
		{errors.New("acquiring capacity lock: input/output error"), http.StatusInternalServerError, transcode.WorkerErrorInternal},
		{errors.New("source path outside allowed roots"), http.StatusBadRequest, transcode.WorkerErrorBadRequest},
	}
	for _, tc := range cases {
		status, code := classifySubmitError(tc.err)
		if status != tc.status || code != tc.code {
			t.Errorf("classifySubmitError(%q) = %d %q, want %d %q", tc.err, status, code, tc.status, tc.code)
		}
	}
}

func TestProtocol_CancelFinishedJobIsAlreadyTerminal(t *testing.T) {
	w, tempDir, sourceFile := newPR3Worker(t, 4, newStubSpawn())
	srv := NewServer(w, "test-exe", "", "")
	finished := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct{ id, status, errText string }{
		{"job-done-1", "completed", ""},
		{"job-failed-1", "failed", "encoder exited with status 1"},
	} {
		pr5SeedJob(t, tempDir, &JobRecord{ID: tc.id, Status: tc.status, Error: tc.errText, Source: sourceFile, Candidate: filepath.Join(tempDir, tc.id+".mkv"), ExecutionSpecDigest: pr5Digest, FinishedAt: finished})
		env := assertEnvelope(t, doRequest(t, srv, http.MethodPost, "/v1/jobs/"+tc.id+"/cancel", "", ""), http.StatusOK, transcode.WorkerErrorAlreadyTerminal, false)
		if env.ID != tc.id || env.Status != tc.status {
			t.Fatalf("%s: ack must carry the unchanged terminal status, got %+v", tc.id, env)
		}
	}

	pr5SeedJob(t, tempDir, &JobRecord{ID: "job-queued-1", Status: "queued", Source: sourceFile, Candidate: filepath.Join(tempDir, "q.mkv"), ExecutionSpecDigest: pr5Digest})
	rec := doRequest(t, srv, http.MethodPost, "/v1/jobs/job-queued-1/cancel", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("queued cancel status=%d (%s)", rec.Code, rec.Body.String())
	}
	if env := decodeEnvelope(t, rec); env.Status != "cancelled" || env.Code != "" {
		t.Fatalf("a real cancellation carries no code, got %+v", env)
	}
}

func TestProtocol_RequestLogCorrelation(t *testing.T) {
	const token = "s3cret-protocol-token"
	w, _, _ := newPR3Worker(t, 1, newStubSpawn())
	srv := NewServer(w, "test-exe", "", token)
	var lines []string
	srv.logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	h := srv.Handler()
	serve := func(method, path string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := serve(http.MethodGet, "/v1/jobs/job-log-1?tail=1", map[string]string{
		"Authorization":           "Bearer " + token,
		transcode.RequestIDHeader: "req-abc123",
		transcode.JobIDHeader:     "job-log-1",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
	if got := rec.Header().Get(transcode.RequestIDHeader); got != "req-abc123" {
		t.Fatalf("request ID must be echoed, got %q", got)
	}
	if len(lines) != 1 {
		t.Fatalf("want one log line, got %q", lines)
	}
	for _, want := range []string{"request_id=req-abc123", "method=GET", `path="/v1/jobs/job-log-1"`, "status=404", "duration=", "job_id=job-log-1"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("log line %q lacks %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], "tail=1") {
		t.Errorf("query strings must not be logged: %q", lines[0])
	}

	// Rejected requests are logged too; a malformed request ID is replaced.
	rec = serve(http.MethodGet, "/v1/ready", map[string]string{"Authorization": "Bearer wrong-" + token, transcode.RequestIDHeader: "bad id;\x01"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", rec.Code)
	}
	if got := rec.Header().Get(transcode.RequestIDHeader); !strings.HasPrefix(got, "w-") {
		t.Fatalf("malformed request ID must be replaced by a worker one, got %q", got)
	}
	if len(lines) != 2 || !strings.Contains(lines[1], "status=401") || !strings.Contains(lines[1], "job_id=-") {
		t.Fatalf("unauthorized request must be logged, got %q", lines)
	}

	// Successful liveness polls are not logged; failed ones are.
	serve(http.MethodGet, "/v1/health", map[string]string{"Authorization": "Bearer " + token})
	serve(http.MethodPost, "/v1/health", map[string]string{"Authorization": "Bearer " + token})
	if len(lines) != 3 || !strings.Contains(lines[2], "status=405") {
		t.Fatalf("want only the failed health request logged, got %q", lines)
	}

	for _, line := range lines {
		if strings.Contains(line, token) {
			t.Fatalf("the bearer token must never be logged: %q", line)
		}
	}
}
