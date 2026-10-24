package action

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

func setDrain(t *testing.T, e *Engine, on bool) {
	t.Helper()
	if _, err := e.SetWorkerDrain(context.Background(), on); err != nil {
		t.Fatal(err)
	}
}

func acceptingWorker() *mockTranscodeExecutor {
	return &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			return transcode.Job{ID: req.ID}, nil
		},
	}
}

// Drain holds a standalone conversion, which a batch pause never reaches, and
// submits it once drain is off.
func TestDrainDefersStandaloneSubmitAndResumeSubmits(t *testing.T) {
	st := setupTestStore(t)
	mock := acceptingWorker()
	seedActionInstance(t, st, "solo", "transcode_media", StatusRunning, 3, "", map[string]any{"path": "/Volumes/media/ep.mkv"}, submitStepState())
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})
	setDrain(t, e, true)

	ec := childECFor("solo", "", submitStepState())
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StepWaitingExternal || res.WaitingCondition != drainWaitCondition || mock.submitCalls != 0 {
		t.Fatalf("drain must hold a standalone submit: %s/%s submits=%d", res.Status, res.WaitingCondition, mock.submitCalls)
	}
	if _, held := ec.State["job_id"]; held {
		t.Fatalf("a held submit must not take a job identity: %+v", ec.State)
	}

	setDrain(t, e, false)
	res, err = e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil || res.Status != StepCompleted || mock.submitCalls != 1 {
		t.Fatalf("resume must let the held job submit once: %s %v submits=%d", res.Status, err, mock.submitCalls)
	}
}

// Drain never touches a batch's own pause: both must be lifted for a child to
// submit, and ending the drain leaves the pause as it was.
func TestDrainDoesNotChangeOrAutoResumeAPausedBatch(t *testing.T) {
	st := setupTestStore(t)
	seedBatchParent(t, st, "parent-paused", StatusWaitingDecision, map[string]any{"paused": true})
	mock := acceptingWorker()
	seedActionInstance(t, st, "child", "transcode_media", StatusRunning, 3, "", map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "parent-paused"}, submitStepState())
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})
	ec := childECFor("child", "parent-paused", submitStepState())

	setDrain(t, e, true)
	res, _ := e.stepTranscodeSubmit(context.Background(), ec)
	if res.WaitingCondition != drainWaitCondition {
		t.Fatalf("drain is checked first: %s/%s", res.Status, res.WaitingCondition)
	}
	setDrain(t, e, false)
	res, _ = e.stepTranscodeSubmit(context.Background(), ec)
	if res.WaitingCondition != "parent_paused" || mock.submitCalls != 0 {
		t.Fatalf("the pause must still hold after drain ends: %s/%s submits=%d", res.Status, res.WaitingCondition, mock.submitCalls)
	}
	parent, err := st.GetActionInstance("parent-paused")
	if err != nil || parent.Status != StatusWaitingDecision {
		t.Fatalf("drain must not resume a paused batch: %+v %v", parent, err)
	}
}

// A job a worker already has is tracked and published as usual under drain.
func TestDrainKeepsTrackingAdmittedJobs(t *testing.T) {
	st := setupTestStore(t)
	mock := &mockTranscodeExecutor{}
	state := submitStepState()
	state["job_id"], state["transcode_submitted"] = "job-accepted", true
	seedActionInstance(t, st, "accepted", "transcode_media", StatusWaitingExternal, 4, "", map[string]any{"path": "/Volumes/media/ep.mkv"}, state)
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})
	setDrain(t, e, true)

	res, err := e.stepTranscodeSubmit(context.Background(), childECFor("accepted", "", state))
	if err != nil || res.Status != StepCompleted || res.Outputs["reused"] != true || mock.submitCalls != 0 {
		t.Fatalf("an admitted job must keep going: %+v %v submits=%d", res, err, mock.submitCalls)
	}
}

// An uncertain submit that the worker answers with 404 would resubmit; drain
// holds that resubmit, while a job the worker does have is adopted.
func TestDrainHoldsResubmitAfterUncertain404(t *testing.T) {
	st := setupTestStore(t)
	mock := &mockTranscodeExecutor{
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, &httpclient.HTTPError{Method: "GET", URL: "/v1/jobs/" + jobID, StatusCode: 404, Message: "not found"}
		},
	}
	state := submitStepState()
	state["job_id"], state["transcode_reconcile"] = "job-child", true
	seedActionInstance(t, st, "uncertain", "transcode_media", StatusWaitingExternal, 3, "", map[string]any{"path": "/Volumes/media/ep.mkv"}, state)
	e := NewEngine(EngineDeps{Store: st, Transcode: mock})
	setDrain(t, e, true)

	res, err := e.stepTranscodeSubmit(context.Background(), childECFor("uncertain", "", state))
	if err != nil || mock.submitCalls != 0 || res.WaitingCondition != drainWaitCondition {
		t.Fatalf("a 404 must not resubmit under drain: %s/%s %v submits=%d", res.Status, res.WaitingCondition, err, mock.submitCalls)
	}
}

