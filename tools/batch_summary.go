package tools

import (
	"encoding/json"

	"github.com/jakenesler/navigatorr/action"
)

// BatchSummary separates workflow completion from the result for the library.
// Counts are small and independent of the (possibly truncated) item list.
type BatchSummary struct {
	Title           string                 `json:"title,omitempty"`
	Outcome         string                 `json:"outcome"`
	DryRun          bool                   `json:"dry_run"`
	Total           int                    `json:"total"`
	Completed       int                    `json:"completed"`
	Failed          int                    `json:"failed"`
	Cancelled       int                    `json:"cancelled"`
	Skipped         int                    `json:"skip"`
	Review          int                    `json:"review"`
	Queued          int                    `json:"queued"`
	Running         int                    `json:"running"`
	WaitingForSlot  int                    `json:"waiting_for_slot"`
	WaitingDecision int                    `json:"waiting_decision"`
	Promotion       *batchPromotionSummary `json:"promotion,omitempty"`
}

type batchPromotionSummary struct {
	Eligible int  `json:"eligible"`
	Promoted int  `json:"promoted"`
	Pending  int  `json:"pending"`
	Approved bool `json:"approved"`
}

func compactBatch(res *action.ActionResult) *BatchSummary {
	if res.ActionName != "transcode_batch" {
		return nil
	}
	// Project only known fields, never serialize raw items or ephemeral recipes.
	fields := compactOperationalFields(res, []string{"series_title", "dry_run", "counts", "batch_promotion", "batch_promotion_approved", "cancel_requested"})
	var data struct {
		Title           string                 `json:"series_title"`
		DryRun          bool                   `json:"dry_run"`
		Counts          BatchSummary           `json:"counts"`
		Promotion       *batchPromotionSummary `json:"batch_promotion"`
		Approved        bool                   `json:"batch_promotion_approved"`
		CancelRequested bool                   `json:"cancel_requested"`
	}
	encoded, err := json.Marshal(fields)
	if err != nil || json.Unmarshal(encoded, &data) != nil {
		return nil
	}
	result := data.Counts
	result.Title, result.DryRun, result.Promotion = data.Title, data.DryRun, data.Promotion
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
	return &result
}
