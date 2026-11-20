package action

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

func controlEngine(t *testing.T) *Engine {
	t.Helper()
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	return e
}

// order is the queue as ids, in order.
func queueIDs(t *testing.T, e *Engine) []string {
	t.Helper()
	v, err := e.TranscodeQueueView()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i, en := range v.Entries {
		if en.Position != i+1 {
			t.Fatalf("entry %s has position %d, want %d", en.ID, en.Position, i+1)
		}
		ids = append(ids, en.ID)
	}
	return ids
}

func sameIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("queue = %v, want %v", got, want)
	}
}

func act(id string) QueueRef          { return QueueRef{Kind: QueueKindAction, ID: id} }
func bat(id string) QueueRef          { return QueueRef{Kind: QueueKindBatch, ID: id} }
func item(id int64) QueueRef          { return QueueRef{Kind: QueueKindItem, ID: strconv.FormatInt(id, 10)} }
func firstMove() QueueMove            { return QueueMove{First: true} }
func beforeMove(r QueueRef) QueueMove { return QueueMove{Before: &r} }
func afterMove(r QueueRef) QueueMove  { return QueueMove{After: &r} }

func finish(t *testing.T, e *Engine, id string) {
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

func queueErrKind(err error) QueueErrorKind {
	var qe *QueueError
	if errors.As(err, &qe) {
		return qe.Kind
	}
	return 0
}

// A held waiter never gets a worker, says why, and does not hold anyone behind
// it back; released, it is where it was.
func TestAHeldWaiterNeverStartsAndHoldsNoOneBack(t *testing.T) {
	e := controlEngine(t)
	ctx := context.Background()
	busy(t, e, "busy-m1", "m1")
	waiter(t, e, "a", 3*time.Minute, map[string]any{}, nil)
	waiter(t, e, "b", 2*time.Minute, map[string]any{}, nil)
	waiter(t, e, "c", time.Minute, map[string]any{}, nil)

	// Without a hold b waits behind a, which would take the free slot.
	if w, _, _ := turn(t, e, "b"); w != "" {
		t.Fatalf("b was given %q ahead of a", w)
	}
	if _, err := e.HoldQueued(ctx, act("a"), nil); err != nil {
		t.Fatal(err)
	}
	w, cond, reason := turn(t, e, "a")
	if w != "" || cond != "queue_held" || !strings.HasPrefix(reason, "Held: it keeps its place") {
		t.Fatalf("held a: worker %q, condition %q, reason %q", w, cond, reason)
	}
	// Still first in line, flagged held.
	v, _ := e.TranscodeQueueView()
	if v.Entries[0].ID != "a" || !v.Entries[0].Held || v.Entries[0].HeldBy != "conversion" {
		t.Fatalf("held a in the queue: %+v", v.Entries[0])
	}
	if w, _, _ := turn(t, e, "b"); w != "m4" {
		t.Fatalf("b behind the held a was given %q, want m4", w)
	}
	finish(t, e, "busy-m1")
	if w, _, _ := turn(t, e, "a"); w != "" {
		t.Fatalf("a took %q while held", w)
	}
	// Release puts it back in the same place: it is ahead of c again.
	if _, err := e.ReleaseQueued(ctx, act("a"), nil); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, queueIDs(t, e), "a", "c")
	if w, _, _ := turn(t, e, "c"); w != "" {
		t.Fatalf("c was given %q ahead of the released a", w)
	}
	if w, _, _ := turn(t, e, "a"); w != "m1" {
		t.Fatalf("the released a was given %q, want m1", w)
	}
}

