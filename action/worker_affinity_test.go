package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// twoWorkers is m1 then m4, with m4 running podcast work.
func twoWorkers(t *testing.T, m1, m4 transcode.Executor, m1Max int) *transcode.WorkerSet {
	t.Helper()
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{
		{Name: "m1", Executor: m1, MaxParallelJobs: m1Max},
		{Name: "m4", Executor: m4, Podcast: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func poolWorker(ceiling int) *slotReportingExecutor {
	return &slotReportingExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{}, slots: sharedSlots(ceiling)}
}

// assignNew seeds a running conversion and assigns it a worker.
func assignNew(t *testing.T, e *Engine, id, action string) (string, StepResult, bool) {
	t.Helper()
	seedActionInstance(t, e.deps.Store, id, action, StatusRunning, 0, "", map[string]any{}, map[string]any{})
	ec := parseExecutionContextMust(t, e, id)
	res, waiting := e.ensureWorkerAssigned(context.Background(), ec)
	return assignedWorker(parseExecutionContextMust(t, e, id).State), res, waiting
}

func TestWorkerAssignmentFillsFreeSlotsInConfigOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		m1Max int
		want  []string
	}{
		{"equal pools alternate, m1 first on a tie", 0, []string{"m1", "m4", "m1", "m4"}},
		// m1 capped at one job: never past it.
		{"a worker cap is a hard cap", 1, []string{"m4", "m1", "m4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: twoWorkers(t, poolWorker(2), poolWorker(2), tc.m1Max)})
			var got []string
			for i := range tc.want {
				name, _, waiting := assignNew(t, e, fmt.Sprintf("conv-%d", i), "transcode_media")
				if waiting {
					t.Fatalf("conv-%d waits with two ready workers", i)
				}
				got = append(got, name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("assigned %v, want %v", got, tc.want)
			}
			// Every slot is taken: no worker queues a job of its own.
			if name, res, waiting := assignNew(t, e, "conv-full", "transcode_media"); !waiting || name != "" || res.WaitingCondition != "worker_busy" {
				t.Fatalf("with every slot taken: assigned %q, %+v", name, res)
			}
		})
	}
}

func TestWorkersAtTheirCapMakeTheJobWait(t *testing.T) {
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{
		{Name: "m1", Executor: poolWorker(2), MaxParallelJobs: 1},
		{Name: "m4", Executor: poolWorker(2), MaxParallelJobs: 1, Podcast: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: set})
	for i, want := range []string{"m1", "m4"} {
		if name, _, waiting := assignNew(t, e, fmt.Sprintf("conv-%d", i), "transcode_media"); waiting || name != want {
			t.Fatalf("conv-%d: assigned %q (waiting %v), want %s", i, name, waiting, want)
		}
	}
	name, res, waiting := assignNew(t, e, "conv-full", "transcode_media")
	if !waiting || name != "" || res.WaitingCondition != "worker_busy" {
		t.Fatalf("both workers at their cap: assigned %q, %+v", name, res)
	}
}

// A job from before workers had names stays on video-worker while one of that
// name is configured, however much room another worker has, and admission
// checks that worker.
func TestAnEarlierJobStaysOnVideoWorker(t *testing.T) {
	video := poolWorker(1)
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{
		{Name: transcode.DefaultWorkerName, Executor: video},
		{Name: "m4", Executor: poolWorker(4), Podcast: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}, Workers: set})
	seedActionInstance(t, st, "earlier", "transcode_media", StatusRunning, 3, "", map[string]any{}, map[string]any{"job_id": "job-1", "transcode_reconcile": true})
	ec := parseExecutionContextMust(t, e, "earlier")
	if _, waiting := e.ensureWorkerAssigned(context.Background(), ec); waiting || assignedWorker(ec.State) != transcode.DefaultWorkerName {
		t.Fatalf("an earlier job moved to %v", ec.State[workerStateKey])
	}
	video.doctorFunc = func(context.Context) error { return errors.New("connection refused") }
	inst := &store.ActionInstance{ActionName: "transcode_media", StateJSON: toJSON(map[string]any{"job_id": "job-1"})}
	if err := e.CheckInstanceWorkerAdmission(context.Background(), inst); err == nil {
		t.Fatal("an earlier job was admitted while video-worker is offline")
	}
}

