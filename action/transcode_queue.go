package action

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// Transcode queue.
//
// Conversions start in the order they were asked for, across batches and
// single files. Each batch used to fill the workers for itself, and a worker
// queues every job it is sent, so every batch kept a job queued on every
// worker and the workers ran them round robin: the oldest batch got one turn
// in N, and each new batch slowed down every older one.
//
// Now a conversion is assigned a worker only when that worker has a free
// video slot and it is the conversion's turn, so no worker holds a backlog
// and the server keeps the order. The order key is:
//   - a single file: its creation time;
//   - a batch item: its batch's creation time, then its place in the batch;
//   - a whole batch asked with queue_priority=true: before everything not asked
//     first, in creation order among such batches, and its files stay together;
//   - a single file asked with queue_priority=true: before everything else, so
//     a long batch asked first does not hold it back, in creation order among
//     such files.
//
// Either still waits for the job running on a worker to end; it only goes
// first in the queue. Pausing a batch takes it out of the way of the rest.
//
// A conversion takes a worker when fewer conversions are ahead of it than
// there are free slots. Ahead are the conversions waiting for a worker with a
// smaller key (transcode_queue_order, stamped the first time one waits) and
// the queued items of live batches with a smaller key: those items have no
// action yet, but their turn comes first. A batch starts a new child only
// for the free slots no older conversion is waiting for.
//
// The queue state is in rows the engine already persists, so a restart keeps
// the order. If the store cannot be read a conversion waits and looks again
// shortly: an unread hold or order must not let it start out of turn.
const (
	transcodeQueueOrderKey = "transcode_queue_order"
	// transcodeQueueSinceKey is when the conversion took its place.
	transcodeQueueSinceKey = "transcode_queue_since"
)

// queueHeldCondition is the waiting condition of a conversion the operator
// holds: it keeps its place in the queue and takes no worker until released.
const (
	queueHeldCondition = "queue_held"
	queueHeldReason    = "Held: it keeps its place in the queue and starts only after you release it"
)

// queueHeldPollDelay is how often a held conversion looks again at its hold.
const queueHeldPollDelay = 15 * time.Second

// queueOrderTimeLayout has a fixed width so keys compare as strings.
const queueOrderTimeLayout = "2006-01-02T15:04:05.000000000Z"

// Queue ranks, first to last. Conversions already waiting keep their stamped
// keys, so the values cannot be renumbered: "0~|" sorts after any "0|" key
// because '~' follows '|', and before any "1|" key.
const (
	queueRankFileFirst  = "0"
	queueRankBatchFirst = "0~"
	queueRankPlain      = "1"
)

func fileQueueRank(first bool) string {
	if first {
		return queueRankFileFirst
	}
	return queueRankPlain
}

func batchQueueRank(first bool) string {
	if first {
		return queueRankBatchFirst
	}
	return queueRankPlain
}

// queueOrder builds a sortable queue key: rank, time, group (the batch or the
// action) and the place within the group.
func queueOrder(rank string, at time.Time, group string, seq int64) string {
	return fmt.Sprintf("%s|%s|%s|%020d", rank, at.UTC().Format(queueOrderTimeLayout), group, seq)
}

// instanceAskedFirst reports an action created with queue_priority=true.
func instanceAskedFirst(inst *store.ActionInstance) bool {
	var inputs map[string]any
	if json.Unmarshal([]byte(inst.InputsJSON), &inputs) != nil {
		return false
	}
	return getBool(inputs, "queue_priority")
}

// batchItemQueueOrder is a queued item's key: the one the operator gave the
// item, else its batch's key prefix (the batch was moved as a block) with its
// place in the batch, else the key its batch's creation gives it.
func batchItemQueueOrder(r store.QueuedBatchItemRow) string {
	switch {
	case r.ItemOrder != "":
		return r.ItemOrder
	case r.BatchOrder != "":
		return batchBlockKey(r.BatchOrder, r.ItemID)
	}
	return queueOrder(batchQueueRank(r.Priority), parseStoreTime(r.BatchCreatedAt), r.BatchID, r.ItemID)
}