func TestHoldAndReleaseAreIdempotentAndNeedAWaiter(t *testing.T) {
	e := controlEngine(t)
	ctx := context.Background()
	busy(t, e, "running", "m1")
	waiter(t, e, "a", time.Minute, map[string]any{}, nil)
	for i := 0; i < 2; i++ {
		if _, err := e.HoldQueued(ctx, act("a"), nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := e.ReleaseQueued(ctx, act("a"), nil); err != nil {
			t.Fatal(err)
		}
	}
	// A conversion that has a worker cannot be held or moved.
	_, err := e.HoldQueued(ctx, act("running"), nil)
	if queueErrKind(err) != QueueConflict || !strings.Contains(err.Error(), "already has a worker") || !strings.Contains(err.Error(), "cancel") {
		t.Fatalf("hold on an assigned conversion: %v", err)
	}
	if _, err := e.MoveQueued(ctx, act("running"), firstMove(), nil); queueErrKind(err) != QueueConflict {
		t.Fatalf("move of an assigned conversion: %v", err)
	}
	if _, err := e.HoldQueued(ctx, act("nobody"), nil); queueErrKind(err) != QueueNotFound {
		t.Fatalf("hold on a missing action: %v", err)
	}
	if _, err := e.HoldQueued(ctx, item(99), nil); queueErrKind(err) != QueueNotFound {
		t.Fatalf("hold on a missing item: %v", err)
	}
	if _, err := e.HoldQueued(ctx, bat("nobody"), nil); queueErrKind(err) != QueueNotFound {
		t.Fatalf("hold on a missing batch: %v", err)
	}
}

func TestMoveFirstBeforeAndAfterPlaceItExactlyAndNobodyElseMoves(t *testing.T) {
	e := controlEngine(t)
	ctx := context.Background()
	for i, id := range []string{"a", "b", "c", "d", "e"} {
		waiter(t, e, id, time.Duration(10-i)*time.Minute, map[string]any{}, nil)
	}
	keys := func() map[string]string {
		v, _ := e.TranscodeQueueView()
		m := map[string]string{}
		for _, en := range v.Entries {
			m[en.ID] = en.key
		}
		return m
	}
	start := keys()
	if _, err := e.MoveQueued(ctx, act("d"), firstMove(), nil); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, queueIDs(t, e), "d", "a", "b", "c", "e")
	if _, err := e.MoveQueued(ctx, act("a"), afterMove(act("e")), nil); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, queueIDs(t, e), "d", "b", "c", "e", "a")
	if _, err := e.MoveQueued(ctx, act("e"), beforeMove(act("b")), nil); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, queueIDs(t, e), "d", "e", "b", "c", "a")
	if _, err := e.MoveQueued(ctx, act("c"), afterMove(act("d")), nil); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, queueIDs(t, e), "d", "c", "e", "b", "a")
	// Only the moved ones have another key.
	now := keys()
	if now["b"] != start["b"] {
		t.Fatalf("b was renumbered: %q -> %q", start["b"], now["b"])
	}
	// Moving it where it already is changes nothing.
	v1, _ := e.TranscodeQueueView()
	if _, err := e.MoveQueued(ctx, act("c"), beforeMove(act("e")), nil); err != nil {
		t.Fatal(err)
	}
	v2, _ := e.TranscodeQueueView()
	if v1.Revision != v2.Revision {
		t.Fatalf("a move to where it already is moved the revision: %d -> %d", v1.Revision, v2.Revision)
	}
	// Bad requests.
	if _, err := e.MoveQueued(ctx, act("c"), beforeMove(act("c")), nil); queueErrKind(err) != QueueInvalid {
		t.Fatalf("move relative to itself: %v", err)
	}
	if _, err := e.MoveQueued(ctx, act("c"), beforeMove(act("nobody")), nil); queueErrKind(err) != QueueNotFound {
		t.Fatalf("move before a missing action: %v", err)
	}
	busy(t, e, "running", "m1")
	if _, err := e.MoveQueued(ctx, act("c"), beforeMove(act("running")), nil); queueErrKind(err) != QueueConflict {
		t.Fatalf("move before a conversion that has a worker: %v", err)
	}
	if _, err := e.MoveQueued(ctx, act("c"), QueueMove{}, nil); queueErrKind(err) != QueueInvalid {
		t.Fatalf("move with nowhere to go: %v", err)
	}
	a, b := act("a"), act("b")
	if _, err := e.MoveQueued(ctx, act("c"), QueueMove{Before: &a, After: &b}, nil); queueErrKind(err) != QueueInvalid {
		t.Fatalf("move with two places: %v", err)
	}
}

