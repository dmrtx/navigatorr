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
// only file waits. When one of A's files finishes, A must not start its third
// file: the freed slot belongs to B's file, which has waited longer.
func TestWorkerSlotQueueGivesFreedSlotToLongestWaitingBatch(t *testing.T) {
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
	waiter := batchItemByPath(t, e, st, batchB.ID, b1)
	if waiter.Status != "waiting_for_slot" || waiter.ChildActionID == "" {
		t.Fatalf("batch B's file should wait for a slot: %+v", waiter)
	}
	state := actionState(t, st, waiter.ChildActionID)
	if !hasSlotTicket(state) {
		t.Fatalf("a refused file must hold a queue ticket: %v", state)
	}
	if work, _ := state["work_progress"].(map[string]any); getString(work, "phase") != waitingForWorkerPhase {
		t.Fatalf("waiting file should show waiting_for_worker, got %v", state["work_progress"])
	}

	// One of A's files finishes; A resumes first (it owns the finished job).
	first := worker.acceptedSources()[0]
	worker.finish(first)
	if _, err := e.Resume(ctx, batchA.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := worker.acceptedSources(); len(got) != 2 {
		t.Fatalf("batch A took the freed slot out of turn: accepted %v", got)
	}
	third := ""
	started := map[string]bool{}
	for _, src := range worker.acceptedSources() {
		started[src] = true
	}
	for _, p := range []string{a1, a2, a3} {
		if !started[mustResolve(t, e, p)] {
			third = p
		}
	}
	if it := batchItemByPath(t, e, st, batchA.ID, third); it.Status != "queued" {
		t.Fatalf("batch A started another file while batch B's file waits: %+v", it)
	}

	// B's waiter now gets the slot.
	if _, err := e.Resume(ctx, batchB.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	got := worker.acceptedSources()
	if len(got) != 3 || got[2] != mustResolve(t, e, b1) {
		t.Fatalf("batch B's waiting file should get the freed slot next, accepted %v", got)
	}
	state = actionState(t, st, waiter.ChildActionID)
	if hasSlotTicket(state) {
		t.Fatalf("an accepted job must leave the queue: %v", state[workerSlotWaitSinceKey])
	}
	if work, _ := state["work_progress"].(map[string]any); getString(work, "phase") == waitingForWorkerPhase {
		t.Fatalf("accepted job still shows waiting_for_worker: %v", work)
	}

	// With B's file running, A's next file may try again (and waits on the
	// full worker in its own turn).
	if _, err := e.Resume(ctx, batchA.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if it := batchItemByPath(t, e, st, batchA.ID, third); it.Status != "waiting_for_slot" {
		t.Fatalf("batch A's next file should now queue for a slot: %+v", it)
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
	seedSlotWaiter(t, st, "standalone-bench", "benchmark_transcode", StatusWaitingExternal, "worker_busy", "", base)
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

	if _, waiting, pos := turn("standalone-bench", actionStateOf("standalone-bench")); waiting || pos != 1 {
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
