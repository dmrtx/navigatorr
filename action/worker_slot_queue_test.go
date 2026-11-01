package action

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// slotWorker is a fake worker with a fixed number of slots shared by every
// batch: a submit while all slots are taken is refused as busy, and an
// idempotent resubmit of an accepted job returns that job.
type slotWorker struct {
	mu       sync.Mutex
	slots    int
	active   map[string]string // job id -> source path
	done     map[string]bool
	accepted []string // source paths, in acceptance order
}

func newSlotWorker(slots int) *slotWorker {
	return &slotWorker{slots: slots, active: map[string]string{}, done: map[string]bool{}}
}

func (w *slotWorker) executor() *mockTranscodeExecutor {
	return &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if _, ok := w.active[req.ID]; ok {
				return transcode.Job{ID: req.ID}, nil
			}
			if len(w.active) >= w.slots {
				return transcode.Job{}, workerBusyError(0)
			}
			w.active[req.ID] = req.SourcePath
			w.accepted = append(w.accepted, req.SourcePath)
			writeCandidateOutput(req.CandidatePath)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.done[jobID] {
				delete(w.active, jobID)
				return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted}, nil
			}
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning}, nil
		},
	}
}

func (w *slotWorker) finish(source string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, src := range w.active {
		if src == source {
			w.done[id] = true
		}
	}
}

func (w *slotWorker) acceptedSources() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.accepted...)
}

func writeSparseMedia(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Truncate(200_000_000)
	_, _ = f.WriteAt([]byte(name), 0)
	_ = f.Close()
	return path
}

func batchItemByPath(t *testing.T, e *Engine, st *store.Store, batchID, path string) store.TranscodeBatchItem {
	t.Helper()
	resolved, err := e.deps.Fs.ResolveRead(path)
	if err != nil {
		t.Fatal(err)
	}
	items, err := st.ListTranscodeBatchItems(batchID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.FilePath == resolved {
			return it
		}
	}
	t.Fatalf("no item for %s in %s", path, batchID)
	return store.TranscodeBatchItem{}
}

func actionState(t *testing.T, st *store.Store, id string) map[string]any {
	t.Helper()
	inst, err := st.GetActionInstance(id)
	if err != nil || inst == nil {
		t.Fatalf("reading %s: %v", id, err)
	}
	var state map[string]any
	_ = json.Unmarshal([]byte(inst.StateJSON), &state)
	return state
}