// A moved conversion is ahead, and takes the slot, as the queue says.
func TestAMovedWaiterTakesTheFreeSlotAheadOfOlderOnes(t *testing.T) {
	e := controlEngine(t)
	ctx := context.Background()
	busy(t, e, "busy-m1", "m1")
	waiter(t, e, "old", 10*time.Minute, map[string]any{}, nil)
	waiter(t, e, "new", time.Minute, map[string]any{}, nil)
	if w, _, _ := turn(t, e, "new"); w != "" {
		t.Fatalf("new was given %q behind old", w)
	}
	if _, err := e.MoveQueued(ctx, act("new"), firstMove(), nil); err != nil {
		t.Fatal(err)
	}
	if w, _, _ := turn(t, e, "old"); w != "" {
		t.Fatalf("old was given %q although new was moved ahead of it", w)
	}
	if w, _, _ := turn(t, e, "new"); w != "m4" {
		t.Fatalf("the moved waiter was given %q, want m4", w)
	}
}

func TestAStaleQueueRevisionIsRefused(t *testing.T) {
	e := controlEngine(t)
	ctx := context.Background()
	waiter(t, e, "a", 2*time.Minute, map[string]any{}, nil)
	waiter(t, e, "b", time.Minute, map[string]any{}, nil)
	v, _ := e.TranscodeQueueView()
	rev := v.Revision
	if _, err := e.HoldQueued(ctx, act("a"), &rev); err != nil {
		t.Fatal(err)
	}
	// The hold moved the revision: the same one is now stale.
	if _, err := e.MoveQueued(ctx, act("b"), firstMove(), &rev); queueErrKind(err) != QueueConflict || !strings.Contains(err.Error(), "queue_revision") {
		t.Fatalf("stale revision: %v", err)
	}
	if _, err := e.ReleaseQueued(ctx, act("a"), &rev); queueErrKind(err) != QueueConflict {
		t.Fatalf("stale revision on release: %v", err)
	}
	sameIDs(t, queueIDs(t, e), "a", "b")
	v, _ = e.TranscodeQueueView()
	if _, err := e.MoveQueued(ctx, act("b"), firstMove(), &v.Revision); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, queueIDs(t, e), "b", "a")
}

func seedQueueBatch(t *testing.T, e *Engine, id string, items int) []int64 {
	t.Helper()
	seedActionInstance(t, e.deps.Store, id, "transcode_batch", StatusWaitingExternal, 0, "", map[string]any{}, map[string]any{})
	var ids []int64
	for i := 1; i <= items; i++ {
		if err := e.deps.Store.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: id, ItemKey: "k" + strconv.Itoa(i), FilePath: "/media/" + id + "/File" + strconv.Itoa(i) + ".mkv", Status: "queued"}); err != nil {
			t.Fatal(err)
		}
		it, err := e.deps.Store.GetTranscodeBatchItem(id, "k"+strconv.Itoa(i))
		if err != nil || it == nil {
			t.Fatal(err)
		}
		ids = append(ids, it.ID)
	}
	return ids
}