// batchBlockKey is the key of an item of a batch that was moved: the batch's
// prefix, then the item's place in the batch (keyBetween keeps the prefix
// from being a prefix of the next key, so the block stays in its place).
func batchBlockKey(prefix string, itemID int64) string {
	return fmt.Sprintf("%s|%020d", prefix, itemID)
}

func parseStoreTime(s string) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, s)
	return at
}

// transcodeQueueWaiting reports whether a conversion is waiting in the
// transcode queue for a worker.
func transcodeQueueWaiting(state map[string]any) bool {
	return getString(state, transcodeQueueOrderKey) != "" && assignedWorker(state) == ""
}

// batchChildItem is the batch item a child action runs, nil when it has none
// yet (found by the item key the child was given, by the link the item keeps
// to its child, or, for a profile batch whose link is made at the child's first
// run, by the child's idempotency key).
func (e *Engine) batchChildItem(instanceID string, inputs map[string]any) (*store.TranscodeBatchItem, error) {
	parentID := getString(inputs, "parent_action_id")
	if itemKey := getString(inputs, "batch_item_key"); itemKey != "" {
		return e.deps.Store.GetTranscodeBatchItem(parentID, itemKey)
	}
	item, err := e.deps.Store.FindTranscodeBatchItemByChildActionID(instanceID)
	if err != nil || item != nil {
		return item, err
	}
	key, err := e.childIdempotencyKey(instanceID)
	if err != nil {
		return nil, err
	}
	return e.deps.Store.FindTranscodeBatchItemByChildKey(key)
}

// ensureQueueOrder returns the conversion's queue key, stamping it the first
// time. A key the operator gave it (a move) wins over the stamped one.
func (e *Engine) ensureQueueOrder(ec *ExecutionContext) (string, error) {
	if e.deps.Store != nil {
		mark, err := e.deps.Store.GetQueueMark(store.QueueMarkAction, ec.InstanceID)
		if err != nil {
			return "", err
		}
		if mark.Order != "" {
			return mark.Order, nil
		}
	}
	if key := getString(ec.State, transcodeQueueOrderKey); key != "" {
		return key, nil
	}
	var key string
	if parentID := getString(ec.Inputs, "parent_action_id"); parentID != "" {
		parent, err := e.deps.Store.GetActionInstanceIfExists(parentID)
		if err != nil {
			return "", err
		}
		if parent == nil {
			return "", fmt.Errorf("parent %s not found", parentID)
		}
		item, err := e.batchChildItem(ec.InstanceID, ec.Inputs)
		if err != nil {
			return "", err
		}
		// Without its item the child goes before the rest of its batch.
		var seq int64
		if item != nil {
			seq = item.ID
		}
		key = queueOrder(batchQueueRank(instanceAskedFirst(parent)), parseStoreTime(parent.CreatedAt), parentID, seq)
		if item != nil {
			// What the operator did to the item, or to the batch as a block,
			// before this child existed carries over to it.
			itemMark, err := e.deps.Store.GetQueueItemMark(item.ID)
			if err != nil {
				return "", err
			}
			batchMark, err := e.deps.Store.GetQueueMark(store.QueueMarkBatch, parentID)
			if err != nil {
				return "", err
			}
			switch {
			case itemMark.Order != "":
				key = itemMark.Order
			case batchMark.Order != "":
				key = batchBlockKey(batchMark.Order, item.ID)
			}
			if itemMark.Held {
				// The hold moves to the child; the item's own hold ends in the
				// same write, or releasing the child would leave the item held.
				if _, err := e.deps.Store.ApplyQueueWrite(store.QueueWrite{
					ActionHeld: map[string]bool{ec.InstanceID: true},
					ItemHeld:   map[int64]bool{item.ID: false},
					ItemOrder:  map[int64]string{item.ID: ""},
				}); err != nil {
					return "", err
				}
			}
		}
	} else {
		inst, err := e.deps.Store.GetActionInstanceIfExists(ec.InstanceID)
		if err != nil {
			return "", err
		}
		if inst == nil {
			return "", fmt.Errorf("action %s not found", ec.InstanceID)
		}
		key = queueOrder(fileQueueRank(getBool(ec.Inputs, "queue_priority")), parseStoreTime(inst.CreatedAt), ec.InstanceID, 0)
	}
	ec.State[transcodeQueueOrderKey] = key
	ec.State[transcodeQueueSinceKey] = e.now().Format(time.RFC3339Nano)
	return key, nil
}

