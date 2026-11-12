package action

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// suspendingExecutor is a worker that can freeze its encodes.
type suspendingExecutor struct {
	*mockTranscodeExecutor
	suspend bool
	active  int // jobs the worker says it runs
	frozen  int // of those, the ones its runners report frozen
	calls   []bool
	err     error
	missing bool // answers like a worker that predates the suspend
}

func (s *suspendingExecutor) status() (httpclient.SuspendStatus, error) {
	switch {
	case s.missing:
		return httpclient.SuspendStatus{}, httpclient.ErrSuspendUnsupported
	case s.err != nil:
		return httpclient.SuspendStatus{}, s.err
	}
	return httpclient.SuspendStatus{Suspend: s.suspend, Active: s.active, Frozen: s.frozen, CountsKnown: true}, nil
}

func (s *suspendingExecutor) Suspend(ctx context.Context) (httpclient.SuspendStatus, error) {
	return s.status()
}

func (s *suspendingExecutor) SetSuspend(ctx context.Context, suspend bool) (httpclient.SuspendStatus, error) {
	s.calls = append(s.calls, suspend)
	if s.err != nil || s.missing {
		return s.status()
	}
	s.suspend = suspend
	if suspend {
		s.frozen = s.active // the runners freeze their encodes
	} else {
		s.frozen = 0
	}
	return s.status()
}

func newSuspendingM1(status string) *suspendingExecutor {
	return &suspendingExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{statusFunc: jobStatusFunc(status)}}
}

// A suspended node keeps its encode: the job stays on the node, nothing is
// cancelled or requeued by any number of sweeps, and the node reports the
// encode as frozen, with its CPU free.
func TestSuspendFreezesTheEncodeAndTheSweepLeavesItAlone(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	seedEncoding(t, e, "enc", "m1", "job-enc")

	status := pauseNode(t, e, "m1", store.WorkerPauseSuspend)
	e.sweepPausedNodes(context.Background())
	e.sweepPausedNodes(context.Background())

	if !m1.suspend || m1.cancelCalls != 0 {
		t.Fatalf("suspend=%v cancels=%d", m1.suspend, m1.cancelCalls)
	}
	state := parseExecutionContextMust(t, e, "enc").State
	if mustInstance(t, e.deps.Store, "enc").Status != StatusWaitingExternal || getString(state, "job_id") != "job-enc" || assignedWorker(state) != "m1" || getInt(state, requeueCountKey) != 0 {
		t.Fatalf("a suspended encode stays assigned and untouched: %+v", state)
	}
	status = e.NodePauseStatuses(context.Background())["m1"]
	if !status.Paused || status.Mode != store.WorkerPauseSuspend || status.Suspended != 1 || status.Running != 0 || !status.Idle {
		t.Fatalf("a frozen encode frees the CPU and is reported as suspended: %+v", status)
	}
}

// Until the worker says its encodes are frozen the node is not idle.
func TestSuspendIsNotIdleUntilTheWorkerReportsFrozen(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	seedEncoding(t, e, "enc", "m1", "job-enc")
	pauseNode(t, e, "m1", store.WorkerPauseSuspend)

	m1.frozen = 0 // a runner has not frozen its child yet
	e.syncWorkerSuspend(context.Background(), mustWorker(t, e, "m1"))
	status := e.NodePauseStatuses(context.Background())["m1"]
	if status.Idle || status.Running != 1 || status.Suspended != 0 {
		t.Fatalf("a not yet frozen encode is still running: %+v", status)
	}
}

