package action

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

func workerBusyError(retryAfter time.Duration) error {
	return &httpclient.HTTPError{Method: "POST", URL: "/v1/jobs", StatusCode: 429, Message: "worker busy: maximum parallel jobs (2) reached", Code: transcode.WorkerErrorBusy, Retryable: true, RetryAfter: retryAfter}
}

type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// A standalone encode (no surface_worker_busy, no parent) whose retry budget
// is already spent waits for a slot instead of failing, honours Retry-After
// plus a second, and resubmits the same identity once the slot frees up.
func TestStandaloneTranscodeSubmitWaitsForWorkerSlot(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	busy := true
	var keys []string
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			keys = append(keys, req.ID+"|"+req.IdempotencyKey)
			if busy {
				return transcode.Job{}, workerBusyError(30 * time.Second)
			}
			return transcode.Job{ID: req.ID}, nil
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock, Now: clock.Now})
	ec := testReconcileEC("job-test-inst")
	ec.ActionName = "transcode_media"
	plan := testReconcilePlan()
	plan.Resilience.MaxAttempts = 1
	plan.Resilience.RetryOn = []string{"worker_busy"}
	ec.State["plan"] = plan

	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StepWaitingExternal || res.WaitingCondition != "worker_busy" {
		t.Fatalf("got %s/%s %q, want waiting_external/worker_busy", res.Status, res.WaitingCondition, res.Error)
	}
	if getInt(ec.State, "attempt") != 1 || getInt(ec.State, "retry_count") != 0 {
		t.Fatalf("a slot wait must not consume attempts: attempt=%v retry=%v", ec.State["attempt"], ec.State["retry_count"])
	}
	if getBool(ec.State, "transcode_reconcile") || getBool(ec.State, "transcode_submitted") {
		t.Fatal("a definitive busy refusal is neither uncertain nor accepted")
	}
	wantNotBefore := clock.Now().Add(31 * time.Second).Format(time.RFC3339Nano)
	if got := getString(ec.State, workerBusyNotBeforeKey); got != wantNotBefore {
		t.Fatalf("not before = %q, want Retry-After + 1s = %q", got, wantNotBefore)
	}
	if res.Outputs["failure_classification"] != string(resilience.WorkerBusy) || res.Outputs["worker_busy"] != true {
		t.Fatalf("outputs = %v", res.Outputs)
	}

	// Inside the Retry-After window nothing is sent, whoever resumes.
	clock.Advance(20 * time.Second)
	busy = false
	res, _ = e.stepTranscodeSubmit(context.Background(), ec)
	if res.WaitingCondition != "worker_busy" || len(keys) != 1 {
		t.Fatalf("submitted inside Retry-After: %s/%s submits=%d", res.Status, res.WaitingCondition, len(keys))
	}

	clock.Advance(11 * time.Second)
	res, _ = e.stepTranscodeSubmit(context.Background(), ec)
	if res.Status != StepCompleted {
		t.Fatalf("got %s/%s %q, want completed once a slot is free", res.Status, res.WaitingCondition, res.Error)
	}
	if len(keys) != 2 || keys[0] != keys[1] || keys[0] != "job-test-inst|job-test-inst" {
		t.Fatalf("resubmit must keep the job identity and idempotency key: %v", keys)
	}
	for _, key := range []string{workerBusyNotBeforeKey, "worker_busy"} {
		if _, ok := ec.State[key]; ok {
			t.Fatalf("%s left in state after acceptance", key)
		}
	}
}

// Without Retry-After the wait still holds and no earlier not-before is
// kept; the reconciler's own interval paces the next try.
func TestStandaloneTranscodeWorkerBusyWithoutRetryAfter(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			return transcode.Job{}, &httpclient.HTTPError{StatusCode: 409, Message: "worker busy: maximum parallel jobs (2) reached"}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["attempt"], ec.State["retry_count"] = 3, 2 // budget spent by earlier attempts
	res, _ := e.stepTranscodeSubmit(context.Background(), ec)
	if res.Status != StepWaitingExternal || res.WaitingCondition != "worker_busy" {
		t.Fatalf("got %s/%s %q", res.Status, res.WaitingCondition, res.Error)
	}
	if getInt(ec.State, "attempt") != 3 || getInt(ec.State, "retry_count") != 2 {
		t.Fatal("busy must not touch the retry budget")
	}

	ec.State[workerBusyNotBeforeKey] = "2999-01-01T00:00:00Z"
	e.workerBusyWait(ec, errors.New("worker busy"), "submit", transcodeSlotWaitReason, map[string]any{})
	if _, ok := ec.State[workerBusyNotBeforeKey]; ok {
		t.Fatal("a refusal without Retry-After must not keep an earlier not-before")
	}
}

