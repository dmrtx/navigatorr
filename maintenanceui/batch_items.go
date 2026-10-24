package maintenanceui

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

type batchItemView struct {
	WaitingReason string `json:"waiting_reason,omitempty"`
	// WaitingDecisions are the decisions the file's attempt offers while it
	// waits on one. recheck means the encode finished and only saving it
	// failed: the file is tried again as it is, never re-encoded.
	WaitingDecisions []string `json:"waiting_decisions,omitempty"`
	ReasonCode       string   `json:"reason_code,omitempty"`
	CandidateReady   bool     `json:"candidate_ready,omitempty"`
	// SavedBytes is how much smaller the finished candidate is than the original.
	SavedBytes int64 `json:"saved_bytes,omitempty"`
	// SizeBudgetOutcome says a candidate came from the operation's size budget
	// (for example below_quality_target_within_budget).
	SizeBudgetOutcome string `json:"size_budget_outcome,omitempty"`
	// TracksMessage says which audio and subtitle tracks the file kept, when it
	// dropped some.
	TracksMessage       string `json:"tracks_message,omitempty"`
	ReplacementActionID string `json:"replacement_action_id,omitempty"`
	ReplacementStatus   string `json:"replacement_status,omitempty"`
	Replaced            bool   `json:"replaced,omitempty"`
	// ReplacementPlan says the file is part of the batch's prepared
	// replacement plan and has no replacement job yet: "review" while the plan
	// waits for approval, "queued" once approved. Such a file offers no
	// replacement of its own.
	ReplacementPlan string `json:"replacement_plan,omitempty"`
	// ReplacementFailure says why a failed replacement stopped and whether the
	// original is intact; Where says where the new file and any backup are.
	ReplacementFailure *replacementFailure `json:"replacement_failure,omitempty"`
	Where              *replacementWhere   `json:"where,omitempty"`
	// RetryActionID is the action whose Retry can re-run this file's
	// replacement without encoding again: the batch when the file failed in
	// plan_promotion, the file's own replacement once it failed after approval.
	// Empty when no retry applies (including a failure in approve_promotion).
	RetryActionID  string `json:"retry_action_id,omitempty"`
	CanReconfigure bool   `json:"can_reconfigure,omitempty"`
	Mode           string `json:"mode,omitempty"`
	// MaxHeight is the frozen compact output height, so the goal label
	// names the size actually produced.
	MaxHeight int `json:"max_height,omitempty"`

	PreviousAttempts []store.TranscodeBatchItem `json:"previous_attempts,omitempty"`
	RetryPending     bool                       `json:"retry_pending,omitempty"`
	ItemKey          string                     `json:"item_key"`
	FilePath         string                     `json:"file_path"`
	DisplayLabel     string                     `json:"display_label"`
	Status           string                     `json:"status"`
	ChildActionID    string                     `json:"child_action_id,omitempty"`
	Decision         string                     `json:"decision"`
	Error            string                     `json:"error,omitempty"`
	Profile          string                     `json:"profile,omitempty"`
	RequestedProfile string                     `json:"requested_profile,omitempty"`
	Reasons          []string                   `json:"reasons,omitempty"`
}

