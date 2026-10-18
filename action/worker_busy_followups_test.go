package action

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// runSubmitStep runs the encode submit step and merges its outputs the way
// the engine does between polls.
func runSubmitStep(t *testing.T, e *Engine, ec *ExecutionContext) StepResult {
	t.Helper()
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	mergeMap(ec.State, res.Outputs)
	mergeMap(ec.Outputs, res.Outputs)
	return res
}

func assertNoBusyMarkers(t *testing.T, ec *ExecutionContext) {
	t.Helper()
	for _, key := range []string{"worker_busy", workerBusyNotBeforeKey, slotWaitLogKey} {
		if _, ok := ec.State[key]; ok {
			t.Errorf("state still has %s", key)
		}
		if _, ok := ec.Outputs[key]; ok {
			t.Errorf("outputs still have %s", key)
		}
	}
}

// Any outcome other than another busy refusal ends the slot wait, so a
// retrying or failed job never reports worker_busy.
func TestNonBusyOutcomeAfterSlotWaitClearsMarkers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		condition string
		class     string
	}{
		{"counted transient retry", errors.New("ssh: broken pipe"), "transcode_retry", "ssh_transient"},
		{"idempotency conflict", &httpclient.HTTPError{StatusCode: 409, Message: "key already persists another job", Code: transcode.WorkerErrorIdempotencyConflict}, "", "idempotency_conflict"},
		{"uncertain transport", &httpclient.UncertainError{Op: "submit", Err: context.DeadlineExceeded}, "worker_reconciling", "worker_unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			mock := &mockTranscodeExecutor{
				submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
					calls++
					if calls == 1 {
						return transcode.Job{}, workerBusyError(0)
					}
					return transcode.Job{}, tc.err
				},
			}
			e := NewEngine(EngineDeps{Transcode: mock})
			ec := testReconcileEC("job-test-inst")
			ec.Outputs = map[string]any{}
			if res := runSubmitStep(t, e, ec); res.WaitingCondition != "worker_busy" || !getBool(ec.State, "worker_busy") {
				t.Fatalf("first submit: %s/%s", res.Status, res.WaitingCondition)
			}
			res := runSubmitStep(t, e, ec)
			if res.WaitingCondition != tc.condition || (tc.condition == "" && res.Status != StepFailed) {
				t.Fatalf("second submit: %s/%s %q", res.Status, res.WaitingCondition, res.Error)
			}
			assertNoBusyMarkers(t, ec)
			if got := getString(ec.State, "failure_classification"); got != tc.class {
				t.Fatalf("failure_classification = %q, want %q", got, tc.class)
			}
		})
	}
}

func TestBenchmarkNonBusyOutcomeAfterSlotWaitClearsMarkers(t *testing.T) {
	e := NewEngine(EngineDeps{})
	ec := testReconcileEC("job-test-inst")
	ec.Outputs = map[string]any{}
	res, _ := e.handleBenchmarkTransientFailure(ec, testReconcilePlan(), workerBusyError(30*time.Second), "benchmark_submit")
	mergeMap(ec.State, res.Outputs)
	mergeMap(ec.Outputs, res.Outputs)
	if !getBool(ec.State, "worker_busy") || getString(ec.State, workerBusyNotBeforeKey) == "" {
		t.Fatalf("busy refusal did not record a slot wait: %v", ec.State)
	}
	res, _ = e.handleBenchmarkTransientFailure(ec, testReconcilePlan(), errors.New("ssh: broken pipe"), "benchmark_submit")
	mergeMap(ec.State, res.Outputs)
	if res.WaitingCondition != "benchmark_retry" {
		t.Fatalf("got %s/%s", res.Status, res.WaitingCondition)
	}
	assertNoBusyMarkers(t, ec)
}

