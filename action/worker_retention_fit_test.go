package action

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/transcode"
)

const gib = int64(1) << 30

// roomReporter is a worker that reports what it can take now in its health, as
// a deferred-publication worker does: the retention budget left and the disk it
// could offer a job.
type roomReporter struct {
	*slotReportingExecutor
	mu       sync.Mutex
	capacity int64 // retention budget
	room     int64 // retention budget left
	held     int64
	disk     int64 // size of the work disk (0: not reported)
	diskRoom int64 // what the disk can give a job now
	floor    int64
	hold     string
	silent   bool
	age      time.Duration
}

func (r *roomReporter) Scheduler(context.Context) (transcode.SchedulerObservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	obs := transcode.SchedulerObservation{Health: "ok", ObservedAt: now, LastSuccessAt: now, FreshUntil: now.Add(time.Minute)}
	if !r.silent {
		obs.Retention = &transcode.RetentionRoom{ObservedAt: now.Add(-r.age), CapacityBytes: r.capacity, RoomBytes: r.room, HeldBytes: r.held,
			DiskCapacityBytes: r.disk, DiskRoomBytes: r.diskRoom, FloorBytes: r.floor, Hold: r.hold}
	}
	return obs, nil
}

// change edits the report and makes the engine read it again.
func (r *roomReporter) change(e *Engine, name string, edit func(*roomReporter)) {
	r.mu.Lock()
	edit(r)
	r.mu.Unlock()
	e.workerStateFor(name).obs.CheckedAt = time.Time{}
}

func (r *roomReporter) setRoom(e *Engine, name string, room int64) {
	r.change(e, name, func(r *roomReporter) { r.room = room })
}

func newRoomReporter(capacity, room int64) *roomReporter {
	return &roomReporter{slotReportingExecutor: poolWorker(2), capacity: capacity, room: room}
}

