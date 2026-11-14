package action

import (
	"context"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// Invariants of placement (written before the code; the scenario tests in
// transcode_queue_placement_test.go hold them).
//
//	I1  What the placer's placeable answers (ready, with room, not refused by
//	    that node a moment ago) is the only eligibility decision for the
//	    queue claim of the conversion asking for a turn, for every waiter and
//	    batch item ahead of it, and for the pick. The pick only chooses among
//	    the placeable workers by free slots, within and preference.
//	    roomNeverFits and roomWaitReason are reporting helpers (they fail or
//	    word a wait); they decide no eligibility.
//	I2  A node that refused a conversion within roomRefusedFor is excluded for
//	    that conversion, each node on its own entry (room_refusals), so a
//	    conversion that keeps being refused never holds that node's slot
//	    against the ones behind it.
//	I3  With a file resolver, a conversion whose size is unknown reaches no
//	    node and so holds no slot; its size is read again at most once a
//	    minute (sourceSizeRetry) while the read only timed out. A source that
//	    can never be read (no path, outside the read roots, missing, irregular
//	    or empty) is marked source_size_unreadable and placed as needing no
//	    room, so the node fails it with the real error instead of it waiting
//	    forever. Without a resolver it is placed as needing no room too.
//	I4  Batch items not yet started as children are disk-only (placement{}):
//	    they are not waiter rows and have no size to read.

// Who may take which free slot.
//
// A conversion that needs retention room can be given only some of the
// workers (see worker_retention_fit.go), so the queue cannot just count the
// conversions ahead of it against the free slots: two files that fit only one
// node must not hold a conversion back from a free slot on the other. Each
// conversion in the queue is a claim on the free slots of the workers it can
// use. A conversion may take a slot when doing so leaves every conversion
// ahead of it as well placed as before, that is, when the conversions ahead of
// it can still all be given the slots they can be given.

// queueClaim is what a conversion in the queue can be given.
type queueClaim struct {
	// reach are the workers it could be given if they had a free slot.
	reach []bool
	// usable are those of them with a free slot now.
	usable []bool
}

// placement describes a conversion for the placer: what it needs, which nodes
// refused it and when, and whether its size is unknown to a server that could
// read it.
type placement struct {
	need        transcode.RoomNeed
	refusals    map[string]time.Time
	sizeUnknown bool
}

// placementOf is the placement of the conversion being scheduled.
func (e *Engine) placementOf(ec *ExecutionContext) placement {
	size := getInt64(ec.State, SubmittedSourceBytesKey)
	return placement{
		need:        conversionRoomNeed(ec),
		refusals:    refusalsFromState(ec.State[roomRefusalsKey]),
		sizeUnknown: e.deps.Fs != nil && size <= 0 && !getBool(ec.State, sourceSizeUnreadableKey),
	}
}

// waiterPlacement is the placement of a conversion waiting in the queue.
func (e *Engine) waiterPlacement(w store.TranscodeQueueWaiterRow) placement {
	return placement{
		need:        roomNeed(w.Standalone, w.SourceBytes, w.ReserveBytes),
		refusals:    parseRefusals(w.Refusals),
		sizeUnknown: e.deps.Fs != nil && w.SourceBytes <= 0 && !w.SizeUnreadable,
	}
}

// refusedRecently says whether the node refused the conversion so lately that
// its room report may not show it yet.
func (e *Engine) refusedRecently(p placement, node string) bool {
	at, ok := p.refusals[node]
	return ok && e.now().Sub(at) < roomRefusedFor
}

// placementReady is which workers count for placement: the ready ones. With
// one worker there is no choice to make and it counts as ready (its readiness
// is checked by the steps, as the jobs are sent). With none ready, none
// counts.
func (e *Engine) placementReady(ctx context.Context, workers []transcode.NamedWorker) []bool {
	if len(workers) == 1 {
		return []bool{true}
	}
	return e.workersReady(ctx, workers)
}

// placer is the one placement decision (I1): the workers, which are ready, and
// the room eligibility of each need, read once.
type placer struct {
	e       *Engine
	ctx     context.Context
	workers []transcode.NamedWorker
	ready   []bool
	elig    map[transcode.RoomNeed][]bool
}

func (e *Engine) newPlacer(ctx context.Context, workers []transcode.NamedWorker, ready []bool) *placer {
	return &placer{e: e, ctx: ctx, workers: workers, ready: ready, elig: map[transcode.RoomNeed][]bool{}}
}

// eligible is, for each worker, whether it is open and reports room for need.
func (pl *placer) eligible(need transcode.RoomNeed) []bool {
	el, ok := pl.elig[need]
	if !ok {
		el = pl.e.retentionEligible(pl.ctx, pl.workers, need)
		pl.elig[need] = el
	}
	return el
}

// placeable says, for each worker, whether the conversion may be given it now,
// free slot or not: the worker is ready, open and reports room for it, and did
// not refuse it a moment ago. A conversion whose size is unknown is placeable
// nowhere.
func (pl *placer) placeable(p placement) []bool {
	out := make([]bool, len(pl.workers))
	if p.sizeUnknown {
		return out
	}
	el := pl.eligible(p.need)
	for i, w := range pl.workers {
		out[i] = pl.ready[i] && el[i] && !pl.e.refusedRecently(p, w.Name)
	}
	return out
}

// waitReason is the one wording of why a conversion cannot be given a worker
// (the queue turn and the pick both use it), by priority. It returns "" when
// the cause is none of these (a full worker, a turn not yet come) and the
// caller words that.
func (pl *placer) waitReason(p placement) (condition, reason string) {
	if !anyOf(pl.ready) {
		return "worker_unreachable", "No video worker is ready; the job waits for one before it is assigned"
	}
	if p.sizeUnknown {
		return "worker_busy", "Its source size could not be read; it is checked again shortly"
	}
	el := pl.eligible(p.need)
	withRoom, refused := 0, 0
	for i, w := range pl.workers {
		if pl.ready[i] && el[i] {
			withRoom++
			if pl.e.refusedRecently(p, w.Name) {
				refused++
			}
		}
	}
	switch {
	case withRoom > 0 && refused == withRoom:
		return "worker_busy", "A node with room refused it a moment ago; it is asked again shortly"
	case withRoom == 0 && p.need.SourceBytes > 0:
		return "worker_busy", pl.e.roomWaitReason(pl.ctx, pl.workers, p.need)
	}
	return "", ""
}

// claimFor is the claim of a conversion with the placement p.
func (e *Engine) claimFor(q queueSlots, p placement) queueClaim {
	c := queueClaim{reach: q.placer.placeable(p), usable: make([]bool, len(q.workers))}
	for i, w := range q.workers {
		c.usable[i] = c.reach[i] && q.free[w.Name] > 0
	}
	return c
}

func (c queueClaim) hasReach() bool  { return anyOf(c.reach) }
func (c queueClaim) hasUsable() bool { return anyOf(c.usable) }

func anyOf(mask []bool) bool {
	for _, on := range mask {
		if on {
			return true
		}
	}
	return false
}

// competesWith reports whether both claims could be given some same worker.
func (c queueClaim) competesWith(o queueClaim) bool {
	for i := range c.reach {
		if c.reach[i] && o.reach[i] {
			return true
		}
	}
	return false
}

// freeSlots is the free slots of each worker, in worker order; a worker that
// is not ready has none.
func (q queueSlots) freeSlots() []int {
	out := make([]int, len(q.workers))
	for i, w := range q.workers {
		out[i] = q.free[w.Name]
	}
	return out
}

// placed is how many of the claims can be given a slot at the same time, each
// on a worker it can use, with free slots on each worker.
func placed(claims []queueClaim, free []int) int {
	var slotWorker []int
	for w, n := range free {
		for ; n > 0; n-- {
			slotWorker = append(slotWorker, w)
		}
	}
	owner := make([]int, len(slotWorker))
	for s := range owner {
		owner[s] = -1
	}
	var give func(c int, seen []bool) bool
	give = func(c int, seen []bool) bool {
		for s, w := range slotWorker {
			if seen[s] || !claims[c].usable[w] {
				continue
			}
			seen[s] = true
			if owner[s] < 0 || give(owner[s], seen) {
				owner[s] = c
				return true
			}
		}
		return false
	}
	n := 0
	for c := range claims {
		if n == len(slotWorker) {
			break
		}
		if give(c, make([]bool, len(slotWorker))) {
			n++
		}
	}
	return n
}

// slotsLeftAfter is how many free slots remain once the claims ahead have each
// taken one.
func (q queueSlots) slotsLeftAfter(ahead []queueClaim) int {
	return max(0, q.total()-placed(ahead, q.freeSlots()))
}

// safeWorkers are the workers on which the claim may take a slot without the
// claims ahead of it losing one they could have had.
func (q queueSlots) safeWorkers(cur queueClaim, ahead []queueClaim) map[string]bool {
	free := q.freeSlots()
	base := placed(ahead, free)
	safe := map[string]bool{}
	for i, w := range q.workers {
		if !cur.usable[i] {
			continue
		}
		free[i]--
		if placed(ahead, free) == base {
			safe[w.Name] = true
		}
		free[i]++
	}
	return safe
}
