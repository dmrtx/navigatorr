package action

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/jakenesler/navigatorr/store"
)

// A batch parks in waiting_decision on behalf of a child. That child can be
// resolved without going through the batch (rejected or cancelled by its own
// ID), and the batch only recomputes its items and counters when it runs. Left
// alone it would keep the child's reason, options and a waiting_decision count
// for a decision nobody can make any more.

// batchDecisionStale reports a transcode_batch parked on a decision that a
// child no longer needs: one of its items is marked waiting_decision while that
// item's child action has moved on. Another child may still wait; re-running
// the batch recomputes the items and surfaces that child's own decision.
// Anything it cannot judge is not stale, so a decision is never dropped on a
// read error or a missing child.
func (e *Engine) batchDecisionStale(inst *store.ActionInstance, ec *ExecutionContext) bool {
	if inst.ActionName != "transcode_batch" || inst.Status != StatusWaitingDecision || getBool(ec.State, "paused") {
		return false
	}
	items, err := e.deps.Store.ListTranscodeBatchItems(inst.ID)
	if err != nil {
		return false
	}
	for _, item := range items {
		if item.Status != "waiting_decision" || item.ChildActionID == "" {
			continue
		}
		child, err := e.deps.Store.GetActionInstanceIfExists(item.ChildActionID)
		if err == nil && child != nil && child.Status != StatusWaitingDecision {
			return true
		}
	}
	return false
}

// refreshBatchParent re-evaluates the batch a child belongs to after the child
// was resolved on its own. A batch that is not waiting for a decision is left
// to its normal schedule, and a caller already holding the batch's lease (the
// batch forwarding a decision) recomputes it itself. When the batch is busy the
// reconciler's stale-decision sweep finishes the job.
func (e *Engine) refreshBatchParent(ctx context.Context, childID string) {
	if e.deps.Store == nil {
		return
	}
	item, err := e.deps.Store.FindTranscodeBatchItemByChildActionID(childID)
	if err != nil || item == nil {
		return
	}
	if _, inherited := leaseOwnerFromContext(ctx, item.BatchID); inherited {
		return
	}
	parent, err := e.deps.Store.GetActionInstanceIfExists(item.BatchID)
	if err != nil || parent == nil || parent.Status != StatusWaitingDecision {
		return
	}
	if _, err := e.resume(ctx, item.BatchID, "", nil, true); err != nil && !errors.Is(err, errActionBusy) && ctx.Err() == nil {
		log.Printf("action: refreshing batch %s after child %s: %v", item.BatchID, childID, err)
	}
}

// settleCancelledBatch brings a directly cancelled batch's items and counters
// in line with its children, which Cancel stops after the batch row is already
// cancelled. Without it the stored counters keep showing the files as queued,
// running or waiting for a decision. A child that has not reached a terminal
// state (remote cancellation still unconfirmed) keeps its item as it was.
func (e *Engine) settleCancelledBatch(ctx context.Context, batchID string) {
	if e.deps.Store == nil {
		return
	}
	inst, err := e.deps.Store.GetActionInstanceIfExists(batchID)
	if err != nil || inst == nil || inst.ActionName != "transcode_batch" || inst.Status != StatusCancelled {
		return
	}
	items, err := e.deps.Store.ListTranscodeBatchItems(batchID)
	if err != nil {
		return
	}
	changed := false
	for i := range items {
		it := &items[i]
		switch it.Status {
		case "completed", "failed", "cancelled", "skip":
			continue
		}
		if it.ChildActionID == "" {
			it.Status, it.Error = "cancelled", "cancelled"
		} else {
			child, err := e.deps.Store.GetActionInstanceIfExists(it.ChildActionID)
			if err != nil || child == nil {
				continue
			}
			switch child.Status {
			case StatusCancelled:
				it.Status, it.Error = "cancelled", "cancelled"
			case StatusFailed:
				it.Status, it.Error = "failed", child.ErrorJSON
			case StatusCompleted:
				var out map[string]any
				_ = json.Unmarshal([]byte(child.OutputsJSON), &out)
				it.Status, it.Error = "completed", ""
				if it.CandidatePath = getString(out, "candidate_path"); it.CandidatePath == "" {
					it.CandidatePath = getString(out, "output_path")
				}
			default:
				continue
			}
			if it.Status != "cancelled" && it.Attempts == 0 {
				it.Attempts = 1
			}
		}
		if err := e.deps.Store.UpdateTranscodeBatchItem(*it); err != nil {
			return
		}
		changed = true
	}
	if !changed {
		return
	}
	leaseCtx, release, err := e.claimExecution(ctx, batchID, true)
	if err != nil {
		return
	}
	defer release()
	inst, err = e.deps.Store.GetActionInstance(batchID)
	if err != nil || inst.Status != StatusCancelled {
		return
	}
	ec := parseExecutionContext(inst, e)
	for key, value := range buildBatchOutputs(batchID, items, getString(ec.State, "series_title"), getBool(ec.State, "is_anime"), false, getMaxOutputItems(ec.Inputs)) {
		ec.Outputs[key] = value
	}
	inst.OutputsJSON = toJSON(ec.Outputs)
	if err := e.updateInstance(leaseCtx, inst); err != nil {
		log.Printf("action: settling cancelled batch %s: %v", batchID, err)
	}
}
