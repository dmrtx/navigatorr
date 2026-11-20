package action

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// batchIndex is the index of the item with this id.
func batchIndex(t *testing.T, items []store.TranscodeBatchItem, id int64) int {
	t.Helper()
	for i, it := range items {
		if it.ID == id {
			return i
		}
	}
	t.Fatalf("item %d not in the batch", id)
	return -1
}

func containsIdx(list []int, idx int) bool {
	for _, i := range list {
		if i == idx {
			return true
		}
	}
	return false
}

// linkChild gives item key a child conversion that waits for a slot with the
// key its batch's creation gives the item, as the engine stamps it.
func linkChild(t *testing.T, e *Engine, batchID, itemKey, childID string, itemID int64) {
	t.Helper()
	parent, err := e.deps.Store.GetActionInstance(batchID)
	if err != nil {
		t.Fatal(err)
	}
	key := queueOrder(queueRankPlain, parseStoreTime(parent.CreatedAt), batchID, itemID)
	seedActionInstance(t, e.deps.Store, childID, "transcode_media", StatusWaitingExternal, 0, "",
		map[string]any{"parent_action_id": batchID, "batch_item_key": itemKey},
		map[string]any{transcodeQueueOrderKey: key})
	if err := e.deps.Store.UpdateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: batchID, ItemKey: itemKey,
		FilePath: "/media/" + batchID + "/File" + itemKey[1:] + ".mkv", Status: "waiting_for_slot", ChildActionID: childID}); err != nil {
		t.Fatal(err)
	}
}

// An item moved ahead of an earlier one of its batch is the one the batch
// starts; the earlier one's child, already waiting, does not hold it back.
func TestAMovedBatchItemIsScheduledAheadOfAnEarlierOne(t *testing.T) {
	ctx := context.Background()
	t.Run("both queued", func(t *testing.T) {
		e := controlEngine(t)
		busy(t, e, "busy-m1", "m1") // one free slot
		ids := seedQueueBatch(t, e, "batch-1", 2)
		if _, err := e.MoveQueued(ctx, item(ids[1]), firstMove(), nil); err != nil {
			t.Fatal(err)
		}
		items, _ := e.deps.Store.ListTranscodeBatchItems("batch-1")
		got, ok := e.batchDispatchCandidates(ctx, "batch-1", items)
		if !ok || len(got) != 1 || got[0] != batchIndex(t, items, ids[1]) {
			t.Fatalf("candidates %v (ok %v), want only the moved item-2", got, ok)
		}
	})
	t.Run("item-1 already waits with a child", func(t *testing.T) {
		e := controlEngine(t)
		busy(t, e, "busy-m1", "m1")
		ids := seedQueueBatch(t, e, "batch-1", 2)
		linkChild(t, e, "batch-1", "k1", "child-1", ids[0])
		if _, err := e.MoveQueued(ctx, item(ids[1]), firstMove(), nil); err != nil {
			t.Fatal(err)
		}
		items, _ := e.deps.Store.ListTranscodeBatchItems("batch-1")
		got, ok := e.batchDispatchCandidates(ctx, "batch-1", items)
		if !ok || !containsIdx(got, batchIndex(t, items, ids[1])) {
			t.Fatalf("candidates %v (ok %v): the moved item-2 was not scheduled, and child-1 waits for it", got, ok)
		}
		if got[0] != batchIndex(t, items, ids[1]) {
			t.Fatalf("candidates %v: the moved item is not first", got)
		}
		// child-1 gives way to item-2; item-2's child takes the slot: no deadlock.
		if w, _, _ := turn(t, e, "child-1"); w != "" {
			t.Fatalf("child-1 was given %q ahead of the moved item-2", w)
		}
		seedActionInstance(t, e.deps.Store, "child-2", "transcode_media", StatusWaitingExternal, 0, "",
			map[string]any{"parent_action_id": "batch-1", "batch_item_key": "k2"}, map[string]any{})
		if err := e.deps.Store.UpdateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-1", ItemKey: "k2", FilePath: "/media/batch-1/File2.mkv", Status: "waiting_for_slot", ChildActionID: "child-2"}); err != nil {
			t.Fatal(err)
		}
		if w, _, _ := turn(t, e, "child-2"); w == "" {
			t.Fatal("the moved item's child was not given the free slot")
		}
	})
	t.Run("a standalone waiter between", func(t *testing.T) {
		e := controlEngine(t) // two free slots
		ids := seedQueueBatch(t, e, "batch-1", 2)
		waiter(t, e, "s1", 10*time.Minute, map[string]any{}, nil)
		linkChild(t, e, "batch-1", "k1", "child-1", ids[0])
		if _, err := e.MoveQueued(ctx, item(ids[1]), afterMove(act("s1")), nil); err != nil {
			t.Fatal(err)
		}
		sameIDs(t, queueIDs(t, e), "s1", strconv.FormatInt(ids[1], 10), "child-1")
		items, _ := e.deps.Store.ListTranscodeBatchItems("batch-1")
		got, ok := e.batchDispatchCandidates(ctx, "batch-1", items)
		if !ok || !containsIdx(got, batchIndex(t, items, ids[1])) {
			t.Fatalf("candidates %v (ok %v): the moved item-2 was not scheduled", got, ok)
		}
	})
}

