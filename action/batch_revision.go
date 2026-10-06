package action

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

func stateObject(state map[string]any, key string) map[string]any {
	value, _ := state[key].(map[string]any)
	if value == nil {
		value = map[string]any{}
		state[key] = value
	}
	return value
}

// Selection identity excludes changing worker telemetry. It changes only when
// a file relation or settings revision changes, so polling cannot stale a form.
func BatchSelectionVersion(inst *store.ActionInstance, items []store.TranscodeBatchItem) string {
	state := map[string]any{}
	_ = json.Unmarshal([]byte(inst.StateJSON), &state)
	var identity []string
	for _, it := range items {
		identity = append(identity, it.ItemKey+"\x00"+it.FilePath+"\x00"+it.ChildActionID)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(toJSON(map[string]any{"items": identity, "revision": state["batch_settings_revision"]}))))
}

func BatchRevisionSelects(item store.TranscodeBatchItem, scope, candidateID string) bool {
	if scope == "candidate" {
		return item.ChildActionID == candidateID && candidateID != ""
	}
	if scope == "all" {
		return true
	}
	return scope == "unfinished" && item.Status != "completed" && item.Status != "skip"
}

func batchChildKey(ec *ExecutionContext, item store.TranscodeBatchItem) string {
	key := fmt.Sprintf("batch-%s-%s", ec.InstanceID, item.ItemKey)
	if generation := getInt(stateObject(ec.State, "batch_item_generations"), item.ItemKey); generation > 0 {
		key += fmt.Sprintf("-attempt-%d", generation)
	}
	return key
}

func recordBatchAttempt(ec *ExecutionContext, item *store.TranscodeBatchItem) {
	history := stateObject(ec.State, "batch_attempt_history")
	var rows []store.TranscodeBatchItem
	_ = json.Unmarshal([]byte(toJSON(history[item.ItemKey])), &rows)
	if item.ChildActionID != "" {
		rows = append(rows, *item)
	}
	history[item.ItemKey] = rows
	generations := stateObject(ec.State, "batch_item_generations")
	generations[item.ItemKey] = getInt(generations, item.ItemKey) + 1
	item.Status, item.Decision = "queued", "transcode"
	item.ChildActionID, item.JobID, item.CandidatePath, item.Error = "", "", "", ""
	item.Attempts = 0
	item.Reasons = []string{"New settings requested; previous attempt retained in history"}
}

