package maintenanceui

import (
	"math"
	"sort"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

// A job waiting for a worker slot gets an estimated wait from the encode
// history: replay the worker's slots, each freed when its job is expected to
// finish, and hand them to the waiters in queue order. Anything the estimate
// needs and the history cannot give leaves the job without a number.

// encodeRecord is the part of a record the encode history is read from; the
// reading itself (action.RecordEncodeShape, action.EncodeSamples) is shared
// with the nodes' availability windows.
func encodeRecord(r operationRecord) action.EncodeRecord {
	return action.EncodeRecord{ID: r.inst.ID, ActionName: r.inst.ActionName, Status: r.inst.Status, UpdatedAt: r.inst.UpdatedAt, Outputs: r.outputs, State: r.state}
}

// recordEncodeShape reads a transcode job's worker, height class, encoder
// family and video length from the trimmed snapshot.
func recordEncodeShape(r operationRecord) (action.EncodeShape, bool) {
	return action.RecordEncodeShape(encodeRecord(r))
}

// encodeSamples are the finished encodes of the history, newest first.
func encodeSamples(records []operationRecord) []action.EncodeSample {
	recs := make([]action.EncodeRecord, 0, len(records))
	for _, r := range records {
		if r.inst.ActionName == "transcode_media" && r.inst.Status == store.ActionStatusCompleted {
			recs = append(recs, encodeRecord(r))
		}
	}
	return action.EncodeSamples(recs)
}

// queueWaiter is a job in a worker's slot queue with the encode time it will
// take once it has a slot (known false when the history cannot say).
type queueWaiter struct {
	id       string
	position int
	encode   float64
	known    bool
	// onWorker is a job already handed to the worker and waiting in its own
	// line: it goes before every coordinator waiter and gets no number shown.
	onWorker bool
}

// simulateQueueWait returns each waiter's wait in seconds. free is when each
// slot of the worker frees up, in seconds from now (0 for an idle slot).
// Jobs already in the worker's own line go first, by the place the worker
// reports, then the coordinator's queue by position. A waiter whose own encode time is unknown, or a gap in
// the positions (a waiter ahead that this page cannot see), leaves everyone
// from there on without a number.
func simulateQueueWait(free []float64, waiters []queueWaiter) map[string]float64 {
	out := map[string]float64{}
	if len(free) == 0 {
		return out
	}
	free = append([]float64(nil), free...)
	sort.Slice(waiters, func(i, j int) bool {
		if waiters[i].onWorker != waiters[j].onWorker {
			return waiters[i].onWorker
		}
		if waiters[i].position == waiters[j].position {
			return waiters[i].id < waiters[j].id
		}
		return waiters[i].position < waiters[j].position
	})
	// Each line must be numbered 1, 2, 3... with no gap or tie: otherwise
	// the order is not known and nobody from there on gets a number.
	next := map[bool]int{true: 1, false: 1}
	for i, w := range waiters {
		if w.position != next[w.onWorker] || i+1 < len(waiters) && waiters[i+1].onWorker == w.onWorker && waiters[i+1].position == w.position {
			break
		}
		next[w.onWorker]++
		sort.Float64s(free)
		if !w.onWorker {
			out[w.id] = free[0]
		}
		if !w.known {
			break
		}
		free[0] += w.encode
	}
	return out
}

// queueWaits estimates the wait of every transcode job in a worker's slot
// queue, in seconds. A worker is left out when its slot count, the remaining
// time of any job on its slots or the history for them is unknown.
func queueWaits(records []operationRecord) map[string]int64 {
	type node struct {
		slots   int
		busy    []float64
		unknown bool
		waiters []queueWaiter
	}
	nodes := map[string]*node{}
	// Most reads have nobody waiting for a slot; skip the history then.
	waiting := false
	for _, r := range records {
		if r.inst.ActionName == "transcode_media" && operationActive(r.inst.Status) && operationString(operationMap(operationValue(r, "work_progress"))["phase"]) == "waiting_for_worker" {
			waiting = true
			break
		}
	}
	if !waiting {
		return map[string]int64{}
	}
	samples := encodeSamples(records)
	for _, r := range records {
		if r.inst.ActionName != "transcode_media" || !operationActive(r.inst.Status) {
			continue
		}
		worker := operationString(r.state["worker"])
		if worker == "" {
			continue
		}
		n := nodes[worker]
		if n == nil {
			n = &node{}
			nodes[worker] = n
		}
		if slots := int(operationNumber(operationValue(r, "worker_slots_total"))); slots > n.slots {
			n.slots = slots
		}
		shape, shaped := recordEncodeShape(r)
		estimate, estimated := 0.0, false
		if shaped {
			estimate, estimated = action.EstimateEncodeSeconds(samples, worker, shape.Class, shape.Family, shape.DurationSec)
		}
		// A frozen node or a paused job starts nothing on a clock this page
		// can see: no numbers for that worker.
		if operationValue(r, "worker_suspended") == true || operationValue(r, "paused") == true {
			n.unknown = true
			continue
		}
		work := operationMap(operationValue(r, "work_progress"))
		if operationString(work["phase"]) == "waiting_for_worker" {
			position := int(operationNumber(work["queue_position"]))
			if position <= 0 {
				n.unknown = true
				continue
			}
			n.waiters = append(n.waiters, queueWaiter{id: r.inst.ID, position: position, encode: estimate, known: estimated})
			continue
		}
		// A job past submission holds a slot. Its remaining time is its
		// measured ETA, else the estimate less the progress made; before or
		// after the encode itself there is nothing to measure it by.
		phase := operationString(operationValue(r, "transcode_phase"))
		if phase == "queued" {
			// Held for room (free disk, the retention budget) rather than a
			// slot: when it starts is not a matter of encode times.
			if reason := r.inst.WaitingReason; strings.Contains(reason, "(free_disk)") || strings.Contains(reason, "(retention_budget)") || strings.Contains(reason, "(retention_alone)") {
				n.unknown = true
				continue
			}
			// The worker reports the job's place in its own line.
			position := int(operationNumber(operationValue(r, "queue_position")))
			n.waiters = append(n.waiters, queueWaiter{id: r.inst.ID, position: position, encode: estimate, known: estimated, onWorker: true})
			continue
		}
		// Before submission (preflight) or after the worker finished, the job
		// holds no worker slot.
		if phase == "" || phase == "completed" {
			continue
		}
		switch progress := operationNumber(operationValue(r, "progress")); {
		case r.savings.ETASeconds != nil:
			n.busy = append(n.busy, float64(*r.savings.ETASeconds))
		case phase == "encoding" && estimated && operationValue(r, "progress_is_stale") != true && progress >= 0 && progress < 100:
			n.busy = append(n.busy, estimate*(1-progress/100))
		default:
			n.unknown = true
		}
	}
	out := map[string]int64{}
	for _, n := range nodes {
		// More jobs on the slots than the slot count: the count is out of date.
		if n.unknown || n.slots == 0 || len(n.waiters) == 0 || len(n.busy) > n.slots {
			continue
		}
		free := append([]float64(nil), n.busy...)
		for len(free) < n.slots {
			free = append(free, 0)
		}
		for id, wait := range simulateQueueWait(free, n.waiters) {
			if seconds := int64(math.Ceil(wait)); seconds > 0 {
				out[id] = seconds
			}
		}
	}
	return out
}

// setQueueWaits puts each waiter's estimated wait on its savings, next to the
// encode ETA.
func setQueueWaits(records []operationRecord) {
	waits := queueWaits(records)
	for i := range records {
		if seconds, ok := waits[records[i].inst.ID]; ok {
			records[i].savings.QueueWaitSeconds = &seconds
		}
	}
}