func TestMovingABatchMovesItsWaitingEntriesAsOneBlock(t *testing.T) {
	e := controlEngine(t)
	ctx := context.Background()
	waiter(t, e, "s1", 10*time.Minute, map[string]any{}, nil)
	waiter(t, e, "s2", 9*time.Minute, map[string]any{}, nil)
	ids := seedQueueBatch(t, e, "batch-1", 3)
	// A child of the batch already waiting for a worker.
	waiter(t, e, "child-1", 5*time.Minute, map[string]any{"parent_action_id": "batch-1", "batch_item_key": "k1"}, nil)
	if err := e.deps.Store.UpdateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-1", ItemKey: "k1", FilePath: "/media/batch-1/File1.mkv", Status: "waiting_for_slot", ChildActionID: "child-1"}); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, queueIDs(t, e), "s1", "s2", "child-1", strconv.FormatInt(ids[1], 10), strconv.FormatInt(ids[2], 10))

	if _, err := e.MoveQueued(ctx, bat("batch-1"), firstMove(), nil); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, queueIDs(t, e), "child-1", strconv.FormatInt(ids[1], 10), strconv.FormatInt(ids[2], 10), "s1", "s2")
	// Between the two singles.
	if _, err := e.MoveQueued(ctx, bat("batch-1"), afterMove(act("s1")), nil); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, queueIDs(t, e), "s1", "child-1", strconv.FormatInt(ids[1], 10), strconv.FormatInt(ids[2], 10), "s2")
	// Relative to a batch: before it, the single goes ahead of the whole block.
	if _, err := e.MoveQueued(ctx, act("s2"), beforeMove(bat("batch-1")), nil); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, queueIDs(t, e), "s1", "s2", "child-1", strconv.FormatInt(ids[1], 10), strconv.FormatInt(ids[2], 10))
	if _, err := e.MoveQueued(ctx, act("s1"), afterMove(bat("batch-1")), nil); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, queueIDs(t, e), "s2", "child-1", strconv.FormatInt(ids[1], 10), strconv.FormatInt(ids[2], 10), "s1")
	// A batch cannot be moved next to one of its own entries.
	if _, err := e.MoveQueued(ctx, bat("batch-1"), beforeMove(act("child-1")), nil); queueErrKind(err) != QueueInvalid {
		t.Fatalf("a batch before its own child: %v", err)
	}
	// A child the batch creates later takes the block's key, so it keeps its place.
	if _, err := e.MoveQueued(ctx, bat("batch-1"), firstMove(), nil); err != nil {
		t.Fatal(err)
	}
	seedActionInstance(t, e.deps.Store, "child-3", "transcode_media", StatusWaitingExternal, 0, "", map[string]any{"parent_action_id": "batch-1", "batch_item_key": "k3"}, map[string]any{})
	if w, _, _ := turn(t, e, "child-3"); w != "" {
		t.Fatalf("child-3 was given %q behind child-1", w)
	}
	v, _ := e.TranscodeQueueView()
	var k1, k3, s2 string
	for _, en := range v.Entries {
		switch en.ID {
		case "child-1":
			k1 = en.key
		case "child-3":
			k3 = en.key
		case "s2":
			s2 = en.key
		}
	}
	if !(k1 < k3 && k3 < s2) || !strings.HasSuffix(k3, "|"+pad20(ids[2])) {
		t.Fatalf("keys: child-1 %q, child-3 %q, s2 %q", k1, k3, s2)
	}
}

func pad20(n int64) string {
	s := strconv.FormatInt(n, 10)
	return strings.Repeat("0", 20-len(s)) + s
}

func TestAnItemHoldAndOrderCarryOverToItsChild(t *testing.T) {
	e := controlEngine(t)
	ctx := context.Background()
	waiter(t, e, "s1", 10*time.Minute, map[string]any{}, nil)
	waiter(t, e, "s2", 9*time.Minute, map[string]any{}, nil)
	ids := seedQueueBatch(t, e, "batch-1", 2)
	if _, err := e.HoldQueued(ctx, item(ids[0]), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MoveQueued(ctx, item(ids[0]), beforeMove(act("s2")), nil); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, queueIDs(t, e), "s1", strconv.FormatInt(ids[0], 10), "s2", strconv.FormatInt(ids[1], 10))
	v, _ := e.TranscodeQueueView()
	if !v.Entries[1].Held || v.Entries[1].WaitingCondition != "queue_held" {
		t.Fatalf("held item entry: %+v", v.Entries[1])
	}
	itemKey := v.Entries[1].key

	// Its child appears: it is the item's place, and held.
	seedActionInstance(t, e.deps.Store, "child-k1", "transcode_media", StatusWaitingExternal, 0, "", map[string]any{"parent_action_id": "batch-1", "batch_item_key": "k1"}, map[string]any{})
	if err := e.deps.Store.UpdateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-1", ItemKey: "k1", FilePath: "/media/batch-1/File1.mkv", Status: "waiting_for_slot", ChildActionID: "child-k1"}); err != nil {
		t.Fatal(err)
	}
	w, cond, _ := turn(t, e, "child-k1")
	if w != "" || cond != "queue_held" {
		t.Fatalf("the held item's child: worker %q, condition %q", w, cond)
	}
	v, _ = e.TranscodeQueueView()
	var got *QueueEntry
	for i := range v.Entries {
		if v.Entries[i].ID == "child-k1" {
			got = &v.Entries[i]
		}
	}
	if got == nil || !got.Held || got.key != itemKey {
		t.Fatalf("child entry %+v, want held with key %q", got, itemKey)
	}
	sameIDs(t, queueIDs(t, e), "s1", "child-k1", "s2", strconv.FormatInt(ids[1], 10))
	// The item itself is no longer a queue entry: its hold is the child's now.
	if _, err := e.HoldQueued(ctx, item(ids[0]), nil); queueErrKind(err) != QueueConflict || !strings.Contains(err.Error(), "child-k1") {
		t.Fatalf("hold on an item that has a conversion: %v", err)
	}
	if _, err := e.ReleaseQueued(ctx, act("child-k1"), nil); err != nil {
		t.Fatal(err)
	}
	if w, _, _ := turn(t, e, "s1"); w == "" {
		t.Fatal("s1 was not given a worker")
	}
}

