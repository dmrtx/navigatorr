package action

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

func seedActionInstance(t *testing.T, st *store.Store, id, name, status string, step int, idemKey string, inputs, state map[string]any) {
	t.Helper()
	if err := st.CreateActionInstance(store.ActionInstance{
		ID:             id,
		ActionName:     name,
		Status:         status,
		CurrentStep:    step,
		IdempotencyKey: idemKey,
		InputsJSON:     toJSON(inputs),
		StateJSON:      toJSON(state),
		OutputsJSON:    "{}",
	}); err != nil {
		t.Fatalf("seeding action %s: %v", id, err)
	}
}

func submitStepState() map[string]any {
	return map[string]any{
		"resolved_path":       "/Volumes/media/ep.mkv",
		"candidate_extension": ".mkv",
		"profile":             "hevc-vt",
		"plan":                testReconcilePlan(),
		"attempt":             1,
		"retry_count":         0,
	}
}

func childECFor(instanceID, parentID string, state map[string]any) *ExecutionContext {
	ec := testReconcileEC(instanceID)
	ec.InstanceID = instanceID
	if parentID != "" {
		ec.Inputs["parent_action_id"] = parentID
	}
	for k, v := range state {
		ec.State[k] = v
	}
	return ec
}

func seedBatchParent(t *testing.T, st *store.Store, parentID, status string, state map[string]any) {
	t.Helper()
	seedActionInstance(t, st, parentID, "transcode_batch", status, 0, "", map[string]any{"service": "sonarr"}, state)
}

// A pending child of a cancelled parent must never start a new remote submit.
func TestChildSubmitBlockedByCancelledParent(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "parent-cancel", StatusCancelled, map[string]any{})
	mock := &mockTranscodeExecutor{}
	seedActionInstance(t, st, "child-pending", "transcode_media", StatusRunning, 3, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "parent-cancel"}, submitStepState())
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	ec := childECFor("child-pending", "parent-cancel", submitStepState())
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mock.submitCalls != 0 {
		t.Fatalf("cancelled parent must not submit, submitCalls=%d", mock.submitCalls)
	}
	if res.Status != StepFailed {
		t.Fatalf("got %s want failed (cancelled parent)", res.Status)
	}
}

// An uncertain child (persisted identity) whose job is absent from the worker
// must query the same job but never resubmit after cancel.
func TestChildUncertain404BlockedByCancelledParent(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "parent-cancel", StatusCancelled, map[string]any{})
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, &transcode.HTTPError{Method: "GET", URL: "/v1/jobs/" + jobID, StatusCode: 404, Message: "not found"}
		},
	}
	state := submitStepState()
	state["job_id"] = "job-child"
	state["transcode_reconcile"] = true
	seedActionInstance(t, st, "child-pending", "transcode_media", StatusWaitingExternal, 3, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "parent-cancel"}, state)
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	ec := childECFor("child-pending", "parent-cancel", state)
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mock.statusCalls != 1 {
		t.Fatalf("uncertain child must query the same job exactly once, statusCalls=%d", mock.statusCalls)
	}
	if mock.submitCalls != 0 {
		t.Fatalf("404 after cancel must not resubmit, submitCalls=%d", mock.submitCalls)
	}
	if res.Status != StepFailed {
		t.Fatalf("got %s want failed", res.Status)
	}
}

// A paused parent defers a new submit (reversible) without contacting the worker.
func TestChildSubmitDeferredByPausedParent(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "parent-paused", StatusRunning, map[string]any{"paused": true})
	mock := &mockTranscodeExecutor{}
	seedActionInstance(t, st, "child-pending", "transcode_media", StatusRunning, 3, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "parent-paused"}, submitStepState())
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	ec := childECFor("child-pending", "parent-paused", submitStepState())
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mock.submitCalls != 0 {
		t.Fatalf("paused parent must not submit, submitCalls=%d", mock.submitCalls)
	}
	if res.Status != StepWaitingExternal || res.WaitingCondition != "parent_paused" {
		t.Fatalf("got %s/%s want waiting_external/parent_paused", res.Status, res.WaitingCondition)
	}
}

