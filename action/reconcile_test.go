package action

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
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
		return transcode.Job{}, &httpclient.UncertainError{Op: "submit", Err: context.DeadlineExceeded}
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
					return transcode.JobStatus{}, &httpclient.UncertainError{Op: "status", Err: context.DeadlineExceeded}
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

func TestRetryAfterRejectedTranscodeAdmissionRetainsAnalysisAndIdentity(t *testing.T) {
	for _, kind := range []string{"missing", "wrapped_missing", "uncertain_missing", "forbidden"} {
		t.Run(kind, func(t *testing.T) {
			st := setupTestStore(t)
			accepted := false
			statusErr := error(&httpclient.HTTPError{StatusCode: 404})
			if kind == "wrapped_missing" {
				statusErr = fmt.Errorf("status: %w", statusErr)
			}
			if kind == "uncertain_missing" {
				statusErr = &httpclient.UncertainError{Op: "status", Err: statusErr}
			}
			if kind == "forbidden" {
				statusErr = &httpclient.HTTPError{StatusCode: 403}
			}
			mock := &mockTranscodeExecutor{
				statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
					if !accepted {
						return transcode.JobStatus{}, statusErr
					}
					return transcode.JobStatus{ID: id, Status: transcode.StatusRunning}, nil
				},
				submitFunc: func(_ context.Context, req transcode.Request) (transcode.Job, error) {
					if req.ID != "job-rejected" || req.IdempotencyKey != "job-rejected" {
						t.Fatalf("identity changed: %+v", req)
					}
					accepted = true
					return transcode.Job{ID: req.ID}, nil
				},
			}
			e := NewEngine(EngineDeps{Store: st, Transcode: mock})
			registerReconcileTestFlow(e, func(context.Context, *ExecutionContext) (StepResult, error) {
				return StepResult{Status: StepCompleted}, nil
			})
			ec := testReconcileEC("")
			ec.State["job_id"], ec.State["benchmark_done"], ec.State["benchmark_job_id"] = "job-rejected", true, "bench-approved"
			ec.State["benchmark_request_digest"], ec.State["benchmark_search_seconds"] = "approved-digest", 83.0
			inst := store.ActionInstance{ID: "retry-rejected", ActionName: "transcode_media", Status: StatusFailed, CurrentStep: 1, StateJSON: toJSON(ec.State)}
			if err := st.CreateActionInstance(inst); err != nil {
				t.Fatal(err)
			}
			r, err := e.Retry(context.Background(), inst.ID)
			if kind == "forbidden" {
				if err == nil || mock.submitCalls != 0 {
					t.Fatalf("unauthorized read permitted retry: %+v %v", r, err)
				}
				return
			}
			wantSubmits := int32(1)
			if kind == "uncertain_missing" {
				wantSubmits = 0
			}
			if err != nil || r.Status != StatusWaitingExternal || mock.submitCalls != wantSubmits {
				t.Fatalf("retry: %+v err=%v submits=%d", r, err, mock.submitCalls)
			}
			if getString(r.State, "job_id") != "job-rejected" || getInt(r.State, "manual_retry_count") != 0 || !getBool(r.State, "benchmark_done") || getString(r.State, "benchmark_job_id") != "bench-approved" || getString(r.State, "benchmark_request_digest") != "approved-digest" || r.State["benchmark_search_seconds"] != 83.0 || mock.benchmarkSubmitCalls != 0 {
				t.Fatalf("retry discarded analysis or changed admission: %+v", r.State)
			}
		})
	}
}