// faultEngine is controlEngine on a database file the test can break.
func faultEngine(t *testing.T) (*Engine, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fault.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{
		{Name: "m4", Executor: diskNode(1000*gib, 10*gib, 500*gib), MaxParallelJobs: 1, PublishPolicy: transcode.PublishPolicyOnApproval},
		{Name: "m1", Executor: diskNode(1000*gib, 10*gib, 500*gib), MaxParallelJobs: 1, PublishPolicy: transcode.PublishPolicyOnApproval, Podcast: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	return NewEngine(EngineDeps{Store: st, Config: &config.Config{}, Workers: set}), raw
}

// When the queue's marks cannot be read, nothing starts: a conversion waits,
// and a batch schedules nothing new.
func TestAnUnreadableQueueFailsClosed(t *testing.T) {
	e, raw := faultEngine(t)
	waiter(t, e, "a", time.Minute, map[string]any{}, nil)
	seedQueueBatch(t, e, "batch-1", 1)
	if _, err := raw.Exec(`DROP TABLE transcode_queue_marks`); err != nil {
		t.Fatal(err)
	}
	ec := parseExecutionContextMust(t, e, "a")
	res, waiting, _ := e.transcodeQueueTurn(context.Background(), ec)
	if !waiting || res.Status != StepWaitingExternal || res.WaitingCondition != "worker_busy" {
		t.Fatalf("turn with an unreadable queue: %+v waiting=%v", res, waiting)
	}
	if assignedWorker(ec.State) != "" {
		t.Fatal("a conversion was assigned with an unreadable queue")
	}
	items, _ := e.deps.Store.ListTranscodeBatchItems("batch-1")
	if _, ok := e.withoutHeldItems("batch-1", items); ok {
		t.Fatal("withoutHeldItems returned the items unfiltered after a read error")
	}
	if got, ok := e.batchDispatchCandidates(context.Background(), "batch-1", items); ok || len(got) != 0 {
		t.Fatalf("the batch scheduled %v with unreadable holds", got)
	}
}

// A hold an item had when its child was made moves to the child; releasing the
// child must leave the item free too.
func TestReleasingAChildClearsItsItemsHold(t *testing.T) {
	e := controlEngine(t)
	ctx := context.Background()
	ids := seedQueueBatch(t, e, "batch-1", 1)
	if _, err := e.HoldQueued(ctx, item(ids[0]), nil); err != nil {
		t.Fatal(err)
	}
	seedActionInstance(t, e.deps.Store, "child-k1", "transcode_media", StatusWaitingExternal, 0, "", map[string]any{"parent_action_id": "batch-1", "batch_item_key": "k1"}, map[string]any{})
	if err := e.deps.Store.UpdateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-1", ItemKey: "k1", FilePath: "/media/batch-1/File1.mkv", Status: "waiting_for_slot", ChildActionID: "child-k1"}); err != nil {
		t.Fatal(err)
	}
	if w, cond, _ := turn(t, e, "child-k1"); w != "" || cond != "queue_held" {
		t.Fatalf("the held item's child: %q %q", w, cond)
	}
	if m, _ := e.deps.Store.GetQueueItemMark(ids[0]); m.Held {
		t.Fatal("the item kept its own hold after the child took it over")
	}
	if _, err := e.ReleaseQueued(ctx, act("child-k1"), nil); err != nil {
		t.Fatal(err)
	}
	held, err := e.deps.Store.HeldBatchItemIDs("batch-1")
	if err != nil || len(held) != 0 {
		t.Fatalf("held items after the release: %v %v", held, err)
	}
	items, _ := e.deps.Store.ListTranscodeBatchItems("batch-1")
	if got, ok := e.batchDispatchCandidates(ctx, "batch-1", items); !ok || len(got) != 1 {
		t.Fatalf("the released item is not scheduled: %v %v", got, ok)
	}
}

// A write decided on a revision that has moved is refused whole, and the
// refusal is the stale-revision conflict, even when the revision moves after
// the check (a writer that does not take queueMu).
func TestAQueueWriteChecksTheRevisionAtomically(t *testing.T) {
	ctx := context.Background()
	e := controlEngine(t)
	waiter(t, e, "a", 2*time.Minute, map[string]any{}, nil)
	waiter(t, e, "b", time.Minute, map[string]any{}, nil)
	v, _ := e.TranscodeQueueView()
	rev := v.Revision
	e.afterQueueCheck = func() { _, _ = e.deps.Store.ApplyQueueWrite(store.QueueWrite{}) }
	for name, call := range map[string]func() error{
		"hold": func() error { _, err := e.HoldQueued(ctx, act("a"), &rev); return err },
		"move": func() error { _, err := e.MoveQueued(ctx, act("b"), firstMove(), &rev); return err },
	} {
		err := call()
		if queueErrKind(err) != QueueConflict || !strings.Contains(err.Error(), "queue_revision") {
			t.Errorf("%s with a revision that moved: %v", name, err)
		}
		// Each attempt moves the revision further: the next caller reads the
		// current one.
		rev, _ = e.deps.Store.QueueRevision()
	}
	e.afterQueueCheck = nil
	if ids := queueIDs(t, e); ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("a stale write changed the queue: %v", ids)
	}
	if m, _ := e.deps.Store.GetQueueMark(store.QueueMarkAction, "a"); m.Held {
		t.Fatal("a stale hold was applied")
	}
}

