package action

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

func registerReconcileTestFlow(e *Engine, final func(context.Context, *ExecutionContext) (StepResult, error)) {
	e.RegisterTemplate(ActionTemplate{Name: "transcode_media", AutoReconcile: true, Steps: []StepDefinition{
		{Name: "preflight", Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
			return StepResult{Status: StepCompleted, Outputs: testReconcileEC("").State}, nil
		}},
		{Name: "submit_transcode", Run: e.stepTranscodeSubmit},
		{Name: "wait_transcode", Run: e.stepTranscodeWait},
		{Name: "accept_result", Run: final},
	}})
}

func TestReconcilerRecoversAcceptedSubmitAfterRestartWithoutClient(t *testing.T) {
	st := setupTestStore(t)
	now := time.Now().UTC()
	var finalized atomic.Int32
	encodeMs := int64(120000)
	mock := &mockTranscodeExecutor{}
	mock.submitFunc = func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
		instID := strings.TrimPrefix(req.ID, "job-")
		inst, err := st.GetActionInstance(instID)
		if err != nil {
			t.Fatal(err)
		}
		ec := parseExecutionContext(inst, nil)
		if getString(ec.State, "job_id") != req.ID || !getBool(ec.State, "transcode_reconcile") {
			t.Fatalf("remote submission preceded durable identity: %+v", ec.State)
		}
		return transcode.Job{}, &transcode.UncertainError{Op: "submit", Err: context.DeadlineExceeded}
	}
	mock.statusFunc = func(ctx context.Context, id string) (transcode.JobStatus, error) {
		return transcode.JobStatus{ID: id, Status: transcode.StatusCompleted, JobTelemetry: transcode.JobTelemetry{FinishedAt: now.Add(-time.Minute), EncodeDurationMs: &encodeMs}}, nil
	}
	final := func(context.Context, *ExecutionContext) (StepResult, error) {
		finalized.Add(1)
		return StepResult{Status: StepCompleted}, nil
	}
	first := NewEngine(EngineDeps{Store: st, Transcode: mock, Now: func() time.Time { return now }})
	registerReconcileTestFlow(first, final)
	res, err := first.Run(context.Background(), "transcode_media", nil)
	if err != nil || res.Status != StatusWaitingExternal {
		t.Fatalf("initial submit: %+v / %v", res, err)
	}
	if getString(res.State, "next_poll_at") == "" {
		t.Fatal("poll schedule was not persisted")
	}
	// Fresh engine represents restart: it has no in-memory knowledge of the job.
	restarted := NewEngine(EngineDeps{Store: st, Transcode: mock, Now: func() time.Time { return now.Add(time.Minute) }})
	registerReconcileTestFlow(restarted, final)
	ctx, cancel := context.WithCancel(context.Background())
	done := restarted.StartReconciler(ctx)
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		res, err = restarted.Status(context.Background(), res.ID)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status == StatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("service loop did not complete action: %+v", res)
	}
	if mock.submitCalls != 1 || finalized.Load() != 1 {
		t.Fatalf("submit=%d finalized=%d", mock.submitCalls, finalized.Load())
	}
	if res.Outputs["transcode_status"] != transcode.StatusCompleted || !getBool(res.State, "transcode_done") {
		t.Fatalf("inconsistent terminal state: %+v", res.Outputs)
	}
	if res.EncodeDurationMs == nil || *res.EncodeDurationMs != encodeMs || res.ReconcileLagMs == nil || *res.ReconcileLagMs != 120000 {
		t.Fatalf("phase durations missing/wrong: %+v", res)
	}
	if getString(res.State, "worker_completed_at") == "" || getString(res.State, "reconciled_at") == "" || getString(res.State, "last_worker_poll_at") == "" {
		t.Fatal("missing durable reconciliation timestamps")
	}
	if _, ok := res.State["next_poll_at"]; ok {
		t.Fatal("completed action still scheduled")
	}
}

