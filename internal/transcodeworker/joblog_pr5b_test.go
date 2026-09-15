package transcodeworker

// Phase 5B tests: persistent bounded per-job logs. The runner log source stays
// <StateDir>/<jobID>/ffmpeg.log; reads are always bounded and traversal-safe,
// missing logs are distinguishable, and the HTTP endpoint preserves auth and
// the existing 400/404/405 mapping.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pr5bWriteLog(t *testing.T, stateDir, id, content string) string {
	t.Helper()
	jobDir := filepath.Join(stateDir, "jobs", id)
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(jobDir, "ffmpeg.log")
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return logPath
}

func TestPR5B_ReadJobLogSmallExact(t *testing.T) {
	w, _, tempDir, _ := newHTTPTestSetup(t, "")
	content := "frame= 1 fps=0.0\nframe= 2 fps=30.0\n"
	pr5bWriteLog(t, tempDir, "job-log-small", content)

	log, err := w.ReadJobLog("job-log-small", 0)
	if err != nil {
		t.Fatalf("small log read must succeed: %v", err)
	}
	if log.Content != content {
		t.Fatalf("content mismatch: got %q want %q", log.Content, content)
	}
	if log.Truncated {
		t.Fatal("small log must not be marked truncated")
	}
	if log.SizeBytes != int64(len(content)) {
		t.Fatalf("size_bytes=%d want %d", log.SizeBytes, len(content))
	}
	if log.JobID != "job-log-small" {
		t.Fatalf("job_id=%q", log.JobID)
	}
}

func TestPR5B_ReadJobLogDefaultTailTruncates(t *testing.T) {
	w, _, tempDir, _ := newHTTPTestSetup(t, "")
	total := int(DefaultJobLogTailBytes) + 4096
	buf := bytes.Repeat([]byte("x"), total)
	marker := "TAIL-END-MARKER"
	copy(buf[total-len(marker):], marker)
	pr5bWriteLog(t, tempDir, "job-log-large", string(buf))

	log, err := w.ReadJobLog("job-log-large", 0)
	if err != nil {
		t.Fatalf("large log read must succeed: %v", err)
	}
	if !log.Truncated {
		t.Fatal("large log must be marked truncated")
	}
	if int64(len(log.Content)) != DefaultJobLogTailBytes {
		t.Fatalf("default tail length=%d want %d", len(log.Content), DefaultJobLogTailBytes)
	}
	if !strings.HasSuffix(log.Content, marker) {
		t.Fatal("tail must end with the file tail")
	}
	want := string(buf[total-int(DefaultJobLogTailBytes):])
	if log.Content != want {
		t.Fatal("tail content must equal the file's final default-tail bytes")
	}
	if log.SizeBytes != int64(total) {
		t.Fatalf("size_bytes=%d want %d", log.SizeBytes, total)
	}
}

func TestPR5B_ReadJobLogRespectsExplicitTail(t *testing.T) {
	w, _, tempDir, _ := newHTTPTestSetup(t, "")
	content := "0123456789abcdef"
	pr5bWriteLog(t, tempDir, "job-log-explicit", content)

	log, err := w.ReadJobLog("job-log-explicit", 4)
	if err != nil {
		t.Fatalf("explicit tail read: %v", err)
	}
	if log.Content != "cdef" || !log.Truncated {
		t.Fatalf("tail_bytes=4 must yield last 4 bytes truncated, got %q truncated=%v", log.Content, log.Truncated)
	}
}

func TestPR5B_ReadJobLogHardMaxEnforced(t *testing.T) {
	w, _, tempDir, _ := newHTTPTestSetup(t, "")
	buf := bytes.Repeat([]byte("y"), int(MaxJobLogTailBytes)*3)
	pr5bWriteLog(t, tempDir, "job-log-hardmax", string(buf))

	// Request far above the hard max: must be clamped, never unbounded.
	log, err := w.ReadJobLog("job-log-hardmax", MaxJobLogTailBytes*100)
	if err != nil {
		t.Fatalf("hard-max read: %v", err)
	}
	if int64(len(log.Content)) != MaxJobLogTailBytes {
		t.Fatalf("content length=%d want hard max %d", len(log.Content), MaxJobLogTailBytes)
	}
	if !log.Truncated {
		t.Fatal("clamped read must be marked truncated")
	}
}

func TestPR5B_ReadJobLogMissingIsNotFound(t *testing.T) {
	w, _, _, _ := newHTTPTestSetup(t, "")
	_, err := w.ReadJobLog("job-log-absent", 0)
	if err == nil {
		t.Fatal("missing log must not be empty success")
	}
	if !IsJobLogNotFound(err) {
		t.Fatalf("missing log must be a distinguishable not-found error, got %v", err)
	}
}

