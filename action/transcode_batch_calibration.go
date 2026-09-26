package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

const defaultBatchCalibrationItems = 2

// BatchCalibrationResult is persisted on the parent before any full-file job
// starts. Children verify its digest and apply the same encoder settings.
type BatchCalibrationResult struct {
	Profile       string         `json:"profile"`
	ProfileDigest string         `json:"profile_digest"`
	Quality       int            `json:"quality"`
	ItemKeys      []string       `json:"item_keys"`
	ActionIDs     []string       `json:"action_ids"`
	Digest        string         `json:"digest"`
	Priority      string         `json:"priority,omitempty"`
	ItemQualities map[string]int `json:"item_qualities,omitempty"`
	SkippedItems  []string       `json:"skipped_items,omitempty"`
}

func (r *BatchCalibrationResult) qualityFor(key string) int {
	if q, ok := r.ItemQualities[key]; ok {
		return q
	}
	return r.Quality
}

func batchCalibrationEnabled(inputs map[string]any) bool {
	if raw, ok := inputs["shared_calibration"]; ok {
		value, _ := raw.(bool)
		return value
	}
	return strings.TrimSpace(getString(inputs, "profile")) == "anime-x265-calibrated"
}

func getBatchCalibrationItems(inputs map[string]any) int {
	if _, present := inputs["calibration_items"]; present {
		value, _ := strictBatchInteger(inputs["calibration_items"])
		return value
	}
	return defaultBatchCalibrationItems
}

func strictBatchInteger(raw any) (int, bool) {
	switch value := raw.(type) {
	case int:
		return value, true
	case float64:
		if !math.IsNaN(value) && !math.IsInf(value, 0) && math.Trunc(value) == value && value >= 0 && value <= 1000000 {
			return int(value), true
		}
	case json.Number:
		v, err := strconv.Atoi(string(value))
		return v, err == nil
	}
	return 0, false
}

func validateBatchCalibrationInputs(inputs map[string]any, requestedProfile string) error {
	if raw, ok := inputs["priority"]; ok {
		if raw != "quality" && raw != "savings" {
			return fmt.Errorf("priority must be quality or savings")
		}
		if !batchCalibrationEnabled(inputs) {
			return fmt.Errorf("priority requires shared calibration")
		}
	}
	if raw, present := inputs["shared_calibration"]; present {
		if _, ok := raw.(bool); !ok {
			return fmt.Errorf("shared_calibration must be a boolean")
		}
	}
	if raw, present := inputs["calibration_items"]; present {
		value, ok := strictBatchInteger(raw)
		if !ok || value < 1 || value > 3 {
			return fmt.Errorf("calibration_items must be 1-3")
		}
	}
	if !batchCalibrationEnabled(inputs) {
		return nil
	}
	if requestedProfile == "auto" {
		return fmt.Errorf("shared_calibration requires one explicit optimized libx265 profile")
	}
	if metric := strings.ToLower(strings.TrimSpace(getString(inputs, "metric"))); metric != "" && metric != "vmaf" {
		return fmt.Errorf("shared_calibration requires VMAF as its metric")
	}
	return nil
}

func representativeBatchItems(items []store.TranscodeBatchItem, requested int) []store.TranscodeBatchItem {
	eligible := make([]store.TranscodeBatchItem, 0, len(items))
	for _, item := range items {
		if item.Decision == "transcode" && item.Status == "queued" {
			eligible = append(eligible, item)
		}
	}
	if requested > len(eligible) {
		requested = len(eligible)
	}
	if requested <= 0 {
		return nil
	}
	if requested == 1 {
		return []store.TranscodeBatchItem{eligible[len(eligible)/2]}
	}
	selected := make([]store.TranscodeBatchItem, 0, requested)
	for i := 0; i < requested; i++ {
		index := i * (len(eligible) - 1) / (requested - 1)
		selected = append(selected, eligible[index])
	}
	return selected
}