func TestWorkerAssignmentSkipsAnOfflineWorkerAndWaitsWhenNoneIsReady(t *testing.T) {
	m1, m4 := poolWorker(2), poolWorker(1)
	m1Down := atomic.Bool{}
	m1Down.Store(true)
	m1.doctorFunc = func(context.Context) error {
		if m1Down.Load() {
			return errors.New("connection refused")
		}
		return nil
	}
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: twoWorkers(t, m1, m4, 0)})
	// m4 takes the job for its one slot; the next waits for a slot, not for
	// the offline m1.
	if name, _, waiting := assignNew(t, e, "conv-0", "transcode_media"); waiting || name != "m4" {
		t.Fatalf("conv-0: assigned %q (waiting %v) while m1 is offline", name, waiting)
	}
	if name, res, waiting := assignNew(t, e, "conv-1", "transcode_media"); !waiting || name != "" || res.WaitingCondition != "worker_busy" {
		t.Fatalf("conv-1 with m4 full and m1 offline: assigned %q, %+v", name, res)
	}
	m4.doctorFunc = func(context.Context) error { return errors.New("connection refused") }
	e.workerStateFor("m4").obs.CheckedAt = time.Time{}
	name, res, waiting := assignNew(t, e, "conv-none", "transcode_media")
	if !waiting || name != "" || res.Status != StepWaitingExternal || res.WaitingCondition != "worker_unreachable" {
		t.Fatalf("no ready worker: assigned %q, %+v", name, res)
	}
	// m1 returns: the waiting job is assigned on its next run.
	m1Down.Store(false)
	e.workerStateFor("m1").obs.CheckedAt = time.Time{}
	ec := parseExecutionContextMust(t, e, "conv-none")
	if _, waiting := e.ensureWorkerAssigned(context.Background(), ec); waiting || assignedWorker(ec.State) != "m1" {
		t.Fatalf("after m1 returned: %v", ec.State[workerStateKey])
	}
}

func TestPodcastWorkAndASingleWorkerAreAssignedWithoutProbing(t *testing.T) {
	m1, m4 := poolWorker(2), poolWorker(2)
	m1.doctorFunc = func(context.Context) error { return errors.New("offline") }
	m4.doctorFunc = m1.doctorFunc
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: twoWorkers(t, m1, m4, 0)})
	if name, _, waiting := assignNew(t, e, "podcast", "clean_podcast_ads"); waiting || name != "m4" {
		t.Fatalf("podcast work goes to the podcast worker: %q", name)
	}
	single := &mockTranscodeExecutor{doctorFunc: m1.doctorFunc}
	e = NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Transcode: single})
	if name, _, waiting := assignNew(t, e, "conv", "transcode_media"); waiting || name != transcode.DefaultWorkerName {
		t.Fatalf("the single worker: %q", name)
	}
	if m1.doctorCalls+m4.doctorCalls+single.doctorCalls != 0 {
		t.Fatal("assignment without a choice must not probe the workers")
	}
	if name, _, _ := assignNew(t, e, "other", "safe_media_replacement"); name != "" {
		t.Fatalf("an action without worker jobs is assigned %q", name)
	}
}