func TestConcurrentResumeAndReconcilerShareDurableClaim(t *testing.T) {
	st := setupTestStore(t)
	var finals atomic.Int32
	entered, unblock := make(chan struct{}), make(chan struct{})
	var polling atomic.Bool
	mock := &mockTranscodeExecutor{statusFunc: func(ctx context.Context, id string) (transcode.JobStatus, error) {
		if !polling.Load() {
			return transcode.JobStatus{ID: id, Status: transcode.StatusRunning}, nil
		}
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-unblock
		return transcode.JobStatus{ID: id, Status: transcode.StatusCompleted}, nil
	}}
	final := func(context.Context, *ExecutionContext) (StepResult, error) {
		finals.Add(1)
		return StepResult{Status: StepCompleted}, nil
	}
	e1 := NewEngine(EngineDeps{Store: st, Transcode: mock})
	e2 := NewEngine(EngineDeps{Store: st, Transcode: mock, Now: func() time.Time { return time.Now().Add(time.Hour) }})
	registerReconcileTestFlow(e1, final)
	registerReconcileTestFlow(e2, final)
	res, err := e1.Run(context.Background(), "transcode_media", nil)
	if err != nil {
		t.Fatal(err)
	}
	polling.Store(true)
	reconciled := make(chan error, 1)
	go func() { reconciled <- e2.ReconcileOnce(context.Background()) }()
	<-entered
	resumed := make(chan error, 1)
	go func() { _, err := e1.Resume(context.Background(), res.ID, "", nil); resumed <- err }()
	close(unblock)
	if err := <-reconciled; err != nil {
		t.Fatal(err)
	}
	if err := <-resumed; err != nil {
		t.Fatal(err)
	}
	if finals.Load() != 1 {
		t.Fatalf("accept_result executed %d times", finals.Load())
	}
}

func TestReconcilerNeverApprovesDecisionAndHonorsPollSchedule(t *testing.T) {
	st := setupTestStore(t)
	var calls atomic.Int32
	now := time.Now()
	e := NewEngine(EngineDeps{Store: st, Now: func() time.Time { return now }})
	e.RegisterTemplate(ActionTemplate{Name: "approval", AutoReconcile: true, Steps: []StepDefinition{{Name: "approval", Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
		calls.Add(1)
		if ec.Decision == "approve" {
			return StepResult{Status: StepCompleted}, nil
		}
		return StepResult{Status: StepWaitingDecision}, nil
	}}}})
	r, err := e.Run(context.Background(), "approval", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(context.Background(), r.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("decision step automatically executed %d times", calls.Load())
	}
	r, err = e.Resume(context.Background(), r.ID, "approve", nil)
	if err != nil || r.Status != StatusCompleted {
		t.Fatalf("explicit approval: %+v %v", r, err)
	}
	e.RegisterTemplate(ActionTemplate{Name: "external", AutoReconcile: true, Steps: []StepDefinition{{Name: "poll", Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
		calls.Add(1)
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_unreachable"}, nil
	}}}})
	r, err = e.Run(context.Background(), "external", nil)
	if err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != before {
		t.Fatal("polled before persisted next_poll_at")
	}
	now = now.Add(time.Minute)
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != before+1 {
		t.Fatal("due poll not resumed")
	}
}

func TestRetryCreatesNewWorkerIdentityOnlyAfterConfirmedFailure(t *testing.T) {
	for _, remote := range []string{transcode.StatusFailed, transcode.StatusRunning, "unreachable"} {
		t.Run(remote, func(t *testing.T) {
			st := setupTestStore(t)
			mock := &mockTranscodeExecutor{statusFunc: func(ctx context.Context, id string) (transcode.JobStatus, error) {
				if strings.HasSuffix(id, "retry-1") {
					return transcode.JobStatus{ID: id, Status: transcode.StatusRunning}, nil
				}
				if remote == "unreachable" {
					return transcode.JobStatus{}, &transcode.UncertainError{Op: "status", Err: context.DeadlineExceeded}
				}
				return transcode.JobStatus{ID: id, Status: remote, FailureClassification: "smb_signing_required", Error: "SMB signing required"}, nil
			}}
			e := NewEngine(EngineDeps{Store: st, Transcode: mock})
			registerReconcileTestFlow(e, func(context.Context, *ExecutionContext) (StepResult, error) {
				return StepResult{Status: StepCompleted}, nil
			})
			ec := testReconcileEC("")
			ec.State["job_id"], ec.State["transcode_submitted"] = "job-old", true
			inst := store.ActionInstance{ID: "retry-source", ActionName: "transcode_media", Status: StatusFailed, CurrentStep: 2, StateJSON: toJSON(ec.State)}
			if err := st.CreateActionInstance(inst); err != nil {
				t.Fatal(err)
			}
			r, err := e.Retry(context.Background(), inst.ID)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != StatusWaitingExternal {
				t.Fatalf("unexpected retry: %+v", r)
			}
			if remote == transcode.StatusFailed {
				if mock.submitCalls != 1 || getString(r.State, "job_id") == "job-old" || getString(r.State, "previous_job_id") != "job-old" {
					t.Fatalf("new confirmed attempt not allocated: %+v", r.State)
				}
			} else if mock.submitCalls != 0 || getString(r.State, "job_id") != "job-old" {
				t.Fatalf("unconfirmed failure was resubmitted: %+v", r.State)
			}
		})
	}
}