func TestQueueWriteExpectRevision(t *testing.T) {
	e := controlEngine(t)
	rev, _ := e.deps.Store.QueueRevision()
	stale := rev - 1
	_, err := e.deps.Store.ApplyQueueWrite(store.QueueWrite{ExpectRevision: &stale, ActionHeld: map[string]bool{"x": true}})
	if _, ok := err.(*store.StaleQueueRevisionError); !ok {
		t.Fatalf("stale write: %v", err)
	}
	if m, _ := e.deps.Store.GetQueueMark(store.QueueMarkAction, "x"); m.Held {
		t.Fatal("a refused write was applied")
	}
	if now, _ := e.deps.Store.QueueRevision(); now != rev {
		t.Fatalf("a refused write moved the revision: %d -> %d", rev, now)
	}
	if _, err := e.deps.Store.ApplyQueueWrite(store.QueueWrite{ExpectRevision: &rev}); err != nil {
		t.Fatal(err)
	}
}

// The view takes no lock: read while a batch hold is inside Resume it returns
// at once, and a write that carries its (older) revision is refused as stale
// once the hold has advanced it.
func TestTheQueueViewDoesNotWaitForABatchHold(t *testing.T) {
	e, batch := pausedBatchEngine(t)
	waiter(t, e, "a", time.Minute, map[string]any{}, nil)
	var seen QueueView
	var viewErr error
	e.afterQueueCheck = func() {
		done := make(chan struct{})
		go func() {
			seen, viewErr = e.TranscodeQueueView()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("the view waited for a batch hold that was inside Resume")
		}
	}
	if _, err := e.ReleaseQueued(context.Background(), bat(batch), nil); err != nil {
		t.Fatal(err)
	}
	e.afterQueueCheck = nil
	if viewErr != nil {
		t.Fatal(viewErr)
	}
	cur, _ := e.deps.Store.QueueRevision()
	if seen.Revision >= cur {
		t.Fatalf("the view saw revision %d, the release moved the store to %d", seen.Revision, cur)
	}
	_, err := e.HoldQueued(context.Background(), act("a"), &seen.Revision)
	if queueErrKind(err) != QueueConflict || !strings.Contains(err.Error(), "queue_revision") {
		t.Fatalf("a write with the revision of a view read during the hold: %v", err)
	}
}