// Two batches share a worker with two slots. Batch A fills both; batch B's
// only file stays queued without being probed. When one of A's files
// finishes, A starts its third file: A was asked for first. B's file starts
// once A has nothing left to start.
func TestTranscodeQueueRunsBatchesInTheOrderTheyWereAsked(t *testing.T) {
	worker := newSlotWorker(2)
	e, st, _, srv, root := setupBatchTestEnv(t, worker.executor(), 2)
	defer st.Close()
	defer srv.Close()
	e.deps.Registry = nil
	ctx := context.Background()

	a1 := writeSparseMedia(t, root, "Batch A One.mkv")
	a2 := writeSparseMedia(t, root, "Batch A Two.mkv")
	a3 := writeSparseMedia(t, root, "Batch A Three.mkv")
	b1 := writeSparseMedia(t, root, "Batch B One.mkv")
	inputs := func(paths ...string) map[string]any {
		return map[string]any{"paths": paths, "profile": "general-hevc", "media_type": "movie", "dry_run": false}
	}

	batchA, err := e.Run(ctx, "transcode_batch", inputs(a1, a2, a3))
	if err != nil || batchA.Status != StatusWaitingExternal {
		t.Fatalf("batch A: %+v %v", batchA, err)
	}
	if got := worker.acceptedSources(); len(got) != 2 {
		t.Fatalf("batch A should fill both slots, accepted %v", got)
	}
	batchB, err := e.Run(ctx, "transcode_batch", inputs(b1))
	if err != nil || batchB.Status != StatusWaitingExternal {
		t.Fatalf("batch B: %+v %v", batchB, err)
	}
	if it := batchItemByPath(t, e, st, batchB.ID, b1); it.Status != "queued" || it.ChildActionID != "" {
		t.Fatalf("batch B's file must stay queued, unprobed, while the slots are taken: %+v", it)
	}

	// One of A's files finishes: A's third file takes the slot.
	worker.finish(worker.acceptedSources()[0])
	if _, err := e.Resume(ctx, batchA.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	got := worker.acceptedSources()
	aFiles := map[string]bool{mustResolve(t, e, a1): true, mustResolve(t, e, a2): true, mustResolve(t, e, a3): true}
	if len(got) != 3 || !aFiles[got[2]] || got[2] == got[0] || got[2] == got[1] {
		t.Fatalf("batch A's third file should get the freed slot, accepted %v", got)
	}
	if _, err := e.Resume(ctx, batchB.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if it := batchItemByPath(t, e, st, batchB.ID, b1); it.Status != "queued" {
		t.Fatalf("batch B started out of turn: %+v", it)
	}

	// Another of A's files finishes; A has nothing left to start, so B's file
	// goes next.
	worker.finish(got[1])
	if _, err := e.Resume(ctx, batchA.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(ctx, batchB.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := worker.acceptedSources(); len(got) != 4 || got[3] != mustResolve(t, e, b1) {
		t.Fatalf("batch B's file should start once A has none queued, accepted %v", got)
	}
}

// A single file waits behind the batches asked before it; with queue_priority
// it goes before them and takes the next free slot.
func TestTranscodeQueuePriorityGoesFirst(t *testing.T) {
	worker := newSlotWorker(1)
	e, st, _, srv, root := setupBatchTestEnv(t, worker.executor(), 1)
	defer st.Close()
	defer srv.Close()
	e.deps.Registry = nil
	ctx := context.Background()

	a1 := writeSparseMedia(t, root, "Batch A One.mkv")
	a2 := writeSparseMedia(t, root, "Batch A Two.mkv")
	plain := writeSparseMedia(t, root, "Plain One.mkv")
	urgent := writeSparseMedia(t, root, "Urgent One.mkv")
	batchA, err := e.Run(ctx, "transcode_batch", map[string]any{"paths": []string{a1, a2}, "profile": "general-hevc", "media_type": "movie", "dry_run": false})
	if err != nil || batchA.Status != StatusWaitingExternal {
		t.Fatalf("batch A: %+v %v", batchA, err)
	}
	queued, err := e.Run(ctx, "transcode_media", map[string]any{"path": plain, "profile": "general-hevc"})
	if err != nil || queued.WaitingCondition != "worker_busy" {
		t.Fatalf("a plain single file should wait in the queue: %+v %v", queued, err)
	}
	if pos := getInt(actionState(t, st, queued.ID), workerSlotPositionKey); pos != 2 {
		t.Fatalf("plain file behind batch A's queued file: position %d, want 2", pos)
	}
	first, err := e.Run(ctx, "transcode_media", map[string]any{"path": urgent, "profile": "general-hevc", "queue_priority": true})
	if err != nil || first.WaitingCondition != "worker_busy" {
		t.Fatalf("a priority file still waits for the running job: %+v %v", first, err)
	}
	if pos := getInt(actionState(t, st, first.ID), workerSlotPositionKey); pos != 1 {
		t.Fatalf("priority file: position %d, want 1", pos)
	}

	// A's file finishes: A does not start its next one, the priority file
	// takes the slot.
	worker.finish(worker.acceptedSources()[0])
	if _, err := e.Resume(ctx, batchA.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if it := batchItemByPath(t, e, st, batchA.ID, a2); it.Status != "queued" {
		t.Fatalf("batch A started its next file ahead of the priority file: %+v", it)
	}
	if res, err := e.Resume(ctx, queued.ID, "", nil); err != nil || res.WaitingCondition != "worker_busy" {
		t.Fatalf("the plain file went ahead of the priority one: %+v %v", res, err)
	}
	accepted, err := e.Resume(ctx, first.ID, "", nil)
	if err != nil || accepted.WaitingCondition != "transcode_complete" {
		t.Fatalf("the priority file should take the free slot: %+v %v", accepted, err)
	}
	if got := worker.acceptedSources(); len(got) != 2 || got[1] != mustResolve(t, e, urgent) {
		t.Fatalf("accepted %v", got)
	}
}

// Regression: on reconciler polls a batch left its own not-yet-due slot
// waiters alone and then started another queued file on every poll, so a
// max_parallel_jobs=1 batch ended up with every file probed, hashed and
// holding a ticket. While the worker is full, a batch starts no file.
func TestBatchStartsNoNewFileWhileItsOwnFileWaitsForSlot(t *testing.T) {
	worker := newSlotWorker(1)
	e, st, _, srv, root := setupBatchTestEnv(t, worker.executor(), 1)
	defer st.Close()
	defer srv.Close()
	e.deps.Registry = nil
	clock := &testClock{at: time.Now().UTC()}
	e.deps.Now = clock.Now
	ctx := context.Background()
	inputs := func(paths ...string) map[string]any {
		return map[string]any{"paths": paths, "profile": "general-hevc", "media_type": "movie", "dry_run": false}
	}
	if _, err := e.Run(ctx, "transcode_batch", inputs(writeSparseMedia(t, root, "Batch A One.mkv"))); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, n := range []string{"Batch B One.mkv", "Batch B Two.mkv", "Batch B Three.mkv", "Batch B Four.mkv"} {
		paths = append(paths, writeSparseMedia(t, root, n))
	}
	b, err := e.Run(ctx, "transcode_batch", inputs(paths...))
	if err != nil {
		t.Fatal(err)
	}
	count := func() (waiting, queued, children int) {
		items, _ := st.ListTranscodeBatchItems(b.ID)
		for _, it := range items {
			switch it.Status {
			case "waiting_for_slot":
				waiting++
			case "queued":
				queued++
			}
			if it.ChildActionID != "" {
				children++
			}
		}
		return
	}
	if w, q, c := count(); w != 0 || q != 4 || c != 0 {
		t.Fatalf("after run: waiting=%d queued=%d children=%d, want 0/4/0", w, q, c)
	}
	for i := range 3 {
		clock.Advance(10 * time.Second)
		if _, err := e.resume(ctx, b.ID, "", nil, true); err != nil {
			t.Fatal(err)
		}
		if w, q, c := count(); w != 0 || q != 4 || c != 0 {
			t.Fatalf("automatic poll %d started a file: waiting=%d queued=%d children=%d", i+1, w, q, c)
		}
	}
	if got := worker.acceptedSources(); len(got) != 1 {
		t.Fatalf("worker accepted %v, want only batch A's file", got)
	}
}

// A ticket holder deferred by its parent's lease (parent_busy) keeps its
// place only briefly: after slotParentBusyGrace without an own slot wait it
// stops blocking the others, and regains its place once it waits again.
func TestParentBusyWaiterHoldsQueueHeadOnlyForGrace(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	clock := &testClock{at: time.Now().UTC()}
	e := NewEngine(EngineDeps{Store: st, Now: clock.Now})
	now := clock.Now()
	seed := func(id, condition, parent string, since, polled time.Time) {
		if err := st.CreateActionInstance(store.ActionInstance{
			ID: id, ActionName: "transcode_media", Status: StatusWaitingExternal, WaitingCondition: condition,
			InputsJSON: toJSON(map[string]any{"parent_action_id": parent}),
			StateJSON: toJSON(map[string]any{
				workerSlotWaitSinceKey: since.Format(time.RFC3339Nano),
				workerSlotPolledAtKey:  polled.Format(time.RFC3339Nano),
			}),
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("held-by-parent", "parent_busy", "batch-a", now.Add(-10*time.Minute), now.Add(-30*time.Second))
	seed("other-batch", "worker_busy", "batch-b", now.Add(-time.Minute), now.Add(-10*time.Second))
	position := func() int {
		ec := &ExecutionContext{InstanceID: "other-batch", State: actionState(t, st, "other-batch"), Outputs: map[string]any{}}
		pos, err := e.slotQueuePosition(ec)
		if err != nil {
			t.Fatal(err)
		}
		return pos
	}
	if got := position(); got != 2 {
		t.Fatalf("a parent_busy waiter inside its grace keeps its place: position %d, want 2", got)
	}
	clock.Advance(slotParentBusyGrace)
	if got := position(); got != 1 {
		t.Fatalf("a parent_busy waiter past its grace still blocks the queue: position %d, want 1", got)
	}
	if e.foreignSlotWaiter("batch-b") {
		t.Fatal("a parent_busy waiter past its grace still holds batch B back")
	}
}

func mustResolve(t *testing.T, e *Engine, path string) string {
	t.Helper()
	resolved, err := e.deps.Fs.ResolveRead(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func seedSlotWaiter(t *testing.T, st *store.Store, id, name, status, condition, parent string, since time.Time) {
	t.Helper()
	inputs := map[string]any{}
	if parent != "" {
		inputs["parent_action_id"] = parent
	}
	if err := st.CreateActionInstance(store.ActionInstance{
		ID: id, ActionName: name, Status: status, WaitingCondition: condition,
		InputsJSON: toJSON(inputs),
		StateJSON:  toJSON(map[string]any{workerSlotWaitSinceKey: since.UTC().Format(time.RFC3339Nano)}),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerSlotTurnOrdersByFirstWaitAcrossBatchesAndStandalone(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := NewEngine(EngineDeps{Store: st})
	base := time.Now().Add(-time.Minute)
	seedSlotWaiter(t, st, "standalone-file", "transcode_media", StatusWaitingExternal, "worker_busy", "", base)
	seedSlotWaiter(t, st, "batch-b-child", "transcode_media", StatusWaitingExternal, "worker_busy", "batch-b", base.Add(time.Second))
	seedSlotWaiter(t, st, "batch-a-child", "transcode_media", StatusWaitingExternal, "parent_busy", "batch-a", base.Add(2*time.Second))
	// Not contending: paused parent, finished action.
	seedSlotWaiter(t, st, "paused-child", "transcode_media", StatusWaitingExternal, "parent_paused", "batch-c", base.Add(-time.Hour))
	seedSlotWaiter(t, st, "done-child", "transcode_media", StatusCompleted, "", "batch-c", base.Add(-time.Hour))

	turn := func(id string, state map[string]any) (StepResult, bool, int) {
		ec := &ExecutionContext{InstanceID: id, State: state, Outputs: map[string]any{}}
		res, waiting := e.workerSlotTurn(ec, "waiting")
		return res, waiting, getInt(ec.State, workerSlotPositionKey)
	}
	actionStateOf := func(id string) map[string]any { return actionState(t, st, id) }

	if _, waiting, pos := turn("standalone-file", actionStateOf("standalone-file")); waiting || pos != 1 {
		t.Fatalf("oldest waiter must go first: waiting=%v position=%d", waiting, pos)
	}
	res, waiting, pos := turn("batch-a-child", actionStateOf("batch-a-child"))
	if !waiting || pos != 3 || res.WaitingCondition != "worker_busy" {
		t.Fatalf("third waiter: waiting=%v position=%d %+v", waiting, pos, res)
	}
	// A newcomer without a ticket queues behind everyone and takes a ticket.
	fresh := map[string]any{}
	if _, waiting, pos := turn("newcomer", fresh); !waiting || pos != 4 || !hasSlotTicket(fresh) {
		t.Fatalf("newcomer: waiting=%v position=%d state=%v", waiting, pos, fresh)
	}
	work, _ := fresh["work_progress"].(map[string]any)
	if getString(work, "phase") != waitingForWorkerPhase || getInt(work, "queue_position") != 4 {
		t.Fatalf("newcomer work phase: %v", work)
	}
	if !e.foreignSlotWaiter("batch-a") || !e.foreignSlotWaiter("batch-b") {
		t.Fatal("each batch must see the other batch's and the standalone waiter")
	}
}

// A standalone encode: probing_source ends with the probe, the slot wait
// shows waiting_for_worker with its place, and acceptance clears it.
func TestStandaloneTranscodeWorkPhaseAroundSlotWait(t *testing.T) {
	worker := newSlotWorker(1)
	e, st, _, srv, root := setupBatchTestEnv(t, worker.executor(), 1)
	defer st.Close()
	defer srv.Close()
	ctx := context.Background()
	first := writeSparseMedia(t, root, "Standalone One.mkv")
	second := writeSparseMedia(t, root, "Standalone Two.mkv")

	running, err := e.Run(ctx, "transcode_media", map[string]any{"path": first, "profile": "general-hevc"})
	if err != nil || running.WaitingCondition != "transcode_complete" {
		t.Fatalf("first encode: %+v %v", running, err)
	}
	if work, ok := actionState(t, st, running.ID)["work_progress"]; ok {
		t.Fatalf("an accepted encode still shows a preflight or wait phase: %v", work)
	}

	waiting, err := e.Run(ctx, "transcode_media", map[string]any{"path": second, "profile": "general-hevc"})
	if err != nil || waiting.WaitingCondition != "worker_busy" {
		t.Fatalf("second encode should wait for a slot: %+v %v", waiting, err)
	}
	work, _ := actionState(t, st, waiting.ID)["work_progress"].(map[string]any)
	if getString(work, "phase") != waitingForWorkerPhase || getInt(work, "queue_position") != 1 || getString(work, "since") == "" {
		t.Fatalf("slot wait phase: %v", work)
	}

	worker.finish(mustResolve(t, e, first))
	if _, err := e.Resume(ctx, running.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	accepted, err := e.Resume(ctx, waiting.ID, "", nil)
	if err != nil || accepted.WaitingCondition != "transcode_complete" {
		t.Fatalf("second encode after a free slot: %+v %v", accepted, err)
	}
	state := actionState(t, st, waiting.ID)
	if work, ok := state["work_progress"]; ok {
		t.Fatalf("acceptance did not clear waiting_for_worker: %v", work)
	}
	if hasSlotTicket(state) {
		t.Fatal("acceptance did not release the queue ticket")
	}
}

func TestClearWaitingForWorkerClearsOnlyTheSlotWait(t *testing.T) {
	ec := &ExecutionContext{State: map[string]any{"work_progress": map[string]any{"phase": waitingForWorkerPhase}}, Outputs: map[string]any{"work_progress": map[string]any{"phase": waitingForWorkerPhase}}}
	clearWaitingForWorker(ec)
	if _, ok := ec.State["work_progress"]; ok {
		t.Fatal("waiting_for_worker survived the end of the wait in state")
	}
	if _, ok := ec.Outputs["work_progress"]; ok {
		t.Fatal("waiting_for_worker survived the end of the wait in outputs")
	}
	// Other phases are not the slot wait's to clear.
	ec.State["work_progress"] = map[string]any{"phase": "hashing_source"}
	clearWaitingForWorker(ec)
	if _, ok := ec.State["work_progress"]; !ok {
		t.Fatal("cleared a phase that is not the slot wait")
	}
}

func TestSlotWaitBackoffHonoursRetryAfterAndFreedSlot(t *testing.T) {
	clock := &testClock{at: time.Now().UTC()}
	e := NewEngine(EngineDeps{Now: clock.Now})
	state := map[string]any{workerSlotWaitSinceKey: clock.Now().Format(time.RFC3339Nano)}
	ec := &ExecutionContext{ActionName: "transcode_media", State: state, Outputs: map[string]any{}}

	// No Retry-After: 30-60 s.
	e.scheduleReconcile(ec, "worker_busy")
	next, _ := time.Parse(time.RFC3339Nano, getString(state, "next_poll_at"))
	if d := next.Sub(clock.Now()); d < slotWaitPollBase || d > slotWaitPollBase+slotWaitPollJitter {
		t.Fatalf("slot wait poll delay %s outside 30-60 s", d)
	}
	// Retry-After from the worker (already including the 1 s margin) wins.
	state[workerBusyNotBeforeKey] = clock.Now().Add(11 * time.Second).Format(time.RFC3339Nano)
	e.scheduleReconcile(ec, "worker_busy")
	next, _ = time.Parse(time.RFC3339Nano, getString(state, "next_poll_at"))
	if d := next.Sub(clock.Now()); d != 11*time.Second {
		t.Fatalf("Retry-After not honoured: next poll in %s", d)
	}
	// A slot freed after the last poll wakes the waiter once Retry-After passed.
	delete(state, workerBusyNotBeforeKey)
	e.scheduleReconcile(ec, "worker_busy")
	if e.slotWakeDue(state) {
		t.Fatal("woken without a freed slot")
	}
	clock.Advance(time.Second)
	e.noteWorkerSlotFreed()
	if !e.slotWakeDue(state) {
		t.Fatal("a freed slot did not wake the waiter")
	}
	state[workerBusyNotBeforeKey] = clock.Now().Add(5 * time.Second).Format(time.RFC3339Nano)
	if e.slotWakeDue(state) {
		t.Fatal("a freed slot overrode the worker's Retry-After")
	}
}