func TestAcceptResolvesSHAOnlyAfterActualVerification(t *testing.T) {
	p := filepath.Join(t.TempDir(), "original.mkv")
	if err := os.WriteFile(p, []byte("original bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte("original bytes")))
	e := NewEngine(EngineDeps{})
	for _, changed := range []bool{false, true} {
		ec := testReconcileEC("")
		ec.State["resolved_path"], ec.State["original_sha256"] = p, want
		ec.State["validation"] = map[string]any{"original_sha256_pending": "accept_result"}
		if changed {
			if err := os.WriteFile(p, []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		r, err := e.stepTranscodeAccept(context.Background(), ec)
		if err != nil {
			t.Fatal(err)
		}
		v := ec.State["validation"].(map[string]any)
		if changed {
			if r.Status != StepFailed || v["original_sha256_pending"] == nil || v["original_integrity"] == "verified" {
				t.Fatalf("changed source passed: %+v", r)
			}
		} else if r.Status != StepCompleted || v["original_sha256_pending"] != nil || v["original_integrity"] != "verified" {
			t.Fatalf("SHA not finalized: %+v", r)
		}
	}
}

func TestCallerCancellationSuspendsWithoutCancellingWorker(t *testing.T) {
	st := setupTestStore(t)
	mock := &mockTranscodeExecutor{}
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})
	ctx, cancel := context.WithCancel(context.Background())
	e.RegisterTemplate(ActionTemplate{Name: "interrupted", AutoReconcile: true, Steps: []StepDefinition{{Name: "validate_result", Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
		ec.State["job_id"] = "existing-worker-job"
		cancel()
		return StepResult{}, context.Canceled
	}}}})
	r, err := e.Run(ctx, "interrupted", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusWaitingExternal || mock.cancelCalls != 0 || mock.benchmarkCancelCalls != 0 {
		t.Fatalf("request cancellation killed workflow: %+v", r)
	}
	ec := testReconcileEC("")
	ec.State["optimization_enabled"], ec.State["benchmark_job_id"] = true, "bench-existing"
	br, err := e.stepBenchmarkWait(ctx, ec)
	if err != nil || br.Status != StepWaitingExternal || mock.benchmarkCancelCalls != 0 {
		t.Fatalf("cancelled benchmark request killed worker: %+v %v", br, err)
	}
}

func TestStatusHTTPFailuresRemainReconciliableWithoutResubmission(t *testing.T) {
	for _, code := range []int{408, 429, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			mock := &mockTranscodeExecutor{
				statusFunc: func(context.Context, string) (transcode.JobStatus, error) {
					return transcode.JobStatus{}, &transcode.HTTPError{StatusCode: code, Message: "status unavailable"}
				},
				benchmarkStatusFunc: func(context.Context, string) (transcode.BenchmarkStatus, error) {
					return transcode.BenchmarkStatus{}, &transcode.HTTPError{StatusCode: code, Message: "status unavailable"}
				},
			}
			e := NewEngine(EngineDeps{Transcode: mock})
			ec := testReconcileEC("")
			ec.State["job_id"], ec.State["retry_count"] = "job-existing", 2
			r, err := e.stepTranscodeWait(context.Background(), ec)
			if err != nil || r.Status != StepWaitingExternal || getInt(ec.State, "retry_count") != 2 || mock.submitCalls != 0 {
				t.Fatalf("HTTP %d became encode failure/retry: %+v %v", code, r, err)
			}
			ec.State["optimization_enabled"], ec.State["benchmark_job_id"], ec.State["benchmark_retry_count"] = true, "bench-existing", 2
			br, err := e.stepBenchmarkWait(context.Background(), ec)
			if err != nil || br.Status != StepWaitingExternal || getInt(ec.State, "benchmark_retry_count") != 2 || mock.benchmarkSubmitCalls != 0 {
				t.Fatalf("HTTP %d became benchmark failure/retry: %+v %v", code, br, err)
			}
		})
	}
}