// Resume continues the frozen encode where it was: the job is the same one on
// the same node, and the worker is told to continue.
func TestResumeContinuesASuspendedEncode(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	seedEncoding(t, e, "enc", "m1", "job-enc")
	pauseNode(t, e, "m1", store.WorkerPauseSuspend)
	pauseNode(t, e, "m1", "")
	e.sweepPausedNodes(context.Background())

	if m1.suspend || m1.cancelCalls != 0 {
		t.Fatalf("resume must continue the encode: suspend=%v cancels=%d", m1.suspend, m1.cancelCalls)
	}
	state := parseExecutionContextMust(t, e, "enc").State
	if getString(state, "job_id") != "job-enc" || assignedWorker(state) != "m1" || getInt(state, requeueCountKey) != 0 {
		t.Fatalf("the encode continues as the same job: %+v", state)
	}
	if st := e.NodePauseStatuses(context.Background())["m1"]; st.Paused || st.Suspended != 0 {
		t.Fatalf("resumed: %+v", st)
	}
}

// What had not started when the node was suspended goes back to the queue, as
// in a graceful pause; the running encode is the only thing that stays.
func TestSuspendReleasesWhatHasNotStarted(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusQueued)
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	seedEncoding(t, e, "waiting", "m1", "job-waiting")
	pauseNode(t, e, "m1", store.WorkerPauseSuspend)
	if _, err := e.requeueForPause(context.Background(), "waiting", store.WorkerPause{Worker: "m1", Mode: store.WorkerPauseSuspend}); err != nil {
		t.Fatal(err)
	}
	if m1.cancelCalls != 1 || assignedWorker(parseExecutionContextMust(t, e, "waiting").State) != "" {
		t.Fatalf("a queued job goes back to the queue: cancels=%d", m1.cancelCalls)
	}
}

// graceful -> suspend freezes what is running; suspend -> graceful lets it
// continue and finish, as a graceful pause does.
func TestSuspendAndGracefulSwitchBothWays(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	seedEncoding(t, e, "enc", "m1", "job-enc")

	pauseNode(t, e, "m1", store.WorkerPauseGraceful)
	if m1.suspend {
		t.Fatal("a graceful pause freezes nothing")
	}
	pauseNode(t, e, "m1", store.WorkerPauseSuspend)
	if !m1.suspend {
		t.Fatal("graceful -> suspend must freeze the running encode")
	}
	pauseNode(t, e, "m1", store.WorkerPauseGraceful)
	e.sweepPausedNodes(context.Background())
	if m1.suspend || m1.cancelCalls != 0 {
		t.Fatalf("suspend -> graceful continues the encode: suspend=%v cancels=%d", m1.suspend, m1.cancelCalls)
	}
	if st := e.NodePauseStatuses(context.Background())["m1"]; st.Mode != store.WorkerPauseGraceful || st.Running != 1 || st.Suspended != 0 {
		t.Fatalf("a graceful pause waits for the encode: %+v", st)
	}
}

// suspend -> immediate keeps the freeze until the cancels have emptied the
// node, so the encode does not run again for the moments before it is moved;
// then the worker is released.
func TestSuspendThenImmediateKeepsTheFreezeUntilTheNodeIsEmpty(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	seedEncoding(t, e, "enc", "m1", "job-enc")
	pauseNode(t, e, "m1", store.WorkerPauseSuspend)

	pauseNode(t, e, "m1", store.WorkerPauseImmediate)
	if !m1.suspend {
		t.Fatal("the encode must stay frozen until it is cancelled")
	}
	e.sweepPausedNodes(context.Background())
	if m1.cancelCalls != 1 || assignedWorker(parseExecutionContextMust(t, e, "enc").State) != "" {
		t.Fatalf("the frozen encode goes back to the queue: cancels=%d", m1.cancelCalls)
	}
	e.syncWorkerSuspend(context.Background(), mustWorker(t, e, "m1"))
	if m1.suspend {
		t.Fatal("with the node empty the worker is released")
	}
}

