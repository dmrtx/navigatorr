package action

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/transcode"
)

const gib = int64(1) << 30

// roomReporter is a worker that reports the room it has left to retain
// candidates in its health, as a deferred-publication worker does.
type roomReporter struct {
	*slotReportingExecutor
	mu       sync.Mutex
	capacity int64
	room     int64
	silent   bool
	age      time.Duration
}

func (r *roomReporter) Scheduler(context.Context) (transcode.SchedulerObservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	obs := transcode.SchedulerObservation{Health: "ok", ObservedAt: now, LastSuccessAt: now, FreshUntil: now.Add(time.Minute)}
	if !r.silent {
		obs.Retention = &transcode.RetentionRoom{ObservedAt: now.Add(-r.age), CapacityBytes: r.capacity, RoomBytes: r.room}
	}
	return obs, nil
}

func (r *roomReporter) setRoom(e *Engine, name string, room int64) {
	r.mu.Lock()
	r.room = room
	r.mu.Unlock()
	e.workerStateFor(name).obs.CheckedAt = time.Time{} // read the report again
}

func newRoomReporter(capacity, room int64) *roomReporter {
	return &roomReporter{slotReportingExecutor: poolWorker(2), capacity: capacity, room: room}
}

// retentionWorkers is m4 then m1, both publishing on approval: m4 would be
// picked on a tie, so a conversion that lands on m1 was kept off m4.
func retentionWorkers(t *testing.T, m4, m1 transcode.Executor) *Engine {
	t.Helper()
	return retentionWorkersInOrder(t, true, m4, m1)
}

// retentionWorkersInOrder is retentionWorkers with m4 first or, when m4First is
// false, m1 first, which is the one a tie then picks.
func retentionWorkersInOrder(t *testing.T, m4First bool, m4, m1 transcode.Executor) *Engine {
	t.Helper()
	workers := []transcode.NamedWorker{
		{Name: "m4", Executor: m4, MaxParallelJobs: 1, PublishPolicy: transcode.PublishPolicyOnApproval},
		{Name: "m1", Executor: m1, MaxParallelJobs: 1, PublishPolicy: transcode.PublishPolicyOnApproval, Podcast: true},
	}
	if !m4First {
		workers[0], workers[1] = workers[1], workers[0]
	}
	set, err := transcode.NewWorkerSet(workers)
	if err != nil {
		t.Fatal(err)
	}
	return NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: set})
}

// convert seeds a conversion of a file of the given size and assigns it a worker.
func convert(t *testing.T, e *Engine, id string, size int64, batch string) (string, StepResult, bool) {
	t.Helper()
	inputs := map[string]any{}
	if batch != "" {
		seedActionInstance(t, e.deps.Store, batch, "transcode_batch", StatusRunning, 0, "", map[string]any{}, map[string]any{})
		inputs["parent_action_id"] = batch
	}
	seedActionInstance(t, e.deps.Store, id, "transcode_media", StatusRunning, 0, "", inputs, map[string]any{SubmittedSourceBytesKey: size})
	ec := parseExecutionContextMust(t, e, id)
	res, waiting := e.ensureWorkerAssigned(context.Background(), ec)
	if waiting {
		// The engine saves the place a waiting conversion took in the queue.
		if err := e.persistExecutionState(context.Background(), ec); err != nil {
			t.Fatal(err)
		}
	}
	return assignedWorker(parseExecutionContextMust(t, e, id).State), res, waiting
}

func finishAction(t *testing.T, e *Engine, id string) {
	t.Helper()
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	inst.Status = StatusCompleted
	if err := e.deps.Store.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
}