// Already-accepted child jobs keep their identity and are still tracked even
// when the parent is cancelled.
func TestAcceptedChildStillTrackedUnderCancelledParent(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "parent-cancel", StatusCancelled, map[string]any{})
	mock := &mockTranscodeExecutor{}
	state := submitStepState()
	state["job_id"] = "job-accepted"
	state["transcode_submitted"] = true
	seedActionInstance(t, st, "child-accepted", "transcode_media", StatusWaitingExternal, 4, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "parent-cancel"}, state)
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	ec := childECFor("child-accepted", "parent-cancel", state)
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != StepCompleted || res.Outputs["reused"] != true {
		t.Fatalf("accepted child must be reused, got %s %+v", res.Status, res.Outputs)
	}
	if mock.submitCalls != 0 {
		t.Fatalf("accepted child must not resubmit, submitCalls=%d", mock.submitCalls)
	}
}

// Explicit resume after unpausing lets the child admit again; a paused parent
// that accepted a benchmark must not launch the follow-on encode.
func TestPauseBlocksEncodeButResumeAllowsSubmit(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "parent-pause", StatusRunning, map[string]any{"paused": true})
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning}, nil
		},
	}
	state := submitStepState()
	state["benchmark_submitted"] = true
	state["benchmark_job_id"] = "bench-accepted"
	seedActionInstance(t, st, "child-encode", "transcode_media", StatusWaitingExternal, 3, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "parent-pause"}, state)
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	ec := childECFor("child-encode", "parent-pause", state)
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != StepWaitingExternal || res.WaitingCondition != "parent_paused" || mock.submitCalls != 0 {
		t.Fatalf("paused parent must not launch encode: %s/%s submits=%d", res.Status, res.WaitingCondition, mock.submitCalls)
	}

	// Explicit resume clears durable State only. The original immutable input may
	// still say paused=true and must not block the next child admission.
	parent, err := st.GetActionInstance("parent-pause")
	if err != nil {
		t.Fatal(err)
	}
	parent.StateJSON = toJSON(map[string]any{"paused": false})
	parent.InputsJSON = toJSON(map[string]any{"service": "sonarr", "paused": true})
	if err := st.UpdateActionInstance(*parent); err != nil {
		t.Fatal(err)
	}
	res, err = e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != StepCompleted || mock.submitCalls != 1 {
		t.Fatalf("unpaused parent must allow exactly one submit, got %s submits=%d", res.Status, mock.submitCalls)
	}
}

// A legacy child without parent_action_id is linked through its exact child
// idempotency key; a worker_busy pending child must stay blocked after cancel,
// even after the Engine is recreated.
func TestLegacyChildKeyRelationBlocksWorkerBusyAfterCancelAndRestart(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "legacy-parent", StatusCancelled, map[string]any{})
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{
		BatchID: "legacy-parent", ItemKey: "ep1", FilePath: "/Volumes/media/ep.mkv",
		Status: "waiting_for_slot", ChildActionID: "",
	}); err != nil {
		t.Fatal(err)
	}
	state := submitStepState()
	state["failure_classification"] = "worker_busy"
	seedActionInstance(t, st, "legacy-child", "transcode_media", StatusWaitingExternal, 3,
		"batch-legacy-parent-ep1", map[string]any{"path": "/Volumes/media/ep.mkv"}, state)
	mock := &mockTranscodeExecutor{}
	e := NewEngine(EngineDeps{Store: st, Transcode: mock}) // recreated engine, same DB

	res, err := e.Resume(context.Background(), "legacy-child", "", nil)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Status != StatusFailed || mock.submitCalls != 0 {
		t.Fatalf("legacy worker_busy child must be blocked: %s submits=%d", res.Status, mock.submitCalls)
	}
}

