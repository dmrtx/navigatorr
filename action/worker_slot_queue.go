package action

import (
	"context"
	"encoding/json"
	"log"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

// Worker slot queue.
//
// A worker runs at most max_parallel_jobs benchmarks and encodes at once and
// refuses a benchmark (and, on older workers, an encode) submitted while it is
// full. Admission used to be per batch only, so the batch whose child had just
// finished dispatched its next child inline and won every freed slot, while
// children of other batches kept polling a full worker.
//
// The queue is durable and has no table of its own: an action that has to wait
// for a slot stamps worker_slot_wait_since into its own state the first time
// it waits, and keeps it until any outcome other than "still waiting" (see
// clearWorkerBusy). Only the oldest live ticket holder may send a slot-taking
// submit; everyone else, including a newcomer without a ticket, defers without
// contacting the worker and takes a ticket of its own. Batches additionally do
// not start new children while another batch's (or a standalone action's)
// child holds a ticket. A restart keeps the order because the tickets are in
// the action rows the engine already persists.
const (
	workerSlotWaitSinceKey = "worker_slot_wait_since"
	workerSlotPositionKey  = "worker_slot_queue_position"
	workerSlotPolledAtKey  = "worker_slot_polled_at"
)

// Slot waiters poll the store, not the worker, but each poll still rewrites
// the action row. A freed slot shortens the wait (see slotWakeDue), so the
// steady-state interval can be long.
const (
	slotWaitPollBase   = 30 * time.Second
	slotWaitPollJitter = 30 * time.Second
)

// slotWaiterStaleAfter drops a ticket holder that has not been polled for this
// long from the queue: something other than the slot wait is holding it (a
// stopped coordinator, an action no longer reconciled), and it must not block
// everyone behind it forever.
const slotWaiterStaleAfter = 15 * time.Minute

const waitingForWorkerPhase = "waiting_for_worker"

type slotWaiter struct {
	id       string
	parentID string
	since    time.Time
}

func slotTicket(state map[string]any) (time.Time, bool) {
	raw := getString(state, workerSlotWaitSinceKey)
	if raw == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	return at, err == nil
}

func hasSlotTicket(state map[string]any) bool {
	_, ok := slotTicket(state)
	return ok
}

// aheadOf reports whether w was waiting before a ticket taken at since by id.
func (w slotWaiter) aheadOf(since time.Time, id string) bool {
	if !w.since.Equal(since) {
		return w.since.Before(since)
	}
	return w.id < id
}

// workerSlotWaiters lists the actions still contending for a worker slot,
// oldest ticket first. A ticket holder that left the wait (paused, its parent
// paused or waiting for a decision, worker unreachable) keeps its place for
// later but does not block anyone meanwhile.
func (e *Engine) workerSlotWaiters() ([]slotWaiter, error) {
	insts, err := e.deps.Store.ListWorkerSlotWaiters()
	if err != nil {
		return nil, err
	}
	var out []slotWaiter
	for _, inst := range insts {
		switch inst.Status {
		case StatusWaitingExternal:
			// parent_busy only means the parent was executing at that poll.
			if inst.WaitingCondition != "worker_busy" && inst.WaitingCondition != "parent_busy" {
				continue
			}
		case StatusRunning, StatusPending:
			// Mid-poll: resume clears the waiting condition while it runs.
		default:
			continue
		}
		if updated, err := time.Parse(time.RFC3339, inst.UpdatedAt); err == nil && time.Since(updated) > slotWaiterStaleAfter {
			continue
		}
		var state, inputs map[string]any
		_ = json.Unmarshal([]byte(inst.StateJSON), &state)
		_ = json.Unmarshal([]byte(inst.InputsJSON), &inputs)
		since, ok := slotTicket(state)
		if !ok || getBool(state, "paused") {
			continue
		}
		parent := getString(inputs, "parent_action_id")
		if parent == "" {
			parent = getString(state, "parent_action_id")
		}
		out = append(out, slotWaiter{id: inst.ID, parentID: parent, since: since})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].aheadOf(out[j].since, out[j].id) })
	return out, nil
}

