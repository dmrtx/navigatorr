package httpclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestHTTPError_EnvelopeCodeFirstWithLegacyFallback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		header     string
		body       string
		busy       bool
		conflict   bool
		retryable  bool
		code       string
		retryAfter time.Duration
	}{
		{"busy envelope", 429, "30", `{"id":"job-1","error":"worker busy: maximum parallel jobs (1) reached","code":"worker_busy","retryable":true}`, true, false, true, transcode.WorkerErrorBusy, 30 * time.Second},
		{"conflict envelope", 409, "", `{"id":"job-1","error":"idempotency_conflict: key already persists job","code":"idempotency_conflict","retryable":false}`, false, true, false, transcode.WorkerErrorIdempotencyConflict, 0},
		{"legacy busy 409", 409, "", `{"id":"job-1","error":"worker busy: maximum parallel jobs (1) reached"}`, true, false, true, "", 0},
		{"legacy conflict 409", 409, "", `{"id":"job-1","error":"idempotency_conflict: key already persists job"}`, false, true, false, "", 0},
		{"legacy unrelated 409", 409, "", `{"error":"slot limit exceeded, try later"}`, false, false, false, "", 0},
		{"proxy 503 page", 503, "", `<html>bad gateway</html>`, false, false, true, "", 0},
		// The code wins over the message text.
		{"code beats message", 409, "", `{"error":"worker busy mentioned in passing","code":"idempotency_conflict","retryable":false}`, false, true, false, transcode.WorkerErrorIdempotencyConflict, 0},
		{"not found envelope", 404, "", `{"error":"job not found","code":"not_found","retryable":false}`, false, false, false, transcode.WorkerErrorNotFound, 0},
		{"internal envelope", 500, "", `{"error":"reading job state: is a directory","code":"internal","retryable":true}`, false, false, true, transcode.WorkerErrorInternal, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := newTestHTTPExecutor(t, srv).Submit(context.Background(), submitTestRequest())
			var he *HTTPError
			if !errors.As(err, &he) {
				t.Fatalf("want *HTTPError, got %v", err)
			}
			if IsTransportUncertain(err) {
				t.Fatalf("a complete response is never uncertain: %v", err)
			}
			if he.StatusCode != tc.status || he.Code != tc.code || he.Retryable != tc.retryable || he.RetryAfter != tc.retryAfter {
				t.Fatalf("got status=%d code=%q retryable=%v retry_after=%s", he.StatusCode, he.Code, he.Retryable, he.RetryAfter)
			}
			if IsWorkerBusy(err) != tc.busy || IsIdempotencyConflict(err) != tc.conflict {
				t.Fatalf("busy=%v conflict=%v, want %v %v", IsWorkerBusy(err), IsIdempotencyConflict(err), tc.busy, tc.conflict)
			}
			if he.RequestID == "" || !strings.Contains(err.Error(), "request_id="+he.RequestID) {
				t.Fatalf("error must carry the request ID: %v", err)
			}
		})
	}
}

func TestHTTPExecutor_CancelFinishedJobIsDefinitive(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"completed", `{"id":"job-1","status":"completed","code":"already_terminal","retryable":false}`},
		{"failed with reason", `{"id":"job-1","status":"failed","error":"encoder exited with status 1","code":"already_terminal","retryable":false}`},
		// Workers that predate the code send the same body without it.
		{"legacy completed", `{"id":"job-1","status":"completed"}`},
		{"legacy failed with reason", `{"id":"job-1","status":"failed","error":"encoder exited with status 1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := singleShotServer(t, &attempts, http.StatusOK, tc.body)
			defer srv.Close()
			if err := newTestHTTPExecutor(t, srv).Cancel(context.Background(), "job-1"); err != nil {
				t.Fatalf("a finished job is a definitive cancel answer, got %v", err)
			}
			if attempts.Load() != 1 {
				t.Fatalf("attempts=%d", attempts.Load())
			}
		})
	}
	var attempts atomic.Int32
	srv := singleShotServer(t, &attempts, http.StatusOK, `{"id":"job-other","status":"completed"}`)
	defer srv.Close()
	if err := newTestHTTPExecutor(t, srv).Cancel(context.Background(), "job-1"); !IsTransportUncertain(err) {
		t.Fatalf("a terminal ack for another job stays ambiguous, got %v", err)
	}
}

var requestIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

func TestHTTPExecutor_SendsCorrelationHeaders(t *testing.T) {
	type seen struct{ path, requestID, jobID string }
	var (
		mu  sync.Mutex
		got []seen
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, seen{r.URL.Path, r.Header.Get(transcode.RequestIDHeader), r.Header.Get(transcode.JobIDHeader)})
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/jobs":
			_, _ = w.Write([]byte(`{"id":"job-1","status":"queued"}`))
		case "/v1/jobs/job-1":
			_, _ = w.Write([]byte(`{"id":"job-1","status":"running"}`))
		case "/v1/jobs/job-1/cancel":
			_, _ = w.Write([]byte(`{"id":"job-1","status":"cancelled"}`))
		default:
			_, _ = w.Write([]byte(`{"ready":true}`))
		}
	}))
	defer srv.Close()
	e := newTestHTTPExecutor(t, srv)
	ctx := context.Background()
	if _, err := e.Submit(ctx, submitTestRequest()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Status(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}
	if err := e.Cancel(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}
	if err := e.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, s := range got {
		if !requestIDPattern.MatchString(s.requestID) {
			t.Errorf("%s: request ID %q", s.path, s.requestID)
		}
		ids[s.requestID] = true
		wantJob := "job-1"
		if s.path == "/v1/ready" {
			wantJob = ""
		}
		if s.jobID != wantJob {
			t.Errorf("%s: job ID header %q want %q", s.path, s.jobID, wantJob)
		}
	}
	if len(got) != 4 || len(ids) != 4 {
		t.Fatalf("each request needs its own ID: %+v", got)
	}
}

func TestHTTPExecutor_TransportErrorCarriesRequestID(t *testing.T) {
	addr, err := newClosedListener(t)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewHTTPExecutor(HTTPConfig{BaseURL: "http://" + addr, RequestTimeout: time.Second, SubmitTimeout: time.Second, PathMappings: testHTTPMappings()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Submit(context.Background(), submitTestRequest())
	if !IsTransportUncertain(err) || !strings.Contains(err.Error(), "request_id=") {
		t.Fatalf("uncertain submit must name its request ID for log correlation, got %v", err)
	}
}