// Same legacy link, uncertain 404 after cancel: query the same job, never resubmit.
func TestLegacyChildKeyRelationBlocksUncertain404AfterCancelAndRestart(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "legacy-parent", StatusCancelled, map[string]any{})
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{
		BatchID: "legacy-parent", ItemKey: "ep1", FilePath: "/Volumes/media/ep.mkv",
		Status: "running", ChildActionID: "",
	}); err != nil {
		t.Fatal(err)
	}
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, &transcode.HTTPError{Method: "GET", URL: "/v1/jobs/" + jobID, StatusCode: 404, Message: "not found"}
		},
	}
	state := submitStepState()
	state["job_id"] = "job-legacy"
	state["transcode_reconcile"] = true
	seedActionInstance(t, st, "legacy-child", "transcode_media", StatusWaitingExternal, 3,
		"batch-legacy-parent-ep1", map[string]any{"path": "/Volumes/media/ep.mkv"}, state)
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	res, err := e.Resume(context.Background(), "legacy-child", "", nil)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if mock.statusCalls != 1 || mock.submitCalls != 0 {
		t.Fatalf("want 1 status, 0 submits; got status=%d submit=%d", mock.statusCalls, mock.submitCalls)
	}
	if res.Status != StatusFailed {
		t.Fatalf("got %s want failed", res.Status)
	}
}

// A new benchmark admission is gated by the parent; an accepted benchmark is
// still tracked even while paused.
func TestBenchmarkAdmissionGatedButAcceptedTracked(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "bench-parent", StatusRunning, map[string]any{"paused": true})
	mock := &mockTranscodeExecutor{}
	seedActionInstance(t, st, "bench-child", "transcode_media", StatusRunning, 1, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "bench-parent"},
		map[string]any{"optimization_enabled": true})
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	ec := childECFor("bench-child", "bench-parent", map[string]any{"optimization_enabled": true})
	res, err := e.stepBenchmarkSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mock.benchmarkSubmitCalls != 0 {
		t.Fatalf("paused parent must not submit benchmark, calls=%d", mock.benchmarkSubmitCalls)
	}
	if res.Status != StepWaitingExternal || res.WaitingCondition != "parent_paused" {
		t.Fatalf("got %s/%s want waiting_external/parent_paused", res.Status, res.WaitingCondition)
	}

	// Already-accepted benchmark: early reuse keeps tracking, no new submit.
	state := map[string]any{"optimization_enabled": true, "benchmark_submitted": true, "benchmark_job_id": "bench-done"}
	seedActionInstance(t, st, "bench-done-child", "transcode_media", StatusWaitingExternal, 2, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "bench-parent"}, state)
	ec2 := childECFor("bench-done-child", "bench-parent", state)
	res2, err := e.stepBenchmarkSubmit(context.Background(), ec2)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res2.Status != StepCompleted || res2.Outputs["reused"] != true {
		t.Fatalf("accepted benchmark must be reused, got %s %+v", res2.Status, res2.Outputs)
	}
}

// A child with a known but missing/non-batch parent must fail closed.
func TestMissingKnownParentFailsClosed(t *testing.T) {
	st := setupTestStore(t)
	mock := &mockTranscodeExecutor{}
	seedActionInstance(t, st, "orphan", "transcode_media", StatusRunning, 3, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "ghost-parent"}, submitStepState())
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	ec := childECFor("orphan", "ghost-parent", submitStepState())
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != StepFailed || mock.submitCalls != 0 {
		t.Fatalf("missing known parent must fail closed, got %s submits=%d", res.Status, mock.submitCalls)
	}

	// A pointer to a non-batch action is also rejected.
	seedActionInstance(t, st, "not-a-batch", "transcode_media", StatusRunning, 0, "", map[string]any{}, map[string]any{})
	ec.Inputs["parent_action_id"] = "not-a-batch"
	res, _ = e.stepTranscodeSubmit(context.Background(), ec)
	if res.Status != StepFailed || mock.submitCalls != 0 {
		t.Fatalf("non-batch parent must fail closed, got %s submits=%d", res.Status, mock.submitCalls)
	}
}

