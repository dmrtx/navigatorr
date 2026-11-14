package action

import (
	"context"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// Invariants of placement (written before the code; the scenario test in
// transcode_queue_placement_test.go and the tests of this file hold them).
//
//	I1  "Which workers may this conversion be given now" has one answer,
//	    placeableOn. The queue claim of the conversion asking for a turn, the
//	    claim of every waiter and batch item ahead of it, and pickVideoWorker
//	    all read it; pickVideoWorker only chooses among those workers by free
//	    slots and preference. A waiter's claim therefore reaches exactly the
//	    workers the pick could give it.
//	I2  A worker that refused a conversion within roomRefusedFor is in neither
//	    its claim nor its pick, so a conversion that keeps being refused never
//	    holds that worker's slot against the ones behind it.
//	I3  A queued standalone conversion has a known source size after its first
//	    queue turn (ensureSourceSize): "unknown" never means "needs nothing"
//	    for longer than one turn. If the size truly cannot be read (no file
//	    system, a stat that does not answer) the conversion is placed as one
//	    that needs no room; the node it is given refuses at once if it has
//	    none, and I2 then keeps that node's slot free for the others.
//	I4  Batch items keep a disk-only claim (RoomNeed{}) and go through the
//	    same function.

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

// placement describes a conversion for placeableOn: what it needs and which
// worker refused it, when.
type placement struct {
	need      transcode.RoomNeed
	refusedBy string
	refusedAt time.Time
}

// placementOf is the placement of the conversion being scheduled.
func placementOf(ec *ExecutionContext) placement {
	return placement{need: conversionRoomNeed(ec), refusedBy: getString(ec.State, roomRefusedByKey), refusedAt: parseStoreTime(getString(ec.State, roomRefusedAtKey))}
}

// waiterPlacement is the placement of a conversion waiting in the queue.
func waiterPlacement(w store.TranscodeQueueWaiterRow) placement {
	return placement{need: roomNeed(w.Standalone, w.SourceBytes, w.ReserveBytes), refusedBy: w.RefusedBy, refusedAt: parseStoreTime(w.RefusedAt)}
}

// refusedRecently says whether the node refused the conversion so lately that
// its room report may not show it yet.
func (e *Engine) refusedRecently(p placement, node string) bool {
	return node != "" && p.refusedBy == node && !p.refusedAt.IsZero() && e.now().Sub(p.refusedAt) < roomRefusedFor
}

// placeableOn says, for each worker, whether the conversion may be given it
// now, free slot or not: the worker is ready, open and reports room for it,
// and did not refuse it a moment ago. It is the one place that decides this
// (see I1 and I2 above).
func (e *Engine) placeableOn(ctx context.Context, workers []transcode.NamedWorker, ready []bool, p placement) []bool {
	eligible := e.retentionEligible(ctx, workers, p.need)
	out := make([]bool, len(workers))
	for i, w := range workers {
		out[i] = ready[i] && eligible[i] && !e.refusedRecently(p, w.Name)
	}
	return out
}

// claimFor is the claim of a conversion with the placement p.
func (e *Engine) claimFor(ctx context.Context, q queueSlots, p placement) queueClaim {
	c := queueClaim{reach: e.placeableOn(ctx, q.workers, q.ready, p), usable: make([]bool, len(q.workers))}
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