// A restarted server keeps every job on its worker: status, cancel and the
// candidate check go there and nowhere else.
func TestAssignedWorkerSurvivesARestartAndReceivesTheCancel(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}, Workers: twoWorkers(t, poolWorker(2), poolWorker(2), 0)})
	assignNew(t, e, "conv-0", "transcode_media")
	if name, _, _ := assignNew(t, e, "conv-1", "transcode_media"); name != "m4" {
		t.Fatalf("conv-1 assigned %q", name)
	}
	inst := mustInstance(t, st, "conv-1")
	inst.StateJSON = toJSON(map[string]any{workerStateKey: "m4", "job_id": "job-1", "transcode_submitted": true})
	if err := st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}

	// The restart: new executors, m4 listed first and now with more room.
	m1, m4 := poolWorker(1), poolWorker(4)
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: "m4", Executor: m4, Podcast: true}, {Name: "m1", Executor: m1}})
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewEngine(EngineDeps{Store: st, Config: &config.Config{}, Workers: set})
	ec := parseExecutionContextMust(t, restarted, "conv-1")
	if _, waiting := restarted.ensureWorkerAssigned(context.Background(), ec); waiting || assignedWorker(ec.State) != "m4" {
		t.Fatalf("worker changed across the restart: %v", ec.State[workerStateKey])
	}
	if restarted.actionWorker(ec) != transcode.Executor(m4) {
		t.Fatal("the job's executor is not its worker's")
	}
	if err := restarted.cancelRemoteActionWork(context.Background(), ec); err != nil {
		t.Fatal(err)
	}
	if m4.cancelCalls != 1 || m1.cancelCalls != 0 {
		t.Fatalf("cancel went to m1 %d times and m4 %d times", m1.cancelCalls, m4.cancelCalls)
	}
	if parseExecutionContextMust(t, restarted, "conv-0").State[workerStateKey] != "m1" {
		t.Fatal("conv-0 lost its worker")
	}
}

func TestAJobWhoseWorkerIsNoLongerConfiguredWaitsForIt(t *testing.T) {
	m1 := poolWorker(2)
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: twoWorkers(t, m1, poolWorker(2), 0)})
	ec := &ExecutionContext{State: map[string]any{workerStateKey: "m9", "job_id": "job-9", "transcode_submitted": true}, Outputs: map[string]any{}}
	_, err := e.actionWorker(ec).Status(context.Background(), "job-9")
	var missing *transcode.WorkerNotConfiguredError
	if !errors.As(err, &missing) || missing.Name != "m9" || !isRetryableWorkerPollError(err) {
		t.Fatalf("an unconfigured worker must be a retryable wait naming it: %v", err)
	}
	if err := e.cancelRemoteActionWork(context.Background(), ec); err == nil || !strings.Contains(err.Error(), `"m9"`) {
		t.Fatalf("cancel of a job on a missing worker must fail naming it: %v", err)
	}
	if m1.cancelCalls != 0 || m1.statusCalls != 0 {
		t.Fatal("another worker was asked about m9's job")
	}
	// An action from before workers had names, or one named video-worker,
	// belongs to the first worker once no worker has that name.
	for _, name := range []string{"", transcode.DefaultWorkerName} {
		if e.executorFor(name) != m1 {
			t.Fatalf("%q does not reach m1", name)
		}
	}
}

