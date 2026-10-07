package action

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

const ModePolicyVersion = "transcode-modes-v1"
const modeOperationBudgetSeconds = 3600
const modeSearchBudgetSeconds = 600

// ModePolicySnapshot is frozen with each operation. These thresholds are a
// bounded SDR sample policy, never a claim of perceptual transparency.
type ModePolicySnapshot struct {
	Version                string         `json:"version"`
	Mode                   string         `json:"mode"`
	MinSavingsPercent      float64        `json:"min_savings_percent"`
	MaxSizeIncreasePercent float64        `json:"max_size_increase_percent"`
	SearchBudgetSeconds    int            `json:"search_budget_seconds"`
	OperationBudgetSeconds int            `json:"operation_budget_seconds"`
	Profile                recipe.Profile `json:"profile"`
	Digest                 string         `json:"digest"`
}

func modePolicy(mode string, anime bool) (ModePolicySnapshot, error) {
	if mode != "size" && mode != "quality" && mode != "x265_preserve" {
		return ModePolicySnapshot{}, fmt.Errorf("mode must be size, quality, or x265_preserve")
	}
	p := automaticBatchSearchProfile(anime)
	p.Optimization.Search.MaxCandidates = 6
	if mode != "size" {
		p.Optimization.Quality.VMAF.Target = 96
		p.Optimization.Quality.VMAF.Minimum = 95
		p5, worst := 93.0, 92.0
		p.Optimization.Quality.VMAF.P5Minimum = &p5
		p.Optimization.Quality.VMAF.WorstWindowMinimum = &worst
	}
	snapshot := ModePolicySnapshot{Version: ModePolicyVersion, Mode: mode, MinSavingsPercent: 15, SearchBudgetSeconds: modeSearchBudgetSeconds, OperationBudgetSeconds: modeOperationBudgetSeconds, Profile: p}
	if mode == "x265_preserve" {
		snapshot.MinSavingsPercent = 0
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return snapshot, err
	}
	h := sha256.Sum256(b)
	snapshot.Digest = "sha256:" + hex.EncodeToString(h[:])
	return snapshot, nil
}

func validateModeInputs(inputs map[string]any) error {
	raw, present := inputs["mode"]
	if !present {
		return nil
	}
	mode, ok := raw.(string)
	if !ok {
		return fmt.Errorf("mode must be a string")
	}
	if _, err := modePolicy(mode, getBool(inputs, "is_anime") || getString(inputs, "media_type") == "anime"); err != nil {
		return err
	}
	for _, key := range []string{"profile", "profile_config", "priority", "metric", "min_savings_percent", "max_size_increase_percent", "preserve_source_bit_depth", "expected_video_codec", "shared_calibration", "calibration_items", "batch_fixed_quality", "batch_calibration_digest"} {
		if _, present := inputs[key]; present {
			return fmt.Errorf("mode cannot be combined with %s; its quality, preservation and size policy is immutable", key)
		}
	}
	return nil
}

func freezeModePolicy(ec *ExecutionContext) (*ModePolicySnapshot, error) {
	mode := getString(ec.Inputs, "mode")
	if mode == "" {
		return nil, nil
	}
	if raw := ec.State["mode_policy"]; raw != nil {
		b, err := json.Marshal(raw)
		var p ModePolicySnapshot
		if err != nil || json.Unmarshal(b, &p) != nil || p.Mode != mode || p.Digest == "" {
			return nil, fmt.Errorf("frozen mode policy is unreadable or mismatched")
		}
		digest := p.Digest
		p.Digest = ""
		canonical, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(canonical)
		p.Digest = digest
		if "sha256:"+hex.EncodeToString(sum[:]) != digest {
			return nil, fmt.Errorf("frozen mode policy digest does not match its content")
		}
		ec.State["search_budget_seconds"] = p.SearchBudgetSeconds
		ec.State["operation_search_budget_seconds"] = p.OperationBudgetSeconds
		return &p, nil
	}
	p, err := modePolicy(mode, getBool(ec.State, "resolved_is_anime") || getBool(ec.Inputs, "is_anime") || getString(ec.Inputs, "media_type") == "anime")
	if err != nil {
		return nil, err
	}
	ec.State["mode_policy"] = p
	ec.State["mode"] = mode
	ec.State["policy_digest"] = p.Digest
	ec.State["search_budget_seconds"] = p.SearchBudgetSeconds
	ec.State["operation_search_budget_seconds"] = p.OperationBudgetSeconds
	return &p, nil
}