// queueSlots are the ready workers and the free video slots of each: its
// video capacity minus the conversions assigned to it. A worker already past
// its capacity frees nothing for the others.
type queueSlots struct {
	workers []transcode.NamedWorker
	ready   []bool
	free    map[string]int
	placer  *placer
}

func (e *Engine) queueSlots(ctx context.Context) (queueSlots, error) {
	workers := e.deps.Workers.All()
	ready := e.placementReady(ctx, workers)
	counts, err := e.deps.Store.CountActiveWorkerActions("transcode_media")
	if err != nil {
		return queueSlots{}, err
	}
	active := map[string]int{}
	for name, n := range counts {
		active[e.resolveWorker(name)] += n
	}
	free := map[string]int{}
	for i, w := range workers {
		if ready[i] {
			free[w.Name] = max(0, e.workerVideoCapacity(ctx, w)-active[w.Name])
		}
	}
	return queueSlots{workers: workers, ready: ready, free: free, placer: e.newPlacer(ctx, workers, ready)}, nil
}

// total is how many more conversions the ready workers can take.
func (q queueSlots) total() int {
	n := 0
	for _, f := range q.free {
		n += f
	}
	return n
}

// transcodeQueueBefore lists the claims of the conversions ahead of key:
// waiters with a smaller key (self excluded) and queued batch items with a
// smaller key. ahead counts those that could be given a worker cur could also
// be given, which is the place cur holds in the queue. A waiter no worker can
// take goes first without holding anything back: it waits for room elsewhere.
func (e *Engine) transcodeQueueBefore(q queueSlots, cur queueClaim, key, self string) ([]queueClaim, int, error) {
	waiters, err := e.deps.Store.ListTranscodeQueueWaiters()
	if err != nil {
		return nil, 0, err
	}
	var before []queueClaim
	ahead := 0
	for _, w := range waiters {
		// A held conversion keeps its place but starts nothing, so it holds no
		// one back.
		if w.ID == self || w.QueueOrder >= key || w.Held {
			continue
		}
		// Something other than the queue holds a waiter that has not been
		// polled for this long; it must not block everyone behind it.
		if updated, err := time.Parse(time.RFC3339Nano, w.UpdatedAt); err == nil && e.now().Sub(updated) > slotWaiterStaleAfter {
			continue
		}
		c := e.claimFor(q, e.waiterPlacement(w))
		before = append(before, c)
		if cur.competesWith(c) {
			ahead++
		}
	}
	items, err := e.deps.Store.ListQueuedBatchItems()
	if err != nil {
		return nil, 0, err
	}
	for _, it := range items {
		if !it.Held && batchItemQueueOrder(it) < key {
			c := e.claimFor(q, placement{})
			before = append(before, c)
			if cur.competesWith(c) {
				ahead++
			}
		}
	}
	return before, ahead, nil
}

// sourceSizeRetry is how often the size of a source that could not be read is
// read again.
const sourceSizeRetry = time.Minute

// sourceSizeCheckedKey is when the server last tried to read the source's size.
const sourceSizeCheckedKey = "source_size_checked_at"

// sourceSizeUnreadableKey marks a conversion whose source size the server
// can never read (no path, outside the read roots, a missing, irregular or
// empty file). It is then placed as needing no room, as it was before sizes
// were read: the node it is given fails it with the real error. Without the
// mark an unknown size reaches no node (placement.sizeUnknown).
const sourceSizeUnreadableKey = "source_size_unreadable"