// The #211 discard rules ask the job's own worker. The other worker answers
// "completed" for everything: if it were asked, files would go.
func TestDiscardAsksOnlyTheJobsOwnWorker(t *testing.T) {
	statusOf := func(status string) func(context.Context, string) (transcode.JobStatus, error) {
		return func(_ context.Context, id string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: id, Status: status}, nil
		}
	}
	failWith := func(err error) func(context.Context, string) (transcode.JobStatus, error) {
		return func(context.Context, string) (transcode.JobStatus, error) { return transcode.JobStatus{}, err }
	}
	for _, tc := range []struct {
		name, worker string
		status       func(context.Context, string) (transcode.JobStatus, error)
		kept         string
	}{
		{name: "terminal on its worker", worker: "m4", status: statusOf(transcode.StatusFailed)},
		{name: "not_found on its worker", worker: "m4", status: failWith(&httpclient.HTTPError{StatusCode: 404, Code: transcode.WorkerErrorNotFound})},
		{name: "bare 404 on its worker", worker: "m4", status: failWith(&httpclient.HTTPError{StatusCode: 404}), kept: "may still be writing"},
		{name: "still running on its worker", worker: "m4", status: statusOf(transcode.StatusRunning), kept: "may still be writing"},
		{name: "its worker is no longer configured", worker: "m9", kept: "may still be writing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := candidateRoot(t)
			st := setupTestStore(t)
			e := discardEngine(t, st, root)
			other := &mockTranscodeExecutor{statusFunc: statusOf(transcode.StatusCompleted)}
			own := &mockTranscodeExecutor{statusFunc: tc.status}
			e.deps.Workers = twoWorkers(t, other, own, 0)
			original := filepath.Join(root, "Sample Show", "sample-episode-01.mkv")
			writeCandidateFile(t, original, "original")
			candidate := seedCandidateAttempt(t, st, "attempt", StatusFailed, original, "job-attempt", nil, map[string]any{workerStateKey: tc.worker})
			writeCandidateFile(t, candidate, strings.Repeat("c", 40))
			_, err := e.DiscardLeftoverCandidate(context.Background(), "attempt")
			if other.statusCalls != 0 {
				t.Fatal("the other worker was asked about this job")
			}
			if tc.kept != "" {
				if err == nil || !strings.Contains(err.Error(), tc.kept) {
					t.Fatalf("expected the candidate kept (%q): %v", tc.kept, err)
				}
				if _, serr := os.Stat(candidate); serr != nil {
					t.Fatalf("candidate removed: %v", serr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if own.statusCalls != 1 {
				t.Fatalf("its worker asked %d times", own.statusCalls)
			}
			if _, serr := os.Lstat(candidate); !os.IsNotExist(serr) {
				t.Fatalf("candidate kept: %v", serr)
			}
		})
	}
}

func TestEarlierJobsAreAssignedTheFirstNamedWorker(t *testing.T) {
	st := setupTestStore(t)
	seedActionInstance(t, st, "legacy-job", "transcode_media", StatusWaitingExternal, 3, "", map[string]any{}, map[string]any{"job_id": "job-1", "transcode_submitted": true})
	seedActionInstance(t, st, "legacy-podcast", "clean_podcast_ads", StatusWaitingExternal, 1, "", map[string]any{}, map[string]any{"external_reference": "pod-1"})
	seedActionInstance(t, st, "not-sent", "transcode_media", StatusPending, 0, "", map[string]any{}, map[string]any{})
	seedActionInstance(t, st, "named", "transcode_media", StatusRunning, 3, "", map[string]any{}, map[string]any{"job_id": "job-2", workerStateKey: "m4"})
	if _, err := st.SetWorkerSlotSettings(transcode.DefaultWorkerName, 2, 1, 3, 0); err != nil {
		t.Fatal(err)
	}
	// The single base_url form names earlier jobs video-worker, as running a
	// step on that config does; the move to a named list below rewrites both.
	legacy := NewEngine(EngineDeps{Store: st, Transcode: &mockTranscodeExecutor{}})
	if err := legacy.AssignEarlierWorkerActions(); err != nil {
		t.Fatal(err)
	}
	if got := assignedWorker(parseExecutionContextMust(t, legacy, "legacy-job").State); got != transcode.DefaultWorkerName {
		t.Fatalf("the legacy config named an earlier job %q", got)
	}
	if row, _ := st.GetWorkerSlotSettings(transcode.DefaultWorkerName); row == nil {
		t.Fatal("the legacy config must keep video-worker's slot settings")
	}
	seedActionInstance(t, st, "stepped", "transcode_media", StatusWaitingExternal, 3, "", map[string]any{}, map[string]any{"job_id": "job-3", workerStateKey: transcode.DefaultWorkerName})

	e := NewEngine(EngineDeps{Store: st, Workers: twoWorkers(t, poolWorker(2), poolWorker(2), 0)})
	for range 2 {
		if err := e.AssignEarlierWorkerActions(); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]string{"legacy-job": "m1", "legacy-podcast": "m1", "stepped": "m1", "not-sent": "", "named": "m4"} {
		if got := assignedWorker(parseExecutionContextMust(t, e, id).State); got != want {
			t.Errorf("%s: worker %q, want %q", id, got, want)
		}
	}
	if row, _ := st.GetWorkerSlotSettings("m1"); row == nil || row.VideoSlots != 2 || row.Ceiling != 3 {
		t.Fatalf("video-worker's slot settings did not become m1's: %+v", row)
	}
	if row, _ := st.GetWorkerSlotSettings(transcode.DefaultWorkerName); row != nil {
		t.Fatalf("video-worker's row kept: %+v", row)
	}
}

func TestBatchSlotsAddEachReadyWorkersRoom(t *testing.T) {
	m1, m4 := poolWorker(2), poolWorker(3)
	e := NewEngine(EngineDeps{Config: &config.Config{}, Workers: twoWorkers(t, m1, m4, 0)})
	running := func(workers ...string) []store.TranscodeBatchItem {
		var items []store.TranscodeBatchItem
		for _, w := range workers {
			items = append(items, store.TranscodeBatchItem{Status: "running", Worker: w})
		}
		return items
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		items []store.TranscodeBatchItem
		want  int
	}{
		{"empty", nil, 5},
		{"two on m1", running("m1", "m1"), 3},
		{"m1 over its pool does not borrow m4's room", running("m1", "m1", "m1"), 3},
		{"a child not yet assigned takes a slot", running("m4", ""), 3},
	} {
		if got := e.batchAvailableSlots(ctx, tc.items, len(tc.items)); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	e.deps.Config.Transcode.MaxParallelJobs = 2
	if got := e.batchAvailableSlots(ctx, running("m4"), 1); got != 1 {
		t.Errorf("the global cap: %d", got)
	}
	e.deps.Config.Transcode.MaxParallelJobs = 0
	m4.doctorFunc = func(context.Context) error { return errors.New("offline") }
	e.workerStateFor("m4").obs.CheckedAt = time.Time{}
	if got := e.batchAvailableSlots(ctx, nil, 0); got != 2 {
		t.Errorf("an offline worker adds no room: %d", got)
	}
	m1.doctorFunc = m4.doctorFunc
	e.workerStateFor("m1").obs.CheckedAt = time.Time{}
	if got := e.batchAvailableSlots(ctx, nil, 0); got != 5 {
		t.Errorf("with no worker ready the children start and wait: %d", got)
	}
}

func TestEachWorkerHasItsOwnSlotQueue(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}, Workers: twoWorkers(t, poolWorker(1), poolWorker(1), 0)})
	base := time.Now().Add(-time.Minute)
	for i, w := range []string{"m1", "m4", "m1"} {
		seedActionInstance(t, st, fmt.Sprintf("wait-%d", i), "transcode_media", StatusRunning, 3, "", map[string]any{}, map[string]any{workerStateKey: w, workerSlotWaitSinceKey: base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)})
	}
	ec := &ExecutionContext{InstanceID: "new", State: map[string]any{workerStateKey: "m4", workerSlotWaitSinceKey: base.Add(time.Hour).Format(time.RFC3339Nano)}, Outputs: map[string]any{}}
	if position, err := e.slotQueuePosition(ec); err != nil || position != 2 {
		t.Fatalf("m4's queue has one waiter ahead: %d %v", position, err)
	}
	ec.State[workerStateKey] = "m1"
	if position, _ := e.slotQueuePosition(ec); position != 3 {
		t.Fatalf("m1's queue has two waiters ahead: %d", position)
	}
}