func previewBatchCalibration(items []store.TranscodeBatchItem, requested int) map[string]any {
	selected := representativeBatchItems(items, requested)
	keys := make([]string, 0, len(selected))
	for _, item := range selected {
		keys = append(keys, item.ItemKey)
	}
	return map[string]any{"mode": "shared", "sample_items": keys, "sample_item_count": len(keys), "full_encode_started": false}
}

func getBatchCalibrationResult(value any) *BatchCalibrationResult {
	if result, ok := value.(*BatchCalibrationResult); ok {
		return result
	}
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var result BatchCalibrationResult
	if json.Unmarshal(data, &result) != nil || result.Digest == "" {
		return nil
	}
	return &result
}

func getBenchmarkDecision(value any) *transcode.BenchmarkDecision {
	if decision, ok := value.(*transcode.BenchmarkDecision); ok {
		return decision
	}
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var decision transcode.BenchmarkDecision
	if json.Unmarshal(data, &decision) != nil {
		return nil
	}
	return &decision
}

func chooseSharedBatchQuality(decisions []*transcode.BenchmarkDecision, search []int, minSavings float64) (int, error) {
	if len(decisions) == 0 || len(search) == 0 {
		return 0, fmt.Errorf("calibration has no completed evidence")
	}
	for i := len(search) - 1; i >= 0; i-- {
		quality := search[i]
		passesAll := true
		for _, decision := range decisions {
			if decision == nil {
				return 0, fmt.Errorf("calibration evidence is incomplete")
			}
			found := false
			for _, evaluation := range decision.Evaluations {
				if evaluation.Quality != quality {
					continue
				}
				found = true
				if evaluation.VideoCodec != transcode.VideoCodecLibX265 || evaluation.MetricType != "vmaf" || !evaluation.Eligible || !evaluation.MinimumMet || evaluation.EstimatedBytes <= 0 || evaluation.SavingsPercent < minSavings {
					passesAll = false
				}
				break
			}
			if !found {
				passesAll = false
			}
		}
		if passesAll {
			return quality, nil
		}
	}
	return 0, fmt.Errorf("no encoder setting passed quality and minimum savings on every representative episode")
}

