package action

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

func TestCancelTranscodeMediaStopsRemoteJob(t *testing.T) {
	st := setupTestStore(t)
	state := submitStepState()
	state["job_id"] = "job-active"
	state["transcode_submitted"] = true
	seedActionInstance(t, st, "child-active", "transcode_media", StatusWaitingExternal, 4, "",
		map[string]any{"path": "/Volumes/media/ep.mkv"}, state)

	var gotJob string
	mock := &mockTranscodeExecutor{
		cancelFunc: func(ctx context.Context, jobID string) error {
			gotJob = jobID
			return nil
		},
	}
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	res, err := e.Cancel(context.Background(), "child-active", "stop this encode")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if res.Status != StatusCancelled {
		t.Fatalf("status=%s want cancelled", res.Status)
	}
	if gotJob != "job-active" {
		t.Fatalf("cancelled worker job %q want job-active", gotJob)
	}
	if atomic.LoadInt32(&mock.cancelCalls) != 1 {
		t.Fatalf("cancel calls=%d want 1", mock.cancelCalls)
	}
}

func TestCancelTranscodeMediaSurfacesRemoteCancelFailure(t *testing.T) {
	st := setupTestStore(t)
	state := submitStepState()
	state["job_id"] = "job-stuck"
	state["transcode_submitted"] = true
	seedActionInstance(t, st, "child-stuck", "transcode_media", StatusWaitingExternal, 4, "",
		map[string]any{"path": "/Volumes/media/ep.mkv"}, state)

	mock := &mockTranscodeExecutor{
		cancelFunc: func(ctx context.Context, jobID string) error {
			return errors.New("worker rejected cancel")
		},
	}
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	res, err := e.Cancel(context.Background(), "child-stuck", "stop")
	if err == nil {
		t.Fatal("remote cancel failure must be surfaced")
	}
	if res == nil || res.Status != StatusCancelled {
		t.Fatalf("local cancellation must remain durable, got %+v", res)
	}
	if !strings.Contains(err.Error(), "remote cancellation was not fully confirmed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCancelUncertainReconcilesBeforeRetry(t *testing.T) {
	st := setupTestStore(t)
	state := submitStepState()
	state["job_id"] = "job-uncertain"
	state["transcode_submitted"] = true
	seedActionInstance(t, st, "child-uncertain", "transcode_media", StatusWaitingExternal, 4, "",
		map[string]any{"path": "/Volumes/media/ep.mkv"}, state)

	mock := &mockTranscodeExecutor{}
	mock.cancelFunc = func(ctx context.Context, jobID string) error {
		if atomic.LoadInt32(&mock.cancelCalls) == 1 {
			return &transcode.UncertainError{Op: "cancel", JobID: jobID, Err: errors.New("timeout")}
		}
		return nil
	}
	mock.statusFunc = func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
		return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning}, nil
	}
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	if _, err := e.Cancel(context.Background(), "child-uncertain", "stop"); err != nil {
		t.Fatalf("cancel should reconcile and retry safely: %v", err)
	}
	if atomic.LoadInt32(&mock.cancelCalls) != 2 {
		t.Fatalf("cancel calls=%d want 2", mock.cancelCalls)
	}
	if atomic.LoadInt32(&mock.statusCalls) != 1 {
		t.Fatalf("status calls=%d want 1", mock.statusCalls)
	}
}

func TestCancelTranscodeMediaRecoversJobIDFromBatchItem(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "batch-fallback", StatusWaitingExternal, map[string]any{})
	seedActionInstance(t, st, "child-fallback", "transcode_media", StatusWaitingExternal, 4, "",
		map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "batch-fallback"},
		map[string]any{"transcode_submitted": true})
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{
		BatchID: "batch-fallback", ItemKey: "epfile-1", FilePath: "/Volumes/media/ep.mkv",
		Status: "running", ChildActionID: "child-fallback", JobID: "job-from-batch-item",
	}); err != nil {
		t.Fatal(err)
	}

	var gotJob string
	mock := &mockTranscodeExecutor{
		cancelFunc: func(ctx context.Context, jobID string) error {
			gotJob = jobID
			return nil
		},
	}
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	if _, err := e.Cancel(context.Background(), "child-fallback", "stop"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if gotJob != "job-from-batch-item" {
		t.Fatalf("worker job=%q want recovered batch job id", gotJob)
	}
}

func TestCancelBatchCascadesToActiveChildren(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "batch-active", StatusWaitingExternal, map[string]any{})

	for _, tc := range []struct {
		child string
		job   string
		key   string
	}{
		{child: "child-one", job: "job-one", key: "epfile-1"},
		{child: "child-two", job: "job-two", key: "epfile-2"},
	} {
		state := submitStepState()
		state["job_id"] = tc.job
		state["transcode_submitted"] = true
		seedActionInstance(t, st, tc.child, "transcode_media", StatusWaitingExternal, 4, "",
			map[string]any{"path": "/Volumes/media/" + tc.key + ".mkv", "parent_action_id": "batch-active"}, state)
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{
			BatchID: "batch-active", ItemKey: tc.key, FilePath: "/Volumes/media/" + tc.key + ".mkv",
			Status: "running", ChildActionID: tc.child, JobID: tc.job,
		}); err != nil {
			t.Fatal(err)
		}
	}

	cancelled := make(map[string]int)
	mock := &mockTranscodeExecutor{
		cancelFunc: func(ctx context.Context, jobID string) error {
			cancelled[jobID]++
			return nil
		},
	}
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})

	res, err := e.Cancel(context.Background(), "batch-active", "switching strategy")
	if err != nil {
		t.Fatalf("batch cancel: %v", err)
	}
	if res.Status != StatusCancelled {
		t.Fatalf("parent status=%s want cancelled", res.Status)
	}
	if cancelled["job-one"] != 1 || cancelled["job-two"] != 1 {
		t.Fatalf("remote cancels=%v want one per active child", cancelled)
	}
	for _, childID := range []string{"child-one", "child-two"} {
		child, err := st.GetActionInstance(childID)
		if err != nil {
			t.Fatal(err)
		}
		if child.Status != StatusCancelled {
			t.Fatalf("%s status=%s want cancelled", childID, child.Status)
		}
	}
}

func TestBatchCancelDecisionStopsAlreadyAdmittedWorkerJob(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning, Progress: 50}, nil
		},
	}
	engine, st, _, srv, _ := setupBatchTestEnv(t, mock, 1)
	defer srv.Close()
	defer st.Close()

	ctx := context.Background()
	res, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service":   "sonarr",
		"series_id": 10,
		"season":    1,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != StatusWaitingExternal {
		t.Fatalf("expected active batch to wait, got %s", res.Status)
	}
	if atomic.LoadInt32(&mock.submitCalls) != 1 {
		t.Fatalf("submit calls=%d want 1", mock.submitCalls)
	}

	cancelled, err := engine.Resume(ctx, res.ID, "cancel", nil)
	if err != nil {
		t.Fatalf("cancel decision: %v", err)
	}
	if cancelled.Status != StatusCompleted {
		t.Fatalf("cancel decision status=%s error=%s", cancelled.Status, cancelled.Error)
	}
	if atomic.LoadInt32(&mock.cancelCalls) != 1 {
		t.Fatalf("remote cancel calls=%d want 1", mock.cancelCalls)
	}
	items, err := st.ListTranscodeBatchItems(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "failed" || !strings.Contains(items[0].Error, "cancelled") {
		t.Fatalf("unexpected cancelled batch items: %+v", items)
	}
}
