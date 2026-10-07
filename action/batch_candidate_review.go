package action

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jakenesler/navigatorr/store"
)

type BatchCandidateReview struct {
	ID       string                 `json:"id"`
	Version  string                 `json:"version"`
	Service  string                 `json:"service"`
	Members  []batchPromotionMember `json:"members"`
	Excluded int                    `json:"excluded"`
}

// Read only durable current attempts. Historical candidates and reserved
// replacements never enter a new review. No NAS hashing occurs on the HTTP path.
func (e *Engine) BatchCandidates(ctx context.Context, id string) (*BatchCandidateReview, error) {
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		return nil, err
	}
	if inst.ActionName != "transcode_batch" || (inst.Status != StatusCompleted && inst.Status != StatusFailed) {
		return nil, fmt.Errorf("finish the batch before reviewing candidates")
	}
	archives, err := e.deps.Store.MaintenanceArchives()
	if err != nil {
		return nil, err
	}
	if archives[id] != "" {
		return nil, fmt.Errorf("restore the batch before reviewing candidates")
	}
	ec := parseExecutionContext(inst, e)
	if getBool(ec.Inputs, "dry_run") || ec.State["batch_promotion_plan"] != nil || len(stateObject(ec.State, "batch_retry_pending")) > 0 {
		return nil, fmt.Errorf("resolve the existing preview, settings change or replacement review first")
	}
	items, err := e.deps.Store.ListTranscodeBatchItems(id)
	if err != nil {
		return nil, err
	}
	eligible := []store.TranscodeBatchItem{}
	for _, item := range items {
		if item.Status != "completed" || item.Decision != "transcode" || item.ChildActionID == "" {
			continue
		}
		child, err := e.deps.Store.GetActionInstance(item.ChildActionID)
		if err != nil {
			return nil, err
		}
		out := map[string]any{}
		if json.Unmarshal([]byte(child.OutputsJSON), &out) != nil || child.Status != StatusCompleted || !getBool(out, "original_intact") {
			continue
		}
		reserved := false
		for _, service := range []string{"sonarr", "radarr", "filesystem"} {
			prior, err := e.deps.Store.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:"+service+":"+child.ID)
			if err != nil {
				return nil, err
			}
			if prior != nil {
				reserved = true
			}
		}
		if !reserved {
			eligible = append(eligible, item)
		}
	}
	plan, err := e.buildBatchPromotionPlan(ec, eligible)
	if err != nil {
		return nil, err
	}
	return &BatchCandidateReview{ID: id, Version: plan.Digest, Service: planService(plan), Members: plan.Members, Excluded: len(items) - len(plan.Members)}, nil
}

func (e *Engine) PrepareBatchPromotion(ctx context.Context, id, version, receipt string, keys []string) (*ActionResult, error) {
	if !e.AllowDestructive() {
		return nil, fmt.Errorf("replacement is disabled in the server settings")
	}
	ctx, release, err := e.claimExecution(ctx, id, true)
	if err != nil {
		return nil, err
	}
	defer release()
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		return nil, err
	}
	ec := parseExecutionContext(inst, e)
	if receipt == "" {
		return nil, fmt.Errorf("a review receipt is required")
	}
	if stateObject(ec.State, "batch_review_receipts")[receipt] == true {
		return e.Status(ctx, id)
	}
	review, err := e.BatchCandidates(ctx, id)
	if err != nil {
		return nil, err
	}
	if review.Version != version {
		return nil, fmt.Errorf("candidates changed; reopen the review")
	}
	selected := map[string]bool{}
	for _, key := range keys {
		if selected[key] {
			return nil, fmt.Errorf("duplicate candidate selection")
		}
		selected[key] = true
	}
	plan := &batchPromotionPlan{BatchID: id, ReviewID: receipt, Service: review.Service, SeriesID: getInt(ec.Inputs, "series_id"), Members: []batchPromotionMember{}}
	for _, member := range review.Members {
		if selected[member.ItemKey] {
			plan.Members = append(plan.Members, member)
			delete(selected, member.ItemKey)
		}
	}
	if len(selected) > 0 || len(plan.Members) == 0 {
		return nil, fmt.Errorf("select current candidates from this review")
	}
	plan.Digest = batchPromotionDigest(plan)
	delete(ec.State, "batch_promotion")
	delete(ec.Outputs, "batch_promotion")
	delete(ec.State, "batch_promotion_commands")
	ec.State["post_batch_promotion"], ec.State["batch_promotion_plan"], ec.State["batch_promotion_approved"] = true, plan, false
	stateObject(ec.State, "batch_review_receipts")[receipt] = true
	inst.Status, inst.CurrentStep = StatusWaitingDecision, 2
	inst.WaitingReason = fmt.Sprintf("Review replacement of %d selected candidates; originals unchanged", len(plan.Members))
	inst.WaitingCondition, inst.ErrorJSON = "", ""
	inst.WaitingOptionsJSON = toJSON([]WaitingOption{{Decision: "approve", Description: "Replace selected candidates after verification"}, {Decision: "reject", Description: "Keep originals and candidates"}})
	inst.StateJSON = toJSON(ec.State)
	inst.OutputsJSON = toJSON(ec.Outputs)
	owner, _ := ctx.Value(actionLeaseOwnerKey{actionID: id}).(string)
	if err := e.deps.Store.CommitBatchRevision(*inst, owner, nil); err != nil {
		return nil, err
	}
	return e.Status(ctx, id)
}

type reviewedBatchKey struct{}
type reviewedBatch struct{ id, digest, receipt, decision string }

func (e *Engine) ResumeReviewedBatch(ctx context.Context, id, digest, decision, receipt string) (*ActionResult, error) {
	if decision != "approve" && decision != "reject" {
		return nil, fmt.Errorf("unsupported replacement decision")
	}
	return e.Resume(context.WithValue(ctx, reviewedBatchKey{}, reviewedBatch{id, digest, receipt, decision}), id, decision, nil)
}
func (e *Engine) checkReviewedBatch(ctx context.Context, inst *store.ActionInstance) error {
	review, ok := ctx.Value(reviewedBatchKey{}).(reviewedBatch)
	if !ok || inst.ID != review.id {
		return nil
	}
	ec := parseExecutionContext(inst, e)
	plan := getBatchPromotionPlan(ec.State["batch_promotion_plan"])
	if inst.Status != StatusWaitingDecision || inst.CurrentStep != 2 || plan == nil || plan.Digest != review.digest {
		return fmt.Errorf("replacement plan changed; reopen the review")
	}
	return nil
}

func (e *Engine) batchReviewReceiptApplied(ctx context.Context, inst *store.ActionInstance) bool {
	review, ok := ctx.Value(reviewedBatchKey{}).(reviewedBatch)
	if !ok || review.receipt == "" || inst.ID != review.id {
		return false
	}
	ec := parseExecutionContext(inst, e)
	return getString(stateObject(ec.State, "batch_decision_receipts"), review.receipt) == review.digest+":"+review.decision
}
func recordBatchReviewDecision(ctx context.Context, ec *ExecutionContext, plan *batchPromotionPlan) {
	if review, ok := ctx.Value(reviewedBatchKey{}).(reviewedBatch); ok && review.receipt != "" && review.id == ec.InstanceID && review.digest == plan.Digest {
		stateObject(ec.State, "batch_decision_receipts")[review.receipt] = review.digest + ":" + review.decision
	}
}
