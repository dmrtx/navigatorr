package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// flakyTransport refuses the first failures requests as a restarting daemon
// does, then answers with body (or status when it is not 200).
type flakyTransport struct {
	failures int32
	calls    atomic.Int32
	status   int
	body     []byte
}

func (f *flakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	n := f.calls.Add(1)
	if n <= f.failures {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(f.body)), Request: r}, nil
}

func capabilitiesExecutor(t *testing.T, tr *flakyTransport, delays []time.Duration) *HTTPExecutor {
	t.Helper()
	exec, err := NewHTTPExecutor(HTTPConfig{BaseURL: "http://worker.invalid:8097", RequestTimeout: 5 * time.Second, SubmitTimeout: 5 * time.Second, PathMappings: testHTTPMappings(), ReadRetryDelays: delays},
		WithHTTPClient(&http.Client{Transport: tr}))
	if err != nil {
		t.Fatal(err)
	}
	return exec
}

func TestCapabilitiesRetriesTransportFailuresWithinBudget(t *testing.T) {
	body, _ := json.Marshal(validTestCaps(t))
	tr := &flakyTransport{failures: 2, body: body}
	exec := capabilitiesExecutor(t, tr, []time.Duration{time.Millisecond, time.Millisecond})
	if _, err := exec.Capabilities(context.Background()); err != nil {
		t.Fatalf("a daemon back after two refusals must answer: %v", err)
	}
	if got := tr.calls.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestCapabilitiesStaysUncertainWhenBudgetIsSpent(t *testing.T) {
	tr := &flakyTransport{failures: 100}
	exec := capabilitiesExecutor(t, tr, []time.Duration{time.Millisecond, time.Millisecond})
	_, err := exec.Capabilities(context.Background())
	if !IsTransportUncertain(err) {
		t.Fatalf("an unreachable worker is uncertain, not a definitive answer: %v", err)
	}
	if got := tr.calls.Load(); got != 3 {
		t.Fatalf("requests = %d, want the first try plus two retries", got)
	}
}

func TestCapabilitiesRetriesCanBeDisabledAndNeverRetryDefinitiveAnswers(t *testing.T) {
	tr := &flakyTransport{failures: 100}
	exec := capabilitiesExecutor(t, tr, []time.Duration{})
	if _, err := exec.Capabilities(context.Background()); !IsTransportUncertain(err) || tr.calls.Load() != 1 {
		t.Fatalf("empty delays must mean one attempt: err=%v calls=%d", err, tr.calls.Load())
	}
	definitive := &flakyTransport{status: http.StatusInternalServerError, body: []byte(`{"error":"boom"}`)}
	exec = capabilitiesExecutor(t, definitive, []time.Duration{time.Millisecond, time.Millisecond})
	_, err := exec.Capabilities(context.Background())
	if err == nil || IsTransportUncertain(err) || definitive.calls.Load() != 1 {
		t.Fatalf("an HTTP error is definitive and not retried: err=%v calls=%d", err, definitive.calls.Load())
	}
}

func TestCapabilitiesRetryStopsWhenCallerCancels(t *testing.T) {
	tr := &flakyTransport{failures: 100}
	exec := capabilitiesExecutor(t, tr, []time.Duration{time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	if _, err := exec.Capabilities(ctx); err == nil {
		t.Fatal("cancelled read succeeded")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the retry wait ignored the caller's cancellation")
	}
}
