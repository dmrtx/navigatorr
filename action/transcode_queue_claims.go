package action

import (
	"context"

	"github.com/jakenesler/navigatorr/transcode"
)

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

// claimFor is the claim of a conversion that needs the room need.
func (e *Engine) claimFor(ctx context.Context, q queueSlots, need transcode.RoomNeed) queueClaim {
	eligible := e.retentionEligible(ctx, q.workers, need)
	c := queueClaim{reach: make([]bool, len(q.workers)), usable: make([]bool, len(q.workers))}
	for i, w := range q.workers {
		c.reach[i] = q.ready[i] && eligible[i]
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