func TestPublicationRecoveryRequiredPausesForInterventionWithoutEncodingAgain(t *testing.T) {
	mock := &mockTranscodeExecutor{statusFunc: func(context.Context, string) (transcode.JobStatus, error) {
		return transcode.JobStatus{ID: "job-existing", Status: transcode.StatusRunning, FailureClassification: "storage_permission_denied", Error: "cannot publish candidate", JobTelemetry: transcode.JobTelemetry{Phase: "publication_pending", RecoveryRequired: true}}, nil
	}}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("")
	ec.State["job_id"] = "job-existing"
	r, err := e.stepTranscodeWait(context.Background(), ec)
	if err != nil || r.Status != StepWaitingDecision || r.Outputs["recovery_required"] != true || mock.submitCalls != 0 {
		t.Fatalf("publication failure was not paused: %+v %v", r, err)
	}
}

func TestPromotionIdentitySurvivesCompletionAndCannotChangeOnResume(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st})
	var promotions atomic.Int32
	e.RegisterTemplate(ActionTemplate{Name: "promote_transcode_candidate", ImmutableInputs: true, Steps: []StepDefinition{{Name: "approve", Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
		if ec.Decision != "approve" {
			return StepResult{Status: StepWaitingDecision}, nil
		}
		promotions.Add(1)
		return StepResult{Status: StepCompleted}, nil
	}}}})
	inputs := map[string]any{"transcode_action_id": "source-action", "series_id": 42}
	r, err := e.Run(context.Background(), "promote_transcode_candidate", inputs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(context.Background(), r.ID, "approve", map[string]any{"series_id": 99}); err == nil {
		t.Fatal("resume could replace approved baseline")
	}
	r, err = e.Resume(context.Background(), r.ID, "approve", nil)
	if err != nil || r.Status != StatusCompleted {
		t.Fatalf("explicit approval failed: %+v %v", r, err)
	}
	again, err := e.Run(context.Background(), "promote_transcode_candidate", map[string]any{"transcode_action_id": "source-action", "series_id": 42}, "different-user-key")
	if err != nil || again.ID != r.ID || promotions.Load() != 1 {
		t.Fatalf("completed promotion duplicated: %+v %v", again, err)
	}
	if _, err := e.Run(context.Background(), "promote_transcode_candidate", map[string]any{"transcode_action_id": "source-action", "series_id": 99}); err == nil {
		t.Fatal("conflicting series reused promotion")
	}
}

