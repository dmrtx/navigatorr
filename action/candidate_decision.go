package action

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/jakenesler/navigatorr/store"
)

type reviewedCandidateKey struct{}
type reviewedCandidate struct{ id, version, root string }

func CandidateDecisionVersion(inst *store.ActionInstance) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s", inst.Status, inst.CurrentStep, inst.WaitingReason, inst.WaitingOptionsJSON, inst.StateJSON))))
}

// Bind a decision to the exact candidate reviewed, under the parent's durable
// execution lease. A newer waiting file must never receive an earlier decision.
func (e *Engine) ResumeReviewedCandidate(ctx context.Context, id, candidateID, version, decision string) (*ActionResult, error) {
	if decision != "reject" && decision != "accept_loss" {
		return nil, fmt.Errorf("unsupported candidate decision")
	}
	return e.Resume(context.WithValue(ctx, reviewedCandidateKey{}, reviewedCandidate{candidateID, version, id}), id, decision, nil)
}

func (e *Engine) checkReviewedCandidate(ctx context.Context, inst *store.ActionInstance, decision string) error {
	review, ok := ctx.Value(reviewedCandidateKey{}).(reviewedCandidate)
	if !ok || inst.ID != review.root && inst.ID != review.id {
		return nil
	}
	target := inst.ID
	if inst.ActionName == "transcode_batch" {
		target = ""
		items, err := e.deps.Store.ListTranscodeBatchItems(inst.ID)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.Status == "waiting_decision" {
				target = item.ChildActionID
				break
			}
		}
	}
	if inst.Status != StatusWaitingDecision || target != review.id {
		return fmt.Errorf("the waiting candidate changed; review it again")
	}
	child, err := e.deps.Store.GetActionInstance(target)
	if err != nil {
		return err
	}
	if child == nil || child.ActionName != "transcode_media" || CandidateDecisionVersion(child) != review.version {
		return fmt.Errorf("candidate validation changed; review it again")
	}
	var options []WaitingOption
	_ = json.Unmarshal([]byte(child.WaitingOptionsJSON), &options)
	for _, option := range options {
		if option.Decision == decision {
			return nil
		}
	}
	return fmt.Errorf("candidate no longer allows that decision")
}