func TestAFileTheWorkersRetentionRoomCannotHoldGoesToTheWorkerThatCan(t *testing.T) {
	m4 := newRoomReporter(30*gib, 11*gib)
	m1 := newRoomReporter(100*gib, 100*gib)
	e := retentionWorkers(t, m4, m1)

	if name, res, waiting := convert(t, e, "conv-a", 29*gib, ""); waiting || name != "m1" {
		t.Fatalf("a 29 GiB file went to %q (waiting %v, %+v); m4 has 11 GiB of room", name, waiting, res)
	}
	// A file that fits m4's room still goes to the first worker with a slot.
	if name, _, waiting := convert(t, e, "conv-b", 5*gib, ""); waiting || name != "m4" {
		t.Fatalf("a 5 GiB file went to %q (waiting %v); it fits m4", name, waiting)
	}
}

func TestAFileThatFitsNoFreeWorkerWaitsAndTheOnesBehindItPass(t *testing.T) {
	m4 := newRoomReporter(30*gib, 11*gib)
	m1 := newRoomReporter(100*gib, 100*gib)
	e := retentionWorkers(t, m4, m1)
	seedActionInstance(t, e.deps.Store, "busy-m1", "transcode_media", StatusRunning, 0, "", map[string]any{}, map[string]any{"worker": "m1"})

	name, res, waiting := convert(t, e, "conv-a", 29*gib, "")
	if !waiting || name != "" || res.WaitingCondition != "worker_busy" || !strings.Contains(res.WaitingReason, "room to retain") {
		t.Fatalf("m4 is the only free node and cannot hold the file: assigned %q, %+v", name, res)
	}
	// m4's slot is free, so the conversion behind it takes it.
	if name, res, waiting := convert(t, e, "conv-b", 5*gib, ""); waiting || name != "m4" {
		t.Fatalf("the big file held m4's slot back: assigned %q (waiting %v), %+v", name, waiting, res)
	}
	// m1 frees and the big file is the next in line for it, ahead of a later one.
	finishAction(t, e, "busy-m1")
	if name, res, waiting := convert(t, e, "conv-c", 1*gib, ""); !waiting || name != "" {
		t.Fatalf("a later conversion took the slot the big file waits for: assigned %q, %+v", name, res)
	}
	if res, waiting := e.ensureWorkerAssigned(context.Background(), parseExecutionContextMust(t, e, "conv-a")); waiting {
		t.Fatalf("the big file did not take m1: %+v", res)
	} else if got := assignedWorker(parseExecutionContextMust(t, e, "conv-a").State); got != "m1" {
		t.Fatalf("the big file went to %q (%+v)", got, res)
	}
}

func TestRoomThatComesBackLetsTheWaitingFileIn(t *testing.T) {
	m4 := newRoomReporter(30*gib, 2*gib)
	m1 := newRoomReporter(100*gib, 0)
	e := retentionWorkers(t, m4, m1)

	if name, res, waiting := convert(t, e, "conv-a", 20*gib, ""); !waiting || name != "" {
		t.Fatalf("no node has room: assigned %q, %+v", name, res)
	}
	m4.setRoom(e, "m4", 25*gib) // a candidate on m4 was approved
	if res, waiting := e.ensureWorkerAssigned(context.Background(), parseExecutionContextMust(t, e, "conv-a")); waiting {
		t.Fatalf("m4 has room now: %+v", res)
	}
	if got := assignedWorker(parseExecutionContextMust(t, e, "conv-a").State); got != "m4" {
		t.Fatalf("assigned %q", got)
	}
}

func TestAFileLargerThanEveryBudgetIsLeftToTheWorkers(t *testing.T) {
	m4 := newRoomReporter(30*gib, 30*gib)
	m1 := newRoomReporter(40*gib, 40*gib)
	e := retentionWorkers(t, m4, m1)
	// Waiting for room would never help: assigned as before, the worker says why.
	if name, res, waiting := convert(t, e, "conv-a", 60*gib, ""); waiting || name != "m4" {
		t.Fatalf("a file no budget holds: assigned %q (waiting %v), %+v", name, waiting, res)
	}
}

