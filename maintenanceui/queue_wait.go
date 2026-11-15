package maintenanceui

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

// A job waiting for a worker slot gets an estimated wait from the encode
// history: replay the worker's slots, each freed when its job is expected to
// finish, and hand them to the waiters in queue order. Anything the estimate
// needs and the history cannot give leaves the job without a number.

// encodeShape is what an encode's duration depends on, read from a record.
type encodeShape struct {
	worker, class, family string
	durationSec           float64
}

// recordEncodeShape reads a transcode job's worker, height class, encoder
// family and video length from the trimmed snapshot. The family comes from the
// encoder the plan chose: the size budget's encoder choice, else the search's
// winner, which the snapshot keeps; an audio-only plan copies the video. A job
// that skipped the transcode has none.
func recordEncodeShape(r operationRecord) (encodeShape, bool) {
	if r.inst.ActionName != "transcode_media" || operationValue(r, "skip_transcode") == true {
		return encodeShape{}, false
	}
	original := operationMap(operationValue(r, "original"))
	width, height := 0, 0
	if w, h, ok := strings.Cut(operationString(original["resolution"]), "x"); ok {
		width, _ = strconv.Atoi(w)
		height, _ = strconv.Atoi(h)
	}
	family := ""
	if operationValue(r, "size_audio_only") == true {
		family = action.EncodeFamilyCopy
	} else {
		codec := operationString(operationMap(operationMap(operationValue(r, "size_budget"))["encoder_choice"])["video_codec"])
		if codec == "" {
			codec = operationString(operationMap(operationMap(operationValue(r, "benchmark_decision"))["winner"])["video_codec"])
		}
		if codec == "" {
			// A size goal without a budget runs the quality ladder instead.
			codec = operationString(operationMap(operationValue(r, "quality_ladder"))["video_codec"])
		}
		family = action.EncodeFamily(codec)
	}
	s := encodeShape{worker: operationString(r.state["worker"]), class: action.ResolutionClass(width, height), family: family, durationSec: operationNumber(original["duration_sec"])}
	return s, s.worker != "" && s.class != "" && s.family != "" && s.durationSec > 0
}

// encodeSamples are the finished encodes of the history, newest first.
func encodeSamples(records []operationRecord) []action.EncodeSample {
	done := []operationRecord{}
	for _, r := range records {
		if r.inst.ActionName == "transcode_media" && r.inst.Status == store.ActionStatusCompleted {
			done = append(done, r)
		}
	}
	sort.Slice(done, func(i, j int) bool {
		if done[i].inst.UpdatedAt == done[j].inst.UpdatedAt {
			return done[i].inst.ID > done[j].inst.ID
		}
		return done[i].inst.UpdatedAt > done[j].inst.UpdatedAt
	})
	var out []action.EncodeSample
	for _, r := range done {
		ms := operationNumber(operationValue(r, "encode_duration_ms"))
		shape, ok := recordEncodeShape(r)
		if ms <= 0 || !ok {
			continue
		}
		out = append(out, action.EncodeSample{Worker: shape.worker, Class: shape.class, Family: shape.family, DurationSec: shape.durationSec, EncodeSec: ms / 1000})
	}
	return out
}

// queueWaiter is a job in a worker's slot queue with the encode time it will
// take once it has a slot (known false when the history cannot say).
type queueWaiter struct {
	id       string
	position int
	encode   float64
	known    bool
}

// simulateQueueWait returns each waiter's wait in seconds. free is when each
// slot of the worker frees up, in seconds from now (0 for an idle slot).
// Waiters take the earliest slot in queue order; a waiter whose own encode
// time is unknown leaves everyone behind it without a number.
func simulateQueueWait(free []float64, waiters []queueWaiter) map[string]float64 {
	out := map[string]float64{}
	if len(free) == 0 {
		return out
	}
	free = append([]float64(nil), free...)
	sort.Slice(waiters, func(i, j int) bool {
		if waiters[i].position == waiters[j].position {
			return waiters[i].id < waiters[j].id
		}
		return waiters[i].position < waiters[j].position
	})
	for _, w := range waiters {
		sort.Float64s(free)
		out[w.id] = free[0]
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
			estimate, estimated = action.EstimateEncodeSeconds(samples, worker, shape.class, shape.family, shape.durationSec)
		}
		work := operationMap(operationValue(r, "work_progress"))
		if operationString(work["phase"]) == "waiting_for_worker" {
			if position := int(operationNumber(work["queue_position"])); position > 0 {
				n.waiters = append(n.waiters, queueWaiter{id: r.inst.ID, position: position, encode: estimate, known: estimated})
			}
			continue
		}
		// A job past submission holds a slot. Its remaining time is its
		// measured ETA, else the estimate less the progress made; before or
		// after the encode itself there is nothing to measure it by.
		phase := operationString(operationValue(r, "transcode_phase"))
		if phase == "" || phase == "queued" || phase == "completed" {
			continue
		}
		switch progress := operationNumber(operationValue(r, "progress")); {
		case r.savings.ETASeconds != nil:
			n.busy = append(n.busy, float64(*r.savings.ETASeconds))
		case phase == "encoding" && estimated && progress >= 0 && progress < 100:
			n.busy = append(n.busy, estimate*(1-progress/100))
		default:
			n.unknown = true
		}
	}
	out := map[string]int64{}
	for _, n := range nodes {
		if n.unknown || n.slots == 0 || len(n.waiters) == 0 {
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
