package action

import (
	"context"
	"fmt"
	"github.com/jakenesler/navigatorr/transcode"
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
	NodePaused  = "paused"  // the operator paused it, or it reports it is on hold or building
	NodeClosed  = "closed"  // its availability window has closed
	NodeOffline = "offline" // not ready: unreachable or failing its checks
	NodeUnknown = "unknown" // no recent report from it: nothing is said about its slots
)

// Reason codes. Those that are also wait conditions of the engine keep their
// name (node_window). Every window code has the wait condition node_window.
const (
	// About the conversion.
	ReasonHeld        = "held"         // the operator holds it (queue_held)
	ReasonBatchHeld   = "batch_held"   // its batch is on hold (parent_paused)
	ReasonBatchGating = "batch_gating" // its batch starts no new conversion while one ahead waits for a slot
	ReasonSizeUnknown = "size_unknown" // its source size could not be read yet
	ReasonNotPolling  = "not_polling"  // nothing has polled it for a long time; it is not waiting for a slot
	// About a node.
	ReasonNodeOffline  = "node_offline"
	ReasonNodePaused   = "node_paused"  // paused by the operator (PUT /pauses/{node})
	ReasonNodeOnHold   = "node_on_hold" // the node's own report says it is on hold
	ReasonNodeUnknown  = "node_unknown" // no recent report from it
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
	// ReportAt is when the server last checked the node, successful or not
	// (zero: never). The pool is read separately and may be older.
	ReportAt time.Time
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
	q.unreported, q.reportAt = e.planUnreported(ctx, q)
	plan.Nodes, plan.started = e.planQueue(q, view.Entries)
	return plan, nil
}

// WarmWorkerReports asks every worker for its report and its slots now, so
// that QueueNext (which asks nothing) can speak for them. Dispatch does the same
// on every poll of a waiting conversion; this is for a caller with nothing
// waiting. It is meant for tests and start-up: the plan itself never asks.
func (e *Engine) WarmWorkerReports(ctx context.Context) {
	if e.deps.Workers == nil {
		return
	}
	for _, w := range e.deps.Workers.All() {
		if ctx.Err() != nil {
			return
		}
		e.workerObservation(ctx, w)
		e.workerVideoCapacity(ctx, w)
	}
}

// planReportMaxAge is how old the last report of a node may be for the plan to
// speak for it. Waiting conversions poll every 30 to 60 seconds and each poll
// refreshes it.
const planReportMaxAge = 2 * time.Minute

// planUnreported marks the nodes that have no recent report, which the plan
// reads without asking (it must not probe). A marked node is made not ready and
// has no free slot: the plan says nothing about it. The age of the pool counts
// only where capacity is read from it (a node that is ready and not on hold).
// With a single node there is no choice to make and dispatch needs no report
// from it (it counts as ready, its slots read when a conversion asks), and with
// nothing waiting nothing refreshes the report, so no node is marked: the plan
// uses what was last read or the configured capacity, as dispatch does.
// A node that reports any hold takes nothing new, so it has no free slot for
// the plan either, and a queued item (placed by media type and window only) is
// never planned onto it; the child dispatch would make for it cannot start
// there.
func (e *Engine) planUnreported(ctx context.Context, q queueSlots) (out []bool, reportAt []time.Time) {
	out = make([]bool, len(q.workers))
	poolUnread := make([]bool, len(q.workers))
	reportAt = make([]time.Time, len(q.workers))
	now := e.now()
	tooOld := func(at time.Time) bool { return at.IsZero() || now.Sub(at) > planReportMaxAge }
	for i, w := range q.workers {
		obs := e.workerObservation(ctx, w)
		reportAt[i] = obs.CheckedAt
		hold := e.planRoomHold(ctx, w)
		old := false
		if len(q.workers) > 1 {
			old = tooOld(obs.CheckedAt)
			if _, reads := w.Executor.(workerSlotReader); reads && !old && q.ready[i] && hold == "" &&
				!e.workerPaused(w.Name) && !e.workerWindowClosed(w.Name) {
				old = tooOld(e.workerPoolAt(w.Name))
				poolUnread[i] = old
			}
		}
		switch {
		case old:
			out[i] = true
			q.ready[i] = false
			delete(q.free, w.Name)
		case len(q.workers) == 1 && !obs.CheckedAt.IsZero() && (!obs.Reachable || !obs.Ready):
			// The last news of the only node was that it is down: the plan
			// keeps saying so until a newer report says otherwise.
			q.ready[i] = false
			delete(q.free, w.Name)
		case hold != "":
			q.free[w.Name] = 0
		}
	}
	q.placer.unreported = out
	q.placer.poolUnread = poolUnread
	return out, reportAt
}

