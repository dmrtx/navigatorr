package tools

import (
	"encoding/json"

	"github.com/jakenesler/navigatorr/action"
)

// BatchSummary separates workflow completion from the result for the library.
// Counts are small and independent of the (possibly truncated) item list.
type BatchSummary struct {
	PromotionPlanReady bool                             `json:"promotion_plan_ready,omitempty"`
	Title              string                           `json:"title,omitempty"`
	Outcome            string                           `json:"outcome"`
	NextStep           string                           `json:"next_step,omitempty"`
	DryRun             bool                             `json:"dry_run"`
	Total              int                              `json:"total"`
	Completed          int                              `json:"completed"`
	Failed             int                              `json:"failed"`
	Cancelled          int                              `json:"cancelled"`
	Skipped            int                              `json:"skip"`
	Review             int                              `json:"review"`
	Queued             int                              `json:"queued"`
	Running            int                              `json:"running"`
	WaitingForSlot     int                              `json:"waiting_for_slot"`
	WaitingDecision    int                              `json:"waiting_decision"`
	Promotion          *batchPromotionSummary           `json:"promotion,omitempty"`
	Selection          *action.BatchSelection           `json:"selection,omitempty"`
	SampleEvidence     *action.BatchCalibrationEvidence `json:"sample_evidence,omitempty"`
}

type batchPromotionSummary struct {
	Eligible              int    `json:"eligible"`
	Promoted              int    `json:"promoted"`
	Pending               int    `json:"pending"`
	Approved              bool   `json:"approved"`
	LastObservationErrors int    `json:"last_observation_errors"`
	CountsNote            string `json:"counts_note"`
}

func compactBatchPromotion(res *action.ActionResult) *batchPromotionSummary {
	raw, exists := res.Outputs["batch_promotion"]
	if !exists {
		raw = res.State["batch_promotion"]
	}
	p, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	// Project scalar counts before applying the size guard. A long error list
	// must not erase the entire promotion summary or enter compact responses.
	fields := compactOperationalFields(&action.ActionResult{Outputs: p}, []string{"eligible", "promoted", "pending", "approved"})
	encoded, err := json.Marshal(fields)
	var result batchPromotionSummary
	if err != nil || json.Unmarshal(encoded, &result) != nil {
		return nil
	}
	switch failures := p["failed"].(type) {
	case []string:
		result.LastObservationErrors = len(failures)
	case []any:
		result.LastObservationErrors = len(failures)
	}
	result.CountsNote = "Last saved coordinator observation; child actions may have advanced. Error details: action_detail(section=state,key=batch_promotion)."
	return &result
}

func compactBatch(res *action.ActionResult) *BatchSummary {
	if res.ActionName != "transcode_batch" {
		return nil
	}
	// Project only known fields, never serialize raw items or ephemeral recipes.
	fields := compactOperationalFields(res, []string{"series_title", "dry_run", "counts", "batch_promotion_approved", "cancel_requested", "batch_selection", "batch_evidence"})
	var data struct {
		Title           string                           `json:"series_title"`
		DryRun          bool                             `json:"dry_run"`
		Counts          BatchSummary                     `json:"counts"`
		Approved        bool                             `json:"batch_promotion_approved"`
		CancelRequested bool                             `json:"cancel_requested"`
		Selection       *action.BatchSelection           `json:"batch_selection"`
		SampleEvidence  *action.BatchCalibrationEvidence `json:"batch_evidence"`
	}
	encoded, err := json.Marshal(fields)
	if err != nil || json.Unmarshal(encoded, &data) != nil {
		return nil
	}
	result := data.Counts
	result.PromotionPlanReady = res.State["batch_promotion_plan"] != nil || res.Outputs["batch_promotion_plan"] != nil
	result.Title, result.DryRun, result.Promotion = data.Title, data.DryRun, compactBatchPromotion(res)
	result.Selection, result.SampleEvidence = data.Selection, data.SampleEvidence
	if result.Promotion != nil && data.Approved {
		result.Promotion.Approved = true
	}
	// Older versions stored user cancellation as completed + failed items.
	// Surface the durable intent without rewriting historic audit records.
	switch {
	case res.Status == action.StatusFailed:
		result.Outcome = "failed"
	case res.Status == action.StatusCancelled || res.Status == action.StatusCompleted && data.CancelRequested:
		result.Outcome = "cancelled"
	case data.CancelRequested:
		result.Outcome = "cancelling"
	case res.Status == action.StatusWaitingDecision:
		result.Outcome = "needs_decision"
	case res.Status != action.StatusCompleted:
		result.Outcome = "in_progress"
	case data.DryRun:
		result.Outcome = "preview"
	case result.Failed > 0 && result.Completed == 0:
		result.Outcome = "failed"
	case result.Failed > 0 || result.Cancelled > 0:
		result.Outcome = "partial"
	case result.Review > 0:
		result.Outcome = "needs_review"
	case result.Completed == 0:
		result.Outcome = "no_changes"
	case result.Promotion != nil && result.Promotion.Promoted == result.Promotion.Eligible && result.Promotion.Promoted > 0:
		result.Outcome = "promoted"
	default:
		result.Outcome = "candidates_ready"
	}
	switch result.Outcome {
	case "preview":
		result.NextStep = "Preview only. To encode, start a new action_run with the same inputs and dry_run=false; use a new top-level idempotency_key, not action_resume."
	case "in_progress", "cancelling":
		result.NextStep = "Monitor this action with action_status; external waits advance automatically. Do not start a duplicate batch."
	case "needs_decision":
		result.NextStep = "Use this action's waiting_options with action_resume. An approve decision authorizes the displayed replacement."
	case "candidates_ready":
		result.NextStep = "Candidates are ready; originals are unchanged. Inspect action_detail(section=outputs,key=items) for candidate paths and child action IDs; do not encode again to promote them."
	case "partial", "needs_review", "failed":
		result.NextStep = "Inspect the error and action_detail(section=outputs,key=items) for affected files before retrying; completed candidates may already exist."
	case "no_changes":
		result.NextStep = "Originals are unchanged. Inspect selection and action_detail(section=outputs,key=items) for skip reasons; no automatic search retry is scheduled."
	}
	return &result
}