func modeSourceReason(mode string, rep *mediainspect.DetailedReport) string {
	if rep == nil || len(rep.Video) != 1 || rep.Video[0].BitDepth != 8 && rep.Video[0].BitDepth != 10 || rep.Video[0].FPS <= 0 || rep.Video[0].FPS >= 45 || isSourceHDRorDV(rep) {
		return "unsupported_source"
	}
	if mode == "x265_preserve" && strings.EqualFold(rep.Video[0].Codec, "hevc") {
		return "already_target_codec"
	}
	return ""
}

func modeAttention(ec *ExecutionContext, reason, message string) StepResult {
	ec.State["reason_code"] = reason
	if ec.Decision == "cancel" || ec.Decision == "reject" {
		return StepResult{Status: StepCancelled, Outputs: map[string]any{"reason_code": "user_cancelled"}}
	}
	return StepResult{Status: StepWaitingDecision, WaitingCondition: reason, WaitingReason: message, WaitingOptions: []WaitingOption{{Decision: "cancel", Description: "Stop this operation and keep the original"}}, Outputs: map[string]any{"mode": getString(ec.Inputs, "mode"), "policy_digest": getString(ec.State, "policy_digest"), "reason_code": reason}}
}

func applyModePlan(plan *transcode.Plan, p *ModePolicySnapshot) error {
	plan.Mode = p.Mode
	plan.PolicyDigest = p.Digest
	plan.SizePolicy = &transcode.SizePolicy{MinSavingsPercent: p.MinSavingsPercent, MaxSizeIncreasePercent: p.MaxSizeIncreasePercent}
	plan.PlanDigest = ""
	d, err := transcode.DigestPlan(plan)
	plan.PlanDigest = d
	return err
}

func (e *Engine) bindModeParent(ec *ExecutionContext) error {
	if getString(ec.Inputs, "mode") == "" || getString(ec.Inputs, "parent_action_id") == "" {
		return nil
	}
	if e.deps.Store == nil {
		return fmt.Errorf("mode parent store is unavailable")
	}
	parent, err := e.deps.Store.GetActionInstance(getString(ec.Inputs, "parent_action_id"))
	if err != nil || parent == nil || parent.ActionName != "transcode_batch" {
		return fmt.Errorf("mode parent is unavailable")
	}
	var inputs, states map[string]any
	if json.Unmarshal([]byte(parent.InputsJSON), &inputs) != nil || json.Unmarshal([]byte(parent.StateJSON), &states) != nil || getString(inputs, "mode") != getString(ec.Inputs, "mode") {
		return fmt.Errorf("mode does not match frozen parent")
	}
	item, err := e.deps.Store.GetTranscodeBatchItem(parent.ID, getString(ec.Inputs, "batch_item_key"))
	if err != nil || item == nil || item.FilePath != getString(ec.Inputs, "path") || item.Decision != "transcode" {
		return fmt.Errorf("mode source is not a member of parent inventory")
	}
	ec.State["mode_policy"] = states["mode_policy"]
	ec.State["policy_digest"] = getString(states, "policy_digest")
	ec.State["mode"] = getString(inputs, "mode")
	ec.State["search_budget_seconds"] = getInt(states, "search_budget_seconds")
	ec.State["operation_search_budget_seconds"] = getInt(states, "operation_search_budget_seconds")
	return nil
}

// SearchBudgetEntry is persisted before any external admission. Uncertain
// accepted effects retain the full reservation; authoritative completion settles
// measured active time and releases unused credit.
type SearchBudgetEntry struct {
	Reserved float64  `json:"reserved_seconds"`
	Consumed *float64 `json:"consumed_seconds,omitempty"`
}

func modeBudgetEntries(ec *ExecutionContext) map[string]SearchBudgetEntry {
	entries := map[string]SearchBudgetEntry{}
	b, _ := json.Marshal(ec.State["search_budget_entries"])
	_ = json.Unmarshal(b, &entries)
	if entries == nil {
		entries = map[string]SearchBudgetEntry{}
	}
	return entries
}