func TestPromotionRunRetriesMatchingStepZeroFailure(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st})
	e.RegisterTemplate(ActionTemplate{Name: "promote_transcode_candidate", Steps: []StepDefinition{{Name: "plan", Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
		if getInt(ec.Inputs, "series_id") <= 0 {
			return StepResult{Status: StepFailed, Error: "positive series_id required"}, nil
		}
		return StepResult{Status: StepWaitingDecision}, nil
	}}}})
	if err := st.CreateActionInstance(store.ActionInstance{
		ID:             "failed-promotion",
		ActionName:     "promote_transcode_candidate",
		Status:         StatusFailed,
		CurrentStep:    0,
		InputsJSON:     `{"service":"sonarr","transcode_action_id":"source-action","series_id":"42"}`,
		OutputsJSON:    `{}`,
		StateJSON:      `{}`,
		ErrorJSON:      "positive series_id required",
		IdempotencyKey: "promote:sonarr:source-action",
	}); err != nil {
		t.Fatal(err)
	}

	result, err := e.Run(context.Background(), "promote_transcode_candidate", map[string]any{
		"transcode_action_id": "source-action",
		"series_id":           42,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "failed-promotion" || result.Status != StatusWaitingDecision {
		t.Fatalf("expected same promotion to retry into approval, got id=%s status=%s error=%s", result.ID, result.Status, result.Error)
	}
}

func TestAuditLogCannotAdvanceBeyondPersistedCheckpoint(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st})
	var first atomic.Int32
	e.RegisterTemplate(ActionTemplate{Name: "checkpoint", AutoReconcile: true, Steps: []StepDefinition{
		{Name: "checkpoint", Run: func(context.Context, *ExecutionContext) (StepResult, error) {
			first.Add(1)
			return StepResult{Status: StepCompleted, Outputs: map[string]any{"durable_output": true}}, nil
		}},
		{Name: "use_checkpoint", Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
			if !getBool(ec.State, "durable_output") {
				return StepResult{Status: StepFailed, Error: "skipped missing state"}, nil
			}
			return StepResult{Status: StepCompleted}, nil
		}},
	}})
	inst := store.ActionInstance{ID: "checkpoint", ActionName: "checkpoint", Status: StatusRunning, StateJSON: `{"job_id":"known"}`}
	if err := st.CreateActionInstance(inst); err != nil {
		t.Fatal(err)
	}
	if err := st.LogActionStep(store.ActionStepLog{InstanceID: inst.ID, StepIndex: 0, StepName: "checkpoint", Status: string(StepCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err := e.Status(context.Background(), inst.ID)
	if err != nil || r.Status != StatusCompleted || first.Load() != 1 {
		t.Fatalf("log advanced past checkpoint: %+v %v", r, err)
	}
}

func TestBenchmarkUncertainSubmitPersistsIdentityAndResumesAutomatically(t *testing.T) {
	var persistedStore *store.Store
	mock := &mockTranscodeExecutor{benchmarkSubmitFunc: func(ctx context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
		inst, err := persistedStore.GetActionInstance(strings.TrimPrefix(req.ID, "bench-"))
		if err != nil {
			t.Fatal(err)
		}
		ec := parseExecutionContext(inst, nil)
		if getString(ec.State, "benchmark_job_id") != req.ID || !getBool(ec.State, "benchmark_reconcile") || ec.State["benchmark_request"] == nil {
			t.Fatal("benchmark request not durable before submit")
		}
		return transcode.BenchmarkJob{}, &transcode.UncertainError{Op: "benchmark_submit", Err: context.DeadlineExceeded}
	}, benchmarkStatusFunc: func(ctx context.Context, id string) (transcode.BenchmarkStatus, error) {
		return transcode.BenchmarkStatus{ID: id, Status: transcode.StatusCompleted, FinishedAt: time.Now().Add(-time.Second), Decision: &transcode.BenchmarkDecision{DecisionReason: "test report complete"}}, nil
	}}
	e, st, source, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
	persistedStore = st
	r, err := e.Run(context.Background(), "benchmark_transcode", map[string]any{"path": source, "profile": "opt-vt"})
	if err != nil || r.Status != StatusWaitingExternal {
		t.Fatalf("uncertain benchmark submit: %+v %v", r, err)
	}
	e.deps.Now = func() time.Time { return time.Now().Add(time.Minute) }
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err = e.Status(context.Background(), r.ID)
	if err != nil || r.Status != StatusCompleted || mock.benchmarkSubmitCalls != 1 || r.Outputs["benchmark_status"] != "completed" {
		t.Fatalf("benchmark not reconciled: %+v %v", r, err)
	}
}

func TestPausedControlStateSurvivesFollowingAutomaticPoll(t *testing.T) {
	st := setupTestStore(t)
	now := time.Now()
	e := NewEngine(EngineDeps{Store: st, Now: func() time.Time { return now }})
	e.RegisterTemplate(ActionTemplate{Name: "paused_batch", AutoReconcile: true, ImmutableInputs: true, Steps: []StepDefinition{{Name: "schedule", Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
		if _, initialized := ec.State["paused"]; !initialized {
			ec.State["paused"] = getBool(ec.Inputs, "paused")
		}
		if ec.Decision == "resume" {
			ec.State["paused"] = false
			return StepResult{Status: StepWaitingExternal, WaitingCondition: "jobs_running"}, nil
		}
		if getBool(ec.State, "paused") {
			return StepResult{Status: StepWaitingDecision}, nil
		}
		return StepResult{Status: StepCompleted}, nil
	}}}})
	r, err := e.Run(context.Background(), "paused_batch", map[string]any{"paused": true})
	if err != nil {
		t.Fatal(err)
	}
	r, err = e.Resume(context.Background(), r.ID, "resume", nil)
	if err != nil || r.Status != StatusWaitingExternal {
		t.Fatalf("resume failed: %+v %v", r, err)
	}
	now = now.Add(time.Minute)
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err = e.Status(context.Background(), r.ID)
	if err != nil || r.Status != StatusCompleted || !getBool(r.Inputs, "paused") || getBool(r.State, "paused") {
		t.Fatalf("pause state/input separation was not preserved: %+v %v", r, err)
	}
}