// "Try again" on a finished encode that could not be saved is not an
// admission: it keeps working while drain is on.
func TestDrainAllowsTryAgainOnUnsavedEncode(t *testing.T) {
	w := &retryingExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{}}
	w.statusFunc = publicationStuck(true)
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Transcode: w})
	setDrain(t, e, true)
	ec := testReconcileEC("")
	ec.State["job_id"], ec.Decision = "job-existing", "recheck"
	if _, err := e.stepTranscodeWait(context.Background(), ec); err != nil {
		t.Fatal(err)
	}
	if len(w.retried) != 1 || w.submitCalls != 0 {
		t.Fatalf("Try again must reach the worker under drain: retries=%v submits=%d", w.retried, w.submitCalls)
	}
}

// A batch starts no new child while draining; running children are left
// alone, and the files go ahead once drain ends.
func TestDrainBatchStartsNoChildrenAndResumes(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			writeCandidateOutput(req.CandidatePath)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted}, nil
		},
	}
	engine, st, _, srv, _ := setupBatchTestEnv(t, mock, 1)
	defer srv.Close()
	defer st.Close()
	ctx := context.Background()
	setDrain(t, engine, true)

	res, err := engine.Run(ctx, "transcode_batch", map[string]any{"service": "sonarr", "series_id": 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusWaitingExternal || res.WaitingCondition != "queued" {
		t.Fatalf("a draining batch waits with its files queued: %s/%s (%s)", res.Status, res.WaitingCondition, res.Error)
	}
	items, _ := st.ListTranscodeBatchItems(res.ID)
	queued := 0
	for _, it := range items {
		if it.ChildActionID != "" || it.Status == "running" {
			t.Fatalf("no child may be created while draining: %+v", it)
		}
		if it.Status == "queued" {
			queued++
		}
	}
	if queued == 0 || mock.submitCalls != 0 {
		t.Fatalf("queued=%d submits=%d", queued, mock.submitCalls)
	}
	if status := engine.WorkerDrainStatus(ctx); status.Queued != queued {
		t.Fatalf("the status must count the unstarted files: %+v", status)
	}

	setDrain(t, engine, false)
	resumed, err := engine.Resume(ctx, res.ID, "", nil)
	if err != nil || resumed.Status != StatusCompleted || mock.submitCalls == 0 {
		t.Fatalf("the files must go ahead after drain: %v %+v submits=%d", err, resumed, mock.submitCalls)
	}
}

func drainJobStates(s transcode.DrainStatus) map[string]string {
	out := map[string]string{}
	for _, j := range s.Jobs {
		out[j.ActionID] = j.State
	}
	return out
}

func TestDrainStatusCountsJobsAndReportsReady(t *testing.T) {
	drainSettle = 0
	t.Cleanup(func() { drainSettle = 15 * time.Second })
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Transcode: &mockTranscodeExecutor{}})
	ctx := context.Background()

	submitted := func() map[string]any {
		s := submitStepState()
		s["job_id"], s["transcode_submitted"] = "job-x", true
		return s
	}
	running := submitted()
	running["worker"] = transcode.DefaultWorkerName
	seedActionInstance(t, st, "running", "transcode_media", StatusWaitingExternal, 4, "", map[string]any{"path": "/Volumes/media/one.mkv"}, running)
	pending := submitted()
	pending["recovery_required"] = true
	seedActionInstance(t, st, "unsaved", "transcode_media", StatusWaitingDecision, 4, "", map[string]any{"path": "/Volumes/media/two.mkv"}, pending)
	held := submitStepState()
	seedActionInstance(t, st, "held", "transcode_media", StatusWaitingExternal, 3, "", map[string]any{"path": "/Volumes/media/three.mkv"}, held)
	heldInst, _ := st.GetActionInstance("held")
	heldInst.WaitingCondition = drainWaitCondition
	if err := st.UpdateActionInstance(*heldInst); err != nil {
		t.Fatal(err)
	}
	// Past its worker job and waiting on a person's review: not an open job.
	reviewed := submitted()
	reviewed["transcode_done"] = true
	seedActionInstance(t, st, "review", "transcode_media", StatusWaitingDecision, 6, "", map[string]any{"path": "/Volumes/media/four.mkv"}, reviewed)
	// A refused submit holds no job.
	rejected := submitStepState()
	rejected["transcode_submit_rejected"] = true
	seedActionInstance(t, st, "refused", "transcode_media", StatusWaitingExternal, 3, "", map[string]any{"path": "/Volumes/media/five.mkv"}, rejected)
	seedActionInstance(t, st, "done", "transcode_media", StatusCompleted, 9, "", map[string]any{"path": "/Volumes/media/six.mkv"}, submitted())

	off := e.WorkerDrainStatus(ctx)
	if off.Enabled || off.ReadyForDeploy {
		t.Fatalf("drain off is never ready: %+v", off)
	}

	setDrain(t, e, true)
	got := e.WorkerDrainStatus(ctx)
	if got.Running != 1 || got.PublicationPending != 1 || got.Queued != 1 || got.ReadyForDeploy {
		t.Fatalf("counts: %+v", got)
	}
	states := drainJobStates(got)
	if states["running"] != "running" || states["unsaved"] != "publication_pending" || states["held"] != "queued" || len(states) != 3 {
		t.Fatalf("jobs: %+v", got.Jobs)
	}
	if got.Jobs[0].ActionID != "unsaved" || got.Jobs[0].File != "ep.mkv" {
		t.Fatalf("publication-pending jobs go first with the file name only: %+v", got.Jobs)
	}
	if len(got.Workers) != 1 || got.Workers[0].ReadyForDeploy || got.Workers[0].Running != 1 || got.Workers[0].PublicationPending != 1 {
		t.Fatalf("workers: %+v", got.Workers)
	}

	finish := func(id string) {
		inst, _ := st.GetActionInstance(id)
		inst.Status = StatusCompleted
		if err := st.UpdateActionInstance(*inst); err != nil {
			t.Fatal(err)
		}
	}
	finish("running")
	if got = e.WorkerDrainStatus(ctx); got.ReadyForDeploy || got.PublicationPending != 1 {
		t.Fatalf("an unsaved encode still blocks a deploy: %+v", got)
	}
	finish("unsaved")
	got = e.WorkerDrainStatus(ctx)
	if !got.ReadyForDeploy || got.Queued != 1 || !got.Workers[0].ReadyForDeploy {
		t.Fatalf("queued work does not block a deploy: %+v", got)
	}
}

