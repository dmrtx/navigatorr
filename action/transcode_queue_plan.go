package action

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

// The queue plan: for each node, what would start there next and why the other
// waiting conversions would not (GET /transcode/v1/queue/next).
//
// It is not a second copy of the dispatch rules. The plan walks the queue in
// order and, for each conversion, asks the very functions the dispatch asks:
// the placer's verdictAt (ready, room, refusals, availability window), the
// queue claims (claimsBefore, safeWorkers) and queueSlots.decideTurn, which in
// turn ends in pickWorker, the choice transcodeQueueTurn and pickVideoWorker
// make. A conversion that gets a node is taken off the free slots and out of
// the line, as the dispatch does when it assigns one, so the next conversion is
// decided against what is left. The test TestQueueNextAgreesWithDispatch runs
// the real dispatch against the same state and compares.
//
// It only reads: the engine's last observation of each worker (never a probe,
// see passiveContext), the store, the clock. It claims nothing and writes
// nothing.

// Node states of the plan.
const (
	NodeReady   = "ready"   // it has a free slot and takes work
	NodeBusy    = "busy"    // every slot is in use
	NodePaused  = "paused"  // the operator paused it
	NodeClosed  = "closed"  // its availability window has closed
	NodeOffline = "offline" // not ready: unreachable or failing its checks
)

// Reason codes. Those that are also wait conditions of the engine keep their
// name (node_window). Every window code has the wait condition node_window.
const (
	// About the conversion.
	ReasonHeld        = "held"         // the operator holds it (queue_held)
	ReasonBatchHeld   = "batch_held"   // its batch is on hold (parent_paused)
	ReasonBatchGating = "batch_gating" // its batch starts no new conversion while one ahead waits for a slot
	ReasonSizeUnknown = "size_unknown" // its source size could not be read yet
	// About a node.
	ReasonNodeOffline  = "node_offline"
	ReasonNodePaused   = "node_paused"
	ReasonNodeBuild    = "node_build"
	ReasonNoDiskRoom   = "no_disk_room"
	ReasonApprovalRoom = "waiting_for_approvals"
	ReasonTooBig       = "too_big_for_node"
	ReasonRefused      = "recently_refused"
	ReasonNoFreeSlot   = "no_free_slot"
	// About the line.
	ReasonSlotTakenAhead  = "slot_taken_ahead" // conversions ahead of it take the free slots first
	ReasonStartsElsewhere = "starts_elsewhere" // it starts on another node instead
	// Why a node has no next.
	// The reasons a node's availability window gives (see worker_window.go).
	ReasonNodeWindow       = windowCodeClosed
	ReasonMediaType        = windowCodeMediaType
	ReasonNoFit            = windowCodeNoFit
	ReasonNoProbe          = windowCodeNoProbe
	ReasonNoHistory        = windowCodeNoHistory
	ReasonWindowUnreadable = windowCodeUnreadable

	ReasonQueueEmpty      = "queue_empty"
	ReasonAllHeld         = "all_held"
	ReasonNothingEligible = "nothing_eligible"
)

// Skipped entries shown for one node.
const maxPlanSkipped = 20

// QueuePlan is the answer: the queue revision it was read at and one entry per
// node, in configuration order.
type QueuePlan struct {
	Revision int64
	Nodes    []PlanNode
	// started is the node each conversion the walk gave one (kind and id
	// joined by a colon), for the tests that compare the plan with the dispatch.
	started map[string]string
}

// PlanNode is what would start next on one node.
type PlanNode struct {
	Worker    string
	State     string
	FreeSlots int
	// Next is the conversion that would start on the node first; nil when none.
	Next *PlanPick
	// WhyNone says why Next is nil.
	WhyNone *PlanReason
	// Skipped are the conversions ahead of Next in queue order (all of the
	// queue when Next is nil), each with why it does not start here, at most
	// maxPlanSkipped of them.
	Skipped          []PlanSkip
	SkippedTruncated bool
}

// PlanPick is the conversion chosen for a node.
type PlanPick struct {
	Kind, ID, BatchID, Path, MediaType string
	Position                           int
	// EstimateSec is the encode time estimated for this node, when known.
	EstimateSec float64
	HasEstimate bool
}

// PlanReason is a machine code and a plain sentence.
type PlanReason struct {
	Code string
	Text string
}

// PlanSkip is a conversion that does not start on the node.
type PlanSkip struct {
	Kind, ID, BatchID string
	Position          int
	PlanReason
}

// QueueNext is the plan of the transcode queue. It only reads.
func (e *Engine) QueueNext(ctx context.Context) (QueuePlan, error) {
	view, err := e.readQueueView()
	if err != nil {
		return QueuePlan{}, err
	}
	plan := QueuePlan{Revision: view.Revision, Nodes: []PlanNode{}}
	if !e.workersConfigured() {
		return plan, nil
	}
	ctx = passiveContext(ctx)
	q, err := e.queueSlots(ctx)
	if err != nil {
		return QueuePlan{}, err
	}
	plan.Nodes, plan.started = e.planQueue(q, view.Entries)
	return plan, nil
}

