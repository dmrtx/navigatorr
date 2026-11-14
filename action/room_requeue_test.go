package action

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// failedPoll is the wait step polling a job its worker reports failed.
func failedPoll(t *testing.T, state map[string]any, st transcode.JobStatus) (StepResult, *ExecutionContext) {
	t.Helper()
	w := newRetainedExecutor()
	e := NewEngine(EngineDeps{Transcode: w})
	ec := testReconcileEC("")
	ec.ActionName = "transcode_media"
	ec.State["job_id"] = "job-existing"
	for k, v := range state {
		ec.State[k] = v
	}
	st.ID = "job-existing"
	st.Status = transcode.StatusFailed
	w.statusFunc = func(context.Context, string) (transcode.JobStatus, error) { return st, nil }
	res, err := e.stepTranscodeWait(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	return res, ec
}

// An encode stopped because the node's free disk fell under its hard floor is
// not a failure of the job: it goes back to the queue.
func TestADiskFloorStopPutsTheConversionBackInTheQueue(t *testing.T) {
	res, ec := failedPoll(t, nil, transcode.JobStatus{ReasonCode: diskFloorReason})
	if res.Status != StepWaitingExternal || !res.Requeue {
		t.Fatalf("result = %+v, want a requeue", res)
	}
	if getString(ec.State, requeueWhyKey) == "" || getString(ec.State, roomRefusedByKey) == "" {
		t.Fatalf("state %v: the requeue must say why and which node stopped it", ec.State)
	}
}

// A runner that died with its worker (restart, crash) lost its encode, not the
// file: the conversion is requeued, up to a bound, then fails with the reason so
// a job that kills its runner cannot loop.
func TestARunnerKilledByARestartIsRequeuedUpToThreeTimesInARow(t *testing.T) {
	killed := transcode.JobStatus{FailureClassification: runnerKilledClass, ReasonCode: "runner_killed", Error: "process terminated unexpectedly"}
	for i := range maxRunnerKilled - 1 {
		res, ec := failedPoll(t, map[string]any{runnerKilledKey: i}, killed)
		if res.Status != StepWaitingExternal || !res.Requeue || getInt(ec.State, runnerKilledKey) != i+1 {
			t.Fatalf("kill %d: result %+v count %d, want a requeue", i+1, res, getInt(ec.State, runnerKilledKey))
		}
	}
	res, _ := failedPoll(t, map[string]any{runnerKilledKey: maxRunnerKilled - 1}, killed)
	if res.Requeue || res.Status != StepFailed {
		t.Fatalf("kill %d: result %+v, want it to fail with the worker's reason", maxRunnerKilled, res)
	}
}

// clearingExecutor is a node's executor that records the cache-clearing hold.
type clearingExecutor struct {
	*mockTranscodeExecutor
	cleared int
}

func (c *clearingExecutor) SetHoldClearCache(context.Context) (httpclient.HoldStatus, error) {
	c.cleared++
	return httpclient.HoldStatus{Hold: true}, nil
}

func clearingEngine(t *testing.T) (*Engine, *clearingExecutor, *clearingExecutor) {
	t.Helper()
	a := &clearingExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{}}
	b := &clearingExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{}}
	return pauseEngine(t, a, b), a, b
}

func sweepCaches(t *testing.T, e *Engine) {
	t.Helper()
	pauses, err := e.workerPauses()
	if err != nil {
		t.Fatal(err)
	}
	e.clearPausedCaches(context.Background(), pauses)
}

// An immediate pause clears the node's deletable source cache, once per pause;
// a graceful one leaves it alone.
func TestAnImmediatePauseClearsTheNodesSourceCacheOnceAndAGracefulOneDoesNot(t *testing.T) {
	e, immediate, graceful := clearingEngine(t)
	pauseNode(t, e, "m1", store.WorkerPauseImmediate)
	pauseNode(t, e, "m4", store.WorkerPauseGraceful)
	sweepCaches(t, e)
	sweepCaches(t, e)
	sweepCaches(t, e)
	if immediate.cleared != 1 || graceful.cleared != 0 {
		t.Fatalf("immediate cleared %d, graceful cleared %d, want 1 and 0 however many sweeps run", immediate.cleared, graceful.cleared)
	}
	// Resumed and paused again is a new pause: it clears again.
	pauseNode(t, e, "m1", "")
	pauseNode(t, e, "m1", store.WorkerPauseImmediate)
	sweepCaches(t, e)
	if immediate.cleared != 2 {
		t.Fatalf("a new immediate pause cleared %d times in all, want 2", immediate.cleared)
	}
}

// A resume that lands between the sweep reading the pause and clearing the
// cache wins: the node is not held again.
func TestAResumeRacingWithTheCacheClearIsNotHeldAgain(t *testing.T) {
	e, immediate, _ := clearingEngine(t)
	pauseNode(t, e, "m1", store.WorkerPauseImmediate)
	stale, err := e.workerPauses() // what the sweep read
	if err != nil {
		t.Fatal(err)
	}
	pauseNode(t, e, "m1", "") // the resume
	e.clearPausedCaches(context.Background(), stale)
	if immediate.cleared != 0 {
		t.Fatalf("cleared %d times after the resume, want none", immediate.cleared)
	}
}

// A worker that predates clear_cache answers 400; the hold falls back to a
// plain one and says so once.
func TestAWorkerThatRefusesClearCacheIsHeldPlainly(t *testing.T) {
	var puts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		puts = append(puts, string(body))
		if strings.Contains(string(body), "clear_cache") {
			http.Error(w, `{"error":"unknown field"}`, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"hold":true}`))
	}))
	defer srv.Close()
	exec, err := httpclient.NewHTTPExecutor(httpclient.HTTPConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	st, err := exec.SetHoldClearCache(context.Background())
	if err != nil || !st.Hold {
		t.Fatalf("hold = %+v (%v), want a plain hold", st, err)
	}
	if len(puts) != 2 || strings.Contains(puts[1], "clear_cache") {
		t.Fatalf("requests %v, want the clear_cache one then a plain hold", puts)
	}
}

// The plan's reserve is stored next to it, so the conversion waits in the
// queue with its exact figure: a size goal's byte budget, not half the source.
func TestTheResolvedPlanStoresItsExactReserve(t *testing.T) {
	ec := testReconcileEC("")
	ec.State[SubmittedSourceBytesKey] = int64(1000)
	plan := &transcode.Plan{SizePolicy: &transcode.SizePolicy{MinSavingsPercent: 10, MaxCandidateBytes: 120}}
	recordRoomReserve(ec, plan)
	if got := getInt64(ec.State, roomReserveKey); got != 120 {
		t.Fatalf("reserve = %d, want the size goal's budget 120", got)
	}
	recordRoomReserve(ec, &transcode.Plan{})
	if got := getInt64(ec.State, roomReserveKey); got != 500 {
		t.Fatalf("reserve = %d, want half the source", got)
	}
}
