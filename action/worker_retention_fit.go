package action

import (
	"context"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

// Which node has room for a conversion.
//
// A node never waits for room (see DEFERRED_CANDIDATE_PUBLICATION.md, "Room"):
// it takes a conversion or refuses it at once. So the coordinator keeps every
// conversion that has no room in its own queue, in order, and gives the oldest
// one that fits some open node to that node. Each node reports what it can take
// now: its free disk plus the cached sources it would delete, less its floor
// and the output reserves of encodes still writing, and the retention budget
// left. A node that does not report, or whose report is old or dated ahead of
// the coordinator's clock, is taken to have room; it refuses at once if it has
// none, and the conversion comes back.
//
// A conversion that no node could take even when empty, because its source,
// output and the node's floor outgrow the disk or its output outgrows the
// retention budget, fails when it is first placed.
//
// Who may take which free slot is worked out in transcode_queue_claims.go.

// roomReserveKey is the output reserve the conversion's plan asks for, stored
// when its plan is built so a requeued conversion is placed by the exact figure.
const roomReserveKey = "room_reserve_bytes"

// roomNeed is the room a conversion needs on a worker that keeps its candidate
// until it is approved. A batch child needs none: its worker starts it to
// publish on encode when the budget is full. Until the plan is known the
// output reserve is the quality goal's half of the source.
func roomNeed(standalone bool, sourceBytes, reserveBytes int64) transcode.RoomNeed {
	if !standalone || sourceBytes <= 0 {
		return transcode.RoomNeed{}
	}
	if reserveBytes <= 0 {
		reserveBytes = transcode.OutputReserveBytes(nil, sourceBytes)
	}
	return transcode.RoomNeed{SourceBytes: sourceBytes, ReserveBytes: reserveBytes}
}

// conversionRoomNeed is roomNeed for the conversion being scheduled.
func conversionRoomNeed(ec *ExecutionContext) transcode.RoomNeed {
	return roomNeed(getString(ec.Inputs, "parent_action_id") == "", getInt64(ec.State, SubmittedSourceBytesKey), getInt64(ec.State, roomReserveKey))
}

// roomOf is each worker's room report; nil for one that publishes on encode
// (it needs no room) or reports none.
func (e *Engine) roomOf(ctx context.Context, workers []transcode.NamedWorker) []*transcode.RetentionRoom {
	rooms := make([]*transcode.RetentionRoom, len(workers))
	for i, w := range workers {
		if w.PublishPolicy == transcode.PublishPolicyOnApproval {
			rooms[i] = e.workerObservation(ctx, w).Retention
		}
	}
	return rooms
}

// retentionEligible says, for each worker, whether a conversion that needs
// need may be given it now: the worker is open (not paused, no build) and
// reports room. Readiness and free slots are judged apart.
func (e *Engine) retentionEligible(ctx context.Context, workers []transcode.NamedWorker, need transcode.RoomNeed) []bool {
	out := make([]bool, len(workers))
	for i := range out {
		out[i] = true
	}
	if need.SourceBytes <= 0 {
		return out
	}
	for i, r := range e.roomOf(ctx, workers) {
		verdict, _ := r.Judge(need, e.now())
		out[i] = verdict == transcode.RoomFits
	}
	return out
}

// roomNeverFits says whether no worker could take the conversion even empty,
// and why. Every worker must say so: one that does not report, or publishes on
// encode, could take it.
func (e *Engine) roomNeverFits(ctx context.Context, workers []transcode.NamedWorker, need transcode.RoomNeed) (string, bool) {
	if need.SourceBytes <= 0 || len(workers) == 0 {
		return "", false
	}
	var biggest int64
	for _, r := range e.roomOf(ctx, workers) {
		if r == nil || r.EverFits(need) {
			return "", false
		}
		biggest = max(biggest, r.DiskCapacityBytes-r.FloorBytes)
	}
	return fmt.Sprintf("Too big for every node: it needs %s (source %s and output %s) and the largest node has %s to give a job", gbText(need.SourceBytes+need.ReserveBytes), gbText(need.SourceBytes), gbText(need.ReserveBytes), gbText(biggest)), true
}

// roomWaitReason words why no node can take a conversion now, in plain terms:
// which node is paused, which has a build, which waits for approvals and how
// much it holds, and how much the best disk can offer against what is needed.
func (e *Engine) roomWaitReason(ctx context.Context, workers []transcode.NamedWorker, need transcode.RoomNeed) string {
	var parts []string
	var bestOffer int64
	space := false
	for i, r := range e.roomOf(ctx, workers) {
		name := workers[i].Name
		verdict, offered := r.Judge(need, e.now())
		switch verdict {
		case transcode.RoomPaused:
			parts = append(parts, fmt.Sprintf("node %s paused", name))
		case transcode.RoomBuild:
			parts = append(parts, fmt.Sprintf("build running on %s", name))
		case transcode.RoomApprovals:
			parts = append(parts, fmt.Sprintf("waiting for approvals on %s (%s held)", name, gbText(r.HeldBytes)))
		case transcode.RoomNoSpace:
			space = true
			bestOffer = max(bestOffer, offered)
		}
	}
	if space {
		parts = append(parts, fmt.Sprintf("no node has room: needs %s, the most any node can offer is %s", gbText(need.SourceBytes+need.ReserveBytes), gbText(bestOffer)))
	}
	if len(parts) == 0 {
		return "No node has room for it; waiting for room, or for a node that has it"
	}
	text := strings.Join(parts, "; ")
	return strings.ToUpper(text[:1]) + text[1:]
}

// retentionNoSlotReason words the wait of a conversion that has a node with
// room, but none with a free slot.
func retentionNoSlotReason(need transcode.RoomNeed) string {
	return fmt.Sprintf("Busy: waiting for a free slot on a node with room for it (%s)", gbText(need.SourceBytes+need.ReserveBytes))
}

func gbText(n int64) string { return fmt.Sprintf("%.1f GB", float64(max(n, 0))/1e9) }