// ensureSourceSize records the size of the conversion's source in its state
// when it has none (I3): one created before sizes were recorded, or whose stat
// did not answer then, is read at its turn, at most once per sourceSizeRetry.
// A read that fails for good marks it unreadable; only a stat that does not
// answer is tried again. Without a file resolver nothing can be read and the
// conversion is placed as needing no room. The size, the mark and the time of
// the attempt are saved with the state the turn leaves (a wait or the
// assignment). It runs before the queue lock is taken: the stat is bounded but
// is I/O.
func (e *Engine) ensureSourceSize(ec *ExecutionContext) {
	if ec.ActionName != "transcode_media" || e.deps.Fs == nil || getInt64(ec.State, SubmittedSourceBytesKey) > 0 || getBool(ec.State, sourceSizeUnreadableKey) {
		return
	}
	now := e.now()
	if at := parseStoreTime(getString(ec.State, sourceSizeCheckedKey)); !at.IsZero() && now.Sub(at) < sourceSizeRetry {
		return
	}
	ec.State[sourceSizeCheckedKey] = now.Format(time.RFC3339Nano)
	size, definitive := e.readFileSize(getString(ec.Inputs, "path"))
	switch {
	case size > 0:
		ec.State[SubmittedSourceBytesKey] = size
	case definitive:
		ec.State[sourceSizeUnreadableKey] = true
	}
}

// transcodeQueueTurn decides whether a conversion without a worker may be
// assigned one now. It returns waiting=true with a worker_busy wait, and the
// conversion's place in the queue, when it is not its turn. When it is, within
// are the workers it may be given without a conversion ahead of it losing a
// slot (nil: no restriction). Call it under bindMu so two conversions cannot
// both take the last free slot.
func (e *Engine) transcodeQueueTurn(ctx context.Context, ec *ExecutionContext) (res StepResult, waiting bool, within map[string]bool) {
	if e.deps.Store == nil || ec.InstanceID == "" {
		return StepResult{}, false, nil
	}
	// A child of a paused, decision-waiting or cancelled batch takes no
	// worker: its submit would stop at the parent check while the worker
	// counted it, holding a slot every other conversion waits for.
	if parentID := getString(ec.Inputs, "parent_action_id"); parentID != "" {
		if policy, err := e.parentPolicy(parentID); err == nil && policy != "" {
			return e.parentBlockedResult("worker assignment", policy), true, nil
		}
	}
	key, err := e.ensureQueueOrder(ec)
	if err != nil {
		log.Printf("transcode queue: ordering %s: %v (waiting)", ec.InstanceID, err)
		return queueUnreadableWait(), true, nil
	}
	held, err := e.deps.Store.GetQueueMark(store.QueueMarkAction, ec.InstanceID)
	if err != nil {
		log.Printf("transcode queue: reading the hold of %s: %v (waiting)", ec.InstanceID, err)
		return queueUnreadableWait(), true, nil
	}
	if held.Held {
		delete(ec.State, workerSlotPositionKey)
		clearWaitingForWorker(ec)
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: queueHeldCondition,
			WaitingReason:    queueHeldReason,
			Outputs:          map[string]any{"queue_held": true},
		}, true, nil
	}
	q, err := e.queueSlots(ctx)
	if err != nil {
		log.Printf("transcode queue: counting free slots for %s: %v (waiting)", ec.InstanceID, err)
		return queueUnreadableWait(), true, nil
	}
	// Only the free slots on workers with room for it are its own. One that no
	// node could take even empty fails now, not after waiting for a room that
	// never comes.
	p := e.placementOf(ec)
	need := p.need
	if why, never := e.roomNeverFits(ctx, q.workers, need, getInt64(ec.State, roomReserveKey) > 0); never {
		return StepResult{Status: StepFailed, Error: why}, true, nil
	}
	cur := e.claimFor(q, p)
	before, ahead, err := e.transcodeQueueBefore(q, cur, key, ec.InstanceID)
	if err != nil {
		log.Printf("transcode queue: listing the queue for %s: %v (waiting)", ec.InstanceID, err)
		return queueUnreadableWait(), true, nil
	}
	if safe := q.safeWorkers(cur, before); len(safe) > 0 {
		delete(ec.State, workerSlotPositionKey)
		clearWaitingForWorker(ec)
		return StepResult{}, false, safe
	}
	position := ahead + 1
	ec.State[workerSlotPositionKey] = position
	markWaitingForWorker(ec)
	condition, reason := q.placer.waitReason(p)
	if reason == "" {
		condition = "worker_busy"
		reason = fmt.Sprintf("Waiting its turn in the transcode queue: %d conversion(s) go first", ahead)
		switch {
		case need.SourceBytes > 0 && !cur.hasUsable() && q.total() > 0:
			reason = retentionNoSlotReason(need)
		case ahead == 0:
			reason = "Next in the transcode queue; waiting for a worker slot to free"
		}
	}
	return StepResult{
		Status:           StepWaitingExternal,
		WaitingCondition: condition,
		WaitingReason:    reason,
		Outputs:          map[string]any{"worker_busy": true, workerSlotPositionKey: position},
	}, true, nil
}