// The encode-phase cancel skip: a refused encode has no remote job, while an
// uncertain resend after the slot wait may have one and is cancelled.
func TestEncodePhaseCancelSkipsOnlyRefusedSubmit(t *testing.T) {
	calls := 0
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			calls++
			if calls == 1 {
				return transcode.Job{}, workerBusyError(0)
			}
			return transcode.Job{}, &httpclient.UncertainError{Op: "submit", Err: context.DeadlineExceeded}
		},
		cancelFunc: func(ctx context.Context, jobID string) error {
			return &httpclient.HTTPError{StatusCode: 404, Message: "job not found", Code: transcode.WorkerErrorNotFound}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.Outputs = map[string]any{}
	runSubmitStep(t, e, ec)
	if getString(ec.State, "job_id") == "" || !noRemoteJob(ec, "transcode_submitted", "transcode_reconcile", transcodeRejectedKey) {
		t.Fatalf("refused encode should be awaiting a slot: %v", ec.State)
	}
	if err := e.cancelRemoteActionWork(context.Background(), ec); err != nil {
		t.Fatalf("cancelling a refused encode reported %v", err)
	}
	if mock.cancelCalls != 0 {
		t.Fatalf("cancel sent for an encode the worker never created (%d calls)", mock.cancelCalls)
	}

	runSubmitStep(t, e, ec)
	if !getBool(ec.State, "transcode_reconcile") || noRemoteJob(ec, "transcode_submitted", "transcode_reconcile", transcodeRejectedKey) {
		t.Fatalf("uncertain resend should be reconciling: %v", ec.State)
	}
	_ = e.cancelRemoteActionWork(context.Background(), ec)
	if mock.cancelCalls != 1 {
		t.Fatalf("cancel calls = %d, want 1 for a possibly created encode", mock.cancelCalls)
	}
}

// A counted retry after a slot wait clears worker_busy, but the worker still
// holds no job: cancelling must not send a cancel it would answer with 404.
func TestCancelAfterBusyThenCountedRetrySkipsRemote(t *testing.T) {
	calls := 0
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			calls++
			if calls == 1 {
				return transcode.Job{}, workerBusyError(0)
			}
			return transcode.Job{}, errors.New("ssh: broken pipe")
		},
		cancelFunc: func(ctx context.Context, jobID string) error {
			return &httpclient.HTTPError{StatusCode: 404, Message: "job not found", Code: transcode.WorkerErrorNotFound}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.Outputs = map[string]any{}
	runSubmitStep(t, e, ec)
	if res := runSubmitStep(t, e, ec); res.WaitingCondition != "transcode_retry" || getBool(ec.State, "worker_busy") {
		t.Fatalf("second submit: %s/%s busy=%v", res.Status, res.WaitingCondition, ec.State["worker_busy"])
	}
	if err := e.cancelRemoteActionWork(context.Background(), ec); err != nil {
		t.Fatalf("cancel reported %v", err)
	}
	if mock.cancelCalls != 0 {
		t.Fatalf("cancel sent for an encode the worker never created (%d calls)", mock.cancelCalls)
	}
}

func TestBenchmarkCancelAfterBusyThenCountedRetrySkipsRemote(t *testing.T) {
	calls := 0
	mock := &mockTranscodeExecutor{
		benchmarkSubmitFunc: func(ctx context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
			calls++
			if calls == 1 {
				return transcode.BenchmarkJob{}, workerBusyError(0)
			}
			return transcode.BenchmarkJob{}, errors.New("ssh: broken pipe")
		},
		benchmarkCancelFunc: func(ctx context.Context, jobID string) error {
			return &httpclient.HTTPError{StatusCode: 404, Message: "benchmark not found", Code: transcode.WorkerErrorNotFound}
		},
		cancelFunc: func(ctx context.Context, jobID string) error {
			return &httpclient.HTTPError{StatusCode: 404, Message: "job not found", Code: transcode.WorkerErrorNotFound}
		},
	}
	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
	res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{"path": mediaFile, "profile": "opt-vt"})
	if err != nil || res.WaitingCondition != "worker_busy" {
		t.Fatalf("got %v %+v", err, res)
	}
	// Give the job a retry budget so the next refusal is a counted retry.
	inst, err := engine.deps.Store.GetActionInstance(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	ec := parseExecutionContext(inst, engine)
	plan := getPlan(ec.State["plan"])
	plan.Resilience.TransientRetries, plan.Resilience.MaxAttempts, plan.Resilience.RetryOn = 2, 3, []string{"ssh_transient"}
	ec.State["plan"] = plan
	inst.StateJSON = toJSON(ec.State)
	if err := engine.deps.Store.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Resume(context.Background(), res.ID, "", nil)
	if err != nil || got.WaitingCondition != "benchmark_retry" || getBool(got.State, "worker_busy") {
		t.Fatalf("counted retry: %v %+v", err, got)
	}
	if _, err := engine.Cancel(context.Background(), res.ID, "user stopped"); err != nil {
		t.Fatalf("cancel reported %v", err)
	}
	if got, _ = engine.Status(context.Background(), res.ID); got.Status != StatusCancelled {
		t.Fatalf("status = %s", got.Status)
	}
	if mock.benchmarkCancelCalls != 0 || mock.cancelCalls != 0 {
		t.Fatalf("cancelled a job the worker never created: benchmark=%d transcode=%d", mock.benchmarkCancelCalls, mock.cancelCalls)
	}
}