// Other transient failures still spend and respect the budget.
func TestStandaloneTranscodeOtherTransientStillUsesBudget(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			return transcode.Job{}, errors.New("ssh: broken pipe")
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	res, _ := e.stepTranscodeSubmit(context.Background(), ec)
	if res.WaitingCondition != "transcode_retry" || getInt(ec.State, "retry_count") != 1 {
		t.Fatalf("got %s/%s retry=%v, want a counted transcode_retry", res.Status, res.WaitingCondition, ec.State["retry_count"])
	}
	ec.State["attempt"], ec.State["retry_count"] = 3, 2
	delete(ec.State, "retry_not_before")
	res, _ = e.stepTranscodeSubmit(context.Background(), ec)
	if res.Status != StepFailed {
		t.Fatalf("got %s/%s, want failed once the budget is spent", res.Status, res.WaitingCondition)
	}
}

// A long slot wait keeps one history entry per run of refusals, not one per
// poll, so the stored state stays small.
func TestWorkerBusyHistoryStaysBounded(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			return transcode.Job{}, workerBusyError(0)
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	appendFailureHistory(ec, "benchmark_submit", "ssh_transient", "earlier failure")
	for i := 0; i < 50; i++ {
		if res, _ := e.stepTranscodeSubmit(context.Background(), ec); res.WaitingCondition != "worker_busy" {
			t.Fatalf("poll %d: %s/%s", i, res.Status, res.WaitingCondition)
		}
		// Persisted state round-trips through JSON between polls.
		raw, _ := json.Marshal(ec.State["failure_history"])
		var history []any
		_ = json.Unmarshal(raw, &history)
		ec.State["failure_history"] = history
	}
	history, _ := ec.State["failure_history"].([]any)
	if len(history) != 2 {
		t.Fatalf("history has %d entries, want the earlier failure and one busy run", len(history))
	}
	busy, _ := history[1].(map[string]any)
	if getString(busy, "classification") != "worker_busy" || getInt(busy, "count") != 50 || getString(busy, "last_at") == "" {
		t.Fatalf("busy entry = %v", busy)
	}
}

// When a resubmit after a slot wait was uncertain and reconciliation finds
// the job, acceptance clears the busy markers like a direct acceptance.
func TestReconciledAcceptanceAfterSlotWaitClearsBusyMarkers(t *testing.T) {
	calls := 0
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			calls++
			if calls == 1 {
				return transcode.Job{}, workerBusyError(0)
			}
			return transcode.Job{}, &httpclient.UncertainError{Op: "submit", Err: context.DeadlineExceeded}
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning}, nil
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	_, _ = e.stepTranscodeSubmit(context.Background(), ec)
	mergeMap(ec.State, map[string]any{"worker_busy": true})
	_, _ = e.stepTranscodeSubmit(context.Background(), ec)
	res, _ := e.stepTranscodeSubmit(context.Background(), ec)
	if res.Status != StepCompleted || calls != 2 {
		t.Fatalf("got %s/%s after %d submits", res.Status, res.WaitingCondition, calls)
	}
	for _, key := range []string{"worker_busy", workerBusyNotBeforeKey, "failure_classification"} {
		if _, ok := ec.State[key]; ok {
			t.Fatalf("%s left in state after reconciled acceptance", key)
		}
	}
}

// A transport failure after a slot wait is still uncertain: reconcile the
// same identity, never send the write again.
func TestTranscodeSlotWaitThenTransportFailureReconciles(t *testing.T) {
	calls := 0
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			calls++
			if calls == 1 {
				return transcode.Job{}, workerBusyError(0)
			}
			return transcode.Job{}, &httpclient.UncertainError{Op: "submit", Err: context.DeadlineExceeded}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	if res, _ := e.stepTranscodeSubmit(context.Background(), ec); res.WaitingCondition != "worker_busy" {
		t.Fatalf("first submit: %s/%s", res.Status, res.WaitingCondition)
	}
	res, _ := e.stepTranscodeSubmit(context.Background(), ec)
	if res.WaitingCondition != "worker_reconciling" || !getBool(ec.State, "transcode_reconcile") {
		t.Fatalf("second submit: %s/%s reconcile=%v", res.Status, res.WaitingCondition, ec.State["transcode_reconcile"])
	}
	if res, _ = e.stepTranscodeSubmit(context.Background(), ec); calls != 2 {
		t.Fatalf("uncertain submit was sent again (%d calls): %s/%s", calls, res.Status, res.WaitingCondition)
	}
}