// queueUnreadableWait is the wait of a conversion whose place in the queue
// could not be read: it takes no worker and asks again at its next poll.
func queueUnreadableWait() StepResult {
	return StepResult{
		Status:           StepWaitingExternal,
		WaitingCondition: "worker_busy",
		WaitingReason:    "The transcode queue could not be read; it will look again shortly",
		Outputs:          map[string]any{"worker_busy": true},
	}
}

// batchQueueAllowance is how many of its queued items the batch may start
// now: the free slots no conversion ahead of its first queued item waits for.
// With nothing queued, or when the queue cannot be read, it does not limit.
func (e *Engine) batchQueueAllowance(ctx context.Context, batchID string) int {
	if e.deps.Store == nil || !e.workersConfigured() {
		return math.MaxInt
	}
	items, err := e.deps.Store.ListQueuedBatchItems()
	if err != nil {
		log.Printf("transcode queue: listing queued items for batch %s: %v", batchID, err)
		return math.MaxInt
	}
	first := ""
	for _, it := range items {
		if it.BatchID == batchID && !it.Held {
			if k := batchItemQueueOrder(it); first == "" || k < first {
				first = k
			}
		}
	}
	if first == "" {
		return math.MaxInt
	}
	q, err := e.queueSlots(ctx)
	if err != nil {
		log.Printf("transcode queue: counting free slots for batch %s: %v", batchID, err)
		return math.MaxInt
	}
	before, _, err := e.transcodeQueueBefore(q, e.claimFor(q, placement{}), first, "")
	if err != nil {
		log.Printf("transcode queue: listing the queue for batch %s: %v", batchID, err)
		return math.MaxInt
	}
	return q.slotsLeftAfter(before)
}

// queueOrderPending keeps the batch's children already waiting for a slot
// and at most allowance of its queued items, in the order of pending (queue
// order, see batchItemKeys).
func queueOrderPending(items []store.TranscodeBatchItem, pending []int, allowance int) []int {
	out := make([]int, 0, len(pending))
	for _, idx := range pending {
		if items[idx].Status == "queued" {
			if allowance <= 0 {
				continue
			}
			allowance--
		}
		out = append(out, idx)
	}
	return out
}

// batchItemKeys is the queue key of each of the batch's items that is queued
// or waiting for a slot. A queued item has the key the queue gives it (its
// own, its batch's block, or the batch's creation); a waiting one the key of
// its child conversion, which is the item's place once the child exists. An
// item whose key cannot be found gets the key the batch's creation gives it,
// which follows the item's id.
func (e *Engine) batchItemKeys(batchID string, items []store.TranscodeBatchItem) map[int64]string {
	keys := map[int64]string{}
	if e.deps.Store == nil {
		for _, it := range items {
			keys[it.ID] = queueOrder(queueRankPlain, time.Time{}, batchID, it.ID)
		}
		return keys
	}
	rows, err := e.deps.Store.ListQueuedBatchItems()
	if err != nil {
		log.Printf("transcode queue: listing queued items for batch %s: %v", batchID, err)
	}
	for _, r := range rows {
		if r.BatchID == batchID {
			keys[r.ItemID] = batchItemQueueOrder(r)
		}
	}
	var parent *store.ActionInstance
	parentRead := false
	for _, it := range items {
		if _, ok := keys[it.ID]; ok || (it.Status != "queued" && it.Status != "waiting_for_slot") {
			continue
		}
		if it.Status == "waiting_for_slot" && it.ChildActionID != "" {
			if k := e.childQueueKey(it.ChildActionID); k != "" {
				keys[it.ID] = k
				continue
			}
		}
		if !parentRead {
			parentRead = true
			if p, err := e.deps.Store.GetActionInstanceIfExists(batchID); err == nil {
				parent = p
			}
		}
		row := store.QueuedBatchItemRow{BatchID: batchID, ItemID: it.ID}
		if parent != nil {
			row.BatchCreatedAt = parent.CreatedAt
			row.Priority = instanceAskedFirst(parent)
		}
		if m, err := e.deps.Store.GetQueueItemMark(it.ID); err == nil {
			row.ItemOrder = m.Order
		}
		if m, err := e.deps.Store.GetQueueMark(store.QueueMarkBatch, batchID); err == nil {
			row.BatchOrder = m.Order
		}
		keys[it.ID] = batchItemQueueOrder(row)
	}
	return keys
}