func (e *Engine) ensureSharedBatchCalibration(ctx context.Context, ec *ExecutionContext, items []store.TranscodeBatchItem) (StepResult, bool) {
	if result := getBatchCalibrationResult(ec.State["shared_calibration_result"]); result != nil {
		return e.applyCalibrationSkips(result, items)
	}
	selected := representativeBatchItems(items, getBatchCalibrationItems(ec.Inputs))
	if len(selected) == 0 {
		return StepResult{}, true
	}
	profileName := strings.TrimSpace(getString(ec.Inputs, "profile"))
	if profileName == "" {
		profileName = "ephemeral"
	}
	var profile recipe.Profile
	if raw := ec.State["shared_calibration_profile"]; raw != nil {
		encoded, err := json.Marshal(raw)
		if err != nil || json.Unmarshal(encoded, &profile) != nil {
			return StepResult{Status: StepFailed, Error: "persisted calibration profile is unreadable"}, false
		}
	} else if raw := ec.Inputs["profile_config"]; raw != nil {
		parsed, _, _, err := decodeEphemeralProfileInput(map[string]any{"profile_config": raw})
		if err != nil {
			return StepResult{Status: StepFailed, Error: err.Error()}, false
		}
		profile = parsed
	} else {
		if e.deps.Config == nil {
			return StepResult{Status: StepFailed, Error: "recipe configuration is unavailable"}, false
		}
		_, parsed, err := e.deps.Config.Transcode.ResolvePlanAndProfileForSource(profileName, nil)
		if err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("resolving calibration profile: %v", err)}, false
		}
		profile = parsed
	}
	if profile.Video.Codec != transcode.VideoCodecLibX265 || profile.Optimization == nil || !profile.Optimization.Enabled || profile.Optimization.Search == nil || len(profile.Optimization.Search.QualityValues) == 0 || profile.Optimization.Quality == nil || profile.Optimization.Quality.VMAF == nil || profile.Optimization.Quality.VMAF.Model == "" || profile.Optimization.Quality.VMAF.GuardrailEnforcement != "reject" || profile.Optimization.Quality.Banding == nil || !profile.Optimization.Quality.Banding.Enabled || profile.Optimization.Quality.Banding.Enforcement != "reject" || profile.Optimization.Quality.FinalValidation == nil || profile.Optimization.Quality.FinalValidation.Mode != "sampled" {
		return StepResult{Status: StepFailed, Error: "shared calibration requires an optimized libx265 profile with VMAF model and sampled final validation"}, false
	}
	_, profileDigest, _, err := decodeEphemeralProfileInput(map[string]any{"profile_config": profile})
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("snapshotting calibration profile: %v", err)}, false
	}
	if ec.State["shared_calibration_profile"] == nil {
		ec.State["shared_calibration_profile"] = profile
		ec.State["shared_calibration_profile_digest"] = profileDigest
		if err := e.persistExecutionState(ctx, ec); err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("persisting calibration profile: %v", err)}, false
		}
	} else if getString(ec.State, "shared_calibration_profile_digest") != profileDigest {
		return StepResult{Status: StepFailed, Error: "calibration profile changed while batch was running"}, false
	}

	decisions := make([]*transcode.BenchmarkDecision, 0, len(selected))
	keys := make([]string, 0, len(selected))
	actionIDs := make([]string, 0, len(selected))
	for _, item := range selected {
		idempotencyKey := fmt.Sprintf("batch-calibration-%s-%s", ec.InstanceID, item.ItemKey)
		inputs := map[string]any{"path": item.FilePath, "profile_config": profile, "metric": "vmaf", "replace_original": false, "surface_worker_busy": true, "parent_action_id": ec.InstanceID}
		var result *ActionResult
		var err error
		prior, lookupErr := e.deps.Store.FindActionByIdempotencyKey("benchmark_transcode", idempotencyKey)
		if lookupErr != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("reading calibration action: %v", lookupErr)}, false
		}
		if prior == nil {
			result, err = e.Run(ctx, "benchmark_transcode", inputs, idempotencyKey)
		} else if prior.Status == StatusWaitingExternal || prior.Status == StatusPending || prior.Status == StatusRunning {
			result, err = e.Resume(ctx, prior.ID, "", nil)
		} else {
			result = buildActionResult(prior, 3, parseExecutionContext(prior, e))
		}
		if err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("representative benchmark %s: %v", item.ItemKey, err)}, false
		}
		if result == nil {
			return StepResult{Status: StepFailed, Error: "representative benchmark returned no action"}, false
		}
		if result.Status == StatusFailed || result.Status == StatusCancelled {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("representative %s benchmark failed: %s", item.ItemKey, result.Error)}, false
		}
		if result.Status != StatusCompleted {
			return StepResult{Status: StepWaitingExternal, WaitingCondition: "batch_calibration", WaitingReason: fmt.Sprintf("Calibrating representative episode %s", item.EpisodeInfo), Outputs: map[string]any{"calibration": map[string]any{"mode": "shared", "completed": len(decisions), "total": len(selected), "current_item": item.ItemKey, "action_id": result.ID}}}, false
		}
		decision := getBenchmarkDecision(result.Outputs["benchmark_decision"])
		if decision == nil || len(decision.Evaluations) == 0 || getString(result.Outputs, "ephemeral_recipe_digest") != profileDigest {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("representative %s has incomplete or mismatched benchmark evidence", item.ItemKey)}, false
		}
		decisions = append(decisions, decision)
		keys = append(keys, item.ItemKey)
		actionIDs = append(actionIDs, result.ID)
	}
	minSavings, _ := e.effectiveSizeGuardrails(ec)
	result := BatchCalibrationResult{Profile: profileName, ProfileDigest: profileDigest, ItemKeys: keys, ActionIDs: actionIDs}
	priority := getString(ec.State, "batch_priority")
	if priority == "" {
		priority = getString(ec.Inputs, "priority")
	}
	if priority == "" { // Preserve the policy of batches created before priority support.
		quality, err := chooseSharedBatchQuality(decisions, profile.Optimization.Search.QualityValues, minSavings)
		if err != nil {
			return StepResult{Status: StepFailed, Error: err.Error()}, false
		}
		result.Quality = quality
	} else {
		result.Priority = priority
		result.ItemQualities = make(map[string]int)
		for i, decision := range decisions {
			q := chooseBatchItemQuality(decision, profile.Optimization.Search.QualityValues, minSavings, priority)
			if q == 0 {
				result.SkippedItems = append(result.SkippedItems, keys[i])
				continue
			}
			result.ItemQualities[keys[i]] = q
			if result.Quality == 0 || priority == "quality" && q < result.Quality || priority == "savings" && q > result.Quality {
				result.Quality = q
			}
		}
		if result.Quality == 0 {
			result.SkippedItems = nil
			for _, item := range items {
				if item.Status == "queued" {
					result.SkippedItems = append(result.SkippedItems, item.ItemKey)
				}
			}
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return StepResult{Status: StepFailed, Error: err.Error()}, false
	}
	sum := sha256.Sum256(encoded)
	result.Digest = "sha256:" + hex.EncodeToString(sum[:])
	ec.State["shared_calibration_result"] = result
	if err := e.persistExecutionState(ctx, ec); err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("persisting shared calibration: %v", err)}, false
	}
	return e.applyCalibrationSkips(&result, items)
}

