package action

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
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
// the order. It is a fairness rule: if the store cannot be read the
// conversion goes ahead as before, and the worker still queues it.
const (
	transcodeQueueOrderKey = "transcode_queue_order"
	// transcodeQueueSinceKey is when the conversion took its place.
	transcodeQueueSinceKey = "transcode_queue_since"
)

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

func batchItemQueueOrder(r store.QueuedBatchItemRow) string {
	return queueOrder(batchQueueRank(r.Priority), parseStoreTime(r.BatchCreatedAt), r.BatchID, r.ItemID)
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

// ensureQueueOrder returns the conversion's queue key, stamping it the first
// time.
func (e *Engine) ensureQueueOrder(ec *ExecutionContext) (string, error) {
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
		var item *store.TranscodeBatchItem
		if itemKey := getString(ec.Inputs, "batch_item_key"); itemKey != "" {
			item, err = e.deps.Store.GetTranscodeBatchItem(parentID, itemKey)
		} else {
			item, err = e.deps.Store.FindTranscodeBatchItemByChildActionID(ec.InstanceID)
			if err == nil && item == nil {
				// A profile batch's child is linked to its item only after its
				// first run; its idempotency key names the item until then.
				var key string
				if key, err = e.childIdempotencyKey(ec.InstanceID); err == nil {
					item, err = e.deps.Store.FindTranscodeBatchItemByChildKey(key)
				}
			}
		}
		if err != nil {
			return "", err
		}
		// Without its item the child goes before the rest of its batch.
		var seq int64
		if item != nil {
			seq = item.ID
		}
		key = queueOrder(batchQueueRank(instanceAskedFirst(parent)), parseStoreTime(parent.CreatedAt), parentID, seq)
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
		if w.ID == self || w.QueueOrder >= key {
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
		if batchItemQueueOrder(it) < key {
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
		log.Printf("transcode queue: ordering %s: %v (assigning without queue order)", ec.InstanceID, err)
		return StepResult{}, false, nil
	}
	q, err := e.queueSlots(ctx)
	if err != nil {
		log.Printf("transcode queue: counting free slots for %s: %v (assigning without queue order)", ec.InstanceID, err)
		return StepResult{}, false, nil
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
		log.Printf("transcode queue: listing the queue for %s: %v (assigning without queue order)", ec.InstanceID, err)
		return StepResult{}, false, nil
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
		if it.BatchID == batchID {
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
// and at most allowance of its queued items, in batch order.
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
