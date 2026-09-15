package action

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func testReconcilePlan() *transcode.Plan {
	return &transcode.Plan{
		Container:  "matroska",
		VideoCodec: "hevc",
		Resilience: transcode.ResiliencePlan{
			MaxAttempts:         3,
			TransientRetries:    2,
			RetryBackoffSeconds: []int{0},
			RetryOn:             []string{"ssh_transient", "storage_io_transient"},
		},
	}
}

func testReconcileEC(jobID string) *ExecutionContext {
	return &ExecutionContext{
		InstanceID: "test-inst",
		Inputs:     map[string]any{"path": "/Volumes/media/ep.mkv"},
		State: map[string]any{
			"resolved_path":       "/Volumes/media/ep.mkv",
			"candidate_extension": ".mkv",
			"profile":             "hevc-vt",
			"plan":                testReconcilePlan(),
			"attempt":             1,
			"retry_count":         0,
			"fallback_count":      0,
		},
		Outputs: map[string]any{},
	}
}

func TestSubmitUncertainSetsReconcileNoBudgetBurn(t *testing.T) {
	var gotKey string
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			gotKey = req.IdempotencyKey
			return transcode.Job{}, &transcode.UncertainError{Op: "submit", JobID: req.ID, Err: errors.New("timeout")}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != StepWaitingExternal || res.WaitingCondition != "worker_reconciling" {
		t.Fatalf("got %s/%s want waiting_external/worker_reconciling", res.Status, res.WaitingCondition)
	}
	if !getBool(ec.State, "transcode_reconcile") {
		t.Fatal("expected transcode_reconcile flag set")
	}
	if getInt(ec.State, "attempt") != 1 || getInt(ec.State, "retry_count") != 0 {
		t.Fatalf("budget changed: attempt=%v retry=%v", ec.State["attempt"], ec.State["retry_count"])
	}
	if getString(ec.State, "job_id") == "" || getString(ec.State, "idempotency_key") == "" {
		t.Fatal("stable job_id/idempotency_key must persist before Submit")
	}
	if gotKey != getString(ec.State, "job_id") {
		t.Fatalf("Request.IdempotencyKey=%q want %q", gotKey, getString(ec.State, "job_id"))
	}
	if atomic.LoadInt32(&mock.submitCalls) != 1 {
		t.Fatalf("submitCalls=%d want 1", mock.submitCalls)
	}
}

func TestReconcileUncertainStatusNoSubmit(t *testing.T) {
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, &transcode.UncertainError{Op: "status", JobID: jobID, Err: errors.New("disconnect")}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["job_id"] = "job-test-inst"
	ec.State["transcode_reconcile"] = true
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != StepWaitingExternal || res.WaitingCondition != "worker_unreachable" {
		t.Fatalf("got %s/%s want waiting_external/worker_unreachable", res.Status, res.WaitingCondition)
	}
	if atomic.LoadInt32(&mock.submitCalls) != 0 {
		t.Fatal("must not Submit during uncertain reconciliation")
	}
	if getInt(ec.State, "attempt") != 1 || getInt(ec.State, "retry_count") != 0 {
		t.Fatal("budget must stay unchanged")
	}
	if !getBool(ec.State, "transcode_reconcile") {
		t.Fatal("reconcile flag must remain until definitive outcome")
	}
}

func TestReconcileKnownJobNoResubmit(t *testing.T) {
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning, Attempt: 2, RetryCount: 1, FallbackCount: 1, AppliedFallbacks: []string{"fb"}, FailureClassification: "ssh_transient"}, nil
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["job_id"] = "job-test-inst"
	ec.State["transcode_reconcile"] = true
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != StepCompleted {
		t.Fatalf("got %s want completed", res.Status)
	}
	if atomic.LoadInt32(&mock.submitCalls) != 0 {
		t.Fatal("known job must not resubmit")
	}
	if !getBool(ec.State, "transcode_submitted") {
		t.Fatal("known job must mark transcode_submitted")
	}
	if getBool(ec.State, "transcode_reconcile") {
		t.Fatal("reconcile flags must clear")
	}
	if getInt(ec.State, "attempt") != 2 || getInt(ec.State, "retry_count") != 1 || getInt(ec.State, "fallback_count") != 1 {
		t.Fatalf("metadata not mirrored: %+v", ec.State)
	}
}