// pausedBatchEngine is an engine with a real batch that starts paused.
func pausedBatchEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	mock := &mockTranscodeExecutor{submitFunc: func(_ context.Context, req transcode.Request) (transcode.Job, error) {
		writeCandidateOutput(req.CandidatePath)
		return transcode.Job{ID: req.ID}, nil
	}}
	e, st, _, srv, root := setupBatchTestEnv(t, mock, 1)
	t.Cleanup(func() { srv.Close(); st.Close() })
	e.deps.Registry = nil
	r, err := e.Run(context.Background(), "transcode_batch", map[string]any{
		"paths": []string{filepath.Join(root, "Moriko S01E01-E02.mkv")}, "profile": "general-hevc", "paused": true, "media_type": "movie",
	})
	if err != nil || r.Status != StatusWaitingDecision {
		t.Fatalf("paused batch: %+v %v", r, err)
	}
	return e, r.ID
}

// While a batch release is between its check and its Resume, another change
// that holds the revision from before is refused as stale once the release is
// done: nobody can act on the new revision with the batch still unchanged.
func TestABatchReleaseIsOneStepAgainstOtherQueueChanges(t *testing.T) {
	e, batch := pausedBatchEngine(t)
	ctx := context.Background()
	waiter(t, e, "a", time.Minute, map[string]any{}, nil)
	v, err := e.TranscodeQueueView()
	if err != nil {
		t.Fatal(err)
	}
	rev := v.Revision
	other := make(chan error, 1)
	fired := false
	e.afterQueueCheck = func() {
		if fired {
			return
		}
		fired = true
		go func() { _, err := e.HoldQueued(ctx, act("a"), &rev); other <- err }()
		// It must wait for the release, not run between its check and its end.
		time.Sleep(100 * time.Millisecond)
		select {
		case err := <-other:
			t.Errorf("another change ran in the middle of the release: %v", err)
		default:
		}
	}
	if _, err := e.ReleaseQueued(ctx, bat(batch), &rev); err != nil {
		t.Fatal(err)
	}
	err = <-other
	if queueErrKind(err) != QueueConflict || !strings.Contains(err.Error(), "queue_revision") {
		t.Fatalf("the change on the old revision: %v", err)
	}
	if m, _ := e.deps.Store.GetQueueMark(store.QueueMarkAction, "a"); m.Held {
		t.Fatal("the stale hold was applied")
	}
	inst, _ := e.deps.Store.GetActionInstance(batch)
	if inst.Status == StatusWaitingDecision {
		t.Fatalf("the batch was not released: %s", inst.Status)
	}
}

// A hold and a release of one batch, asked with the same revision at the same
// time, run one after the other in either order: the release always succeeds
// and ends the pause, the revision moves once, and the hold either did nothing
// (it came first) or was refused as stale (it came after the release).
func TestConcurrentBatchHoldAndReleaseSerialise(t *testing.T) {
	e, batch := pausedBatchEngine(t)
	ctx := context.Background()
	v, _ := e.TranscodeQueueView()
	before := v.Revision
	var wg sync.WaitGroup
	errs := map[bool]error{}
	var mu sync.Mutex
	for _, hold := range []bool{true, false} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if hold {
				_, err = e.HoldQueued(ctx, bat(batch), &before)
			} else {
				_, err = e.ReleaseQueued(ctx, bat(batch), &before)
			}
			mu.Lock()
			errs[hold] = err
			mu.Unlock()
		}()
	}
	wg.Wait()
	if errs[false] != nil {
		t.Fatalf("the release: %v", errs[false])
	}
	if err := errs[true]; err != nil && (queueErrKind(err) != QueueConflict || !strings.Contains(err.Error(), "queue_revision")) {
		t.Fatalf("the hold was refused for another reason than a stale revision: %v", err)
	}
	if after, _ := e.deps.Store.QueueRevision(); after != before+1 {
		t.Fatalf("revision %d -> %d, want one move for the one change", before, after)
	}
	if inst, _ := e.deps.Store.GetActionInstance(batch); inst.Status == StatusWaitingDecision {
		t.Fatalf("the batch is still paused: %s", inst.Status)
	}
}

