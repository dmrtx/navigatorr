package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/jakenesler/navigatorr/store"
)

type batchPromotionMember struct {
	ItemKey       string `json:"item_key"`
	Episode       string `json:"episode"`
	SourceAction  string `json:"transcode_action_id"`
	OriginalPath  string `json:"original_path"`
	CandidatePath string `json:"candidate_path"`
	CandidateSHA  string `json:"candidate_sha256"`
}

type batchPromotionPlan struct {
	BatchID  string                 `json:"batch_id"`
	SeriesID int                    `json:"series_id"`
	Members  []batchPromotionMember `json:"members"`
	Digest   string                 `json:"digest"`
}

func getBatchPromotionPlan(raw any) *batchPromotionPlan {
	if p, ok := raw.(*batchPromotionPlan); ok {
		return p
	}
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var p batchPromotionPlan
	if json.Unmarshal(b, &p) != nil || p.Digest == "" || p.BatchID == "" || p.SeriesID <= 0 {
		return nil
	}
	return &p
}

func (e *Engine) validateBatchPromotionInputs(inputs map[string]any) error {
	if raw, present := inputs["promote_candidates"]; present {
		if _, ok := raw.(bool); !ok {
			return fmt.Errorf("promote_candidates must be a boolean")
		}
	}
	if raw, present := inputs["promotion_parallelism"]; present {
		v, ok := strictBatchInteger(raw)
		if !ok || v < 1 || v > 2 {
			return fmt.Errorf("promotion_parallelism must be 1 or 2")
		}
	}
	if getBool(inputs, "promote_candidates") && !getBool(inputs, "dry_run") {
		if !e.AllowDestructive() {
			return fmt.Errorf("allow_destructive must be enabled for batch promotion")
		}
		v, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(inputs["series_id"])))
		if err != nil || v <= 0 {
			return fmt.Errorf("batch promotion requires a numeric Sonarr series_id")
		}
	}
	return nil
}

func (e *Engine) buildBatchPromotionPlan(ec *ExecutionContext, items []store.TranscodeBatchItem) (*batchPromotionPlan, error) {
	seriesID, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(ec.Inputs["series_id"])))
	if err != nil || seriesID <= 0 {
		return nil, fmt.Errorf("batch promotion requires a numeric Sonarr series_id")
	}
	p := &batchPromotionPlan{BatchID: ec.InstanceID, SeriesID: seriesID, Members: []batchPromotionMember{}}
	for _, item := range items {
		if item.Status != "completed" || item.Decision != "transcode" {
			continue
		}
		if item.ChildActionID == "" || item.CandidatePath == "" {
			return nil, fmt.Errorf("completed item %s has no candidate action or path", item.ItemKey)
		}
		child, err := e.deps.Store.GetActionInstance(item.ChildActionID)
		if err != nil || child == nil || child.ActionName != "transcode_media" || child.Status != StatusCompleted {
			return nil, fmt.Errorf("completed item %s has no completed transcode action", item.ItemKey)
		}
		var state, out map[string]any
		if json.Unmarshal([]byte(child.StateJSON), &state) != nil || json.Unmarshal([]byte(child.OutputsJSON), &out) != nil {
			return nil, fmt.Errorf("completed item %s has unreadable transcode evidence", item.ItemKey)
		}
		if getString(state, "resolved_path") != item.FilePath || getString(out, "candidate_path") != item.CandidatePath || getString(state, "candidate_sha256") == "" {
			return nil, fmt.Errorf("completed item %s no longer matches its candidate evidence", item.ItemKey)
		}
		p.Members = append(p.Members, batchPromotionMember{ItemKey: item.ItemKey, Episode: item.EpisodeInfo, SourceAction: child.ID, OriginalPath: item.FilePath, CandidatePath: item.CandidatePath, CandidateSHA: getString(state, "candidate_sha256")})
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	p.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return p, nil
}