func TestWorkerObservationListsEveryWorkerAndOneOfflineDoesNotBlock(t *testing.T) {
	m1, m4 := poolWorker(2), poolWorker(2)
	m1.doctorFunc = func(context.Context) error { return errors.New("offline") }
	e := NewEngine(EngineDeps{Config: &config.Config{}, Workers: twoWorkers(t, m1, m4, 0)})
	o := e.WorkerObservation(context.Background())
	if !o.Ready || len(o.Workers) != 2 || o.Workers[0].Name != "m1" || o.Workers[0].Ready || !o.Workers[1].Ready || !o.Workers[1].Podcast {
		t.Fatalf("observation: %+v", o)
	}
	if !strings.Contains(o.Message, "1 of 2") || !strings.Contains(o.Message, "m1") {
		t.Fatalf("message: %q", o.Message)
	}
	if err := e.CheckWorkerAdmission(context.Background()); err != nil {
		t.Fatalf("one ready worker admits new jobs: %v", err)
	}
	pinned := &store.ActionInstance{ActionName: "transcode_media", StateJSON: toJSON(map[string]any{workerStateKey: "m1"})}
	if err := e.CheckInstanceWorkerAdmission(context.Background(), pinned); err == nil || !strings.Contains(err.Error(), "m1") {
		t.Fatalf("a job on m1 is not admitted while m1 is offline: %v", err)
	}
	m4.doctorFunc = m1.doctorFunc
	if err := e.CheckWorkerAdmissionFor(context.Background(), "clean_podcast_ads"); err == nil {
		t.Fatal("podcast work needs the podcast worker")
	}
}