// workerSlotTurn decides whether this action may now send a submit that needs
// a free worker slot. It returns handled=true with a worker_busy wait when an
// older waiter goes first; nothing is sent to the worker then. Call it after
// the parent admission checks, so a paused batch's child leaves the queue
// instead of blocking it.
//
// The queue is a fairness rule, not a safety one: if the store cannot be read
// the submit goes ahead and the worker's own capacity check still applies.
func (e *Engine) workerSlotTurn(ec *ExecutionContext, reason string) (StepResult, bool) {
	if e.deps.Store == nil || ec == nil || ec.InstanceID == "" {
		return StepResult{}, false
	}
	position, err := e.slotQueuePosition(ec)
	if err != nil {
		log.Printf("worker slot queue: listing waiters for %s: %v (submitting without queue order)", ec.InstanceID, err)
		return StepResult{}, false
	}
	if position == 1 {
		return StepResult{}, false
	}
	e.ensureSlotTicket(ec)
	markWaitingForWorker(ec)
	return StepResult{
		Status:           StepWaitingExternal,
		WaitingCondition: "worker_busy",
		WaitingReason:    reason,
		Outputs:          map[string]any{"worker_busy": true, workerSlotPositionKey: position},
	}, true
}

// slotQueuePosition stores and returns this action's place among the live
// slot waiters (1 = next). Without a ticket every other waiter is ahead.
func (e *Engine) slotQueuePosition(ec *ExecutionContext) (int, error) {
	waiters, err := e.workerSlotWaiters()
	if err != nil {
		return 0, err
	}
	since, ticketed := slotTicket(ec.State)
	ahead := 0
	for _, w := range waiters {
		if w.id == ec.InstanceID {
			continue
		}
		if !ticketed || w.aheadOf(since, ec.InstanceID) {
			ahead++
		}
	}
	ec.State[workerSlotPositionKey] = ahead + 1
	return ahead + 1, nil
}

// ensureSlotTicket stamps the action's place in the queue the first time it
// waits for a slot; later waits keep the original place.
func (e *Engine) ensureSlotTicket(ec *ExecutionContext) {
	if !hasSlotTicket(ec.State) {
		ec.State[workerSlotWaitSinceKey] = e.now().Format(time.RFC3339Nano)
	}
}

