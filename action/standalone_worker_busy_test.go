package action

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
	"github.com/jakenesler/navigatorr/transcode/recipe"
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

// Without Retry-After the wait still holds; the reconciler's own interval
// paces the next try.
func TestStandaloneTranscodeWorkerBusyWithoutRetryAfter(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			return transcode.Job{}, &httpclient.HTTPError{StatusCode: 409, Message: "worker busy: maximum parallel jobs (2) reached"}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["attempt"], ec.State["retry_count"] = 3, 2 // budget spent by earlier attempts
	ec.State[workerBusyNotBeforeKey] = "2000-01-01T00:00:00Z"
	res, _ := e.stepTranscodeSubmit(context.Background(), ec)
	if res.Status != StepWaitingExternal || res.WaitingCondition != "worker_busy" {
		t.Fatalf("got %s/%s %q", res.Status, res.WaitingCondition, res.Error)
	}
	if _, ok := ec.State[workerBusyNotBeforeKey]; ok {
		t.Fatal("no Retry-After must not leave a stale not-before")
	}
	if getInt(ec.State, "attempt") != 3 || getInt(ec.State, "retry_count") != 2 {
		t.Fatal("busy must not touch the retry budget")
	}
}

// Other transient failures still spend and respect the budget.
func TestStandaloneTranscodeOtherTransientStillFailsWhenBudgetSpent(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			return transcode.Job{}, &httpclient.HTTPError{StatusCode: 400, Message: "ssh: handshake failed"}
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.State["attempt"], ec.State["retry_count"] = 3, 2
	res, _ := e.stepTranscodeSubmit(context.Background(), ec)
	if res.Status != StepFailed {
		t.Fatalf("got %s/%s, want failed", res.Status, res.WaitingCondition)
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

// End to end: a standalone benchmark started without surface_worker_busy
// waits for a slot, the reconciler leaves it alone until Retry-After has
// passed, then submits the same benchmark identity without spending attempts.
func TestStandaloneBenchmarkWaitsForSlotAndReconcilerResubmits(t *testing.T) {
	clock := &testClock{at: time.Now().UTC()}
	busy := true
	var ids []string
	mock := &mockTranscodeExecutor{
		benchmarkSubmitFunc: func(ctx context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
			ids = append(ids, req.ID)
			if busy {
				return transcode.BenchmarkJob{}, workerBusyError(10 * time.Second)
			}
			return transcode.BenchmarkJob{ID: req.ID}, nil
		},
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{ID: jobID, Status: transcode.StatusRunning}, nil
		},
	}
	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
	engine.deps.Now = clock.Now
	res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{"path": mediaFile, "profile": "opt-vt"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusWaitingExternal || res.WaitingCondition != "worker_busy" {
		t.Fatalf("got %s/%s %q, want waiting_external/worker_busy", res.Status, res.WaitingCondition, res.Error)
	}
	if res.WaitingReason != benchmarkSlotWaitReason {
		t.Fatalf("waiting reason = %q", res.WaitingReason)
	}
	attempt, retries := getInt(res.State, "benchmark_attempt"), getInt(res.State, "benchmark_retry_count")

	if err := engine.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("reconciler resubmitted inside Retry-After: %v", ids)
	}

	busy = false
	clock.Advance(11 * time.Second)
	if err := engine.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("want one resubmit of the same benchmark identity, got %v", ids)
	}
	got, err := engine.Status(context.Background(), res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusWaitingExternal || got.WaitingCondition != "benchmark_complete" {
		t.Fatalf("after a free slot: %s/%s %q", got.Status, got.WaitingCondition, got.Error)
	}
	if getInt(got.State, "benchmark_attempt") != attempt || getInt(got.State, "benchmark_retry_count") != retries {
		t.Fatalf("slot wait consumed attempts: %v/%v -> %v/%v", attempt, retries, got.State["benchmark_attempt"], got.State["benchmark_retry_count"])
	}
	if got.Outputs["worker_busy"] != nil {
		t.Fatalf("worker_busy marker left after acceptance: %v", got.Outputs["worker_busy"])
	}
}

// A slot wait stays cancellable, so it needs no admission deadline.
func TestStandaloneSlotWaitIsCancellable(t *testing.T) {
	mock := &mockTranscodeExecutor{
		benchmarkSubmitFunc: func(ctx context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
			return transcode.BenchmarkJob{}, workerBusyError(0)
		},
	}
	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
	res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{"path": mediaFile, "profile": "opt-vt"})
	if err != nil || res.WaitingCondition != "worker_busy" {
		t.Fatalf("got %v %+v", err, res)
	}
	if _, err := engine.Cancel(context.Background(), res.ID, "user stopped waiting"); err != nil {
		t.Fatal(err)
	}
	got, _ := engine.Status(context.Background(), res.ID)
	if got.Status != StatusCancelled {
		t.Fatalf("status = %s", got.Status)
	}
	before := mock.benchmarkSubmitCalls
	if err := engine.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.benchmarkSubmitCalls != before {
		t.Fatal("a cancelled slot wait must not submit again")
	}
}