// childQueueKey is the key of a waiting child conversion: the one the operator
// gave it, else the one stamped when it first waited ("" when it has none).
func (e *Engine) childQueueKey(childID string) string {
	if m, err := e.deps.Store.GetQueueMark(store.QueueMarkAction, childID); err == nil && m.Order != "" {
		return m.Order
	}
	child, err := e.deps.Store.GetActionInstanceIfExists(childID)
	if err != nil || child == nil {
		return ""
	}
	var state map[string]any
	_ = json.Unmarshal([]byte(child.StateJSON), &state)
	return getString(state, transcodeQueueOrderKey)
}

// sortByQueueKey orders the candidates by their place in the queue (the
// operator may have moved an item ahead of an earlier one), ties by item id.
func sortByQueueKey(items []store.TranscodeBatchItem, pending []int, keys map[int64]string) {
	sort.SliceStable(pending, func(a, b int) bool {
		ia, ib := items[pending[a]], items[pending[b]]
		if ka, kb := keys[ia.ID], keys[ib.ID]; ka != kb {
			return ka < kb
		}
		return ia.ID < ib.ID
	})
}

// batchDispatchCandidates are the indices of the items the batch may start or
// retry now, in queue order. ok is false when the holds could not be read: the
// batch then schedules nothing new this pass.
func (e *Engine) batchDispatchCandidates(ctx context.Context, batchID string, items []store.TranscodeBatchItem) (pending []int, ok bool) {
	// A conversion the operator holds keeps its place and starts nothing: it
	// is left out of what the batch schedules and of what the other items
	// wait for, and the batch goes on with the rest.
	view, ok := e.withoutHeldItems(batchID, items)
	if !ok {
		return nil, false
	}
	for i := range view {
		if view[i].Status == "queued" || view[i].Status == "waiting_for_slot" {
			pending = append(pending, i)
		}
	}
	pending = e.slotFairPending(ctx, batchID, view, pending)
	// New children only for the free slots older work does not wait for
	// (see transcode_queue.go).
	return queueOrderPending(view, pending, e.batchQueueAllowance(ctx, batchID)), true
}

// withoutHeldItems is items with the ones the operator holds marked
// queue_held, so the batch's fan-out neither schedules them nor lets them hold
// the others back (a held item waiting for a slot would otherwise keep every
// later queued item behind it). The slice is a copy, the indices are the same.
// ok is false when the holds could not be read.
func (e *Engine) withoutHeldItems(batchID string, items []store.TranscodeBatchItem) ([]store.TranscodeBatchItem, bool) {
	if e.deps.Store == nil {
		return items, true
	}
	held, err := e.deps.Store.HeldBatchItemIDs(batchID)
	if err != nil {
		log.Printf("transcode queue: reading the held items of batch %s: %v (scheduling nothing new)", batchID, err)
		return nil, false
	}
	if len(held) == 0 {
		return items, true
	}
	view := make([]store.TranscodeBatchItem, len(items))
	copy(view, items)
	for i := range view {
		if held[view[i].ID] {
			view[i].Status = queueHeldCondition
		}
	}
	return view, true
}