func TestDrainIsNotReadyWhileSettling(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Transcode: &mockTranscodeExecutor{}})
	setDrain(t, e, true)
	got := e.WorkerDrainStatus(context.Background())
	if got.ReadyForDeploy || got.Workers[0].Reason == "" {
		t.Fatalf("a drain that just started may have admissions in flight: %+v", got)
	}
}

func TestDrainNotReadyWhenWorkerUnreachable(t *testing.T) {
	drainSettle = 0
	t.Cleanup(func() { drainSettle = 15 * time.Second })
	st := setupTestStore(t)
	down := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		return transcode.WorkerCapabilities{}, context.DeadlineExceeded
	}}
	e := NewEngine(EngineDeps{Store: st, Transcode: down})
	setDrain(t, e, true)
	got := e.WorkerDrainStatus(context.Background())
	if got.ReadyForDeploy || got.Workers[0].Reachable {
		t.Fatalf("an unreachable worker's jobs cannot be confirmed: %+v", got)
	}
}

// The status rides on the shared worker observation used by the UI and MCP.
func TestWorkerObservationCarriesDrain(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Transcode: &mockTranscodeExecutor{}})
	setDrain(t, e, true)
	obs := e.WorkerObservation(context.Background())
	if obs.Drain == nil || !obs.Drain.Enabled {
		t.Fatalf("observation must carry the drain: %+v", obs.Drain)
	}
}

// Drain survives a restart: a new engine over the same store still holds.
func TestDrainSurvivesEngineRestart(t *testing.T) {
	st := setupTestStore(t)
	setDrain(t, NewEngine(EngineDeps{Store: st, Transcode: acceptingWorker()}), true)
	e := NewEngine(EngineDeps{Store: st, Transcode: acceptingWorker()})
	if held, _ := e.drainHold(); !held {
		t.Fatal("a restarted server must keep draining")
	}
}