// A batch that was held but whose revision could not be advanced says so, is
// not a conflict, and still wakes the reconciler.
func TestABatchHoldWhoseRevisionBumpFailsIsReported(t *testing.T) {
	e, raw := faultEngine(t)
	seedQueueBatch(t, e, "batch-1", 1)
	for len(e.reconcileWake) > 0 {
		<-e.reconcileWake
	}
	e.afterQueueCheck = func() {
		if _, err := raw.Exec(`DROP TABLE transcode_queue_revision`); err != nil {
			t.Error(err)
		}
	}
	_, err := e.HoldQueued(context.Background(), bat("batch-1"), nil)
	if err == nil || queueErrKind(err) != 0 || !strings.Contains(err.Error(), "revision could not be advanced") {
		t.Fatalf("hold with a failing bump: %v", err)
	}
	select {
	case <-e.reconcileWake:
	default:
		t.Fatal("the reconciler was not woken")
	}
}

// A child whose state cannot be read fails the batch's keys closed.
func TestAnUnreadableChildStateFailsClosed(t *testing.T) {
	e := controlEngine(t)
	ids := seedQueueBatch(t, e, "batch-1", 1)
	if err := e.deps.Store.CreateActionInstance(store.ActionInstance{ID: "child-1", ActionName: "transcode_media", Status: StatusWaitingExternal, InputsJSON: "{}", OutputsJSON: "{}", StateJSON: "not json"}); err != nil {
		t.Fatal(err)
	}
	if err := e.deps.Store.UpdateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-1", ItemKey: "k1", FilePath: "/media/batch-1/File1.mkv", Status: "waiting_for_slot", ChildActionID: "child-1"}); err != nil {
		t.Fatal(err)
	}
	items, _ := e.deps.Store.ListTranscodeBatchItems("batch-1")
	if _, ok := e.batchItemKeys("batch-1", items); ok {
		t.Fatalf("keys were derived for item %d whose child's state is unreadable", ids[0])
	}
	if k, err := e.childQueueKey("gone"); err != nil || k != "" {
		t.Fatalf("a child that no longer exists: %q %v", k, err)
	}
}

// When the keys of the batch's items cannot be read the batch schedules
// nothing new, rather than ordering on guessed keys.
func TestUnreadableItemKeysFailClosed(t *testing.T) {
	e, raw := faultEngine(t)
	seedQueueBatch(t, e, "batch-1", 2)
	items, _ := e.deps.Store.ListTranscodeBatchItems("batch-1")
	if got, ok := e.batchDispatchCandidates(context.Background(), "batch-1", items); !ok || len(got) == 0 {
		t.Fatalf("readable: %v %v", got, ok)
	}
	// The holds (queue_held) stay readable; the order column does not.
	if _, err := raw.Exec(`ALTER TABLE transcode_batch_items DROP COLUMN queue_order`); err != nil {
		t.Fatal(err)
	}
	if got, ok := e.batchDispatchCandidates(context.Background(), "batch-1", items); ok || len(got) != 0 {
		t.Fatalf("the batch scheduled %v with unreadable keys (ok %v)", got, ok)
	}
	if _, ok := e.batchItemKeys("batch-1", items); ok {
		t.Fatal("keys were returned after a read error")
	}
}

// A hold and a move wake the reconciler, as a release does.
func TestHoldAndMoveWakeTheReconciler(t *testing.T) {
	e := controlEngine(t)
	ctx := context.Background()
	waiter(t, e, "a", 2*time.Minute, map[string]any{}, nil)
	waiter(t, e, "b", time.Minute, map[string]any{}, nil)
	woken := func() bool {
		select {
		case <-e.reconcileWake:
			return true
		default:
			return false
		}
	}
	woken()
	if _, err := e.HoldQueued(ctx, act("a"), nil); err != nil || !woken() {
		t.Fatalf("hold did not wake the reconciler (%v)", err)
	}
	if _, err := e.MoveQueued(ctx, act("b"), firstMove(), nil); err != nil || !woken() {
		t.Fatalf("move did not wake the reconciler (%v)", err)
	}
}