// diskNode reports a work disk of the given size, floor and room.
func diskNode(disk, floor, room int64) *roomReporter {
	r := newRoomReporter(0, 0)
	r.disk, r.floor, r.diskRoom = disk, floor, room
	return r
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
	seedActionInstance(t, e.deps.Store, id, "transcode_media", StatusRunning, 0, "", inputs, map[string]any{SubmittedSourceBytesKey: size, roomReserveKey: transcode.OutputReserveBytes(nil, size)})
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

func assignAgain(t *testing.T, e *Engine, id string) (string, StepResult, bool) {
	t.Helper()
	res, waiting := e.ensureWorkerAssigned(context.Background(), parseExecutionContextMust(t, e, id))
	return assignedWorker(parseExecutionContextMust(t, e, id).State), res, waiting
}

// A file the retention budget of one node cannot hold now goes to the node that
// can; a smaller one still goes to the first node with a slot.
func TestAFileTheWorkersRetentionRoomCannotHoldGoesToTheWorkerThatCan(t *testing.T) {
	m4 := newRoomReporter(30*gib, 11*gib)
	m1 := newRoomReporter(100*gib, 100*gib)
	e := retentionWorkers(t, m4, m1)

	if name, res, waiting := convert(t, e, "conv-a", 29*gib, ""); waiting || name != "m1" {
		t.Fatalf("a 29 GiB file went to %q (waiting %v, %+v); m4 has 11 GiB of room", name, waiting, res)
	}
	if name, _, waiting := convert(t, e, "conv-b", 5*gib, ""); waiting || name != "m4" {
		t.Fatalf("a 5 GiB file went to %q (waiting %v); it fits m4", name, waiting)
	}
}

// A file that fits only the big disk goes there.
func TestAFileThatFitsOnlyTheBigNodeGoesThere(t *testing.T) {
	small := diskNode(500*gib, 20*gib, 60*gib)
	big := diskNode(4000*gib, 20*gib, 900*gib)
	e := retentionWorkers(t, small, big)
	if name, res, waiting := convert(t, e, "conv-a", 120*gib, ""); waiting || name != "m1" {
		t.Fatalf("a 120 GiB file went to %q (waiting %v, %+v); only m1 has the room", name, waiting, res)
	}
}

// A file no node could take even when empty fails when it is first placed,
// with what it needs against what the largest node has.
func TestAFileTooBigForEveryNodeFailsAtSubmission(t *testing.T) {
	e := retentionWorkers(t, diskNode(400*gib, 20*gib, 300*gib), diskNode(600*gib, 20*gib, 500*gib))
	// 500 GiB source + 250 GiB output + floor: more than any disk.
	name, res, waiting := convert(t, e, "conv-a", 500*gib, "")
	if !waiting || name != "" || res.Status != StepFailed || !strings.Contains(res.Error, "Too big for every node") {
		t.Fatalf("assigned %q (waiting %v), %+v; want a failure naming every node", name, waiting, res)
	}
	// An output past every retention budget is as hopeless.
	e = retentionWorkers(t, newRoomReporter(30*gib, 30*gib), newRoomReporter(40*gib, 40*gib))
	if _, res, waiting := convert(t, e, "conv-b", 100*gib, ""); !waiting || res.Status != StepFailed {
		t.Fatalf("an output over every retention budget: %+v (waiting %v)", res, waiting)
	}
	// One node that could take it empty keeps it waiting, not failed.
	e = retentionWorkers(t, diskNode(400*gib, 20*gib, 0), diskNode(2000*gib, 20*gib, 0))
	if _, res, waiting := convert(t, e, "conv-c", 500*gib, ""); !waiting || res.Status == StepFailed {
		t.Fatalf("m1 could take it empty, so it waits: %+v (waiting %v)", res, waiting)
	}
}

// A file no node has room for now stays in the server queue with a plain
// reason, is not failed, and is placed once room appears.
func TestAFileNoNodeHasRoomForNowWaitsInTheQueueAndIsPlacedWhenRoomAppears(t *testing.T) {
	m4 := diskNode(1000*gib, 10*gib, 20*gib)
	m1 := diskNode(1000*gib, 10*gib, 30*gib)
	e := retentionWorkers(t, m4, m1)
	name, res, waiting := convert(t, e, "conv-a", 60*gib, "")
	need := transcode.RoomNeed{SourceBytes: 60 * gib, ReserveBytes: 30 * gib}
	wantReason := "no node has room: needs " + gbText(need.SourceBytes+need.ReserveBytes) + ", the most any node can offer is " + gbText(30*gib)
	if !waiting || name != "" || res.Status != StepWaitingExternal || res.WaitingCondition != "worker_busy" || !strings.Contains(strings.ToLower(res.WaitingReason), strings.ToLower(wantReason)) {
		t.Fatalf("assigned %q (waiting %v), %+v; want a wait saying %q", name, waiting, res, wantReason)
	}
	m1.change(e, "m1", func(r *roomReporter) { r.diskRoom = 200 * gib }) // a cache entry went, or a candidate was approved
	if name, res, waiting := assignAgain(t, e, "conv-a"); waiting || name != "m1" {
		t.Fatalf("m1 has room now: assigned %q (waiting %v), %+v", name, waiting, res)
	}
}

// The reasons a node is passed over are told in plain words.
func TestTheWaitNamesWhyEachNodeIsPassedOver(t *testing.T) {
	m4 := newRoomReporter(30*gib, 0)
	m4.held = 30 * gib
	m1 := newRoomReporter(100*gib, 100*gib)
	m1.hold = "paused"
	e := retentionWorkers(t, m4, m1)
	_, res, waiting := convert(t, e, "conv-a", 20*gib, "")
	reason := strings.ToLower(res.WaitingReason)
	if !waiting || !strings.Contains(reason, strings.ToLower("waiting for approvals on m4 ("+gbText(30*gib)+" held)")) || !strings.Contains(reason, "node m1 paused") {
		t.Fatalf("reason %q", res.WaitingReason)
	}
	m1.change(e, "m1", func(r *roomReporter) { r.hold = "build" })
	if _, res, _ := assignAgain(t, e, "conv-a"); !strings.Contains(res.WaitingReason, "build running on m1") {
		t.Fatalf("reason %q", res.WaitingReason)
	}
	// Resumed, the node takes it.
	m1.change(e, "m1", func(r *roomReporter) { r.hold = "" })
	if name, res, waiting := assignAgain(t, e, "conv-a"); waiting || name != "m1" {
		t.Fatalf("resumed m1 did not take it: %q (waiting %v), %+v", name, waiting, res)
	}
}

// Two conversions for one open node: the oldest gets it, the other waits in the
// queue (the node would refuse it at once) and goes elsewhere when a slot opens.
func TestTwoConversionsForOneNodeTheOldestGetsItAndTheOtherWaits(t *testing.T) {
	e := retentionWorkers(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	seedActionInstance(t, e.deps.Store, "busy-m1", "transcode_media", StatusRunning, 0, "", map[string]any{}, map[string]any{"worker": "m1"})
	if name, res, waiting := convert(t, e, "conv-a", 100*gib, ""); waiting || name != "m4" {
		t.Fatalf("the oldest: assigned %q (waiting %v), %+v", name, waiting, res)
	}
	if name, res, waiting := convert(t, e, "conv-b", 100*gib, ""); !waiting || name != "" {
		t.Fatalf("the younger one took a node no slot of which is free: assigned %q, %+v", name, res)
	}
	finishAction(t, e, "busy-m1")
	if name, res, waiting := assignAgain(t, e, "conv-b"); waiting || name != "m1" {
		t.Fatalf("when m1 freed the waiting one went to %q (waiting %v), %+v", name, waiting, res)
	}
}

// A worker that reports nothing, or an old report, is taken to have room: it
// refuses at once if it has none.
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

	ahead := newRoomReporter(30*gib, 0)
	ahead.age = -2 * transcode.RetentionRoomMaxAge // observed "in the future"
	e = retentionWorkers(t, ahead, newRoomReporter(100*gib, 100*gib))
	if name, _, waiting := convert(t, e, "conv-c", 29*gib, ""); waiting || name != "m4" {
		t.Fatalf("a report from the future was trusted: assigned %q (waiting %v)", name, waiting)
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

// A requeued conversion goes back to the node it came from when that node has
// room, and not at all to the one that just refused it.
func TestARequeuedConversionPrefersItsNodeUnlessThatNodeJustRefusedIt(t *testing.T) {
	e := retentionWorkers(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	seedActionInstance(t, e.deps.Store, "conv-a", "transcode_media", StatusRunning, 0, "", map[string]any{},
		map[string]any{SubmittedSourceBytesKey: 50 * gib, requeuedFromKey: "m1"})
	if name, _, waiting := assignAgain(t, e, "conv-a"); waiting || name != "m1" {
		t.Fatalf("a requeued conversion went to %q (waiting %v), want the node it came from", name, waiting)
	}
	seedActionInstance(t, e.deps.Store, "conv-b", "transcode_media", StatusRunning, 0, "", map[string]any{},
		map[string]any{SubmittedSourceBytesKey: 50 * gib, requeuedFromKey: "m1", roomRefusedByKey: "m1", roomRefusedAtKey: e.now().Format(time.RFC3339Nano)})
	if name, _, waiting := assignAgain(t, e, "conv-b"); waiting || name != "m4" {
		t.Fatalf("a conversion m1 just refused went to %q (waiting %v), want m4", name, waiting)
	}
}

// The queue decides on the size stamped when the conversion was admitted, so
// it must be there before the conversion is first given a worker.
func TestAnAdmittedConversionNeedsTheRoomOfItsFileBeforeItIsGivenAWorker(t *testing.T) {
	mediaDir := t.TempDir()
	source := mediaDir + "/big.mkv"
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(29 * gib); err != nil { // sparse
		t.Fatal(err)
	}
	f.Close()
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, createFakeFFprobeScript(t, defaultValidFFprobeJSON), []string{mediaDir}, []string{mediaDir}, false)
	defer st.Close()
	res, err := e.Enqueue(WithOrigin(context.Background(), "web"), "transcode_media", map[string]any{"path": source, "mode": "quality"}, "big")
	if err != nil {
		t.Fatal(err)
	}
	if need := conversionRoomNeed(parseExecutionContextMust(t, e, res.ID)); need.SourceBytes != 29*gib || need.ReserveBytes != 29*gib/2 {
		t.Fatalf("the admitted conversion needs %+v, want its source and half of it", need)
	}
}

func TestAFileOnlyAnOfflineWorkerCouldEverHoldWaitsForIt(t *testing.T) {
	// A worker that is down either still has its last report or none at all.
	for name, silent := range map[string]bool{"with its last report": false, "with no report": true} {
		t.Run(name, func(t *testing.T) {
			m4 := newRoomReporter(30*gib, 30*gib)
			m1 := newRoomReporter(100*gib, 100*gib)
			m1.silent = silent
			m1.doctorFunc = func(context.Context) error { return errors.New("offline") }
			e := retentionWorkers(t, m4, m1)

			// A 70 GiB file's output never fits m4's budget, even empty; m1 could
			// take it once it is back. It is not failed (m1 could take it) and not
			// left to m4.
			if name, res, waiting := convert(t, e, "conv-a", 70*gib, ""); !waiting || name != "" || res.Status == StepFailed {
				t.Fatalf("assigned %q (waiting %v), %+v", name, waiting, res)
			}
			m1.doctorFunc = nil
			e.workerStateFor("m1").obs.CheckedAt = time.Time{} // check it again
			if name, res, waiting := assignAgain(t, e, "conv-a"); waiting || name != "m1" {
				t.Fatalf("m1 is back and holds the file: assigned %q (waiting %v), %+v", name, waiting, res)
			}
		})
	}
}