func TestAFileThatNoBudgetOfAFreeNodeHoldsStillPicksTheOneThatCouldEver(t *testing.T) {
	m4 := newRoomReporter(30*gib, 30*gib)
	m1 := newRoomReporter(100*gib, 100*gib)
	e := retentionWorkers(t, m4, m1)
	// 50 GiB can never be retained on m4, even empty.
	if name, _, waiting := convert(t, e, "conv-a", 50*gib, ""); waiting || name != "m1" {
		t.Fatalf("assigned %q (waiting %v), want m1", name, waiting)
	}
}

func TestAWorkerThatReportsNothingOrAnOldReportIsTakenToHaveRoom(t *testing.T) {
	silent := newRoomReporter(30*gib, 0)
	silent.silent = true
	e := retentionWorkers(t, silent, newRoomReporter(100*gib, 100*gib))
	if name, _, waiting := convert(t, e, "conv-a", 29*gib, ""); waiting || name != "m4" {
		t.Fatalf("a worker that does not report its room: assigned %q (waiting %v)", name, waiting)
	}

	old := newRoomReporter(30*gib, 0)
	old.age = 2 * transcode.RetentionRoomMaxAge
	e = retentionWorkers(t, old, newRoomReporter(100*gib, 100*gib))
	if name, _, waiting := convert(t, e, "conv-b", 29*gib, ""); waiting || name != "m4" {
		t.Fatalf("an old report was trusted: assigned %q (waiting %v)", name, waiting)
	}
}

func TestABatchChildNeedsNoRetentionRoom(t *testing.T) {
	m4 := newRoomReporter(30*gib, 0)
	m1 := newRoomReporter(100*gib, 100*gib)
	e := retentionWorkers(t, m4, m1)
	// The worker starts a child to publish on encode when the budget is full.
	if name, _, waiting := convert(t, e, "child-a", 29*gib, "batch-a"); waiting || name != "m4" {
		t.Fatalf("a batch child was kept off m4: assigned %q (waiting %v)", name, waiting)
	}
}

func TestAWorkerThatPublishesOnEncodeNeedsNoRetentionRoom(t *testing.T) {
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{
		{Name: "m4", Executor: newRoomReporter(30*gib, 0), MaxParallelJobs: 1},
		{Name: "m1", Executor: newRoomReporter(100*gib, 100*gib), MaxParallelJobs: 1, Podcast: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: set})
	if name, _, waiting := convert(t, e, "conv-a", 29*gib, ""); waiting || name != "m4" {
		t.Fatalf("a worker that publishes on encode was kept off: assigned %q (waiting %v)", name, waiting)
	}
}

func TestAConversionBehindOneOnlyOneWorkerCanHoldDoesNotTakeThatWorkersSlot(t *testing.T) {
	m4 := newRoomReporter(30*gib, 11*gib)
	m1 := newRoomReporter(100*gib, 100*gib)
	// m1 comes first, so a tie between two free workers picks it.
	e := retentionWorkersInOrder(t, false, m4, m1)
	seedActionInstance(t, e.deps.Store, "busy-m1", "transcode_media", StatusRunning, 0, "", map[string]any{}, map[string]any{"worker": "m1"})

	// The big file fits m1 only, and waits for its slot.
	if name, res, waiting := convert(t, e, "conv-a", 29*gib, ""); !waiting || name != "" {
		t.Fatalf("m1 is busy and m4 cannot hold the file: assigned %q, %+v", name, res)
	}
	finishAction(t, e, "busy-m1")
	// m1 is free and the big file has not been polled yet: a small file, which
	// fits both and would tie on m1, takes m4 and leaves m1 to the big one.
	if name, res, waiting := convert(t, e, "conv-b", 1*gib, ""); waiting || name != "m4" {
		t.Fatalf("the small file took the slot the big one waits for: assigned %q (waiting %v), %+v", name, waiting, res)
	}
	if res, waiting := e.ensureWorkerAssigned(context.Background(), parseExecutionContextMust(t, e, "conv-a")); waiting {
		t.Fatalf("the big file did not take m1: %+v", res)
	} else if got := assignedWorker(parseExecutionContextMust(t, e, "conv-a").State); got != "m1" {
		t.Fatalf("the big file went to %q (%+v)", got, res)
	}
}