// Two waiting children that cannot be tied to their items would get the same
// key in the block: the move is refused instead.
func TestMovingABatchWithUntiedChildrenIsRefused(t *testing.T) {
	e := controlEngine(t)
	seedQueueBatch(t, e, "batch-1", 0)
	waiter(t, e, "x1", 3*time.Minute, map[string]any{"parent_action_id": "batch-1"}, nil)
	waiter(t, e, "x2", 2*time.Minute, map[string]any{"parent_action_id": "batch-1"}, nil)
	waiter(t, e, "s1", time.Minute, map[string]any{}, nil)
	before := queueIDs(t, e)
	_, err := e.MoveQueued(context.Background(), bat("batch-1"), afterMove(act("s1")), nil)
	if queueErrKind(err) != QueueConflict {
		t.Fatalf("moving a batch with children tied to no item: %v, want a conflict", err)
	}
	if got := queueIDs(t, e); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("a refused move changed the queue: %v -> %v", before, got)
	}
}

// slotWaiterWithKey is a conversion of no batch that waits for a worker slot
// and has a place in the transcode queue.
func slotWaiterWithKey(t *testing.T, e *Engine, id string, age time.Duration) {
	t.Helper()
	at := e.now()
	if err := e.deps.Store.CreateActionInstance(store.ActionInstance{
		ID: id, ActionName: "transcode_media", Status: StatusWaitingExternal, WaitingCondition: "worker_busy",
		InputsJSON: "{}", OutputsJSON: "{}",
		StateJSON: toJSON(map[string]any{
			transcodeQueueOrderKey: queueOrder(queueRankPlain, at.Add(-age), id, 0),
			workerSlotWaitSinceKey: at.Add(-age).UTC().Format(time.RFC3339Nano),
		}),
	}); err != nil {
		t.Fatal(err)
	}
}