// A pending waiting_decision is preserved: the reconciler never auto-approves
// or auto-cancels it.
func TestWaitingDecisionNotAutoResolved(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "wd-parent", StatusWaitingDecision, map[string]any{})
	seedActionInstance(t, st, "wd-child", "transcode_media", StatusWaitingDecision, 5, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "wd-parent"},
		map[string]any{"candidate_path": "/Volumes/media/.navigatorr-candidates/ep.mkv"})
	mock := &mockTranscodeExecutor{}
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	child, _ := st.GetActionInstance("wd-child")
	if child.Status != StatusWaitingDecision {
		t.Fatalf("waiting_decision child changed to %s", child.Status)
	}
	if mock.submitCalls != 0 || mock.benchmarkSubmitCalls != 0 {
		t.Fatal("reconciler must not submit for a waiting_decision")
	}
}

// Race: a submit that holds the parent admission lease must finish persisting
// before Engine.Cancel can confirm, and no further submit may follow.
func TestSubmitVsCancelRaceNoNewSubmitAfterCancel(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "race-parent", StatusWaitingExternal, map[string]any{})
	state := submitStepState()
	seedActionInstance(t, st, "race-child", "transcode_media", StatusWaitingExternal, 3, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "race-parent"}, state)

	submitEntered := make(chan struct{})
	submitRelease := make(chan struct{})
	var enterOnce sync.Once
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			enterOnce.Do(func() { close(submitEntered) })
			<-submitRelease
			return transcode.Job{}, &transcode.UncertainError{Op: "submit", JobID: req.ID, Err: errors.New("timeout")}
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, &transcode.HTTPError{Method: "GET", URL: "/v1/jobs/" + jobID, StatusCode: 404, Message: "not found"}
		},
	}
	eA := NewEngine(EngineDeps{Store: st, Transcode: mock})
	eB := NewEngine(EngineDeps{Store: st, Transcode: mock})

	aDone := make(chan *ActionResult, 1)
	go func() {
		res, _ := eA.Resume(context.Background(), "race-child", "", nil)
		aDone <- res
	}()
	select {
	case <-submitEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("submit never entered")
	}

	// While Submit is in flight the parent lease must actually be held: another
	// owner cannot claim it. This fails without the admission guard.
	if ok, err := st.ClaimActionExecution("race-parent", "intruder", time.Now(), actionLeaseTTL); err != nil || ok {
		t.Fatalf("parent lease must be held while submit blocked (claimed=%v err=%v)", ok, err)
	}

	bDone := make(chan *ActionResult, 1)
	go func() {
		res, err := eB.Cancel(context.Background(), "race-parent", "race cancel")
		if err != nil {
			bDone <- nil
			return
		}
		bDone <- res
	}()

	close(submitRelease) // let A finish Submit and persist uncertainty

	select {
	case <-aDone:
	case <-time.After(5 * time.Second):
		t.Fatal("submit side deadlocked")
	}
	select {
	case res := <-bDone:
		if res == nil || res.Status != StatusCancelled {
			t.Fatalf("cancel did not confirm: %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel side deadlocked waiting for the parent lease")
	}

	// The child checkpoint was persisted before cancel confirmed.
	child, err := st.GetActionInstance("race-child")
	if err != nil {
		t.Fatal(err)
	}
	if !getBool(parseExecutionContext(child, eA).State, "transcode_reconcile") {
		t.Fatal("child uncertainty checkpoint must be persisted before cancel confirms")
	}

	// After cancel is confirmed, a further admission must not submit again.
	if _, err := eA.Resume(context.Background(), "race-child", "", nil); err != nil {
		t.Fatalf("post-cancel resume: %v", err)
	}
	if mock.submitCalls != 1 {
		t.Fatalf("no new submit may start after cancel confirms, submitCalls=%d", mock.submitCalls)
	}
}