func TestTheQueuePositionCountsTheConversionsThatCouldUseTheSameWorkers(t *testing.T) {
	m4 := newRoomReporter(30*gib, 30*gib)
	m1 := newRoomReporter(100*gib, 100*gib)
	e := retentionWorkers(t, m4, m1)
	seedActionInstance(t, e.deps.Store, "busy-m4", "transcode_media", StatusRunning, 0, "", map[string]any{}, map[string]any{"worker": "m4"})
	seedActionInstance(t, e.deps.Store, "busy-m1", "transcode_media", StatusRunning, 0, "", map[string]any{}, map[string]any{"worker": "m1"})

	// Both slots are busy: where a conversion stands does not depend on which
	// slot frees first.
	for i, id := range []string{"conv-a", "conv-b", "conv-c"} {
		name, res, waiting := convert(t, e, id, 5*gib, "")
		if !waiting || name != "" {
			t.Fatalf("%s: assigned %q (waiting %v), %+v", id, name, waiting, res)
		}
		if got := getInt(res.Outputs, workerSlotPositionKey); got != i+1 {
			t.Fatalf("%s: position %d, want %d (%+v)", id, got, i+1, res)
		}
	}
	// A slot frees: the first in line takes it, the others keep their places.
	finishAction(t, e, "busy-m4")
	if name, res, waiting := convert(t, e, "conv-d", 5*gib, ""); !waiting || name != "" || getInt(res.Outputs, workerSlotPositionKey) != 4 {
		t.Fatalf("a later conversion took the slot ahead of the ones waiting: assigned %q (waiting %v), %+v", name, waiting, res)
	}
	if res, waiting := e.ensureWorkerAssigned(context.Background(), parseExecutionContextMust(t, e, "conv-a")); waiting {
		t.Fatalf("the first in line did not take the free slot: %+v", res)
	}
}

func TestAFileOnlyAnOfflineWorkerCouldEverHoldWaitsForIt(t *testing.T) {
	m4 := newRoomReporter(30*gib, 30*gib)
	m1 := newRoomReporter(100*gib, 100*gib)
	m1.doctorFunc = func(context.Context) error { return errors.New("offline") }
	e := retentionWorkers(t, m4, m1)

	// 50 GiB never fits m4, even empty; m1 could, once it is back. Leaving the
	// file to m4 would strand it there.
	if name, res, waiting := convert(t, e, "conv-a", 50*gib, ""); !waiting || name != "" || !strings.Contains(res.WaitingReason, "room to retain") {
		t.Fatalf("assigned %q (waiting %v), %+v", name, waiting, res)
	}
	m1.doctorFunc = nil
	e.workerStateFor("m1").obs.CheckedAt = time.Time{} // check it again
	if res, waiting := e.ensureWorkerAssigned(context.Background(), parseExecutionContextMust(t, e, "conv-a")); waiting {
		t.Fatalf("m1 is back and holds the file: %+v", res)
	}
	if got := assignedWorker(parseExecutionContextMust(t, e, "conv-a").State); got != "m1" {
		t.Fatalf("assigned %q, want m1", got)
	}
}

func TestAReportDatedAheadOfTheClockIsNotTrusted(t *testing.T) {
	ahead := newRoomReporter(30*gib, 0)
	ahead.age = -2 * transcode.RetentionRoomMaxAge // observed "in the future"
	e := retentionWorkers(t, ahead, newRoomReporter(100*gib, 100*gib))
	if name, _, waiting := convert(t, e, "conv-a", 29*gib, ""); waiting || name != "m4" {
		t.Fatalf("a report from the future was trusted: assigned %q (waiting %v)", name, waiting)
	}
}