// markWaitingForWorker replaces the preflight's last work phase (often
// probing_source) with the slot wait, with the queue position when known.
// updated_at only moves when the shown value changes, so a long wait does not
// look like fresh activity.
func markWaitingForWorker(ec *ExecutionContext) {
	position := getInt(ec.State, workerSlotPositionKey)
	if prev, ok := ec.State["work_progress"].(map[string]any); ok && getString(prev, "phase") == waitingForWorkerPhase && getInt(prev, "queue_position") == position {
		return
	}
	work := map[string]any{"phase": waitingForWorkerPhase, "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
	if since := getString(ec.State, workerSlotWaitSinceKey); since != "" {
		work["since"] = since
	}
	if position > 0 {
		work["queue_position"] = position
	}
	ec.State["work_progress"] = work
}

// clearWaitingForWorker drops the slot-wait phase once the wait ended.
func clearWaitingForWorker(ec *ExecutionContext) {
	for _, m := range []map[string]any{ec.State, ec.Outputs} {
		if work, ok := m["work_progress"].(map[string]any); ok && getString(work, "phase") == waitingForWorkerPhase {
			delete(m, "work_progress")
		}
	}
}

// foreignSlotWaiter reports whether an action outside batchID is waiting for
// a worker slot. A batch must not start another child then: the freed slot
// belongs to the older waiter.
func (e *Engine) foreignSlotWaiter(batchID string) bool {
	if e.deps.Store == nil {
		return false
	}
	waiters, err := e.workerSlotWaiters()
	if err != nil {
		log.Printf("worker slot queue: listing waiters for batch %s: %v", batchID, err)
		return false
	}
	for _, w := range waiters {
		if w.parentID != batchID {
			return true
		}
	}
	return false
}

// slotFairPending narrows a batch's dispatch candidates to what it may start
// without taking a freed worker slot out of turn:
//   - no new child (a queued item) while an action outside this batch waits
//     for a slot; the batch's own waiting children still try, in queue order;
//   - on a reconciler poll, a waiting child is left to its own backoff
//     instead of being resumed every time the batch polls.
func (e *Engine) slotFairPending(ctx context.Context, batchID string, items []store.TranscodeBatchItem, pending []int) []int {
	if len(pending) == 0 {
		return pending
	}
	hasQueued := false
	for _, idx := range pending {
		if items[idx].Status == "queued" {
			hasQueued = true
			break
		}
	}
	foreign := hasQueued && e.foreignSlotWaiter(batchID)
	out := make([]int, 0, len(pending))
	for _, idx := range pending {
		it := items[idx]
		if foreign && it.Status == "queued" {
			continue
		}
		if it.Status == "waiting_for_slot" && e.slotWaitNotDue(ctx, it.ChildActionID) {
			continue
		}
		out = append(out, idx)
	}
	return out
}

// noteWorkerSlotFreed records that a worker job this process tracks has ended
// and wakes the reconciler, so the head of the slot queue tries now instead of
// at the end of its backoff.
func (e *Engine) noteWorkerSlotFreed() {
	e.slotFreedAt.Store(e.now().UnixNano())
	e.WakeReconciler()
}

// slotWaitPollDelay spreads slot waiters' polls between 30 and 60 seconds.
func slotWaitPollDelay() time.Duration {
	return slotWaitPollBase + rand.N(slotWaitPollJitter)
}

// slotWakeDue reports whether a slot waiter should be polled before its
// scheduled time because a worker job ended after its last poll. The worker's
// Retry-After (worker_busy_not_before) is still honoured.
func (e *Engine) slotWakeDue(state map[string]any) bool {
	if !hasSlotTicket(state) {
		return false
	}
	freed := e.slotFreedAt.Load()
	if freed == 0 {
		return false
	}
	polled, err := time.Parse(time.RFC3339Nano, getString(state, workerSlotPolledAtKey))
	if err != nil || freed <= polled.UnixNano() {
		return false
	}
	if at, err := time.Parse(time.RFC3339Nano, getString(state, workerBusyNotBeforeKey)); err == nil && e.now().Before(at) {
		return false
	}
	return true
}

// automaticResumeKey marks a context driven by the reconciler, as opposed to
// a caller resuming an action explicitly.
type automaticResumeKey struct{}

func withAutomaticResume(ctx context.Context) context.Context {
	return context.WithValue(ctx, automaticResumeKey{}, true)
}

func isAutomaticResume(ctx context.Context) bool {
	v, _ := ctx.Value(automaticResumeKey{}).(bool)
	return v
}

// slotWaitNotDue reports whether a reconciler-driven batch should leave a
// child's slot wait to the child's own schedule: resuming it on every batch
// poll would only rewrite its row while it waits its turn.
func (e *Engine) slotWaitNotDue(ctx context.Context, childID string) bool {
	if !isAutomaticResume(ctx) || childID == "" || e.deps.Store == nil {
		return false
	}
	child, err := e.deps.Store.GetActionInstanceIfExists(childID)
	if err != nil || child == nil || child.Status != StatusWaitingExternal || child.WaitingCondition != "worker_busy" {
		return false
	}
	var state map[string]any
	_ = json.Unmarshal([]byte(child.StateJSON), &state)
	at, err := time.Parse(time.RFC3339Nano, getString(state, "next_poll_at"))
	if err != nil || !e.now().Before(at) {
		return false
	}
	return !e.slotWakeDue(state)
}