// nodeOwnHold is the hold the node's report names when the operator has not
// paused it ("" otherwise).
func (e *Engine) nodeOwnHold(ctx context.Context, w transcode.NamedWorker) string {
	if e.workerPaused(w.Name) {
		return ""
	}
	return e.planRoomHold(ctx, w)
}

// planRoomHold is the hold the node's own fresh report names ("paused",
// "build"), or "".
func (e *Engine) planRoomHold(ctx context.Context, w transcode.NamedWorker) string {
	r := e.workerObservation(ctx, w).Retention
	if r == nil {
		return ""
	}
	// With one node the last news counts whatever its age: nothing else says
	// the hold is over, and dispatch has no other node to turn to. With
	// several, an old hold is ignored as the placement ignores it.
	if len(e.deps.Workers.All()) > 1 && !r.Fresh(e.now()) {
		return ""
	}
	return r.Hold
}

// planNodeState is the state of worker i and its free slots.
func (e *Engine) planNodeState(ctx context.Context, q queueSlots, i int) (string, int) {
	w := q.workers[i]
	free := q.free[w.Name]
	switch {
	case i < len(q.unreported) && q.unreported[i]:
		return NodeUnknown, 0
	case !q.ready[i]:
		return NodeOffline, 0
	case e.workerPaused(w.Name), e.planRoomHold(ctx, w) != "":
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
	gates := planGates{e: e, byBatch: map[string]*planGate{}}
	doneActions := map[string]bool{}
	accs := make([]planAcc, len(q.workers))
	for i := range accs {
		accs[i].state, accs[i].free = e.planNodeState(q.placer.ctx, q, i)
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
		if !c.held && !c.stale {
			self := ""
			if c.kind == QueueKindAction {
				self = c.id
			}
			cur := sim.claimOf(c)
			before, ahead := sim.claimsBefore(cs, cur, c.key, self)
			dec = sim.decideTurn(c.p, cur, before, ahead)
			gated = c.kind == QueueKindItem && gates.gated(en.BatchID, c.key, doneActions)
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
			if en.Kind == QueueKindAction {
				doneActions[en.ID] = true
			}
			startedBy[en.Kind+":"+en.ID] = started
			sim.free[started]--
		}
	}
	nodes := make([]PlanNode, len(q.workers))
	for ni, w := range q.workers {
		a := accs[ni]
		n := PlanNode{Worker: w.Name, State: a.state, FreeSlots: a.free, ReportAt: q.reportAtOf(ni), Next: a.next, Skipped: a.skips, SkippedTruncated: a.truncated}
		if n.Skipped == nil {
			n.Skipped = []PlanSkip{}
		}
		if a.next == nil {
			if len(a.skips) >= maxPlanSkipped && len(cs) > maxPlanSkipped {
				n.SkippedTruncated = true
			}
			n.WhyNone = planWhyNone(w.Name, a.state, e.nodeOwnHold(q.placer.ctx, w), ni < len(q.placer.poolUnread) && q.placer.poolUnread[ni], entries)
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

// planGates reads, once per batch, what decides whether its queued items may
// start (batchSlotGate, the batch's own dispatch rule), and asks it again as
// the walk gives conversions their nodes.
type planGates struct {
	e       *Engine
	byBatch map[string]*planGate
	// waiters are the slot waiters, read once for the whole plan.
	waiters []slotWaiterKey
	read    bool
	readOK  bool
}

type planGate struct {
	items []store.TranscodeBatchItem
	keys  map[int64]string
	ok    bool
}

// gated says whether the batch would not start the queued item with this key
// now. A batch that cannot be read starts nothing, as in dispatch.
func (g *planGates) gated(batchID, key string, done map[string]bool) bool {
	pg, have := g.byBatch[batchID]
	if !have {
		pg = &planGate{}
		g.byBatch[batchID] = pg
		if items, err := g.e.deps.Store.ListTranscodeBatchItems(batchID); err == nil {
			if view, ok := g.e.withoutHeldItems(batchID, items); ok {
				if keys, ok := g.e.batchItemKeys(batchID, view); ok {
					pg.items, pg.keys, pg.ok = view, keys, true
				}
			}
		}
	}
	if !pg.ok {
		return true
	}
	if !g.read {
		g.waiters, g.readOK = g.e.slotWaitersWithKeys()
		g.read = true
	}
	if !g.readOK {
		return true
	}
	foreign, ok := foreignOf(g.waiters, batchID)
	return !ok || batchSlotGateFrom(pg.items, pg.keys, foreign, done).blocks(key)
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
	case c.stale:
		return PlanReason{ReasonNotPolling, "Nothing has polled it for a long time, so it is not waiting for a slot and holds no one back; look at the conversion itself to see what keeps it."}
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
		case e.planRoomHold(sim.placer.ctx, w) == "build":
			return PlanReason{ReasonNodeBuild, "A build is running on node " + w.Name + ", so it takes nothing new."}
		case e.planRoomHold(sim.placer.ctx, w) != "":
			return PlanReason{ReasonNodeOnHold, "Node " + w.Name + " reports that it is on hold and takes nothing new."}
		case e.workerWindowClosed(w.Name):
			return PlanReason{windowWaitCondition, "Node " + w.Name + "'s availability window has closed; it takes nothing new."}
		}
		return PlanReason{ReasonNoFreeSlot, "Every video slot on node " + w.Name + " is in use."}
	}
	if !dec.Within[w.Name] {
		return PlanReason{ReasonSlotTakenAhead, fmt.Sprintf("%d conversion(s) ahead of it in the queue need the free slots it could use, so they go first.", dec.Ahead)}
	}
	return PlanReason{ReasonStartsElsewhere, "It starts on node " + dec.Node + " instead."}
}

// planWhyNone says why nothing would start on the node.
func planWhyNone(node, state, hold string, poolOnly bool, entries []QueueEntry) *PlanReason {
	// The state of the node wins: an empty queue does not make an unknown,
	// offline, paused or closed node look idle.
	switch state {
	case NodeOffline:
		return &PlanReason{ReasonNodeOffline, "Node " + node + " is not ready (offline or failing its checks), so it takes nothing new."}
	case NodeUnknown:
		return &PlanReason{ReasonNodeUnknown, unknownNodeText("Node", node, poolOnly) + "."}
	case NodePaused:
		switch hold {
		case "build":
			return &PlanReason{ReasonNodeBuild, "A build is running on node " + node + ", so it takes nothing new."}
		case "":
		default:
			return &PlanReason{ReasonNodeOnHold, "Node " + node + " reports that it is on hold and takes nothing new."}
		}
		return &PlanReason{ReasonNodePaused, "Node " + node + " is paused and takes nothing new."}
	case NodeClosed:
		return &PlanReason{windowWaitCondition, "Node " + node + "'s availability window has closed; it takes nothing new until the window changes."}
	case NodeBusy:
		return &PlanReason{ReasonNoFreeSlot, "Every video slot on node " + node + " is in use."}
	}
	if len(entries) == 0 {
		return &PlanReason{ReasonQueueEmpty, "Nothing is waiting in the transcode queue."}
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

// reportAtOf is when worker i last reported (zero: never, or not read).
func (q queueSlots) reportAtOf(i int) time.Time {
	if i < len(q.reportAt) {
		return q.reportAt[i]
	}
	return time.Time{}
}
