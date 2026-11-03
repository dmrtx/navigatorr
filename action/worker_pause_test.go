package action

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

func pauseEngine(t *testing.T, m1, m4 transcode.Executor) *Engine {
	t.Helper()
	return NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: twoWorkers(t, m1, m4, 0), DrainSettle: -1})
}

func pauseNode(t *testing.T, e *Engine, node, mode string) transcode.NodePauseStatus {
	t.Helper()
	status, err := e.SetNodePause(context.Background(), node, mode)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

// seedEncoding seeds a conversion sent to a node and waiting for its encode.
func seedEncoding(t *testing.T, e *Engine, id, node, jobID string) {
	t.Helper()
	tmpl, _ := e.GetTemplate("transcode_media")
	seedActionInstance(t, e.deps.Store, id, "transcode_media", StatusWaitingExternal, actionStepIndex(tmpl, "wait_transcode"), "",
		map[string]any{"path": "/Volumes/media/" + id + ".mkv"},
		map[string]any{workerStateKey: node, "job_id": jobID, "transcode_submitted": true, transcodeQueueOrderKey: float64(7)})
}

func jobStatusFunc(status string) func(context.Context, string) (transcode.JobStatus, error) {
	return func(_ context.Context, jobID string) (transcode.JobStatus, error) {
		return transcode.JobStatus{ID: jobID, Status: status}, nil
	}
}

// A graceful pause lets the encode finish: the job stays on its node, nothing
// is cancelled, and the node is reported idle only when the job is done.
func TestGracefulPauseLeavesTheRunningEncodeAlone(t *testing.T) {
	m1 := &mockTranscodeExecutor{statusFunc: jobStatusFunc(transcode.StatusRunning)}
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	seedEncoding(t, e, "enc", "m1", "job-enc")

	pauseNode(t, e, "m1", store.WorkerPauseGraceful)
	e.sweepPausedNodes(context.Background())

	inst := mustInstance(t, e.deps.Store, "enc")
	if m1.cancelCalls != 0 || inst.Status != StatusWaitingExternal || getString(parseExecutionContextMust(t, e, "enc").State, "job_id") != "job-enc" {
		t.Fatalf("a graceful pause must not touch a running encode: cancels=%d %s", m1.cancelCalls, inst.Status)
	}
	st := e.NodePauseStatuses(context.Background())["m1"]
	if !st.Paused || st.Mode != store.WorkerPauseGraceful || st.Idle || st.Running != 1 {
		t.Fatalf("the node is draining, not idle: %+v", st)
	}
	if other := e.NodePauseStatuses(context.Background())["m4"]; other.Paused {
		t.Fatalf("the other node is not paused: %+v", other)
	}
}

// An immediate pause cancels the encode on the node and puts the file back in
// the transcode queue: the action stays alive, is not cancelled, forgets the
// node and the job, and keeps its place.
func TestImmediatePauseSendsTheEncodeBackToTheQueue(t *testing.T) {
	m1 := &mockTranscodeExecutor{statusFunc: jobStatusFunc(transcode.StatusRunning)}
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	seedEncoding(t, e, "enc", "m1", "job-enc")

	pauseNode(t, e, "m1", store.WorkerPauseImmediate)
	e.sweepPausedNodes(context.Background())

	if m1.cancelCalls != 1 {
		t.Fatalf("the encode must be cancelled on its node once: %d", m1.cancelCalls)
	}
	inst := mustInstance(t, e.deps.Store, "enc")
	if inst.Status != StatusWaitingExternal || inst.CurrentStep != 0 || inst.WaitingCondition != "worker_busy" || inst.ErrorJSON != "" {
		t.Fatalf("the file must wait in the queue, not end: %+v", inst)
	}
	state := parseExecutionContextMust(t, e, "enc").State
	if assignedWorker(state) != "" || holdsWorkerJob(state) {
		t.Fatalf("the requeued file must hold no node and no job: %+v", state)
	}
	if getInt(state, transcodeQueueOrderKey) != 7 || getInt(state, requeueRetryCountKey) != 1 || getString(state, "previous_job_id") != "job-enc" || getString(state, requeuedFromKey) != "m1" {
		t.Fatalf("the place in the queue and the attempt record must be kept: %+v", state)
	}
	if st := e.NodePauseStatuses(context.Background())["m1"]; !st.Idle || st.Running != 0 {
		t.Fatalf("an immediate pause leaves the node idle: %+v", st)
	}

	// A second pass changes nothing.
	e.sweepPausedNodes(context.Background())
	if m1.cancelCalls != 1 {
		t.Fatalf("the sweep must be idempotent: %d cancels", m1.cancelCalls)
	}
}

// A graceful pause does release a job the node has accepted and not started.
func TestGracefulPauseReleasesAJobThatHasNotStarted(t *testing.T) {
	m1 := &mockTranscodeExecutor{statusFunc: jobStatusFunc(transcode.StatusQueued)}
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	seedEncoding(t, e, "waiting", "m1", "job-waiting")
	if _, err := e.requeueForPause(context.Background(), "waiting", store.WorkerPause{Worker: "m1", Mode: store.WorkerPauseGraceful}); err != nil {
		t.Fatal(err)
	}
	if m1.cancelCalls != 1 || assignedWorker(parseExecutionContextMust(t, e, "waiting").State) != "" {
		t.Fatalf("a queued job goes back to the queue: cancels=%d", m1.cancelCalls)
	}
}

// A node that cannot confirm the cancel keeps the job: the file is not moved
// while the encode may still be running there.
func TestImmediatePauseKeepsTheJobWhenTheCancelFails(t *testing.T) {
	m1 := &mockTranscodeExecutor{statusFunc: jobStatusFunc(transcode.StatusRunning), cancelFunc: func(context.Context, string) error { return errors.New("worker refused") }}
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	seedEncoding(t, e, "enc", "m1", "job-enc")
	pauseNode(t, e, "m1", store.WorkerPauseImmediate)
	e.sweepPausedNodes(context.Background())

	state := parseExecutionContextMust(t, e, "enc").State
	if getString(state, "job_id") != "job-enc" || assignedWorker(state) != "m1" {
		t.Fatalf("an unconfirmed cancel must keep the job: %+v", state)
	}
	if st := e.NodePauseStatuses(context.Background())["m1"]; st.Idle || st.Running != 1 {
		t.Fatalf("the node is not idle: %+v", st)
	}
}

// A job already past its encode (finished, waiting to be saved) is left alone
// even by an immediate pause.
func TestImmediatePauseLeavesAFinishedEncodeAlone(t *testing.T) {
	m1 := &mockTranscodeExecutor{statusFunc: jobStatusFunc(transcode.StatusCompleted)}
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	seedEncoding(t, e, "done", "m1", "job-done")
	pauseNode(t, e, "m1", store.WorkerPauseImmediate)
	if moved, err := e.requeueForPause(context.Background(), "done", store.WorkerPause{Worker: "m1", Mode: store.WorkerPauseImmediate}); err != nil || moved {
		t.Fatalf("a finished encode must stay: moved=%v %v", moved, err)
	}
	if m1.cancelCalls != 0 {
		t.Fatal("a finished encode must not be cancelled")
	}
}

// A file assigned to the node that was never sent goes back to the queue.
func TestPauseReleasesAFileAssignedButNotSent(t *testing.T) {
	m1 := &mockTranscodeExecutor{}
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	seedActionInstance(t, e.deps.Store, "unsent", "transcode_media", StatusWaitingExternal, 1, "", map[string]any{"path": "/Volumes/media/u.mkv"},
		map[string]any{workerStateKey: "m1", transcodeQueueOrderKey: float64(3)})
	pauseNode(t, e, "m1", store.WorkerPauseGraceful)
	e.sweepPausedNodes(context.Background())

	state := parseExecutionContextMust(t, e, "unsent").State
	if assignedWorker(state) != "" || getInt(state, transcodeQueueOrderKey) != 3 || m1.cancelCalls != 0 || getInt(state, requeueRetryCountKey) != 0 {
		t.Fatalf("an unsent file only loses its node: cancels=%d %+v", m1.cancelCalls, state)
	}
}

// A paused node has no room: the queue sends the next file to the other node,
// and the node takes files again once resumed.
func TestPausedNodeGetsNoNewFileAndResumeGivesItBack(t *testing.T) {
	e := pauseEngine(t, poolWorker(2), poolWorker(2))
	pauseNode(t, e, "m1", store.WorkerPauseGraceful)
	for i, id := range []string{"a", "b"} {
		if name, _, waiting := assignNew(t, e, id, "transcode_media"); waiting || name != "m4" {
			t.Fatalf("file %d went to %q (waiting %v) while m1 is paused", i, name, waiting)
		}
	}
	pauseNode(t, e, "m1", "")
	if name, _, waiting := assignNew(t, e, "c", "transcode_media"); waiting || name != "m1" {
		t.Fatalf("a resumed node must take files again: %q waiting=%v", name, waiting)
	}
}

// With every node paused the file waits, and nothing is sent.
func TestEveryNodePausedMakesFilesWait(t *testing.T) {
	e := pauseEngine(t, poolWorker(2), poolWorker(2))
	pauseNode(t, e, "m1", store.WorkerPauseGraceful)
	pauseNode(t, e, "m4", store.WorkerPauseImmediate)
	name, res, waiting := assignNew(t, e, "x", "transcode_media")
	if !waiting || name != "" || res.WaitingCondition != "worker_busy" {
		t.Fatalf("with every node paused the file waits: %q %+v", name, res)
	}
}

// Admission is gated for a file already on a paused node, and resumes with it.
func TestPausedNodeHoldsSubmitAndResumeSubmits(t *testing.T) {
	m1 := acceptingWorker()
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	state := submitStepState()
	state[workerStateKey] = "m1"
	seedActionInstance(t, e.deps.Store, "solo", "transcode_media", StatusRunning, 3, "", map[string]any{"path": "/Volumes/media/ep.mkv"}, state)
	pauseNode(t, e, "m1", store.WorkerPauseGraceful)

	ec := childECFor("solo", "", state)
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil || res.Status != StepWaitingExternal || res.WaitingCondition != pauseWaitCondition || m1.submitCalls != 0 {
		t.Fatalf("a paused node must not receive a submit: %+v %v submits=%d", res, err, m1.submitCalls)
	}
	if _, held := ec.State["job_id"]; held {
		t.Fatalf("a held submit takes no job identity: %+v", ec.State)
	}
	pauseNode(t, e, "m1", "")
	if res, err = e.stepTranscodeSubmit(context.Background(), ec); err != nil || res.Status != StepCompleted || m1.submitCalls != 1 {
		t.Fatalf("resume must let the file submit: %+v %v submits=%d", res, err, m1.submitCalls)
	}
}

// The requeued attempt gets its own job identity, since the node still knows
// the cancelled one.
func TestRequeuedFileSubmitsUnderANewJobIdentity(t *testing.T) {
	m4 := acceptingWorker()
	var sent []string
	m4.submitFunc = func(_ context.Context, req transcode.Request) (transcode.Job, error) {
		sent = append(sent, req.ID)
		return transcode.Job{ID: req.ID}, nil
	}
	e := pauseEngine(t, &mockTranscodeExecutor{statusFunc: jobStatusFunc(transcode.StatusRunning)}, m4)
	seedEncoding(t, e, "enc", "m1", "job-enc")
	pauseNode(t, e, "m1", store.WorkerPauseImmediate)
	e.sweepPausedNodes(context.Background())

	state := parseExecutionContextMust(t, e, "enc").State
	state[workerStateKey] = "m4"
	for k, v := range submitStepState() {
		if _, ok := state[k]; !ok {
			state[k] = v
		}
	}
	if _, err := e.stepTranscodeSubmit(context.Background(), childECFor("enc", "", state)); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] == "job-enc" || !strings.HasPrefix(sent[0], "job-enc-retry-") {
		t.Fatalf("the new attempt must not reuse the cancelled job's identity: %v", sent)
	}
}

