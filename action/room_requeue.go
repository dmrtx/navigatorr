package action

import (
	"context"
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
	// roomRefusedByKey and roomRefusedAtKey name the node that refused or
	// stopped it and when: that node is not picked again until its report has
	// had time to change, so a stale report cannot bounce the conversion.
	roomRefusedByKey = "room_refused_by"
	roomRefusedAtKey = "room_refused_at"
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
	ec.State[roomRefusedByKey] = e.instanceWorker(ec.State)
	ec.State[roomRefusedAtKey] = e.now().Format(time.RFC3339Nano)
	return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_busy", WaitingReason: why, Requeue: true}
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
