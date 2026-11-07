package action

import (
	"context"
	"fmt"

	"github.com/jakenesler/navigatorr/transcode"
)

// Keeping a conversion off a worker that cannot retain its candidate.
//
// A standalone conversion on a worker that publishes on approval waits, queued
// on the worker, until the worker has room to retain its candidate (see
// DEFERRED_CANDIDATE_PUBLICATION.md). The worker reserves the size of the
// file for it, since the candidate's size is not known before the encode. A
// worker whose retained candidates already fill most of its budget cannot take
// a large file, while another may have room; and the conversion, once given a
// worker, stays on it. So the coordinator gives a conversion only to a worker
// that reports room for it, and one that fits nowhere waits in the queue
// unassigned, out of the way of the conversions behind it (see
// transcodeQueueTurn).
//
// A worker that does not report its room, or whose report is old, is taken to
// have it, as before workers reported it. A file larger than the whole budget
// of every ready worker fits nowhere, ever: waiting would never help, so the
// workers decide it as before.

// retentionNeed is the room a conversion needs on a worker that keeps its
// candidate until it is approved: the size of its file. A batch child needs
// none: the worker starts it to publish on encode when the budget is full.
func retentionNeed(standalone bool, sourceBytes int64) int64 {
	if !standalone || sourceBytes <= 0 {
		return 0
	}
	return sourceBytes
}

// conversionRetentionNeed is retentionNeed for the conversion being scheduled.
func conversionRetentionNeed(ec *ExecutionContext) int64 {
	return retentionNeed(getString(ec.Inputs, "parent_action_id") == "", getInt64(ec.State, SubmittedSourceBytesKey))
}

// retentionEligible says, for each worker, whether a conversion that needs
// need bytes of retention room may be given it. Workers that are not ready
// are left eligible: readiness is judged apart.
func (e *Engine) retentionEligible(ctx context.Context, workers []transcode.NamedWorker, ready []bool, need int64) []bool {
	out := make([]bool, len(workers))
	for i := range out {
		out[i] = true
	}
	if need <= 0 {
		return out
	}
	fits := make([]transcode.RetentionFit, len(workers))
	holdsSomewhere := false
	for i, w := range workers {
		if !ready[i] || w.PublishPolicy != transcode.PublishPolicyOnApproval {
			if ready[i] {
				holdsSomewhere = true // publishes on encode: needs no room
			}
			continue
		}
		fits[i] = e.workerObservation(ctx, w).Retention.Fit(need, e.now())
		if fits[i] != transcode.RetentionTooLarge {
			holdsSomewhere = true
		}
	}
	if !holdsSomewhere {
		return out
	}
	for i := range workers {
		out[i] = fits[i] == transcode.RetentionFits
	}
	return out
}

// retentionWaitReason words the wait of a conversion that has a free slot
// nowhere it could be retained.
func retentionWaitReason(need int64) string {
	return fmt.Sprintf("No node with a free slot has room to retain its candidate (%.1f GiB); waiting for room, or for a node that has it", float64(need)/(1<<30))
}