// Change scheduling intent in the same coordinator. Original InputsJSON and
// old child inputs remain audit records. Active files finish their existing
// attempt before a replacement attempt is admitted, never concurrently.
func (e *Engine) ReconfigureBatch(ctx context.Context, id, scope, candidateID, version, decisionVersion, receipt string, settings map[string]any) (*ActionResult, error) {
	ctx, release, err := e.claimExecution(ctx, id, true)
	if err != nil {
		return nil, err
	}
	defer release()
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		return nil, err
	}
	if inst.ActionName != "transcode_batch" {
		return nil, fmt.Errorf("only video batches can be reconfigured")
	}
	ec := parseExecutionContext(inst, e)
	if ec.State == nil {
		ec.State = map[string]any{}
	}
	if receipt == "" {
		return nil, fmt.Errorf("a settings receipt is required")
	}
	if stateObject(ec.State, "batch_revision_receipts")[receipt] == true {
		return e.Status(ctx, id)
	}
	if getBool(ec.Inputs, "dry_run") || scope != "all" && scope != "unfinished" && scope != "candidate" {
		return nil, fmt.Errorf("invalid batch scope")
	}
	if ec.State["batch_promotion_plan"] != nil {
		return nil, fmt.Errorf("replacement review has started; resolve it before changing encoding settings")
	}
	if len(stateObject(ec.State, "batch_retry_pending")) > 0 {
		return nil, fmt.Errorf("a settings change is already waiting for active files to finish")
	}
	items, err := e.deps.Store.ListTranscodeBatchItems(id)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 || BatchSelectionVersion(inst, items) != version {
		return nil, fmt.Errorf("batch selection changed; reopen the settings")
	}
	if scope == "candidate" {
		child, err := e.deps.Store.GetActionInstance(candidateID)
		if err != nil {
			return nil, err
		}
		if child.Status == StatusWaitingDecision && CandidateDecisionVersion(child) != decisionVersion {
			return nil, fmt.Errorf("candidate validation changed; review it again")
		}
		if child.Status != StatusWaitingDecision && (child.Status != StatusFailed || !strings.Contains(child.ErrorJSON, "rejected")) {
			return nil, fmt.Errorf("candidate no longer awaits your decision")
		}
	}
	// No selected candidate may already be reserved for a library replacement.
	selected := 0
	for _, item := range items {
		if !BatchRevisionSelects(item, scope, candidateID) {
			continue
		}
		selected++
		for _, service := range []string{"sonarr", "radarr"} {
			if old, err := e.deps.Store.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:"+service+":"+item.ChildActionID); err != nil {
				return nil, err
			} else if old != nil {
				return nil, fmt.Errorf("a selected file has a replacement job; keep its recovery history separate")
			}
		}
	}
	if selected == 0 {
		return nil, fmt.Errorf("no files selected")
	}
	var changed []store.TranscodeBatchItem
	for i := range items {
		item := &items[i]
		if !BatchRevisionSelects(*item, scope, candidateID) {
			continue
		}
		merged := map[string]any{}
		if old, ok := stateObject(ec.State, "batch_item_settings")[item.ItemKey].(map[string]any); ok {
			for key, value := range old {
				merged[key] = value
			}
		}
		for key, value := range settings {
			merged[key] = value
		}
		stateObject(ec.State, "batch_item_settings")[item.ItemKey] = merged
		if item.ChildActionID == "" {
			if old, err := e.deps.Store.FindActionByIdempotencyKey("transcode_media", batchChildKey(ec, *item)); err != nil {
				return nil, err
			} else if old != nil {
				item.ChildActionID = old.ID
			}
		}
		var child *store.ActionInstance
		if item.ChildActionID != "" {
			child, err = e.deps.Store.GetActionInstance(item.ChildActionID)
			if err != nil {
				return nil, err
			}
		}
		if child != nil && (child.Status == StatusRunning || child.Status == StatusPending || child.Status == StatusWaitingExternal) {
			stateObject(ec.State, "batch_retry_pending")[item.ItemKey] = true
			item.Status = "running"
			changed = append(changed, *item)
			continue
		}
		if child != nil && child.Status == StatusWaitingDecision {
			if _, err := e.ResumeReviewedCandidate(ctx, child.ID, child.ID, CandidateDecisionVersion(child), "reject"); err != nil {
				return nil, err
			}
		}
		if child != nil {
			child, _ = e.deps.Store.GetActionInstance(child.ID)
			if child != nil {
				item.Status = child.Status
				item.Error = child.ErrorJSON
			}
		}
		recordBatchAttempt(ec, item)
		changed = append(changed, *item)
	}
	ec.State["batch_settings_revision"] = getInt(ec.State, "batch_settings_revision") + 1
	stateObject(ec.State, "batch_revision_receipts")[receipt] = true
	ec.State["batch_revision_receipt"], ec.State["batch_revision_at"] = receipt, time.Now().UTC().Format(time.RFC3339Nano)
	ec.State["paused"], ec.State["cancel_requested"] = false, false
	ec.State["batch_promotion_approved"] = false
	delete(ec.State, "next_poll_at")
	inst.Status, inst.CurrentStep = StatusRunning, 1
	inst.WaitingReason, inst.WaitingCondition, inst.WaitingOptionsJSON, inst.ErrorJSON = "", "", "[]", ""
	inst.StateJSON = toJSON(ec.State)
	inst.OutputsJSON = toJSON(buildBatchOutputs(id, items, getString(ec.State, "series_title"), getBool(ec.State, "is_anime"), false))
	owner, _ := ctx.Value(actionLeaseOwnerKey{actionID: id}).(string)
	if err := e.deps.Store.CommitBatchRevision(*inst, owner, changed); err != nil {
		return nil, err
	}
	e.WakeReconciler()
	return e.Status(ctx, id)
}

func (e *Engine) applyPendingBatchAttempts(ctx context.Context, ec *ExecutionContext, items []store.TranscodeBatchItem) error {
	pending := stateObject(ec.State, "batch_retry_pending")
	var changed []store.TranscodeBatchItem
	for i := range items {
		item := &items[i]
		if pending[item.ItemKey] != true {
			continue
		}
		child, err := e.deps.Store.GetActionInstance(item.ChildActionID)
		if err != nil {
			return err
		}
		if child.Status == StatusRunning || child.Status == StatusPending || child.Status == StatusWaitingExternal {
			continue
		}
		if child.Status == StatusWaitingDecision {
			if _, err := e.ResumeReviewedCandidate(ctx, child.ID, child.ID, CandidateDecisionVersion(child), "reject"); err != nil {
				return err
			}
		}
		if child != nil {
			child, _ = e.deps.Store.GetActionInstance(child.ID)
			if child != nil {
				item.Status = child.Status
				item.Error = child.ErrorJSON
			}
		}
		recordBatchAttempt(ec, item)
		delete(pending, item.ItemKey)
		changed = append(changed, *item)
	}
	if len(changed) == 0 {
		return nil
	}
	inst, err := e.deps.Store.GetActionInstance(ec.InstanceID)
	if err != nil {
		return err
	}
	inst.StateJSON = toJSON(ec.State)
	owner, _ := ctx.Value(actionLeaseOwnerKey{actionID: ec.InstanceID}).(string)
	return e.deps.Store.CommitBatchRevision(*inst, owner, changed)
}

// Existing immutable child plans need no new calibration. An explicitly
// selected profile must not trigger the original batch's automatic search.
func batchNeedsCalibration(ec *ExecutionContext, items []store.TranscodeBatchItem) bool {
	if !batchCalibrationEnabled(ec.Inputs, ec.State) {
		return false
	}
	for _, item := range items {
		if item.ChildActionID != "" || item.Status != "queued" && item.Status != "waiting_for_slot" {
			continue
		}
		settings, _ := stateObject(ec.State, "batch_item_settings")[item.ItemKey].(map[string]any)
		if getString(settings, "profile") == "" {
			return true
		}
	}
	return false
}