func TestReconcile404ControlledSameRequestResubmit(t *testing.T) {
	var submitReqs []transcode.Request
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, &transcode.HTTPError{Method: "GET", URL: "/v1/jobs/" + jobID, StatusCode: 404, Message: "not found"}
		},
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			submitReqs = append(submitReqs, req)
			return transcode.Job{ID: req.ID}, nil
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["job_id"] = "job-test-inst"
	ec.State["transcode_reconcile"] = true
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != StepCompleted {
		t.Fatalf("got %s/%s want completed", res.Status, res.Error)
	}
	if atomic.LoadInt32(&mock.statusCalls) != 1 || atomic.LoadInt32(&mock.submitCalls) != 1 {
		t.Fatalf("want exactly 1 status + 1 controlled submit, got status=%d submit=%d", mock.statusCalls, mock.submitCalls)
	}
	if len(submitReqs) != 1 || submitReqs[0].IdempotencyKey != "job-test-inst" {
		t.Fatalf("controlled resubmit must reuse same idempotency key, got %+v", submitReqs)
	}
	if !getBool(ec.State, "transcode_submitted") || getBool(ec.State, "transcode_reconcile") {
		t.Fatal("must mark submitted and clear reconcile flags")
	}
}

func TestReconcileDefinitiveOtherErrorFailsClosed(t *testing.T) {
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, &transcode.HTTPError{Method: "GET", URL: "/v1/jobs/" + jobID, StatusCode: 500, Message: "boom"}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["job_id"] = "job-test-inst"
	ec.State["transcode_reconcile"] = true
	res, _ := e.stepTranscodeSubmit(context.Background(), ec)
	if res.Status != StepFailed {
		t.Fatalf("got %s want failed", res.Status)
	}
	if atomic.LoadInt32(&mock.submitCalls) != 0 {
		t.Fatal("definitive reconcile error must not submit")
	}
}

func TestSubmitConflict409TerminalNoBudgetBurn(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			return transcode.Job{}, &transcode.HTTPError{Method: "POST", URL: "/v1/jobs", StatusCode: 409, Message: "idempotency_conflict: key already persists job with different execution_spec_digest"}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	res, _ := e.stepTranscodeSubmit(context.Background(), ec)
	if res.Status != StepFailed {
		t.Fatalf("got %s want failed", res.Status)
	}
	if !strings.Contains(res.Error, "idempotency_conflict") {
		t.Fatalf("error must carry idempotency_conflict, got %q", res.Error)
	}
	if getInt(ec.State, "retry_count") != 0 {
		t.Fatal("conflict must not burn budget")
	}
	if fc := getString(ec.State, "failure_classification"); fc != "idempotency_conflict" {
		t.Fatalf("classification=%q want idempotency_conflict", fc)
	}
}

func TestJobIDAloneDoesNotMeanSubmitted(t *testing.T) {
	mock := &mockTranscodeExecutor{}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["job_id"] = "job-test-inst"
	res, _ := e.stepTranscodeSubmit(context.Background(), ec)
	if res.Status != StepCompleted {
		t.Fatalf("got %s want completed via fresh submit", res.Status)
	}
	if atomic.LoadInt32(&mock.submitCalls) != 1 {
		t.Fatal("job_id alone must not short-circuit; must Submit")
	}
}

func TestWaitStatusUncertaintyNoTransientRetry(t *testing.T) {
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, &transcode.UncertainError{Op: "status", JobID: jobID, Err: errors.New("timeout")}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["job_id"] = "job-test-inst"
	ec.State["attempt"] = 2
	ec.State["retry_count"] = 1
	res, _ := e.stepTranscodeWait(context.Background(), ec)
	if res.Status != StepWaitingExternal || res.WaitingCondition != "worker_unreachable" {
		t.Fatalf("got %s/%s want waiting_external/worker_unreachable", res.Status, res.WaitingCondition)
	}
	if getInt(ec.State, "retry_count") != 1 {
		t.Fatal("uncertain wait must not burn retry budget")
	}
}

func TestWait404TerminalNoResubmit(t *testing.T) {
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, &transcode.HTTPError{Method: "GET", URL: "/v1/jobs/" + jobID, StatusCode: 404, Message: "not found"}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["job_id"] = "job-test-inst"
	res, _ := e.stepTranscodeWait(context.Background(), ec)
	if res.Status != StepFailed {
		t.Fatalf("got %s want failed", res.Status)
	}
	if !strings.Contains(strings.ToLower(res.Error), "disappeared") {
		t.Fatalf("404 must be terminal disappeared, got %q", res.Error)
	}
	if atomic.LoadInt32(&mock.submitCalls) != 0 {
		t.Fatal("wait 404 must never resubmit")
	}
}