// The node's pause is durable: a restarted engine still holds it.
func TestNodePauseSurvivesAnEngineRestart(t *testing.T) {
	e := pauseEngine(t, poolWorker(2), poolWorker(2))
	pauseNode(t, e, "m1", store.WorkerPauseImmediate)
	restarted := NewEngine(EngineDeps{Store: e.deps.Store, Config: &config.Config{}, Workers: twoWorkers(t, poolWorker(2), poolWorker(2), 0), DrainSettle: -1})
	if !restarted.workerPaused("m1") || restarted.workerPaused("m4") {
		t.Fatal("the pause must be kept across a restart, for that node only")
	}
}

func TestPausingAnUnknownNodeIsRefused(t *testing.T) {
	e := pauseEngine(t, poolWorker(2), poolWorker(2))
	_, err := e.SetNodePause(context.Background(), "m9", store.WorkerPauseGraceful)
	var unknown *transcode.WorkerNotConfiguredError
	if !errors.As(err, &unknown) {
		t.Fatalf("an unknown node must be refused: %v", err)
	}
	if pauses, _ := e.deps.Store.ListWorkerPauses(); len(pauses) != 0 {
		t.Fatalf("a refused pause must save nothing: %+v", pauses)
	}
}

// A node's pause holds that node's own queue and no other, and resume lifts
// it; an unrelated drain still holds every node.
func TestNodePauseHoldsOnlyThatNodesQueue(t *testing.T) {
	m1 := &holdingExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{}}
	m4 := &holdingExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{}}
	e := pauseEngine(t, m1, m4)

	pauseNode(t, e, "m1", store.WorkerPauseGraceful)
	e.syncWorkerHolds(context.Background())
	if !m1.hold || m4.hold {
		t.Fatalf("only the paused node holds its queue: m1=%v m4=%v", m1.hold, m4.hold)
	}
	setDrain(t, e, true)
	e.syncWorkerHolds(context.Background())
	if !m1.hold || !m4.hold {
		t.Fatalf("drain still holds every node: m1=%v m4=%v", m1.hold, m4.hold)
	}
	setDrain(t, e, false)
	e.syncWorkerHolds(context.Background())
	if !m1.hold || m4.hold {
		t.Fatalf("ending drain keeps the paused node held: m1=%v m4=%v", m1.hold, m4.hold)
	}
	pauseNode(t, e, "m1", "")
	if m1.hold {
		t.Fatalf("resume must lift the node's hold: %v", m1.calls)
	}
}