func reserveModeSearch(ec *ExecutionContext, key string) bool {
	entries := modeBudgetEntries(ec)
	if _, ok := entries[key]; ok {
		return true
	}
	used := 0.0
	for _, entry := range entries {
		if entry.Consumed != nil {
			used += *entry.Consumed
		} else {
			used += entry.Reserved
		}
	}
	sourceBudget, operationBudget := getInt(ec.State, "search_budget_seconds"), getInt(ec.State, "operation_search_budget_seconds")
	if sourceBudget <= 0 || operationBudget <= 0 {
		return false
	}
	if used+float64(sourceBudget) > float64(operationBudget) {
		return false
	}
	entries[key] = SearchBudgetEntry{Reserved: float64(sourceBudget)}
	ec.State["search_budget_entries"] = entries
	return true
}

func (e *Engine) settleModeSearches(ec *ExecutionContext, items []store.TranscodeBatchItem) {
	entries := modeBudgetEntries(ec)
	for _, item := range items {
		if item.ChildActionID == "" {
			continue
		}
		child, err := e.deps.Store.GetActionInstance(item.ChildActionID)
		if err != nil || child == nil {
			continue
		}
		var state map[string]any
		_ = json.Unmarshal([]byte(child.StateJSON), &state)
		measured, known := state["benchmark_search_seconds"]
		if !known {
			definitelyUnsubmitted := !getBool(state, "benchmark_submitted") && !getBool(state, "benchmark_reconcile")
			if definitelyUnsubmitted && (child.Status == StatusCompleted || child.Status == StatusFailed || child.Status == StatusCancelled || child.Status == StatusWaitingDecision && getString(state, "reason_code") == "quality_policy_unavailable") {
				measured = float64(0)
			} else {
				continue
			}
		}
		value, valid := measuredSearchSeconds(measured)
		if !valid {
			continue
		}
		key := batchChildKey(ec, item)
		entry, exists := entries[key]
		if !exists || entry.Consumed != nil || value > entry.Reserved {
			continue
		}
		entry.Consumed = &value
		entries[key] = entry
	}
	ec.State["search_budget_entries"] = entries
}

func verifyModeBenchmarkWinner(ec *ExecutionContext, winner *transcode.BenchmarkWinner) error {
	if getString(ec.Inputs, "mode") == "" {
		return nil
	}
	var request transcode.BenchmarkRequest
	b, err := json.Marshal(ec.State["benchmark_request"])
	if err != nil || json.Unmarshal(b, &request) != nil {
		return fmt.Errorf("mode benchmark request missing")
	}
	digest, err := transcode.DigestBenchmarkRequest(&request)
	if err != nil || digest != getString(ec.State, "benchmark_request_digest") || request.Mode != getString(ec.Inputs, "mode") || request.SourceSHA256 != getString(ec.State, "original_sha256") {
		return fmt.Errorf("mode benchmark evidence request identity mismatch")
	}
	for _, candidate := range request.Candidates {
		if candidate.ID != winner.CandidateID {
			continue
		}
		if transcode.BenchmarkCandidateVideoCodec(candidate) != winner.VideoCodec || candidate.Quality != winner.Quality || candidate.AverageBitrateKbps != winner.AverageBitrateKbps || candidate.Preset != winner.Preset || candidate.Tune != winner.Tune || candidate.VideoProfile != winner.VideoProfile || candidate.PixelFormat != winner.PixelFormat {
			return fmt.Errorf("mode benchmark winner does not match measured candidate plan")
		}
		snapshot, err := freezeModePolicy(ec)
		if err != nil {
			return err
		}
		if !winner.MinimumMet || winner.MetricType != "vmaf" || winner.Score < snapshot.Profile.Optimization.Quality.VMAF.Minimum {
			return fmt.Errorf("mode benchmark winner does not meet frozen quality policy")
		}
		return nil
	}
	return fmt.Errorf("mode benchmark winner was not part of frozen request")
}

func measuredSearchSeconds(raw any) (float64, bool) {
	var seconds float64
	switch value := raw.(type) {
	case float64:
		seconds = value
	case int:
		seconds = float64(value)
	case int64:
		seconds = float64(value)
	default:
		return 0, false
	}
	return seconds, seconds >= 0 && !math.IsNaN(seconds) && !math.IsInf(seconds, 0)
}