func TestLadderRecordsRunOnlyOnTheirWorker(t *testing.T) {
	e := NewEngine(EngineDeps{Workers: twoWorkers(t, poolWorker(1), poolWorker(1), 0)})
	for _, tc := range []struct {
		record, action string
		want           bool
	}{
		{"", "m4", true},
		{"m4", "m4", true},
		{"m1", "m4", false},
		// Planned under the single http block: video-worker is now m1.
		{transcode.DefaultWorkerName, "m1", true},
		{transcode.DefaultWorkerName, "m4", false},
	} {
		if got := e.workerMatches(tc.record, map[string]any{workerStateKey: tc.action}); got != tc.want {
			t.Errorf("record for %q on %s: %v", tc.record, tc.action, got)
		}
	}
}

// Checks made before a file has a worker ask only the workers it could be
// assigned now: an offline worker does not decide them.
func TestAnOfflineWorkerDoesNotDecideCoverArt(t *testing.T) {
	coverArt := func(ok bool) func(context.Context) (transcode.WorkerCapabilities, error) {
		return func(context.Context) (transcode.WorkerCapabilities, error) {
			return transcode.WorkerCapabilities{CoverArt: ok}, nil
		}
	}
	m1, m4 := poolWorker(1), poolWorker(1)
	m1.capabilitiesFunc = coverArt(true)
	m4.doctorFunc = func(context.Context) error { return errors.New("connection refused") }
	m4.capabilitiesFunc = func(context.Context) (transcode.WorkerCapabilities, error) {
		return transcode.WorkerCapabilities{}, errors.New("connection refused")
	}
	e := NewEngine(EngineDeps{Config: &config.Config{}, Workers: twoWorkers(t, m1, m4, 0)})
	if !e.cachedCoverArtSupport(context.Background())() {
		t.Fatal("offline m4 made cover art unsupported")
	}
	// Once m4 is ready, it may be assigned the file and must support it too.
	m4.doctorFunc, m4.capabilitiesFunc = nil, coverArt(false)
	e.workerStateFor("m4").obs.CheckedAt = time.Time{}
	if e.cachedCoverArtSupport(context.Background())() {
		t.Fatal("ready m4 without cover art was ignored")
	}
}

// Capability gates read the job's own worker: a plan that drops tracks is
// refused on a worker without track selection even when the other worker has
// it, and never sent to either.
func TestTrackSelectionGateReadsTheAssignedWorker(t *testing.T) {
	withTracks := func(capable bool) *mockTranscodeExecutor {
		return &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
			return transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, SizeAudioLevels: true, SizeAudioObjectBed: true, TrackSelection: capable}, nil
		}}
	}
	for _, tc := range []struct {
		worker  string
		refused bool
	}{{"m4", true}, {"m1", false}} {
		m1, m4 := withTracks(true), withTracks(false)
		e := NewEngine(EngineDeps{Workers: twoWorkers(t, m1, m4, 0)})
		ec := testReconcileEC("job-tracks")
		ec.State[workerStateKey] = tc.worker
		plan := testReconcilePlan()
		plan.Mode, plan.ModePolicyVersion, plan.AudioMode = "size", transcode.ModePolicyV6, transcode.AudioModeSizeAAC
		plan.Tracks = &transcode.TrackSelection{KeepAudio: []int{1}, KeepSubtitles: []int{}}
		ec.State["plan"] = plan
		res, err := e.stepTranscodeSubmit(context.Background(), ec)
		if err != nil {
			t.Fatal(err)
		}
		refused := res.Status == StepFailed && strings.Contains(res.Error, "audio and subtitle tracks")
		if refused != tc.refused {
			t.Fatalf("on %s: status %s error %q", tc.worker, res.Status, res.Error)
		}
		if m4.submitCalls != 0 || (tc.worker == "m4" && m1.submitCalls != 0) {
			t.Fatalf("on %s: submitted to m1 %d, m4 %d times", tc.worker, m1.submitCalls, m4.submitCalls)
		}
	}
}