// A batch item the operator put ahead of a waiting conversion is started, and
// the conversion waits behind it; with nothing moved the older conversion
// still goes first.
func TestAMovedItemAheadOfAForeignWaiterIsScheduled(t *testing.T) {
	ctx := context.Background()
	e := controlEngine(t)
	busy(t, e, "busy-m1", "m1") // one free slot
	ids := seedQueueBatch(t, e, "batch-1", 1)
	slotWaiterWithKey(t, e, "s", 10*time.Minute)
	items, _ := e.deps.Store.ListTranscodeBatchItems("batch-1")
	if got, ok := e.batchDispatchCandidates(ctx, "batch-1", items); !ok || len(got) != 0 {
		t.Fatalf("with the older waiter first the batch started %v (ok %v)", got, ok)
	}
	if _, err := e.MoveQueued(ctx, item(ids[0]), beforeMove(act("s")), nil); err != nil {
		t.Fatal(err)
	}
	got, ok := e.batchDispatchCandidates(ctx, "batch-1", items)
	if !ok || len(got) != 1 {
		t.Fatalf("the moved item was not scheduled: %v (ok %v)", got, ok)
	}
	if w, _, _ := turn(t, e, "s"); w != "" {
		t.Fatalf("s was given %q ahead of the moved item", w)
	}
	// A held waiter in front holds no one back.
	if _, err := e.MoveQueued(ctx, item(ids[0]), afterMove(act("s")), nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.batchDispatchCandidates(ctx, "batch-1", items); len(got) != 0 {
		t.Fatalf("the batch started %v behind s", got)
	}
	if _, err := e.HoldQueued(ctx, act("s"), nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.batchDispatchCandidates(ctx, "batch-1", items); len(got) != 1 {
		t.Fatalf("a held waiter held the batch back: %v", got)
	}
}

// When the queue cannot be read a batch may start no new item.
func TestBatchAllowanceFailsClosed(t *testing.T) {
	e, raw := faultEngine(t)
	seedQueueBatch(t, e, "batch-1", 1)
	if n := e.batchQueueAllowance(context.Background(), "batch-1"); n < 1 {
		t.Fatalf("readable allowance %d", n)
	}
	if _, err := raw.Exec(`ALTER TABLE transcode_batch_items DROP COLUMN queue_order`); err != nil {
		t.Fatal(err)
	}
	if n := e.batchQueueAllowance(context.Background(), "batch-1"); n != 0 {
		t.Fatalf("allowance %d with an unreadable queue", n)
	}
}

// A batch with nothing queued has nothing to limit.
func TestBatchAllowanceWithNothingQueuedIsUnlimited(t *testing.T) {
	e := controlEngine(t)
	seedActionInstance(t, e.deps.Store, "batch-1", "transcode_batch", StatusWaitingExternal, 0, "", map[string]any{}, map[string]any{})
	if n := e.batchQueueAllowance(context.Background(), "batch-1"); n != math.MaxInt {
		t.Fatalf("allowance %d for a batch with nothing queued", n)
	}
}

// An item's place, held or not, goes to its child's mark in one write with the
// end of the item's own: the position survives a crash before the child's
// state is saved.
func TestAnItemsPlaceMovesToItsChildsMarkAtOnce(t *testing.T) {
	e := controlEngine(t)
	ctx := context.Background()
	waiter(t, e, "s1", 10*time.Minute, map[string]any{}, nil)
	ids := seedQueueBatch(t, e, "batch-1", 1)
	if _, err := e.MoveQueued(ctx, item(ids[0]), firstMove(), nil); err != nil { // moved, not held
		t.Fatal(err)
	}
	m, _ := e.deps.Store.GetQueueItemMark(ids[0])
	itemKey := m.Order
	if itemKey == "" {
		t.Fatal("the item has no place of its own")
	}
	seedActionInstance(t, e.deps.Store, "child-k1", "transcode_media", StatusWaitingExternal, 0, "", map[string]any{"parent_action_id": "batch-1", "batch_item_key": "k1"}, map[string]any{})
	if err := e.deps.Store.UpdateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-1", ItemKey: "k1", FilePath: "/media/batch-1/File1.mkv", Status: "waiting_for_slot", ChildActionID: "child-k1"}); err != nil {
		t.Fatal(err)
	}
	ec := parseExecutionContextMust(t, e, "child-k1")
	key, err := e.ensureQueueOrder(ec)
	if err != nil || key != itemKey {
		t.Fatalf("key %q (%v), want %q", key, err, itemKey)
	}
	// The state is not saved (a crash): the place is still the child's.
	if cm, _ := e.deps.Store.GetQueueMark(store.QueueMarkAction, "child-k1"); cm.Order != itemKey {
		t.Fatalf("the child's mark has place %q, want %q", cm.Order, itemKey)
	}
	if im, _ := e.deps.Store.GetQueueItemMark(ids[0]); im.Order != "" || im.Held {
		t.Fatalf("the item kept its own mark: %+v", im)
	}
	again := parseExecutionContextMust(t, e, "child-k1")
	if k2, err := e.ensureQueueOrder(again); err != nil || k2 != itemKey {
		t.Fatalf("after the crash the child's key is %q (%v)", k2, err)
	}
}

// A hold or release of a batch that cannot be resumed is refused, and the
// revision does not move.
func TestAFailedBatchHoldDoesNotMoveTheRevision(t *testing.T) {
	e := controlEngine(t)
	seedQueueBatch(t, e, "batch-x", 1)
	before, _ := e.deps.Store.QueueRevision()
	// The pause runs under a context that is already over, so it fails.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.HoldQueued(ctx, bat("batch-x"), &before)
	if queueErrKind(err) != QueueConflict {
		t.Fatalf("hold of a batch that cannot be paused: %v", err)
	}
	if after, _ := e.deps.Store.QueueRevision(); after != before {
		t.Fatalf("a failed hold moved the revision: %d -> %d", before, after)
	}
}

func TestBatchReleaseWakesTheReconciler(t *testing.T) {
	e, batch := pausedBatchEngine(t)
	for len(e.reconcileWake) > 0 {
		<-e.reconcileWake
	}
	if _, err := e.ReleaseQueued(context.Background(), bat(batch), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-e.reconcileWake:
	default:
		t.Fatal("releasing a batch did not wake the reconciler")
	}
}