// Held conversions and items do not count among what is ahead.
func TestAHeldWaiterAheadOfABatchLeavesItsAllowance(t *testing.T) {
	e := controlEngine(t)
	ctx := context.Background()
	busy(t, e, "busy-m1", "m1")
	waiter(t, e, "old-a", 10*time.Minute, map[string]any{}, nil)
	seedQueueBatch(t, e, "batch-1", 1)
	if n := e.batchQueueAllowance(ctx, "batch-1"); n != 0 {
		t.Fatalf("the batch may start %d item(s) with old-a waiting for the free slot", n)
	}
	if _, err := e.HoldQueued(ctx, act("old-a"), nil); err != nil {
		t.Fatal(err)
	}
	if n := e.batchQueueAllowance(ctx, "batch-1"); n != 1 {
		t.Fatalf("the batch may start %d item(s); the held old-a holds the slot against it", n)
	}
}

// Held items stay out of what the batch's fan-out schedules, and a held child
// waiting for a slot does not keep the queued items behind it from starting.
func TestHeldItemsAreLeftOutOfTheFanOut(t *testing.T) {
	e := controlEngine(t)
	ctx := context.Background()
	ids := seedQueueBatch(t, e, "batch-1", 3)
	for _, id := range ids[:2] {
		if _, err := e.HoldQueued(ctx, item(id), nil); err != nil {
			t.Fatal(err)
		}
	}
	items, _ := e.deps.Store.ListTranscodeBatchItems("batch-1")
	view, _ := e.withoutHeldItems("batch-1", items)
	if view[0].Status != "queue_held" || view[1].Status != "queue_held" || view[2].Status != "queued" {
		t.Fatalf("view: %s %s %s", view[0].Status, view[1].Status, view[2].Status)
	}
	if items[0].Status != "queued" {
		t.Fatal("the batch's own items were changed")
	}
	// A held child waiting for a slot is left out the same way.
	if err := e.deps.Store.UpdateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-1", ItemKey: "k3", FilePath: "/media/batch-1/File3.mkv", Status: "waiting_for_slot", ChildActionID: "c3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deps.Store.ApplyQueueWrite(store.QueueWrite{ActionHeld: map[string]bool{"c3": true}}); err != nil {
		t.Fatal(err)
	}
	items, _ = e.deps.Store.ListTranscodeBatchItems("batch-1")
	view, _ = e.withoutHeldItems("batch-1", items)
	if view[2].Status != "queue_held" {
		t.Fatalf("the held child's item is %s", view[2].Status)
	}
}

func TestAPausedBatchShowsItsEntriesHeldByTheBatch(t *testing.T) {
	e := controlEngine(t)
	seedActionInstance(t, e.deps.Store, "batch-p", "transcode_batch", StatusWaitingDecision, 0, "", map[string]any{}, map[string]any{"paused": true})
	if err := e.deps.Store.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-p", ItemKey: "k1", FilePath: "/media/p/File.mkv", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	// The queue does not wait for it.
	items, _ := e.deps.Store.ListQueuedBatchItems()
	if len(items) != 0 {
		t.Fatalf("a paused batch's item counts ahead: %+v", items)
	}
	v, err := e.TranscodeQueueView()
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Entries) != 1 || !v.Entries[0].Held || v.Entries[0].HeldBy != "batch" || v.Entries[0].Path != "/media/p/File.mkv" {
		t.Fatalf("entries: %+v", v.Entries)
	}
}
