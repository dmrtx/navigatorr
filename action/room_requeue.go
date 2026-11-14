package action

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// A node that has no room for a conversion refuses it at once (see
// worker_retention_fit.go); one whose disk ran low under an encode stops it. In
// both cases the conversion goes back to the coordinator's queue, to be placed
// again when some node has room. It is never failed for lack of room.
const (
	// requeueWhyKey is why the conversion is being put back, read once by
	// resetForRequeue for the reason it shows.
	requeueWhyKey = "requeue_why"
	// roomRefusalsKey holds, per node that refused or stopped the conversion,
	// when it did: {"<node>": "<RFC3339Nano time>"}. A node is not picked
	// again until its report has had time to change, so a stale report cannot
	// bounce the conversion; each node has its own entry.
	roomRefusalsKey = "room_refusals"
	// diskFloorReason is the reason code of an encode stopped for free disk.
	diskFloorReason = "disk_floor"
	// runnerKilledClass is the failure class of a job whose runner died with
	// its worker (a restart or a crash): the encode is lost, not wrong.
	runnerKilledClass = "runner_killed"
	// runnerKilledKey counts the times in a row it happened to this request.
	runnerKilledKey = "runner_killed_count"
	// runnerKilledJobKey is the worker job whose failure was last counted, so
	// polling the same failure again does not count it twice.
	runnerKilledJobKey = "runner_killed_job"
	// maxRunnerKilled is the failure that is not requeued: the first two go
	// back to the queue and the third fails with the worker's reason, so a job
	// that kills its runner cannot loop.
	maxRunnerKilled = 3
)

// roomRefusedFor is how long the node that refused a conversion is passed over.
const roomRefusedFor = 2 * transcode.RetentionRoomMaxAge

// roomRequeue is the step result that puts the conversion back in the queue
// for the reason why. The engine applies it (see applyRoomRequeue); when the
// node's job cannot be confirmed cancelled it waits as an ordinary busy wait.
func (e *Engine) roomRequeue(ec *ExecutionContext, why string) StepResult {
	why = strings.TrimSpace(why)
	ec.State[requeueWhyKey] = why
	e.recordRoomRefusal(ec, e.instanceWorker(ec.State))
	return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_busy", WaitingReason: why, Requeue: true}
}

// recordRoomRefusal adds (or overwrites) the refusing node's entry and drops
// the entries that are past the window.
func (e *Engine) recordRoomRefusal(ec *ExecutionContext, node string) {
	now := e.now()
	out := map[string]any{}
	for n, at := range refusalsFromState(ec.State[roomRefusalsKey]) {
		if now.Sub(at) < roomRefusedFor {
			out[n] = at.Format(time.RFC3339Nano)
		}
	}
	if node != "" {
		out[node] = now.Format(time.RFC3339Nano)
	}
	ec.State[roomRefusalsKey] = out
}

// refusalsFromState reads the refusals kept in a conversion's state (an object
// of node to time; anything else is no refusals).
func refusalsFromState(v any) map[string]time.Time {
	var out map[string]time.Time
	add := func(node, at string) {
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil && node != "" {
			if out == nil {
				out = map[string]time.Time{}
			}
			out[node] = t
		}
	}
	switch m := v.(type) {
	case map[string]any:
		for n, a := range m {
			if s, ok := a.(string); ok {
				add(n, s)
			}
		}
	case map[string]string:
		for n, a := range m {
			add(n, a)
		}
	}
	return out
}

// parseRefusals reads the raw JSON a waiter row carries; malformed text is no
// refusals.
func parseRefusals(raw string) map[string]time.Time {
	if raw == "" {
		return nil
	}
	var m map[string]string
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil
	}
	return refusalsFromState(m)
}

// applyRoomRequeue finishes roomRequeue: the job is cancelled on its node (it
// may not exist there) and the action goes back to the queue. It reports
// whether it did.
func (e *Engine) applyRoomRequeue(ctx context.Context, inst *store.ActionInstance, ec *ExecutionContext) (bool, error) {
	jobID := getString(ec.State, "job_id")
	if jobID == "" {
		jobID = getString(ec.State, "external_reference")
	}
	return e.finishPauseRequeue(ctx, inst, ec, jobID, store.WorkerPauseImmediate)
}

func roomRefusalText(err error) string {
	if he, ok := transcodeHTTPError(err); ok && he.Message != "" {
		return he.Message
	}
	return fmt.Sprint(err)
}
