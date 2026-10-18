package action

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

func TestWorkerPollErrorClassificationPrefersEnvelopeCode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		retryable bool
		conflict  bool
		class     resilience.FailureClass
	}{
		{"busy envelope", &httpclient.HTTPError{StatusCode: 429, Message: "worker busy: maximum parallel jobs (1) reached", Code: transcode.WorkerErrorBusy, Retryable: true, RetryAfter: 30 * time.Second}, false, false, resilience.WorkerBusy},
		{"legacy busy 409", &httpclient.HTTPError{StatusCode: 409, Message: "worker busy: maximum parallel jobs (1) reached", Retryable: true}, false, false, resilience.WorkerBusy},
		{"bare 429 from a proxy", &httpclient.HTTPError{StatusCode: 429, Message: "Too Many Requests", Retryable: true}, true, false, resilience.Classify("Too Many Requests")},
		{"conflict envelope", &httpclient.HTTPError{StatusCode: 409, Message: "key already persists another job", Code: transcode.WorkerErrorIdempotencyConflict}, false, true, resilience.IdempotencyConflict},
		{"legacy conflict 409", &httpclient.HTTPError{StatusCode: 409, Message: "idempotency_conflict: key already persists another job"}, false, true, resilience.IdempotencyConflict},
		// The code wins over misleading text in either direction.
		{"code beats conflict text", &httpclient.HTTPError{StatusCode: 404, Message: "idempotency_conflict mentioned", Code: transcode.WorkerErrorNotFound}, false, false, resilience.Classify("idempotency_conflict mentioned")},
		{"internal envelope", &httpclient.HTTPError{StatusCode: 500, Message: "reading job state: is a directory", Code: transcode.WorkerErrorInternal, Retryable: true}, true, false, resilience.Classify("reading job state: is a directory")},
		{"not found envelope", &httpclient.HTTPError{StatusCode: 404, Message: "job not found", Code: transcode.WorkerErrorNotFound}, false, false, resilience.Classify("job not found")},
		{"wrapped busy", fmt.Errorf("submit: %w", &httpclient.HTTPError{StatusCode: 429, Message: "worker busy", Code: transcode.WorkerErrorBusy, Retryable: true}), false, false, resilience.WorkerBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableWorkerPollError(tc.err); got != tc.retryable {
				t.Errorf("isRetryableWorkerPollError=%v want %v", got, tc.retryable)
			}
			if got := isTranscodeIdempotencyConflict(tc.err); got != tc.conflict {
				t.Errorf("isTranscodeIdempotencyConflict=%v want %v", got, tc.conflict)
			}
			if got := classifyWorkerFailure(tc.err); got != tc.class {
				t.Errorf("classifyWorkerFailure=%q want %q", got, tc.class)
			}
		})
	}
	if !isRetryableWorkerPollError(&httpclient.UncertainError{Op: "status", Err: errors.New("timeout")}) {
		t.Fatal("uncertain transport errors still reconcile")
	}
}

// A busy worker refused the submit and created nothing: it must take the
// worker_busy transient path, never the reconcile path that would poll for a
// job that does not exist.
func TestSubmitWorkerBusyEnvelopeIsNotReconciled(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			return transcode.Job{}, &httpclient.HTTPError{StatusCode: 429, Message: "worker busy: maximum parallel jobs (1) reached", Code: transcode.WorkerErrorBusy, Retryable: true, RetryAfter: 30 * time.Second}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.WaitingCondition == "worker_reconciling" || getBool(ec.State, "transcode_reconcile") {
		t.Fatalf("busy must not reconcile: %+v state=%v", res, ec.State["transcode_reconcile"])
	}
	if got := getString(ec.State, "failure_classification"); got != string(resilience.WorkerBusy) {
		t.Fatalf("failure_classification=%q want %q", got, resilience.WorkerBusy)
	}
}

func TestSubmitIdempotencyConflictEnvelopeFailsDefinitively(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			return transcode.Job{}, &httpclient.HTTPError{StatusCode: 409, Message: "key already persists another job", Code: transcode.WorkerErrorIdempotencyConflict}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != StepFailed || getString(ec.State, "failure_classification") != string(resilience.IdempotencyConflict) {
		t.Fatalf("conflict must fail definitively: %+v class=%v", res, ec.State["failure_classification"])
	}
}