func TestWaitTerminalFailureMirrorsMetadataNoRetry(t *testing.T) {
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusFailed, Error: "killed", Attempt: 3, RetryCount: 2, FallbackCount: 1, AppliedFallbacks: []string{"fb1"}, FailureClassification: "runner_killed"}, nil
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["job_id"] = "job-test-inst"
	res, _ := e.stepTranscodeWait(context.Background(), ec)
	if res.Status != StepFailed {
		t.Fatalf("got %s want failed", res.Status)
	}
	if res.Outputs["attempt"] != 3 || res.Outputs["retry_count"] != 2 || res.Outputs["fallback_count"] != 1 {
		t.Fatalf("outputs must mirror worker metadata, got %+v", res.Outputs)
	}
	if res.Outputs["failure_classification"] != "runner_killed" {
		t.Fatalf("classification must mirror worker, got %+v", res.Outputs)
	}
	if getInt(ec.State, "attempt") != 3 || getInt(ec.State, "retry_count") != 2 {
		t.Fatalf("state must mirror worker metadata, got %+v", ec.State)
	}
	if !strings.Contains(res.Error, "runner_killed") {
		t.Fatalf("error must carry mirrored classification, got %q", res.Error)
	}
}

func TestReconcileTakesPrecedenceOverSubmittedReuse(t *testing.T) {
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning, Attempt: 1}, nil
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["job_id"] = "job-test-inst"
	ec.State["transcode_submitted"] = true
	ec.State["transcode_reconcile"] = true
	res, _ := e.stepTranscodeSubmit(context.Background(), ec)
	if res.Status != StepCompleted {
		t.Fatalf("got %s want completed via reconciliation", res.Status)
	}
	if atomic.LoadInt32(&mock.statusCalls) != 1 {
		t.Fatalf("reconciliation Status must run when both flags set, statusCalls=%d", mock.statusCalls)
	}
	if atomic.LoadInt32(&mock.submitCalls) != 0 {
		t.Fatal("reconciliation must not Submit")
	}
	if res.Outputs["reconciled"] != true {
		t.Fatalf("expected reconciled output, got %+v", res.Outputs)
	}
}

func TestSubmit409UnrelatedMessageNotConflict(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			return transcode.Job{}, &transcode.HTTPError{Method: "POST", URL: "/v1/jobs", StatusCode: 409, Message: "slot limit exceeded, try later"}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	res, _ := e.stepTranscodeSubmit(context.Background(), ec)
	if res.Status != StepFailed {
		t.Fatalf("got %s want failed", res.Status)
	}
	if strings.Contains(res.Error, "idempotency_conflict") {
		t.Fatalf("unrelated 409 must not classify as idempotency_conflict, got %q", res.Error)
	}
	if fc := getString(ec.State, "failure_classification"); fc == "idempotency_conflict" {
		t.Fatal("unrelated 409 must not set idempotency_conflict classification")
	}
}

func TestWaitFailedWorkerBusyIsTerminal(t *testing.T) {
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusFailed, Error: "worker busy: maximum parallel jobs reached", Attempt: 1, FailureClassification: "worker_busy"}, nil
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["job_id"] = "job-test-inst"
	ec.Inputs["surface_worker_busy"] = true
	res, _ := e.stepTranscodeWait(context.Background(), ec)
	if res.Status != StepFailed {
		t.Fatalf("got %s/%s want failed; worker StatusFailed is always terminal", res.Status, res.WaitingCondition)
	}
	if res.Outputs["failure_classification"] != "worker_busy" {
		t.Fatalf("must mirror worker classification, got %+v", res.Outputs)
	}
}

func TestWaitDefinitive500FailsClosedBudgetUnchanged(t *testing.T) {
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, &transcode.HTTPError{Method: "GET", URL: "/v1/jobs/" + jobID, StatusCode: 500, Message: "connection reset by peer"}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["job_id"] = "job-test-inst"
	ec.State["attempt"] = 2
	ec.State["retry_count"] = 1
	res, _ := e.stepTranscodeWait(context.Background(), ec)
	if res.Status != StepFailed {
		t.Fatalf("got %s want failed closed", res.Status)
	}
	if getInt(ec.State, "attempt") != 2 || getInt(ec.State, "retry_count") != 1 {
		t.Fatalf("definitive status error must not burn budget, got attempt=%v retry=%v", ec.State["attempt"], ec.State["retry_count"])
	}
}