// Batch items come from their durable relation, rather than the intentionally
// truncated MCP output snapshot. Known batch ownership prevents arbitrary
// workflow IDs from being used to inspect unrelated maintenance records.
func (s *Server) batchItems(w http.ResponseWriter, req *http.Request) {
	offset, limit, err := operationPaging(req)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	id := strings.TrimSpace(req.URL.Query().Get("id"))
	if id == "" {
		fail(w, 400, "batch id is required")
		return
	}
	st := s.engine.Deps().Store
	if st == nil {
		fail(w, 503, "maintenance store is required")
		return
	}
	inst, err := st.GetActionInstanceIfExists(id)
	if err != nil {
		fail(w, 500, "read batch")
		return
	}
	if inst == nil || inst.ActionName != "transcode_batch" {
		fail(w, 404, "batch not found")
		return
	}
	items, err := st.ListTranscodeBatchItems(id)
	if err == nil {
		items, _, err = s.previewPendingItems(*inst, items)
	}
	if err != nil {
		fail(w, 500, "read batch items")
		return
	}
	inputs := decodeOperationJSON(inst.InputsJSON)
	state := decodeOperationJSON(inst.StateJSON)
	settings := operationMap(state["batch_item_settings"])
	history, pending := operationMap(state["batch_attempt_history"]), operationMap(state["batch_retry_pending"])
	views := []batchItemView{}
	end := min(offset, len(items)) + min(limit, max(0, len(items)-offset))
	if offset < len(items) {
		for _, item := range items[offset:end] {
			var previous []store.TranscodeBatchItem
			data, _ := json.Marshal(history[item.ItemKey])
			_ = json.Unmarshal(data, &previous)
			view := batchItemView{RequestedProfile: operationString(operationMap(settings[item.ItemKey])["profile"]), PreviousAttempts: previous, RetryPending: pending[item.ItemKey] == true, ItemKey: item.ItemKey, FilePath: item.FilePath, DisplayLabel: item.DisplayLabel, Status: item.Status, ChildActionID: item.ChildActionID, Decision: item.Decision, Error: batchReasonText(item.Error, true), Profile: item.Profile, Reasons: item.Reasons}
			view.Mode = operationString(inputs["mode"])
			policy := state["mode_policy"]
			if mode := operationString(operationMap(settings[item.ItemKey])["mode"]); mode != "" {
				view.Mode, policy = mode, operationMap(settings[item.ItemKey])["mode_policy"]
			}
			if view.Mode == "compact" {
				view.MaxHeight = compactMaxHeight(policy)
			}
			reserved, err := s.engine.BatchItemReplacementReserved(item)
			if err != nil {
				fail(w, 500, "read file replacement")
				return
			}
			view.CanReconfigure = inputs["dry_run"] != true && state["batch_promotion_plan"] == nil && !reserved && !view.RetryPending
			if item.ChildActionID != "" {
				child, err := st.GetActionInstanceIfExists(item.ChildActionID)
				if err != nil {
					fail(w, 500, "read file attempt")
					return
				}
				if child != nil && child.ActionName == "transcode_media" {
					childInputs := decodeOperationJSON(child.InputsJSON)
					if childInputs["parent_action_id"] == id && childInputs["path"] == item.FilePath {
						r := operationRecord{inst: *child, inputs: childInputs, outputs: decodeOperationJSON(child.OutputsJSON), state: decodeOperationJSON(child.StateJSON)}
						view.WaitingReason = batchReasonText(child.WaitingReason, true)
						if child.Status == store.ActionStatusWaitingDecision {
							var options []struct {
								Decision string `json:"decision"`
							}
							_ = json.Unmarshal([]byte(child.WaitingOptionsJSON), &options)
							for _, option := range options {
								view.WaitingDecisions = append(view.WaitingDecisions, option.Decision)
							}
							// Changing the goal retires the attempt and encodes again;
							// a finished encode that only failed to save is retried.
							if slices.Contains(view.WaitingDecisions, "recheck") {
								view.CanReconfigure = false
							}
						}
						view.ReasonCode = operationString(operationValue(r, "reason_code"))
						if !view.RetryPending {
							if child.Status == store.ActionStatusCompleted && (operationValue(r, "skip_transcode") == true || operationValue(r, "skipped") == true) {
								view.Status = "skip"
							} else if child.Status == store.ActionStatusCompleted || child.Status == store.ActionStatusFailed || child.Status == store.ActionStatusCancelled || child.Status == store.ActionStatusWaitingDecision {
								view.Status = child.Status
							}
						}
						view.CandidateReady = !view.RetryPending && child.Status == store.ActionStatusCompleted && operationValue(r, "skip_transcode") != true && operationValue(r, "skipped") != true && operationValue(r, "original_intact") == true && operationValue(r, "candidate_removed") != true && projectOperationSavings(r, time.Now()).CandidateBytes != nil && !reserved
						view.SizeBudgetOutcome = operationSizeBudgetOutcome(r)
						view.TracksMessage = operationString(operationTracks(r)["message"])
						for _, service := range []string{"sonarr", "radarr", "filesystem"} {
							prior, err := st.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:"+service+":"+child.ID)
							if err != nil {
								fail(w, 500, "read file replacement")
								return
							}
							if prior != nil {
								view.ReplacementActionID, view.ReplacementStatus = prior.ID, prior.Status
								pr := operationRecord{inst: *prior, inputs: decodeOperationJSON(prior.InputsJSON), outputs: decodeOperationJSON(prior.OutputsJSON), state: decodeOperationJSON(prior.StateJSON)}
								if prior.Status == store.ActionStatusFailed || prior.Status == store.ActionStatusCancelled {
									view.ReplacementFailure = s.failureOf(pr)
									view.RetryActionID = s.replacementRetryTarget(*inst, *prior)
								}
								view.Where = whereOf(pr)
								view.Replaced = prior.Status == store.ActionStatusCompleted && decodeOperationJSON(prior.OutputsJSON)["promoted"] == true
								break
							}
						}
						// The saving of a candidate that was cleaned up is shown only once it replaced the original.
						if saved := projectOperationSavings(r, time.Now()).CandidateSavedBytes; saved != nil && child.Status == store.ActionStatusCompleted && !view.RetryPending && operationValue(r, "skip_transcode") != true && operationValue(r, "skipped") != true && (operationValue(r, "candidate_removed") != true || view.Replaced) {
							view.SavedBytes = *saved
						}
					}
				}
			}
			if view.ReplacementActionID == "" && inst.Status == store.ActionStatusFailed {
				// A file whose promotion never produced an action of its own is
				// reported only in the batch's promotion output. Retrying the batch
				// would reproduce that error, so no retry is offered.
				if detail := batchPromotionFailure(decodeOperationJSON(inst.OutputsJSON), item.ItemKey); detail != "" {
					view.ReplacementFailure = &replacementFailure{Reason: plainReplacementReason(detail), Detail: batchReasonText(detail, true), OriginalState: originalInPlace, OriginalIntact: true}
				}
			}
			if view.ReplacementActionID == "" && view.ReplacementFailure == nil && item.ChildActionID != "" {
				if view.ReplacementPlan = replacementPlanState(state, item.ChildActionID); view.ReplacementPlan != "" {
					view.CandidateReady = false
				}
			}
			views = append(views, view)
		}
	}
	writeJSON(w, 200, map[string]any{"items": views, "total": len(items), "offset": offset, "has_more": end < len(items)})
}

// compactMaxHeight reads a frozen compact policy's output height; the
// default applies when none was frozen yet.
func compactMaxHeight(policy any) int {
	if h := int(operationNumber(operationMap(operationMap(policy)["compact"])["max_height"])); h > 0 {
		return h
	}
	return transcode.CompactDefaultMaxHeight
}