// A calibrated batch never asks for per-episode approval. A child that needs
// human acceptance is rejected and the original remains in place.
func (e *Engine) rejectCalibratedBatchDecisions(ctx context.Context, items []store.TranscodeBatchItem) error {
	for i := range items {
		item := &items[i]
		if item.Status != "waiting_decision" || item.ChildActionID == "" {
			continue
		}
		result, err := e.Resume(ctx, item.ChildActionID, "reject", nil)
		if err != nil {
			return fmt.Errorf("rejecting calibrated candidate %s: %w", item.ItemKey, err)
		}
		if result == nil || (result.Status != StatusFailed && result.Status != StatusCancelled) {
			return fmt.Errorf("calibrated candidate %s did not confirm rejection", item.ItemKey)
		}
		item.Status = "failed"
		item.Error = "candidate required manual acceptance and was rejected automatically; original preserved"
		if item.Attempts == 0 {
			item.Attempts = 1
		}
		if err := e.deps.Store.UpdateTranscodeBatchItem(*item); err != nil {
			return fmt.Errorf("persisting calibrated rejection: %w", err)
		}
	}
	return nil
}

// Only reuse measurements already made. A difficult representative never vetoes
// another episode, and no additional benchmark or full encode retry is started.
func chooseBatchItemQuality(decision *transcode.BenchmarkDecision, search []int, minSavings float64, priority string) int {
	best := 0
	if decision == nil {
		return best
	}
	for _, ev := range decision.Evaluations {
		allowed := false
		for _, q := range search {
			if ev.Quality == q {
				allowed = true
				break
			}
		}
		if !allowed || !ev.Eligible || !ev.MinimumMet || ev.VideoCodec != transcode.VideoCodecLibX265 || ev.MetricType != "vmaf" || ev.EstimatedBytes <= 0 || ev.SavingsPercent < minSavings {
			continue
		}
		if best == 0 || priority == "quality" && ev.Quality < best || priority == "savings" && ev.Quality > best {
			best = ev.Quality
		}
	}
	return best
}

func (e *Engine) applyCalibrationSkips(result *BatchCalibrationResult, items []store.TranscodeBatchItem) (StepResult, bool) {
	skipped := make(map[string]bool)
	for _, key := range result.SkippedItems {
		skipped[key] = true
	}
	for i := range items {
		item := &items[i]
		if !skipped[item.ItemKey] || item.Status != "queued" {
			continue
		}
		item.Status, item.Decision = "skip", "skip"
		item.Reasons = append(item.Reasons, "calibration found no suitable candidate within the bounded search; original preserved")
		if err := e.deps.Store.UpdateTranscodeBatchItem(*item); err != nil {
			return StepResult{Status: StepFailed, Error: err.Error()}, false
		}
	}
	return StepResult{}, true
}