// --- batch-level projection / pause-cancel semantics (real Engine + store) ---

func TestBatchCancelDecisionFromPausedCancelsPendingItems(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			writeCandidateOutput(req.CandidatePath)
			return transcode.Job{ID: req.ID}, nil
		},
	}
	engine, st, _, srv, _ := setupBatchTestEnv(t, mock, 1)
	defer srv.Close()
	defer st.Close()
	ctx := context.Background()

	res, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service": "sonarr", "series_id": 10, "paused": true,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected paused waiting_decision, got %s", res.Status)
	}

	cancelled, err := engine.Resume(ctx, res.ID, "cancel", nil)
	if err != nil {
		t.Fatalf("cancel from pause: %v", err)
	}
	if cancelled.Status != StatusCompleted {
		t.Fatalf("cancel from pause must complete, got %s (%s)", cancelled.Status, cancelled.Error)
	}
	items, _ := st.ListTranscodeBatchItems(res.ID)
	for _, it := range items {
		if it.Status == "queued" || it.Status == "waiting_for_slot" || it.Status == "running" {
			t.Fatalf("item %s still active after cancel: %s", it.ItemKey, it.Status)
		}
	}
	if mock.submitCalls != 0 {
		t.Fatalf("paused batch must not submit before or after cancel, submitCalls=%d", mock.submitCalls)
	}
}

func TestBatchPauseFromExecutionPersistsPausedFlag(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			writeCandidateOutput(req.CandidatePath)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning}, nil
		},
	}
	engine, st, _, srv, _ := setupBatchTestEnv(t, mock, 1)
	defer srv.Close()
	defer st.Close()
	ctx := context.Background()

	res, err := engine.Run(ctx, "transcode_batch", map[string]any{"service": "sonarr", "series_id": 10})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != StatusWaitingExternal {
		t.Fatalf("expected waiting_external, got %s", res.Status)
	}
	paused, err := engine.Resume(ctx, res.ID, "pause", nil)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if paused.Status != StatusWaitingDecision {
		t.Fatalf("pause must wait for decision, got %s", paused.Status)
	}
	inst, err := st.GetActionInstance(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	ec := parseExecutionContext(inst, engine)
	if !getBool(ec.State, "paused") {
		t.Fatal("decision=pause must persist paused=true in durable state")
	}
	if mock.submitCalls != 1 {
		t.Fatalf("pausing must not submit further items, submitCalls=%d", mock.submitCalls)
	}
}

func TestBatchProjectsCancelledChildAsFailed(t *testing.T) {
	st := setupTestStore(t)
	parentID := "batch-proj"
	seedBatchParent(t, st, parentID, StatusWaitingExternal, map[string]any{})
	seedActionInstance(t, st, "child-cancelled", "transcode_media", StatusCancelled, 0, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": parentID}, map[string]any{})
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{
		BatchID: parentID, ItemKey: "epfile-101", FilePath: "/Volumes/media/ep.mkv",
		Status: "running", ChildActionID: "child-cancelled",
	}); err != nil {
		t.Fatal(err)
	}

	e := NewEngine(EngineDeps{Store: st})
	ec := &ExecutionContext{
		InstanceID: parentID,
		ActionName: "transcode_batch",
		Inputs:     map[string]any{"service": "sonarr", "series_id": 10},
		State:      map[string]any{},
		Outputs:    map[string]any{},
	}
	res, err := e.stepTranscodeBatchSchedule(context.Background(), ec)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	items, _ := st.ListTranscodeBatchItems(parentID)
	if len(items) != 1 || items[0].Status != "failed" {
		t.Fatalf("cancelled child must project failed, got %+v", items)
	}
	if res.Status == StatusWaitingExternal && res.WaitingCondition == "transcode_running" {
		t.Fatal("cancelled child must not keep the batch waiting as running")
	}
}