func drainPreflightFixture(t *testing.T, id string) (*Engine, *ExecutionContext, func(), []byte) {
	t.Helper()
	content := []byte("source bytes that would be encoded")
	dir := t.TempDir()
	file := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, writeProbeScript(t, dir, sdrProbeJSON), []string{dir}, []string{dir}, false)
	ec := &ExecutionContext{InstanceID: id, ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": "size"}, State: map[string]any{}}
	seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	return e, ec, func() { st.Close() }, content
}

// A file that has not begun hashing has not begun its encode: drain holds it
// before the read, nothing is counted as encoding, and it hashes and goes on
// when drain ends.
func TestDrainHoldsAFileBeforeItHashesAndResumeHashesIt(t *testing.T) {
	e, ec, closeStore, content := drainPreflightFixture(t, "drain-prehash")
	defer closeStore()
	setDrain(t, e, true)

	res, err := e.stepTranscodePreflight(context.Background(), ec)
	if err != nil || res.Status != StepWaitingExternal || res.WaitingCondition != drainWaitCondition {
		t.Fatalf("preflight must hold before the hash: %+v %v", res, err)
	}
	if _, hashed := ec.State["original_sha256"]; hashed {
		t.Fatal("a held file must not have read its source")
	}
	if _, hashed := phaseCostMap(ec)["coordinator_source_hash"]; hashed {
		t.Fatal("a held file recorded a source hash phase")
	}
	if got := classifyDrainRow(store.DrainActionRow{Status: StatusRunning}); got != "" {
		t.Fatalf("a file that is hashing is not encoding: %q", got)
	}
	if got := classifyDrainRow(store.DrainActionRow{Status: StatusWaitingExternal, WaitingCondition: drainWaitCondition}); got != "queued" {
		t.Fatalf("a held file is waiting, not encoding: %q", got)
	}

	setDrain(t, e, false)
	res, err = e.stepTranscodePreflight(context.Background(), ec)
	if err != nil || res.Status != StepCompleted {
		t.Fatalf("resume must let the file go on: %+v %v", res, err)
	}
	if want := fmt.Sprintf("%x", sha256.Sum256(content)); getString(ec.State, "original_sha256") != want {
		t.Fatalf("hash after resume %q, want %q", getString(ec.State, "original_sha256"), want)
	}
}

// Drain turned on while a file waits for a hash slot releases the wait without
// reading anything, and leaves the slots to the hashes that already run.
func TestDrainReleasesAFileWaitingForAHashSlot(t *testing.T) {
	prev := drainSlotRecheck
	drainSlotRecheck = 10 * time.Millisecond
	defer func() { drainSlotRecheck = prev }()
	e, ec, closeStore, _ := drainPreflightFixture(t, "drain-slotwait")
	defer closeStore()
	for i := 0; i < mediaHashConcurrency; i++ {
		e.hashSlots <- struct{}{}
	}
	persisted := func() string {
		inst, err := e.deps.Store.GetActionInstance(ec.InstanceID)
		if err != nil {
			t.Fatal(err)
		}
		var state map[string]any
		if err := json.Unmarshal([]byte(inst.StateJSON), &state); err != nil {
			t.Fatal(err)
		}
		progress, _ := state["work_progress"].(map[string]any)
		phase, _ := progress["phase"].(string)
		return phase
	}
	done := make(chan StepResult, 1)
	go func() {
		r, err := e.stepTranscodePreflight(context.Background(), ec)
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	for deadline := time.Now().Add(3 * time.Second); persisted() != "waiting_for_hash_slot"; {
		if time.Now().After(deadline) {
			t.Fatalf("file never waited for a hash slot: %q", persisted())
		}
		time.Sleep(time.Millisecond)
	}
	setDrain(t, e, true)
	select {
	case r := <-done:
		if r.Status != StepWaitingExternal || r.WaitingCondition != drainWaitCondition {
			t.Fatalf("a file waiting for a hash slot must be held: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not release the wait for a hash slot")
	}
	if _, ok := ec.State["work_progress"]; ok {
		t.Fatalf("a held file still reports hash progress: %+v", ec.State["work_progress"])
	}
	if _, hashed := ec.State["original_sha256"]; hashed {
		t.Fatal("a held file read its source")
	}
	if len(e.hashSlots) != mediaHashConcurrency {
		t.Fatalf("the held file must not take or free a slot it never held: %d", len(e.hashSlots))
	}
}