func TestRejectedBenchmarkAdmissionRetryKeepsRequestAndBudget(t *testing.T) {
	for _, code := range []int{404, 403} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			mock := &mockTranscodeExecutor{benchmarkStatusFunc: func(context.Context, string) (transcode.BenchmarkStatus, error) {
				return transcode.BenchmarkStatus{}, fmt.Errorf("worker read: %w", &httpclient.HTTPError{StatusCode: code})
			}}
			e := NewEngine(EngineDeps{Transcode: mock})
			ec := testReconcileEC("")
			ec.State["benchmark_job_id"], ec.State["benchmark_request_digest"], ec.State["search_budget_seconds"] = "bench-rejected", "frozen-request", 600
			tmpl := ActionTemplate{Steps: []StepDefinition{{Name: "preflight"}, {Name: "submit_benchmark"}, {Name: "wait_benchmark"}}}
			resume, err := e.prepareRemoteRetry(context.Background(), &store.ActionInstance{ActionName: "benchmark_transcode"}, ec, tmpl, 2)
			if code == 404 && (err != nil || resume != 1 || !getBool(ec.State, "benchmark_reconcile")) {
				t.Fatalf("missing admission cannot reconcile: %d %v %+v", resume, err, ec.State)
			}
			if code == 403 && (err == nil || getBool(ec.State, "benchmark_reconcile")) {
				t.Fatalf("forbidden read admitted retry: %v", err)
			}
			if getString(ec.State, "benchmark_job_id") != "bench-rejected" || getString(ec.State, "benchmark_request_digest") != "frozen-request" || getInt(ec.State, "search_budget_seconds") != 600 || getInt(ec.State, "benchmark_manual_retry_count") != 0 || mock.benchmarkSubmitCalls != 0 {
				t.Fatalf("retry preparation changed request or budget: %+v", ec.State)
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
					return transcode.JobStatus{}, &httpclient.HTTPError{StatusCode: code, Message: "status unavailable"}
				},
				benchmarkStatusFunc: func(context.Context, string) (transcode.BenchmarkStatus, error) {
					return transcode.BenchmarkStatus{}, &httpclient.HTTPError{StatusCode: code, Message: "status unavailable"}
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
		return transcode.BenchmarkJob{}, &httpclient.UncertainError{Op: "benchmark_submit", Err: context.DeadlineExceeded}
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

// A continuation that blocks for minutes (a source hash or candidate
// validation) must not stop the service loop from polling other running jobs;
// otherwise their worker heartbeat goes stale although the worker is alive.
func TestReconcilerKeepsPollingWhileAnotherContinuationIsSlow(t *testing.T) {
	st := setupTestStore(t)
	release := make(chan struct{})
	var slowEntered atomic.Bool
	var fastPolls atomic.Int32
	var started atomic.Bool
	var slowID atomic.Value
	mock := &mockTranscodeExecutor{statusFunc: func(ctx context.Context, id string) (transcode.JobStatus, error) {
		if started.Load() {
			if id == slowID.Load() {
				slowEntered.Store(true)
				select {
				case <-release:
				case <-ctx.Done():
					return transcode.JobStatus{}, ctx.Err()
				}
			} else {
				fastPolls.Add(1)
			}
		}
		return transcode.JobStatus{ID: id, Status: transcode.StatusRunning}, nil
	}}
	final := func(context.Context, *ExecutionContext) (StepResult, error) {
		return StepResult{Status: StepCompleted}, nil
	}
	e := NewEngine(EngineDeps{Store: st, Transcode: mock, ReconcileInterval: 5 * time.Millisecond})
	registerReconcileTestFlow(e, final)
	slow, err := e.Run(context.Background(), "transcode_media", nil)
	if err != nil || slow.Status != StatusWaitingExternal {
		t.Fatalf("slow submit: %+v / %v", slow, err)
	}
	fast, err := e.Run(context.Background(), "transcode_media", nil)
	if err != nil || fast.Status != StatusWaitingExternal {
		t.Fatalf("fast submit: %+v / %v", fast, err)
	}
	slowID.Store("job-" + slow.ID)
	started.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := e.StartReconciler(ctx)
	defer func() { close(release); cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !(slowEntered.Load() && fastPolls.Load() >= 5) {
		time.Sleep(5 * time.Millisecond)
	}
	if !slowEntered.Load() {
		t.Fatal("slow continuation never started")
	}
	if got := fastPolls.Load(); got < 5 {
		t.Fatalf("other job polled %d times while one continuation was blocked; want repeated polls", got)
	}
}

func TestReconcilerPollsEveryJobWhenSlowContinuationsHoldMostSlots(t *testing.T) {
	st := setupTestStore(t)
	release := make(chan struct{})
	var started atomic.Bool
	var mu sync.Mutex
	slow := map[string]bool{}
	polls := map[string]int{}
	mock := &mockTranscodeExecutor{statusFunc: func(ctx context.Context, id string) (transcode.JobStatus, error) {
		if started.Load() {
			mu.Lock()
			isSlow := slow[id]
			if !isSlow {
				polls[id]++
			}
			mu.Unlock()
			if isSlow {
				select {
				case <-release:
				case <-ctx.Done():
					return transcode.JobStatus{}, ctx.Err()
				}
			}
		}
		return transcode.JobStatus{ID: id, Status: transcode.StatusRunning}, nil
	}}
	final := func(context.Context, *ExecutionContext) (StepResult, error) {
		return StepResult{Status: StepCompleted}, nil
	}
	e := NewEngine(EngineDeps{Store: st, Transcode: mock, ReconcileInterval: 5 * time.Millisecond})
	registerReconcileTestFlow(e, final)
	const slowJobs, fastJobs = reconcileConcurrency - 2, 6
	var fast []string
	for i := 0; i < slowJobs+fastJobs; i++ {
		inst, err := e.Run(context.Background(), "transcode_media", nil)
		if err != nil || inst.Status != StatusWaitingExternal {
			t.Fatalf("submit %d: %+v / %v", i, inst, err)
		}
		if i < slowJobs {
			slow["job-"+inst.ID] = true
		} else {
			fast = append(fast, "job-"+inst.ID)
		}
	}
	started.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := e.StartReconciler(ctx)
	defer func() { close(release); cancel(); <-done }()
	starved := func() []string {
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for _, id := range fast {
			if polls[id] < 3 {
				out = append(out, id)
			}
		}
		return out
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(starved()) > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if s := starved(); len(s) > 0 {
		t.Fatalf("%d of %d jobs were polled fewer than 3 times while slow continuations held most slots", len(s), fastJobs)
	}
}

type blockingHashReader struct {
	active, peak *atomic.Int32
	release      <-chan struct{}
	done         bool
}

func (r *blockingHashReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	n := r.active.Add(1)
	for {
		peak := r.peak.Load()
		if n <= peak || r.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	<-r.release
	r.active.Add(-1)
	r.done = true
	return copy(p, "x"), nil
}

func TestMediaHashesShareABoundedPool(t *testing.T) {
	e := NewEngine(EngineDeps{})
	release := make(chan struct{})
	var active, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < mediaHashConcurrency+4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.hashMedia(context.Background(), io.Discard, &blockingHashReader{active: &active, peak: &peak, release: release})
		}()
	}
	for deadline := time.Now().Add(3 * time.Second); active.Load() < mediaHashConcurrency && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if got := active.Load(); got != mediaHashConcurrency {
		t.Fatalf("%d hashes reading at once, want %d", got, mediaHashConcurrency)
	}
	close(release)
	wg.Wait()
	if got := peak.Load(); got > mediaHashConcurrency {
		t.Fatalf("peak %d concurrent hashes, want at most %d", got, mediaHashConcurrency)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < mediaHashConcurrency; i++ {
		e.hashSlots <- struct{}{}
	}
	if _, err := e.hashMedia(ctx, io.Discard, strings.NewReader("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting for a hash slot ignored cancellation: %v", err)
	}
}

func TestDispatcherSlotsAreFreeWhileContinuationsWaitForHashes(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, ReconcileInterval: time.Hour})
	release := make(chan struct{})
	var hashing atomic.Int32
	e.RegisterTemplate(ActionTemplate{Name: "hash_burst", AutoReconcile: true, Steps: []StepDefinition{
		{Name: "preflight", Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
			done, err := e.acquireHashSlot(ctx)
			if err != nil {
				return StepResult{}, err
			}
			defer done()
			hashing.Add(1)
			defer hashing.Add(-1)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return StepResult{Status: StepCompleted}, nil
		}},
	}})
	const burst = reconcileConcurrency + 4
	for i := 0; i < burst; i++ {
		inst := store.ActionInstance{ID: fmt.Sprintf("act-hash-burst-%02d", i), ActionName: "hash_burst", Status: StatusPending, InputsJSON: "{}", StateJSON: `{"background_admission":true}`, OutputsJSON: "{}"}
		if err := st.CreateActionInstance(inst); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := newReconcileDispatcher()
	t.Cleanup(func() { close(release); cancel(); d.wg.Wait() })
	finished := make(chan error, 1)
	go func() { finished <- e.dispatchReconcile(ctx, d) }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch blocked behind continuations waiting for hashes")
	}
	for deadline := time.Now().Add(3 * time.Second); (len(d.sem) > 0 || hashing.Load() < mediaHashConcurrency) && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	d.mu.Lock()
	inflight := len(d.inflight)
	d.mu.Unlock()
	if inflight != burst || hashing.Load() != mediaHashConcurrency || len(d.sem) != 0 {
		t.Fatalf("in flight %d, hashing %d, dispatcher slots held %d; want %d, %d, 0", inflight, hashing.Load(), len(d.sem), burst, mediaHashConcurrency)
	}
}