// planNodeState is the state of worker i and its free slots.
func (e *Engine) planNodeState(q queueSlots, i int) (string, int) {
	w := q.workers[i]
	free := q.free[w.Name]
	switch {
	case !q.ready[i]:
		return NodeOffline, 0
	case e.workerPaused(w.Name):
		return NodePaused, 0
	case e.workerWindowClosed(w.Name):
		return NodeClosed, 0
	case free <= 0:
		return NodeBusy, 0
	}
	return NodeReady, free
}

// planAcc collects one node's answer while the queue is walked.
type planAcc struct {
	state     string
	free      int
	next      *PlanPick
	skips     []PlanSkip
	truncated bool
}

// planQueue walks the entries (in queue order) and decides each as the
// dispatch would, given what the ones before it took.
func (e *Engine) planQueue(q queueSlots, entries []QueueEntry) ([]PlanNode, map[string]string) {
	sim := q
	sim.free = maps.Clone(q.free)
	cs := make([]queueContender, len(entries))
	for i, en := range entries {
		c := queueContender{kind: en.Kind, id: en.ID, key: en.key, held: en.Held}
		switch {
		case en.waiter != nil:
			c.p = e.waiterPlacement(*en.waiter)
			c.stale = e.waiterStale(*en.waiter)
		case en.item != nil:
			c.p = queuedItemPlacement(*en.item)
		}
		cs[i] = c
	}
	startedBy := map[string]string{}
	accs := make([]planAcc, len(q.workers))
	for i := range accs {
		accs[i].state, accs[i].free = e.planNodeState(q, i)
	}
	// open says whether the walk still has something to learn: a node needs its
	// next while it has a free slot, and its list of skipped until it is full.
	open := func() bool {
		for i, a := range accs {
			if a.next == nil && (sim.free[q.workers[i].Name] > 0 || len(a.skips) < maxPlanSkipped) {
				return true
			}
		}
		return false
	}
	for i := range cs {
		if !open() {
			break
		}
		c := &cs[i]
		en := entries[i]
		var dec turnDecision
		gated := false
		started := ""
		if !c.held {
			self := ""
			if c.kind == QueueKindAction {
				self = c.id
			}
			cur := sim.claimOf(c)
			before, ahead := sim.claimsBefore(cs, cur, c.key, self)
			dec = sim.decideTurn(c.p, cur, before, ahead)
			gated = c.kind == QueueKindItem && queuedItemGatedInPlan(cs, entries, i)
			if dec.Node != "" && !gated {
				started = dec.Node
			}
		}
		for ni, w := range q.workers {
			a := &accs[ni]
			if a.next != nil {
				continue
			}
			if started == w.Name {
				a.next = e.planPick(en, c, w.Name)
				continue
			}
			if len(a.skips) >= maxPlanSkipped {
				a.truncated = true
				continue
			}
			a.skips = append(a.skips, PlanSkip{
				Kind: en.Kind, ID: en.ID, BatchID: en.BatchID, Position: en.Position,
				PlanReason: e.planPairReason(sim, c, en, dec, gated, ni),
			})
		}
		if started != "" {
			c.started = true
			startedBy[en.Kind+":"+en.ID] = started
			sim.free[started]--
		}
	}
	nodes := make([]PlanNode, len(q.workers))
	for ni, w := range q.workers {
		a := accs[ni]
		n := PlanNode{Worker: w.Name, State: a.state, FreeSlots: a.free, Next: a.next, Skipped: a.skips, SkippedTruncated: a.truncated}
		if n.Skipped == nil {
			n.Skipped = []PlanSkip{}
		}
		if a.next == nil {
			if len(a.skips) >= maxPlanSkipped && len(cs) > maxPlanSkipped {
				n.SkippedTruncated = true
			}
			n.WhyNone = planWhyNone(w.Name, a.state, entries)
		}
		nodes[ni] = n
	}
	return nodes, startedBy
}

// waiterStale says whether a waiter has not been polled for so long that
// something other than the queue holds it.
func (e *Engine) waiterStale(w store.TranscodeQueueWaiterRow) bool {
	updated, err := time.Parse(time.RFC3339Nano, w.UpdatedAt)
	return err == nil && e.now().Sub(updated) > slotWaiterStaleAfter
}

