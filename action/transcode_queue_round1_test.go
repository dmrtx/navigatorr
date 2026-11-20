package action

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
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
// refusal is the stale-revision conflict.
func TestAQueueWriteChecksTheRevisionAtomically(t *testing.T) {
	ctx := context.Background()
	e := controlEngine(t)
	waiter(t, e, "a", 2*time.Minute, map[string]any{}, nil)
	waiter(t, e, "b", time.Minute, map[string]any{}, nil)
	seedQueueBatch(t, e, "batch-1", 1)
	v, _ := e.TranscodeQueueView()
	rev := v.Revision
	// Every read of the queue is followed by another change: the revision a
	// caller holds is current when the view is read and stale at the write.
	e.afterQueueRead = func() { _, _ = e.deps.Store.ApplyQueueWrite(store.QueueWrite{}) }
	for name, call := range map[string]func() error{
		"hold":       func() error { _, err := e.HoldQueued(ctx, act("a"), &rev); return err },
		"move":       func() error { _, err := e.MoveQueued(ctx, act("b"), firstMove(), &rev); return err },
		"batch hold": func() error { _, err := e.HoldQueued(ctx, bat("batch-1"), &rev); return err },
	} {
		err := call()
		if queueErrKind(err) != QueueConflict || !strings.Contains(err.Error(), "queue_revision") {
			t.Errorf("%s with a revision that moved: %v", name, err)
		}
		// Each attempt moves the revision further: the next caller reads the
		// current one.
		rev, _ = e.deps.Store.QueueRevision()
	}
	e.afterQueueRead = nil
	if ids := queueIDs(t, e); ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("a stale write changed the queue: %v", ids)
	}
	if m, _ := e.deps.Store.GetQueueMark(store.QueueMarkAction, "a"); m.Held {
		t.Fatal("a stale hold was applied")
	}
	inst, _ := e.deps.Store.GetActionInstance("batch-1")
	if strings.Contains(inst.StateJSON, `"paused":true`) {
		t.Fatalf("a stale batch hold paused the batch: %s", inst.StateJSON)
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

// A change between the reads of the view is read again: the entries and the
// revision belong together.
func TestTheQueueViewIsReadAgainWhenItChangesUnderneath(t *testing.T) {
	e := controlEngine(t)
	waiter(t, e, "a", 2*time.Minute, map[string]any{}, nil)
	waiter(t, e, "b", time.Minute, map[string]any{}, nil)
	once := true
	e.afterQueueRead = func() {
		if once {
			once = false
			if _, err := e.deps.Store.ApplyQueueWrite(store.QueueWrite{ActionHeld: map[string]bool{"a": true}}); err != nil {
				t.Error(err)
			}
		}
	}
	v, err := e.TranscodeQueueView()
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := e.deps.Store.QueueRevision()
	if v.Revision != cur || !v.Entries[0].Held {
		t.Fatalf("view at revision %d (store at %d), first entry %+v", v.Revision, cur, v.Entries[0])
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
	if err == nil {
		t.Fatal("the batch was moved with children that are tied to no item")
	}
	if got := queueIDs(t, e); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("a refused move changed the queue: %v -> %v", before, got)
	}
}