// A node that cannot freeze refuses the suspend and keeps the pause it had.
func TestSuspendIsRefusedWhenTheNodeCannotFreeze(t *testing.T) {
	cases := map[string]*suspendingExecutor{
		"worker not updated": {mockTranscodeExecutor: &mockTranscodeExecutor{}, missing: true},
		"node unreachable":   {mockTranscodeExecutor: &mockTranscodeExecutor{}, err: errors.New("connection refused")},
	}
	for name, m1 := range cases {
		t.Run(name, func(t *testing.T) {
			e := pauseEngine(t, m1, &mockTranscodeExecutor{})
			seedEncoding(t, e, "enc", "m1", "job-enc")
			pauseNode(t, e, "m1", store.WorkerPauseGraceful)
			if _, err := e.SetNodePause(context.Background(), "m1", store.WorkerPauseSuspend); !errors.Is(err, transcode.ErrSuspendUnavailable) {
				t.Fatalf("err = %v", err)
			}
			if st := e.NodePauseStatuses(context.Background())["m1"]; st.Mode != store.WorkerPauseGraceful {
				t.Fatalf("an unconfirmed suspend leaves the pause as it was: %+v", st)
			}
			e.sweepPausedNodes(context.Background())
			if m1.cancelCalls != 0 {
				t.Fatal("nothing is cancelled")
			}
		})
	}
	// A node that was not paused stays not paused.
	plain := &mockTranscodeExecutor{}
	e := pauseEngine(t, plain, &mockTranscodeExecutor{})
	if _, err := e.SetNodePause(context.Background(), "m1", store.WorkerPauseSuspend); !errors.Is(err, transcode.ErrSuspendUnavailable) {
		t.Fatalf("a node without a remote worker cannot suspend: %v", err)
	}
	if st := e.NodePauseStatuses(context.Background())["m1"]; st.Paused {
		t.Fatalf("refused suspend must not pause: %+v", st)
	}
}

// A worker that lost the suspend (restart with the state directory replaced) or
// kept one after a resume it could not hear is corrected on the next sync.
func TestSuspendSyncCorrectsADriftedWorker(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	w := mustWorker(t, e, "m1")

	m1.suspend = true // kept after a resume the server could not deliver
	e.syncWorkerSuspend(context.Background(), w)
	if m1.suspend {
		t.Fatal("a node that is not suspended must not keep a suspend")
	}
	pauseNode(t, e, "m1", store.WorkerPauseSuspend)
	m1.suspend = false // lost
	e.syncWorkerSuspend(context.Background(), w)
	if !m1.suspend {
		t.Fatal("a suspended node must get its suspend back")
	}
	// An unreadable worker is not told anything: the call count stays.
	calls := len(m1.calls)
	m1.err = errors.New("down")
	e.syncWorkerSuspend(context.Background(), w)
	if len(m1.calls) != calls {
		t.Fatal("no change is sent to a worker that cannot be read")
	}
}

// A suspended job is reported as running with its progress, not as stalled,
// failed or unreachable, and the action keeps waiting on the same job.
func TestSuspendedJobIsNotStalledOrFailed(t *testing.T) {
	m1 := &mockTranscodeExecutor{statusFunc: func(_ context.Context, jobID string) (transcode.JobStatus, error) {
		st := transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning, Progress: 41.5}
		st.Suspended = true
		return st, nil
	}}
	e := pauseEngine(t, m1, &mockTranscodeExecutor{})
	seedEncoding(t, e, "enc", "m1", "job-enc")
	ec := parseExecutionContextMust(t, e, "enc")

	res, err := e.stepTranscodeWait(context.Background(), ec)
	if err != nil || res.Status != StepWaitingExternal || res.WaitingCondition != "transcode_complete" {
		t.Fatalf("a suspended job keeps waiting: %+v %v", res, err)
	}
	if res.Outputs["worker_suspended"] != true || !strings.Contains(res.WaitingReason, "suspended") || !strings.Contains(res.WaitingReason, "41.5%") {
		t.Fatalf("the status must say suspended with the progress: %q %+v", res.WaitingReason, res.Outputs)
	}
	if m1.cancelCalls != 0 {
		t.Fatal("polling a suspended job cancels nothing")
	}
}

func mustWorker(t *testing.T, e *Engine, name string) transcode.NamedWorker {
	t.Helper()
	w, ok := e.deps.Workers.Get(name)
	if !ok {
		t.Fatalf("no worker %s", name)
	}
	return w
}