// queuedItemGatedInPlan applies the rule a batch uses before it starts a
// queued item (queuedItemGated): not while a conversion of another batch, or
// one of its own batch that is ahead, still waits for a slot. The conversions
// that already got a node in the walk wait no longer.
func queuedItemGatedInPlan(cs []queueContender, entries []QueueEntry, i int) bool {
	var foreign []string
	hasWaiting := false
	firstWaiting := ""
	for j := range cs {
		c := &cs[j]
		if c.kind != QueueKindAction || c.held || c.stale || c.started || entries[j].waiter == nil {
			continue
		}
		// Only those that contend for a slot: not one a node's window holds.
		if cond := entries[j].waiter.WaitingCondition; cond != "worker_busy" && cond != "" {
			continue
		}
		if entries[j].BatchID == entries[i].BatchID {
			if !hasWaiting || c.key < firstWaiting {
				hasWaiting, firstWaiting = true, c.key
			}
			continue
		}
		foreign = append(foreign, c.key)
	}
	return queuedItemGated(cs[i].key, foreign, hasWaiting, firstWaiting)
}

// queuedItemGated says whether a batch must not start the queued item with
// this key now: a waiter of another batch is ahead of it, or one of its own
// batch's children waits for a slot and the item is behind it. slotFairPending
// (the batch's dispatch) and the queue plan both ask it.
func queuedItemGated(key string, foreign []string, hasWaiting bool, firstWaiting string) bool {
	for _, k := range foreign {
		if k < key {
			return true
		}
	}
	return hasWaiting && key > firstWaiting
}

// planPick describes the conversion chosen for node.
func (e *Engine) planPick(en QueueEntry, c *queueContender, node string) *PlanPick {
	pick := &PlanPick{Kind: en.Kind, ID: en.ID, BatchID: en.BatchID, Path: en.Path, MediaType: en.MediaType, Position: en.Position}
	if c.p.window.check && !c.p.window.noFit && c.p.window.durationSec > 0 && c.p.window.class != "" {
		if est, ok := e.windowEstimate(node, c.p.window); ok {
			pick.EstimateSec, pick.HasEstimate = est, true
		}
	}
	return pick
}

// planPairReason is why the conversion c does not start on node ni, judged at
// the step of the walk where it was decided: the conversion's own state first
// (a hold, its batch), then the placer's verdict for that node (the same one
// the claims use), then the node's slots, then the line.
func (e *Engine) planPairReason(sim queueSlots, c *queueContender, en QueueEntry, dec turnDecision, gated bool, ni int) PlanReason {
	w := sim.workers[ni]
	switch {
	case c.held && en.HeldBy == "batch":
		return PlanReason{ReasonBatchHeld, "Its batch is on hold; it keeps its place and starts after the batch is released."}
	case c.held:
		return PlanReason{ReasonHeld, "Held by the operator; it keeps its place and starts after it is released."}
	case gated:
		return PlanReason{ReasonBatchGating, "Its batch starts no new conversion while one ahead of it in the queue waits for a free slot."}
	}
	if code, text := sim.placer.verdictAt(c.p, ni, true); code != "" {
		return PlanReason{code, capitalize(text) + "."}
	}
	if sim.free[w.Name] <= 0 {
		switch {
		case e.workerPaused(w.Name):
			return PlanReason{ReasonNodePaused, "Node " + w.Name + " is paused and takes nothing new."}
		case e.workerWindowClosed(w.Name):
			return PlanReason{windowWaitCondition, "Node " + w.Name + "'s availability window has closed; it takes nothing new."}
		}
		return PlanReason{ReasonNoFreeSlot, "Every video slot on node " + w.Name + " is in use."}
	}
	if !dec.Within[w.Name] {
		return PlanReason{ReasonSlotTakenAhead, fmt.Sprintf("%d conversion(s) ahead of it can use the free slot(s) on node %s and take them first.", dec.Ahead, w.Name)}
	}
	return PlanReason{ReasonStartsElsewhere, "It starts on node " + dec.Node + " instead."}
}

// planWhyNone says why nothing would start on the node.
func planWhyNone(node, state string, entries []QueueEntry) *PlanReason {
	if len(entries) == 0 {
		return &PlanReason{ReasonQueueEmpty, "Nothing is waiting in the transcode queue."}
	}
	switch state {
	case NodeOffline:
		return &PlanReason{ReasonNodeOffline, "Node " + node + " is not ready (offline or failing its checks), so it takes nothing new."}
	case NodePaused:
		return &PlanReason{ReasonNodePaused, "Node " + node + " is paused and takes nothing new."}
	case NodeClosed:
		return &PlanReason{windowWaitCondition, "Node " + node + "'s availability window has closed; it takes nothing new until the window changes."}
	case NodeBusy:
		return &PlanReason{ReasonNoFreeSlot, "Every video slot on node " + node + " is in use."}
	}
	held := 0
	for _, en := range entries {
		if en.Held {
			held++
		}
	}
	if held == len(entries) {
		return &PlanReason{ReasonAllHeld, "Everything waiting is on hold."}
	}
	return &PlanReason{ReasonNothingEligible, "None of the " + strconv.Itoa(len(entries)) + " waiting conversion(s) can start on node " + node + " now; skipped says why for each."}
}