func (e *Engine) stepTranscodeBatchPromote(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if err := e.validateBatchPromotionInputs(ec.Inputs); err != nil {
		return StepResult{Status: StepFailed, Error: err.Error()}, nil
	}
	if !getBool(ec.Inputs, "promote_candidates") || getBool(ec.Inputs, "dry_run") {
		return StepResult{Status: StepCompleted}, nil
	}
	parallelism := 2
	if raw, ok := ec.Inputs["promotion_parallelism"]; ok {
		v, _ := strictBatchInteger(raw)
		parallelism = v
	}
	plan := getBatchPromotionPlan(ec.State["batch_promotion_plan"])
	if plan == nil {
		items, err := e.deps.Store.ListTranscodeBatchItems(ec.InstanceID)
		if err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("listing batch candidates: %v", err)}, nil
		}
		plan, err = e.buildBatchPromotionPlan(ec, items)
		if err != nil {
			return StepResult{Status: StepFailed, Error: err.Error()}, nil
		}
		ec.State["batch_promotion_plan"] = plan
		if err := e.persistExecutionState(ctx, ec); err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("persisting batch promotion plan: %v", err)}, nil
		}
	}
	if plan.BatchID != ec.InstanceID || plan.SeriesID <= 0 {
		return StepResult{Status: StepFailed, Error: "batch promotion plan does not match parent"}, nil
	}
	if len(plan.Members) == 0 {
		return StepResult{Status: StepCompleted, Outputs: map[string]any{"batch_promotion": map[string]any{"eligible": 0, "promoted": 0}}}, nil
	}
	if !getBool(ec.State, "batch_promotion_approved") {
		switch strings.ToLower(ec.Decision) {
		case "approve":
			ec.State["batch_promotion_approved"] = true
			if err := e.persistExecutionState(ctx, ec); err != nil {
				return StepResult{Status: StepFailed, Error: fmt.Sprintf("persisting batch approval: %v", err)}, nil
			}
		case "reject", "cancel":
			return StepResult{Status: StepCompleted, Outputs: map[string]any{"batch_promotion": map[string]any{"eligible": len(plan.Members), "promoted": 0, "approved": false}}}, nil
		default:
			return StepResult{Status: StepWaitingDecision, WaitingReason: fmt.Sprintf("Approve promotion of %d verified candidates in one batch", len(plan.Members)), WaitingOptions: []WaitingOption{{Decision: "approve", Description: "Promote all listed candidates with recovery copies and per-file verification"}, {Decision: "reject", Description: "Keep all originals and candidates unchanged"}}, Outputs: map[string]any{"batch_promotion_plan": plan}}, nil
		}
	}
	type promotionOutcome struct {
		Member batchPromotionMember
		Result *ActionResult
		Err    error
	}
	outcomes := make([]promotionOutcome, len(plan.Members))
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for i, member := range plan.Members {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, member batchPromotionMember) {
			defer wg.Done()
			defer func() { <-sem }()
			key := "promote:sonarr:" + member.SourceAction
			prior, err := e.deps.Store.FindActionByIdempotencyKey("promote_transcode_candidate", key)
			if err != nil {
				outcomes[i] = promotionOutcome{member, nil, err}
				return
			}
			inputs := map[string]any{"transcode_action_id": member.SourceAction, "series_id": plan.SeriesID, "service": "sonarr", "batch_promote_parent_id": ec.InstanceID, "batch_promote_item_key": member.ItemKey, "batch_promote_digest": plan.Digest}
			var result *ActionResult
			if prior == nil {
				result, err = e.Run(ctx, "promote_transcode_candidate", inputs)
			} else {
				result, err = e.existingPromotion(ctx, prior, mustPromotionTemplate(e), inputs)
				if err == nil && result != nil {
					switch result.Status {
					case StatusWaitingDecision:
						result, err = e.Resume(ctx, result.ID, "approve", nil)
					case StatusWaitingExternal, StatusRunning, StatusPending:
						result, err = e.Resume(ctx, result.ID, "", nil)
					}
				}
			}
			outcomes[i] = promotionOutcome{member, result, err}
		}(i, member)
	}
	wg.Wait()
	completed := 0
	pending := 0
	failed := []string{}
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			failed = append(failed, outcome.Member.ItemKey+": "+outcome.Err.Error())
			continue
		}
		if outcome.Result == nil {
			failed = append(failed, outcome.Member.ItemKey+": promotion returned no action")
			continue
		}
		switch outcome.Result.Status {
		case StatusCompleted:
			completed++
		case StatusFailed, StatusCancelled:
			failed = append(failed, outcome.Member.ItemKey+": "+outcome.Result.Error)
		default:
			pending++
		}
	}
	progress := map[string]any{"eligible": len(plan.Members), "promoted": completed, "pending": pending, "failed": failed, "plan_digest": plan.Digest}
	if len(failed) > 0 {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("%d batch promotion(s) need attention: %s", len(failed), strings.Join(failed, "; ")), Outputs: map[string]any{"batch_promotion": progress}}, nil
	}
	if pending > 0 {
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "batch_promotion", WaitingReason: fmt.Sprintf("Promoting %d verified candidates; %d completed", len(plan.Members), completed), Outputs: map[string]any{"batch_promotion": progress}}, nil
	}
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"batch_promotion": progress}}, nil
}

func mustPromotionTemplate(e *Engine) ActionTemplate {
	t, _ := e.GetTemplate("promote_transcode_candidate")
	return t
}

func (e *Engine) verifyBatchPromotionApproval(ec *ExecutionContext, p *promotionState) error {
	parentID := strings.TrimSpace(getString(ec.Inputs, "batch_promote_parent_id"))
	itemKey := strings.TrimSpace(getString(ec.Inputs, "batch_promote_item_key"))
	digest := strings.TrimSpace(getString(ec.Inputs, "batch_promote_digest"))
	if parentID == "" || itemKey == "" || digest == "" {
		return fmt.Errorf("batch promotion approval link is incomplete")
	}
	parent, err := e.deps.Store.GetActionInstance(parentID)
	if err != nil || parent == nil || parent.ActionName != "transcode_batch" || parent.Status == StatusFailed || parent.Status == StatusCancelled {
		return fmt.Errorf("approved parent batch is unavailable")
	}
	var state, inputs map[string]any
	if json.Unmarshal([]byte(parent.StateJSON), &state) != nil || json.Unmarshal([]byte(parent.InputsJSON), &inputs) != nil || !getBool(inputs, "promote_candidates") || !getBool(state, "batch_promotion_approved") {
		return fmt.Errorf("parent batch has not approved candidate promotion")
	}
	plan := getBatchPromotionPlan(state["batch_promotion_plan"])
	if plan == nil || plan.BatchID != parentID || plan.Digest != digest || plan.SeriesID != p.SeriesID {
		return fmt.Errorf("parent batch promotion plan does not match candidate")
	}
	item, err := e.deps.Store.GetTranscodeBatchItem(parentID, itemKey)
	if err != nil || item == nil || item.Status != "completed" {
		return fmt.Errorf("candidate is not a completed member of the batch")
	}
	for _, member := range plan.Members {
		if member.ItemKey == itemKey && member.SourceAction == p.SourceActionID && member.OriginalPath == p.OriginalPath && member.CandidatePath == p.CandidatePath && member.CandidateSHA == p.CandidateSHA && item.ChildActionID == member.SourceAction && item.CandidatePath == member.CandidatePath {
			return nil
		}
	}
	return fmt.Errorf("candidate is not in the approved batch promotion plan")
}
