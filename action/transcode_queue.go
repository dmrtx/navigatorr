package action

import (
	"context"
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
//   - a single file asked with queue_priority=true: before everything else, in
//     creation order among such files. It still waits for the job running on
//     a worker to end; it only goes first in the queue.
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

// queueOrder builds a sortable queue key: priority rank, time, group (the
// batch or the action) and the place within the group.
func queueOrder(priority bool, at time.Time, group string, seq int64) string {
	rank := 1
	if priority {
		rank = 0
	}
	return fmt.Sprintf("%d|%s|%s|%020d", rank, at.UTC().Format(queueOrderTimeLayout), group, seq)
}

func batchItemQueueOrder(r store.QueuedBatchItemRow) string {
	return queueOrder(false, parseStoreTime(r.BatchCreatedAt), r.BatchID, r.ItemID)
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
		}
		if err != nil {
			return "", err
		}
		// Without its item the child goes before the rest of its batch.
		var seq int64
		if item != nil {
			seq = item.ID
		}
		key = queueOrder(false, parseStoreTime(parent.CreatedAt), parentID, seq)
	} else {
		inst, err := e.deps.Store.GetActionInstanceIfExists(ec.InstanceID)
		if err != nil {
			return "", err
		}
		if inst == nil {
			return "", fmt.Errorf("action %s not found", ec.InstanceID)
		}
		key = queueOrder(getBool(ec.Inputs, "queue_priority"), parseStoreTime(inst.CreatedAt), ec.InstanceID, 0)
	}
	ec.State[transcodeQueueOrderKey] = key
	ec.State[transcodeQueueSinceKey] = e.now().Format(time.RFC3339Nano)
	return key, nil
}

// freeVideoSlots is how many more conversions the ready workers can take:
// for each, its video capacity minus the conversions assigned to it. A worker
// already past its capacity frees nothing for the others.
func (e *Engine) freeVideoSlots(ctx context.Context, workers []transcode.NamedWorker, ready []bool) (int, error) {
	counts, err := e.deps.Store.CountActiveWorkerActions("transcode_media")
	if err != nil {
		return 0, err
	}
	active := map[string]int{}
	for name, n := range counts {
		active[e.resolveWorker(name)] += n
	}
	free := 0
	for i, w := range workers {
		if ready[i] {
			free += max(0, e.workerVideoCapacity(ctx, w)-active[w.Name])
		}
	}
	return free, nil
}

// transcodeQueueAhead counts the conversions ahead of key: waiters with a
// smaller key (self excluded) and queued batch items with a smaller key.
func (e *Engine) transcodeQueueAhead(key, self string) (int, error) {
	waiters, err := e.deps.Store.ListTranscodeQueueWaiters()
	if err != nil {
		return 0, err
	}
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
		ahead++
	}
	items, err := e.deps.Store.ListQueuedBatchItems()
	if err != nil {
		return 0, err
	}
	for _, it := range items {
		if batchItemQueueOrder(it) < key {
			ahead++
		}
	}
	return ahead, nil
}

// readyForQueue is which workers count for the queue: the ready ones, or all
// of them when there is one or none is ready (the job then waits for its
// worker as before).
func (e *Engine) readyForQueue(ctx context.Context, workers []transcode.NamedWorker) []bool {
	ready := make([]bool, len(workers))
	if len(workers) > 1 {
		ready = e.workersReady(ctx, workers)
	}
	for _, r := range ready {
		if r {
			return ready
		}
	}
	for i := range ready {
		ready[i] = true
	}
	return ready
}

// transcodeQueueTurn decides whether a conversion without a worker may be
// assigned one now. It returns handled=true with a worker_busy wait, and the
// conversion's place in the queue, when it is not its turn. Call it under
// bindMu so two conversions cannot both take the last free slot.
func (e *Engine) transcodeQueueTurn(ctx context.Context, ec *ExecutionContext) (StepResult, bool) {
	if e.deps.Store == nil || ec.InstanceID == "" {
		return StepResult{}, false
	}
	key, err := e.ensureQueueOrder(ec)
	if err != nil {
		log.Printf("transcode queue: ordering %s: %v (assigning without queue order)", ec.InstanceID, err)
		return StepResult{}, false
	}
	workers := e.deps.Workers.All()
	free, err := e.freeVideoSlots(ctx, workers, e.readyForQueue(ctx, workers))
	if err != nil {
		log.Printf("transcode queue: counting free slots for %s: %v (assigning without queue order)", ec.InstanceID, err)
		return StepResult{}, false
	}
	ahead, err := e.transcodeQueueAhead(key, ec.InstanceID)
	if err != nil {
		log.Printf("transcode queue: listing the queue for %s: %v (assigning without queue order)", ec.InstanceID, err)
		return StepResult{}, false
	}
	if ahead < free {
		delete(ec.State, workerSlotPositionKey)
		clearWaitingForWorker(ec)
		return StepResult{}, false
	}
	position := ahead + 1
	ec.State[workerSlotPositionKey] = position
	markWaitingForWorker(ec)
	reason := fmt.Sprintf("Waiting its turn in the transcode queue: %d conversion(s) go first", ahead)
	if ahead == 0 {
		reason = "Next in the transcode queue; waiting for a worker slot to free"
	}
	return StepResult{
		Status:           StepWaitingExternal,
		WaitingCondition: "worker_busy",
		WaitingReason:    reason,
		Outputs:          map[string]any{"worker_busy": true, workerSlotPositionKey: position},
	}, true
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
	workers := e.deps.Workers.All()
	free, err := e.freeVideoSlots(ctx, workers, e.readyForQueue(ctx, workers))
	if err != nil {
		log.Printf("transcode queue: counting free slots for batch %s: %v", batchID, err)
		return math.MaxInt
	}
	ahead, err := e.transcodeQueueAhead(first, "")
	if err != nil {
		log.Printf("transcode queue: listing the queue for batch %s: %v", batchID, err)
		return math.MaxInt
	}
	return max(0, free-ahead)
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
