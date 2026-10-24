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

func BatchRevisionSelects(item store.TranscodeBatchItem, scope, selectionID string) bool {
	if scope == "file" {
		return item.ItemKey == selectionID && selectionID != ""
	}
	if scope == "candidate" {
		return item.ChildActionID == selectionID && selectionID != ""
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
	item.Reasons = []string{"New settings requested"}
}

// A decision before any candidate exists has no review to reject. Retire its
// attempt with cancellation; validated candidates retain their versioned review.
func (e *Engine) retireBatchDecision(ctx context.Context, child *store.ActionInstance) error {
	var options []WaitingOption
	if err := json.Unmarshal([]byte(child.WaitingOptionsJSON), &options); err != nil {
		return err
	}
	canCancel := false
	for _, option := range options {
		if option.Decision == "recheck" {
			return fmt.Errorf("this file's encode finished and only saving it failed; use Try again to save it as it is, because changing the goal would encode it again")
		}
		if option.Decision == "reject" {
			_, err := e.ResumeReviewedCandidate(ctx, child.ID, child.ID, CandidateDecisionVersion(child), "reject")
			return err
		}
		canCancel = canCancel || option.Decision == "cancel"
	}
	if canCancel {
		_, err := e.Cancel(ctx, child.ID, "Encoding settings changed; original preserved")
		return err
	}
	return fmt.Errorf("resolve the previous attempt's decision before changing encoding settings")
}

// Change scheduling intent in the same coordinator. Original InputsJSON and
// old child inputs remain audit records. Active files finish their existing
// attempt before a replacement attempt is admitted, never concurrently.
func (e *Engine) ReconfigureBatch(ctx context.Context, id, scope, selectionID, version, decisionVersion, receipt string, settings map[string]any) (*ActionResult, error) {
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
	if getBool(ec.Inputs, "dry_run") || scope != "all" && scope != "unfinished" && scope != "candidate" && scope != "file" {
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
		child, err := e.deps.Store.GetActionInstance(selectionID)
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

	if mode := getString(settings, "mode"); mode != "" {
		allowed := 1
		for _, key := range []string{"max_height", "allow_hardware", "hardware_margin_percent", "allow_slow_preset", "size_budget_percent", "size_budget_bytes", "size_budget_source", "size_budget_sibling_multiplier"} {
			if _, ok := settings[key]; ok {
				allowed++
			}
		}
		if getString(ec.Inputs, "mode") == "" || len(settings) != allowed {
			return nil, fmt.Errorf("goal changes cannot include technical settings")
		}
		maxHeight, err := modeMaxHeightInput(settings)
		if err != nil {
			return nil, err
		}
		hardware, err := modeHardwareInput(mode, settings)
		if err != nil {
			return nil, err
		}
		allowSlow, err := modeSlowPresetInput(mode, settings)
		if err != nil {
			return nil, err
		}
		budget, err := modeSizeBudgetInput(mode, settings)
		if err != nil {
			return nil, err
		}
		// The batch keeps the languages it was created with (its own
		// keep_languages, else the list it froze); only a batch that predates
		// the setting reads the global list as it is now.
		tracks, err := e.reconfiguredTrackPolicy(mode, ec)
		if err != nil {
			return nil, err
		}
		policy, err := modePolicyWithTracks(mode, getBool(ec.State, "is_anime"), maxHeight, hardware, allowSlow, budget, tracks)
		if err != nil {
			return nil, err
		}
		settings = map[string]any{"mode": mode, "mode_policy": policy}
	}
	// A completed or reserved replacement keeps its own file and history.
	// Only the remaining original candidates may receive new encoding settings.
	selectedKeys := map[string]bool{}
	for _, item := range items {
		if !BatchRevisionSelects(item, scope, selectionID) {
			continue
		}
		reserved, err := e.BatchItemReplacementReserved(item)
		if err != nil {
			return nil, err
		}
		if reserved {
			continue
		}
		if scope == "file" && item.ChildActionID != "" {
			child, err := e.deps.Store.GetActionInstance(item.ChildActionID)
			if err != nil {
				return nil, err
			}
			if (child.Status == StatusWaitingDecision || decisionVersion != "") && CandidateDecisionVersion(child) != decisionVersion {
				return nil, fmt.Errorf("file decision changed; reopen the settings")
			}
		}
		selectedKeys[item.ItemKey] = true
	}
	if len(selectedKeys) == 0 {
		return nil, fmt.Errorf("no unreplaced files selected")
	}
	var changed []store.TranscodeBatchItem
	var superseded []string
	for i := range items {
		item := &items[i]
		if !selectedKeys[item.ItemKey] {
			continue
		}
		merged := map[string]any{}
		if old, ok := stateObject(ec.State, "batch_item_settings")[item.ItemKey].(map[string]any); ok {
			for key, value := range old {
				merged[key] = value
			}
		}
		if getString(settings, "mode") != "" {
			merged = map[string]any{}
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
			if err := e.retireBatchDecision(ctx, child); err != nil {
				return nil, err
			}
		}
		if child != nil {
			superseded = append(superseded, child.ID)
			child, _ = e.deps.Store.GetActionInstance(child.ID)
			if child != nil {
				item.Status = child.Status
				item.Error = child.ErrorJSON
			}
		}
		recordBatchAttempt(ec, item)
		changed = append(changed, *item)
	}
	if getString(settings, "mode") != "" && scope == "all" {
		ec.State["batch_goal"] = getString(settings, "mode")
	}
	ec.State["batch_settings_revision"] = getInt(ec.State, "batch_settings_revision") + 1
	stateObject(ec.State, "batch_revision_receipts")[receipt] = true
	ec.State["batch_revision_receipt"], ec.State["batch_revision_at"] = receipt, time.Now().UTC().Format(time.RFC3339Nano)
	ec.State["paused"], ec.State["cancel_requested"] = false, false
	ec.State["batch_promotion_approved"] = false
	delete(ec.State, "reason_code")
	delete(ec.State, "next_poll_at")
	inst.Status, inst.CurrentStep = StatusRunning, 1
	inst.WaitingReason, inst.WaitingCondition, inst.WaitingOptionsJSON, inst.ErrorJSON = "", "", "[]", ""
	outputs := buildBatchOutputs(id, items, getString(ec.State, "series_title"), getBool(ec.State, "is_anime"), false)
	ec.State["counts"] = outputs["counts"]
	inst.StateJSON = toJSON(ec.State)
	inst.OutputsJSON = toJSON(outputs)
	owner, _ := ctx.Value(actionLeaseOwnerKey{actionID: id}).(string)
	if err := e.deps.Store.CommitBatchRevision(*inst, owner, changed); err != nil {
		return nil, err
	}
	// The revision is durable: the replaced attempts can never be promoted.
	e.retireSupersededCandidates(ctx, superseded)
	e.WakeReconciler()
	return e.Status(ctx, id)
}

func (e *Engine) applyPendingBatchAttempts(ctx context.Context, ec *ExecutionContext, items []store.TranscodeBatchItem) error {
	pending := stateObject(ec.State, "batch_retry_pending")
	var changed []store.TranscodeBatchItem
	var superseded []string
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
			if err := e.retireBatchDecision(ctx, child); err != nil {
				return err
			}
		}
		if child != nil {
			superseded = append(superseded, child.ID)
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
	if err := e.deps.Store.CommitBatchRevision(*inst, owner, changed); err != nil {
		return err
	}
	e.retireSupersededCandidates(ctx, superseded)
	return nil
}

func (e *Engine) BatchItemReplacementReserved(item store.TranscodeBatchItem) (bool, error) {
	if item.ChildActionID == "" {
		return false, nil
	}
	for _, service := range []string{"sonarr", "radarr", "filesystem"} {
		old, err := e.deps.Store.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:"+service+":"+item.ChildActionID)
		if err != nil {
			return false, err
		}
		if old != nil {
			return true, nil
		}
	}
	return false, nil
}