// A lost parent guard blocks a new submit while the original child context
// remains alive, and preserves the persisted identity checkpoint.
func TestParentLeaseLossBlocksSubmitKeepsOriginalContext(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "guard-parent", StatusRunning, map[string]any{})
	state := submitStepState()
	seedActionInstance(t, st, "guard-child", "transcode_media", StatusRunning, 3, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "guard-parent"}, state)
	mock := &mockTranscodeExecutor{}
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	// Inherited parent token whose lease row does not exist: the guard cannot be
	// renewed, but the original child context stays alive.
	orig := context.WithValue(context.Background(), actionLeaseOwnerKey{actionID: "guard-parent"}, "stale-owner")
	ec := childECFor("guard-child", "guard-parent", state)
	res, err := e.stepTranscodeSubmit(orig, ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if orig.Err() != nil {
		t.Fatal("original context must remain alive")
	}
	if mock.submitCalls != 0 {
		t.Fatalf("lost parent lease must not submit, calls=%d", mock.submitCalls)
	}
	if res.Status != StepWaitingExternal || res.WaitingCondition != "parent_busy" {
		t.Fatalf("got %s/%s want waiting_external/parent_busy", res.Status, res.WaitingCondition)
	}
	if getString(ec.State, "job_id") == "" || !getBool(ec.State, "transcode_reconcile") {
		t.Fatal("identity checkpoint must be preserved when the guard is lost")
	}
}

// If Cancel confirms during the uncertain GET, the 404 must not resubmit and the
// job identity is unchanged.
func TestUncertain404AfterConcurrentCancelNoResubmit(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "edge-parent", StatusWaitingExternal, map[string]any{})
	state := submitStepState()
	state["job_id"] = "job-edge"
	state["transcode_reconcile"] = true
	seedActionInstance(t, st, "edge-child", "transcode_media", StatusWaitingExternal, 3, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "edge-parent"}, state)

	eA := NewEngine(EngineDeps{Store: st})
	eB := NewEngine(EngineDeps{Store: st})
	var cancelOnce sync.Once
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			cancelOnce.Do(func() { _, _ = eB.Cancel(context.Background(), "edge-parent", "edge cancel") })
			return transcode.JobStatus{}, &transcode.HTTPError{Method: "GET", URL: "/v1/jobs/" + jobID, StatusCode: 404, Message: "not found"}
		},
	}
	eA.deps.Transcode = mock

	ec := childECFor("edge-child", "edge-parent", state)
	res, err := eA.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mock.statusCalls != 1 || mock.submitCalls != 0 {
		t.Fatalf("want 1 status and 0 submits; got status=%d submit=%d", mock.statusCalls, mock.submitCalls)
	}
	if res.Status != StepFailed {
		t.Fatalf("got %s want failed (parent cancelled during GET)", res.Status)
	}
	if getString(ec.State, "job_id") != "job-edge" {
		t.Fatalf("job identity changed: %q", getString(ec.State, "job_id"))
	}
}

// A parent waiting for a user decision blocks a pending sibling reversibly and
// never auto-resolves the waiting sibling.
func TestWaitingDecisionParentBlocksPendingSibling(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "wd-parent", StatusWaitingDecision, map[string]any{})
	seedActionInstance(t, st, "wd-wait", "transcode_media", StatusWaitingDecision, 5, "",
		map[string]any{"path": "/a.mkv", "parent_action_id": "wd-parent"},
		map[string]any{"candidate_path": "/Volumes/media/.navigatorr-candidates/a.mkv"})
	state := submitStepState()
	seedActionInstance(t, st, "wd-pending", "transcode_media", StatusWaitingExternal, 3, "",
		map[string]any{"path": "/b.mkv", "parent_action_id": "wd-parent"}, state)
	mock := &mockTranscodeExecutor{}
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	res, err := e.stepTranscodeSubmit(context.Background(), childECFor("wd-pending", "wd-parent", state))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != StepWaitingExternal || res.WaitingCondition != "parent_waiting_decision" {
		t.Fatalf("got %s/%s want waiting_external/parent_waiting_decision", res.Status, res.WaitingCondition)
	}
	if mock.submitCalls != 0 {
		t.Fatalf("waiting-decision parent must not submit, calls=%d", mock.submitCalls)
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	wait, _ := st.GetActionInstance("wd-wait")
	if wait.Status != StatusWaitingDecision {
		t.Fatalf("waiting_decision sibling was auto-resolved to %s", wait.Status)
	}
	if mock.submitCalls != 0 {
		t.Fatal("reconciler must not submit for a waiting_decision parent")
	}
}