func TestPR5B_ReadJobLogEmptyIsNotNotFound(t *testing.T) {
	w, _, tempDir, _ := newHTTPTestSetup(t, "")
	pr5bWriteLog(t, tempDir, "job-log-empty", "")

	log, err := w.ReadJobLog("job-log-empty", 0)
	if err != nil {
		t.Fatalf("empty log must read successfully: %v", err)
	}
	if log.Content != "" || log.Truncated || log.SizeBytes != 0 {
		t.Fatalf("empty log must be empty success, got %+v", log)
	}
}

func TestPR5B_ReadJobLogRejectsTraversalAndInvalidIDs(t *testing.T) {
	w, _, tempDir, _ := newHTTPTestSetup(t, "")
	// A real log outside StateDir that traversal might try to reach.
	outside := filepath.Join(tempDir, "outside.log")
	if err := os.WriteFile(outside, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{
		"", " ", ".", "..", "../outside", "a/b", "a\\b", "..\\outside",
		"bench-reserved", "job\x00null",
	} {
		_, err := w.ReadJobLog(id, 0)
		if err == nil {
			t.Fatalf("invalid job id %q must be rejected", id)
		}
		if IsJobLogNotFound(err) {
			t.Fatalf("job id %q must be a validation error, not not-found", id)
		}
	}
}

func TestHTTP_JobLogsEndpointExactAndBounded(t *testing.T) {
	_, srv, tempDir, _ := newHTTPTestSetup(t, "")
	content := "ffmpeg version 7\nencoded 10 frames\n"
	pr5bWriteLog(t, tempDir, "job-http-log", content)

	rec := doRequest(t, srv, http.MethodGet, "/v1/jobs/job-http-log/logs", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("logs code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got JobLog
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("logs json: %v body=%s", err, rec.Body.String())
	}
	if got.JobID != "job-http-log" || got.Content != content || got.Truncated {
		t.Fatalf("unexpected logs body: %+v", got)
	}

	// Large log: default response bounded + truncated.
	big := bytes.Repeat([]byte("L"), int(DefaultJobLogTailBytes)+1234)
	pr5bWriteLog(t, tempDir, "job-http-big", string(big))
	rec = doRequest(t, srv, http.MethodGet, "/v1/jobs/job-http-big/logs", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("big logs code=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("big logs json: %v", err)
	}
	if !got.Truncated || int64(len(got.Content)) != DefaultJobLogTailBytes {
		t.Fatalf("default body must be bounded+truncated, got len=%d truncated=%v", len(got.Content), got.Truncated)
	}
}

func TestHTTP_JobLogsTailBytesParam(t *testing.T) {
	_, srv, tempDir, _ := newHTTPTestSetup(t, "")
	pr5bWriteLog(t, tempDir, "job-http-tail", "0123456789")

	rec := doRequest(t, srv, http.MethodGet, "/v1/jobs/job-http-tail/logs?tail_bytes=3", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("tail_bytes=3 code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got JobLog
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Content != "789" || !got.Truncated {
		t.Fatalf("tail_bytes=3 must yield last 3 bytes, got %q truncated=%v", got.Content, got.Truncated)
	}

	// Over-max tail_bytes is clamped, never rejected and never unbounded.
	huge := bytes.Repeat([]byte("z"), int(MaxJobLogTailBytes)+500)
	pr5bWriteLog(t, tempDir, "job-http-huge", string(huge))
	rec = doRequest(t, srv, http.MethodGet, "/v1/jobs/job-http-huge/logs?tail_bytes=99999999", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("over-max tail_bytes code=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if int64(len(got.Content)) != MaxJobLogTailBytes || !got.Truncated {
		t.Fatalf("over-max tail_bytes must clamp to hard max, got len=%d truncated=%v", len(got.Content), got.Truncated)
	}

	for _, raw := range []string{"0", "-1", "abc", "1.5", ""} {
		rec = doRequest(t, srv, http.MethodGet, "/v1/jobs/job-http-tail/logs?tail_bytes="+raw, "", "")
		if raw == "" {
			// Empty value means "no param": default behavior, not an error.
			if rec.Code != http.StatusOK {
				t.Fatalf("empty tail_bytes must use default, got %d (%s)", rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("tail_bytes=%q must be 400, got %d (%s)", raw, rec.Code, rec.Body.String())
		}
	}
}

func TestHTTP_JobLogsNotFoundAndMethod(t *testing.T) {
	_, srv, _, _ := newHTTPTestSetup(t, "")
	rec := doRequest(t, srv, http.MethodGet, "/v1/jobs/job-http-missing/logs", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing log must map to 404, got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, srv, http.MethodPost, "/v1/jobs/job-http-missing/logs", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST logs must map to 405, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestHTTP_JobLogsValidationAndTraversal(t *testing.T) {
	_, srv, _, _ := newHTTPTestSetup(t, "")
	for _, path := range []string{
		"/v1/jobs/../evil/logs",
		"/v1/jobs/a%2Fb/logs",
		"/v1/jobs/%2e%2e/logs",
		"/v1/jobs/bench-reserved/logs",
	} {
		rec := doRequest(t, srv, http.MethodGet, path, "", "")
		if rec.Code >= 300 && rec.Code < 400 {
			t.Fatalf("%s must never redirect, got %d", path, rec.Code)
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s must be 400, got %d (%s)", path, rec.Code, rec.Body.String())
		}
	}
}

func TestHTTP_JobLogsRequiresAuth(t *testing.T) {
	_, srv, tempDir, _ := newHTTPTestSetup(t, "s3cret-token")
	pr5bWriteLog(t, tempDir, "job-http-auth", "authed log")

	rec := doRequest(t, srv, http.MethodGet, "/v1/jobs/job-http-auth/logs", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("logs without token must be 401, got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, srv, http.MethodGet, "/v1/jobs/job-http-auth/logs", "", "s3cret-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("logs with token must be 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// pr5bSymlinkOrSkip creates a symlink or skips when the platform forbids it.
func pr5bSymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("os.Symlink unsupported on this platform: %v", err)
	}
}

func TestPR5B_ReadJobLogRejectsFfmpegLogSymlink(t *testing.T) {
	w, _, tempDir, _ := newHTTPTestSetup(t, "")
	secret := "PR5B-OUTSIDE-SECRET-LOG-CONTENT"
	outside := filepath.Join(tempDir, "outside-secret.log")
	if err := os.WriteFile(outside, []byte(secret), 0644); err != nil {
		t.Fatal(err)
	}

	jobDir := filepath.Join(tempDir, "jobs", "job-symlink-log")
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		t.Fatal(err)
	}
	pr5bSymlinkOrSkip(t, outside, filepath.Join(jobDir, "ffmpeg.log"))

	log, err := w.ReadJobLog("job-symlink-log", 0)
	if err == nil {
		t.Fatal("symlinked ffmpeg.log must be rejected, never read")
	}
	if IsJobLogNotFound(err) {
		t.Fatalf("symlink rejection must be fail-closed, not not-found: %v", err)
	}
	if strings.Contains(log.Content, secret) {
		t.Fatal("secret from outside StateDir must never be returned")
	}
}

func TestPR5B_ReadJobLogRejectsJobDirSymlink(t *testing.T) {
	w, _, tempDir, _ := newHTTPTestSetup(t, "")
	secret := "PR5B-OUTSIDE-SECRET-JOBDIR"
	outsideDir := filepath.Join(tempDir, "outside-jobdir")
	if err := os.MkdirAll(outsideDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "ffmpeg.log"), []byte(secret), 0644); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(tempDir, "jobs")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(stateDir, "job-symlink-dir")
	pr5bSymlinkOrSkip(t, outsideDir, link)

	log, err := w.ReadJobLog("job-symlink-dir", 0)
	if err == nil {
		t.Fatal("symlinked job directory must be rejected, never followed")
	}
	if IsJobLogNotFound(err) {
		t.Fatalf("job-dir symlink rejection must be fail-closed, not not-found: %v", err)
	}
	if strings.Contains(log.Content, secret) {
		t.Fatal("secret from an outside job directory must never be returned")
	}
}

func TestHTTP_JobLogsSymlinkNeverLeaks(t *testing.T) {
	_, srv, tempDir, _ := newHTTPTestSetup(t, "")
	secret := "PR5B-HTTP-SECRET-SYMLINK"
	outside := filepath.Join(tempDir, "http-secret.log")
	if err := os.WriteFile(outside, []byte(secret), 0644); err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(tempDir, "jobs", "job-http-symlink")
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		t.Fatal(err)
	}
	pr5bSymlinkOrSkip(t, outside, filepath.Join(jobDir, "ffmpeg.log"))

	rec := doRequest(t, srv, http.MethodGet, "/v1/jobs/job-http-symlink/logs", "", "")
	if rec.Code == http.StatusOK {
		t.Fatalf("symlinked log must never return 200 (got %d)", rec.Code)
	}
	if rec.Code < 400 {
		t.Fatalf("symlinked log must fail closed (>=400), got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("HTTP response must never leak the symlink target content: %s", rec.Body.String())
	}
}