// The check before admission guards the write even when reconciliation of an
// earlier identity found nothing and falls through to a new submit.
func TestTranscodeBusyWindowCheckedBeforeAdmission(t *testing.T) {
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, &httpclient.HTTPError{StatusCode: 404, Message: "job not found", Code: transcode.WorkerErrorNotFound}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["job_id"] = "job-test-inst"
	ec.State["transcode_reconcile"] = true
	ec.State[workerBusyNotBeforeKey] = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	if res.WaitingCondition != "worker_busy" || mock.submitCalls != 0 {
		t.Fatalf("got %s/%s with %d submits, want a worker_busy wait and no write", res.Status, res.WaitingCondition, mock.submitCalls)
	}
	if mock.statusCalls != 1 {
		t.Fatalf("reconciliation is a read and must not wait: %d status calls", mock.statusCalls)
	}
}

// A long slot wait writes one step-log row, not one per poll; the next
// distinct wait is logged again.
func TestSlotWaitWritesOneStepLogRow(t *testing.T) {
	clock := &testClock{at: time.Now().UTC()}
	busy := true
	mock := &mockTranscodeExecutor{
		benchmarkSubmitFunc: func(ctx context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
			if busy {
				return transcode.BenchmarkJob{}, workerBusyError(10 * time.Second)
			}
			return transcode.BenchmarkJob{ID: req.ID}, nil
		},
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{ID: jobID, Status: transcode.StatusRunning}, nil
		},
	}
	engine, st, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
	engine.deps.Now = clock.Now
	res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{"path": mediaFile, "profile": "opt-vt"})
	if err != nil || res.WaitingCondition != "worker_busy" {
		t.Fatalf("got %v %+v", err, res)
	}
	waitingRows := func(step string) int {
		rows, err := st.GetActionSteps(res.ID)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range rows {
			if r.StepName == step && r.Status == string(StepWaitingExternal) {
				n++
			}
		}
		return n
	}
	for i := 0; i < 20; i++ {
		// Inside the window (the gate) and after it (a new refusal).
		if _, err := engine.Resume(context.Background(), res.ID, "", nil); err != nil {
			t.Fatal(err)
		}
		clock.Advance(6 * time.Second)
	}
	if n := waitingRows("submit_benchmark"); n != 1 {
		t.Fatalf("slot wait wrote %d waiting rows, want 1", n)
	}
	got, _ := engine.Status(context.Background(), res.ID)
	if got.WaitingCondition != "worker_busy" || got.Outputs["next_poll_at"] == nil {
		t.Fatalf("collapsed rows must not change the wait itself: %+v", got)
	}

	busy = false
	clock.Advance(11 * time.Second)
	if got, err = engine.Resume(context.Background(), res.ID, "", nil); err != nil || got.WaitingCondition != "benchmark_complete" {
		t.Fatalf("after a free slot: %v %+v", err, got)
	}
	if n := waitingRows("wait_benchmark"); n != 1 {
		t.Fatalf("next wait wrote %d rows, want 1", n)
	}
	if _, ok := got.State[slotWaitLogKey]; ok {
		t.Fatal("slot wait log marker left after acceptance")
	}
}