// Retry from a confirmed terminal failure under a cancelled parent must keep the
// identity and counters and send nothing.
func TestRetryUnderCancelledParentPreservesIdentity(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "retry-parent", StatusCancelled, map[string]any{})
	state := submitStepState()
	state["job_id"] = "job-original"
	state["transcode_submitted"] = true
	state["manual_retry_count"] = 0
	seedActionInstance(t, st, "retry-child", "transcode_media", StatusFailed, 4, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "retry-parent"}, state)
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusFailed, Error: "killed", FailureClassification: "runner_killed"}, nil
		},
	}
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	if _, err := e.Retry(context.Background(), "retry-child"); err == nil {
		t.Fatal("retry under a cancelled parent must be refused")
	}
	child, err := st.GetActionInstance("retry-child")
	if err != nil {
		t.Fatal(err)
	}
	cec := parseExecutionContext(child, e)
	if getString(cec.State, "job_id") != "job-original" {
		t.Fatalf("identity changed on blocked retry: %q", getString(cec.State, "job_id"))
	}
	if getInt(cec.State, "manual_retry_count") != 0 {
		t.Fatal("retry counter changed on blocked retry")
	}
	if mock.submitCalls != 0 {
		t.Fatalf("blocked retry must not submit, calls=%d", mock.submitCalls)
	}
}

// A minimal real-steps flow keeps wait_benchmark -> submit_transcode: a benchmark
// already accepted reaches submit_transcode under pause without launching the
// encode, and unpausing the parent lets the encode proceed.
func TestBenchmarkAcceptedAdvancesToEncodeBlockedByPause(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "flow-parent", StatusRunning, map[string]any{"paused": true})
	state := submitStepState()
	state["benchmark_done"] = true
	seedActionInstance(t, st, "flow-child", "bench_then_encode", StatusWaitingExternal, 0, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "flow-parent"}, state)
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning}, nil
		},
	}
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})
	var benchmarkAdvanced bool
	e.RegisterTemplate(ActionTemplate{Name: "bench_then_encode", AutoReconcile: true, Steps: []StepDefinition{
		{Name: "wait_benchmark", Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
			benchmarkAdvanced = true
			return StepResult{Status: StepCompleted, Outputs: map[string]any{"benchmark_done": true}}, nil
		}},
		{Name: "submit_transcode", Run: e.stepTranscodeSubmit},
	}})

	res, err := e.Resume(context.Background(), "flow-child", "", nil)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !benchmarkAdvanced {
		t.Fatal("accepted benchmark did not advance to submit_transcode")
	}
	if res.Status != StatusWaitingExternal || res.WaitingCondition != "parent_paused" {
		t.Fatalf("got %s/%s want waiting_external/parent_paused", res.Status, res.WaitingCondition)
	}
	if mock.submitCalls != 0 {
		t.Fatalf("paused parent must not launch encode, calls=%d", mock.submitCalls)
	}

	// Unpause the parent durably; the next continuation admits the encode.
	parent, err := st.GetActionInstance("flow-parent")
	if err != nil {
		t.Fatal(err)
	}
	parent.StateJSON = toJSON(map[string]any{"paused": false})
	if err := st.UpdateActionInstance(*parent); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(context.Background(), "flow-child", "", nil); err != nil {
		t.Fatalf("resume after unpause: %v", err)
	}
	if mock.submitCalls != 1 {
		t.Fatalf("unpaused parent must launch exactly one encode, calls=%d", mock.submitCalls)
	}
}
