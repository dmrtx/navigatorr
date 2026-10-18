package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/quality"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

const ModePolicyVersion = "transcode-modes-v2"

// SizeModePolicyVersion freezes new size-mode operations. Older frozen size
// policies keep their own version and behaviour.
const SizeModePolicyVersion = optimization.ModePolicyV3
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
	// Compact is set only for mode=compact, so other snapshots and their
	// digests are unchanged.
	Compact *ModeCompactPolicy `json:"compact,omitempty"`
	// Size is set only for size policy v3, so v2 snapshots and their digests
	// are unchanged.
	Size   *ModeSizePolicy `json:"size,omitempty"`
	Digest string          `json:"digest"`
}

// ModeSizePolicy freezes what size v3 adds on top of the v2 quality floor:
// Main10 output for 8-bit sources on workers that prove native 10-bit
// quality, an x265-only ladder, and final windows scaled with duration.
type ModeSizePolicy struct {
	EightBitMain10          bool  `json:"eight_bit_main10"`
	VideoToolboxCandidates  bool  `json:"videotoolbox_candidates"`
	FallbackQualityValues   []int `json:"fallback_quality_values"`
	FallbackSavingsValues   []int `json:"fallback_savings_values"`
	FinalWindowSeconds      int   `json:"final_window_seconds"`
	FinalWindowEverySeconds int   `json:"final_window_every_seconds"`
	FinalWindowsMinimum     int   `json:"final_windows_minimum"`
	FinalWindowsMaximum     int   `json:"final_windows_maximum"`
	// AudioOnlyFallback lets a source whose video cannot shrink within the
	// floor still compact its lossless audio, with the video stream-copied
	// and proven bit-identical. The same minimum savings applies.
	AudioOnlyFallback bool `json:"audio_only_fallback,omitempty"`
	// LosslessObjectAudioToAAC converts lossless Atmos and DTS:X (TrueHD,
	// DTS-HD MA) to AAC on their channel bed, like any lossless track; lossy
	// object audio (E-AC-3 JOC) is still copied.
	LosslessObjectAudioToAAC bool `json:"lossless_object_audio_to_aac"`
}

// sizeModeV3Policy is the frozen size v3 addition. The ladder is x265 only:
// medium CRF 22-30 first, then one slow family chosen from what failed.
func sizeModeV3Policy() *ModeSizePolicy {
	return &ModeSizePolicy{EightBitMain10: true, FallbackQualityValues: []int{16, 18, 20}, FallbackSavingsValues: []int{24, 27, 30}, FinalWindowSeconds: 10, FinalWindowEverySeconds: 600, FinalWindowsMinimum: 5, FinalWindowsMaximum: 12, AudioOnlyFallback: true, LosslessObjectAudioToAAC: true}
}

// ModeCompactPolicy freezes the output shape of compact mode: SDR BT.709 no
// taller than MaxHeight, through the named worker chain.
type ModeCompactPolicy struct {
	MaxHeight    int    `json:"max_height"`
	ChainVersion string `json:"chain_version"`
}

func modePolicy(mode string, anime bool) (ModePolicySnapshot, error) {
	return modePolicyFor(mode, anime, 0)
}

// modePolicyFor freezes a mode. maxHeight applies only to compact mode, where
// zero selects the 1080p default.
func modePolicyFor(mode string, anime bool, maxHeight int) (ModePolicySnapshot, error) {
	if mode != "size" && mode != "quality" && mode != "x265_preserve" && mode != "compact" {
		return ModePolicySnapshot{}, fmt.Errorf("mode must be size, quality, x265_preserve, or compact")
	}
	if mode != "compact" && maxHeight != 0 {
		return ModePolicySnapshot{}, fmt.Errorf("max_height is only supported with mode=compact")
	}
	if mode == "compact" {
		if maxHeight == 0 {
			maxHeight = transcode.CompactDefaultMaxHeight
		}
		if !transcode.ValidCompactMaxHeight(maxHeight) {
			return ModePolicySnapshot{}, fmt.Errorf("max_height must be one of %v", transcode.CompactMaxHeights)
		}
	}
	p := automaticBatchSearchProfile(anime)
	p.Optimization.Search.MaxCandidates = 12
	if mode == "compact" {
		// Compact keeps the size-mode quality floor: the visible change is
		// the intended SDR/resolution conversion, measured separately.
		p.Audio.Mode = "compact"
	} else if mode == "size" {
		// Same quality floor as v2; only lossless audio is converted.
		p.Audio.Mode = transcode.AudioModeCompactLossless
		p.Optimization.Search.QualityValues = []int{22, 24, 26, 28, 30}
	} else {
		p.Optimization.Quality.VMAF.Target = 96
		p.Optimization.Quality.VMAF.Minimum = 95
		p5, worst := 93.0, 92.0
		p.Optimization.Quality.VMAF.P5Minimum = &p5
		p.Optimization.Quality.VMAF.WorstWindowMinimum = &worst
	}
	snapshot := ModePolicySnapshot{Version: ModePolicyVersion, Mode: mode, MinSavingsPercent: 15, SearchBudgetSeconds: modeSearchBudgetSeconds, OperationBudgetSeconds: modeOperationBudgetSeconds, Profile: p}
	if mode == "x265_preserve" || mode == "quality" {
		snapshot.MinSavingsPercent = 0
	}
	if mode == "size" {
		snapshot.Version = SizeModePolicyVersion
		snapshot.Size = sizeModeV3Policy()
	}
	if mode == "compact" {
		snapshot.MinSavingsPercent = optimization.CompactMinimumSavingsPercent
		snapshot.Compact = &ModeCompactPolicy{MaxHeight: maxHeight, ChainVersion: transcode.CompactChainVersion}
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
		if _, has := inputs["max_height"]; has {
			return fmt.Errorf("max_height is only supported with mode=compact")
		}
		return nil
	}
	mode, ok := raw.(string)
	if !ok {
		return fmt.Errorf("mode must be a string")
	}
	maxHeight, err := modeMaxHeightInput(inputs)
	if err != nil {
		return err
	}
	if _, err := modePolicyFor(mode, getBool(inputs, "is_anime") || getString(inputs, "media_type") == "anime", maxHeight); err != nil {
		return err
	}
	for _, key := range []string{"profile", "profile_config", "priority", "metric", "min_savings_percent", "max_size_increase_percent", "preserve_source_bit_depth", "expected_video_codec", "shared_calibration", "calibration_items", "batch_fixed_quality", "batch_calibration_digest"} {
		if _, present := inputs[key]; present {
			return fmt.Errorf("mode cannot be combined with %s; its quality, preservation and size policy is immutable", key)
		}
	}
	return nil
}

// modeMaxHeightInput reads the optional compact max_height input.
func modeMaxHeightInput(inputs map[string]any) (int, error) {
	raw, present := inputs["max_height"]
	if !present || raw == nil {
		return 0, nil
	}
	var v float64
	switch n := raw.(type) {
	case float64:
		v = n
	case int:
		v = float64(n)
	case int64:
		v = float64(n)
	default:
		return 0, fmt.Errorf("max_height must be a number")
	}
	if v != math.Trunc(v) || !transcode.ValidCompactMaxHeight(int(v)) {
		return 0, fmt.Errorf("max_height must be one of %v", transcode.CompactMaxHeights)
	}
	return int(v), nil
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
		ec.State["policy_digest"] = p.Digest
		ec.State["mode"] = p.Mode
		ec.State["search_budget_seconds"] = p.SearchBudgetSeconds
		ec.State["operation_search_budget_seconds"] = p.OperationBudgetSeconds
		return &p, nil
	}
	maxHeight, err := modeMaxHeightInput(ec.Inputs)
	if err != nil {
		return nil, err
	}
	p, err := modePolicyFor(mode, getBool(ec.State, "resolved_is_anime") || getBool(ec.Inputs, "is_anime") || getString(ec.Inputs, "media_type") == "anime", maxHeight)
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

// modeSourceReason classifies a source for an automatic mode. hfrSupported
// must come from the worker's proven decimated-VMAF capability; without it
// progressive 45-61 FPS sources are skipped as hfr_quality_unavailable (a new
// goal will not help; a capable worker will), and interlaced HFR-class
// sources and rates above that class stay unsupported. Compact mode accepts
// HDR and Dolby Vision; compactSourcePlan decides them from stream and frame
// evidence.
func modeSourceReason(mode string, rep *mediainspect.DetailedReport, hfrSupported bool) string {
	video := rep.MainVideo()
	if len(video) != 1 || video[0].BitDepth != 8 && video[0].BitDepth != 10 {
		return "unsupported_source"
	}
	// Only the compact goal converts HDR and Dolby Vision; a goal change can
	// pick these files up, unlike unsupported_source.
	if mode != "compact" && isSourceHDRorDV(rep) {
		return "hdr_requires_compact"
	}
	factor, err := quality.TemporalDecimationForStream(video[0].FPS, video[0].FieldOrder)
	if err != nil {
		return "unsupported_source"
	}
	if factor > 1 && !hfrSupported {
		return "hfr_quality_unavailable"
	}
	if mode == "x265_preserve" && strings.EqualFold(video[0].Codec, "hevc") {
		return "already_target_codec"
	}
	return ""
}

// hfrQualitySupport asks the worker at most once per step, and only when a
// source actually needs decimation. A failed lookup is returned, never
// cached as "unsupported": callers wait on retryable errors and fail closed
// on the rest.
type hfrQualitySupport struct {
	e         *Engine
	checked   bool
	supported bool
}

func (h *hfrQualitySupport) forSource(ctx context.Context, rep *mediainspect.DetailedReport) (bool, error) {
	if rep == nil {
		return false, nil
	}
	video := rep.MainVideo()
	if len(video) != 1 {
		return false, nil
	}
	if factor, err := quality.TemporalDecimationForStream(video[0].FPS, video[0].FieldOrder); err != nil || factor == 1 {
		return false, nil
	}
	if h.checked {
		return h.supported, nil
	}
	if h.e == nil || h.e.deps.Transcode == nil {
		h.checked = true
		return false, nil
	}
	caps, err := h.e.deps.Transcode.Capabilities(ctx)
	if err != nil {
		return false, err
	}
	h.checked, h.supported = true, caps.SupportsHFRQuality()
	return h.supported, nil
}

// hfrCapabilityLookupResult turns a failed HFR capability lookup into the
// step result: a transient worker error waits, anything else fails closed.
func hfrCapabilityLookupResult(err error) StepResult {
	return workerCapabilityLookupResult(err, "HFR quality")
}

func workerCapabilityLookupResult(err error, what string) StepResult {
	if isRetryableWorkerPollError(err) {
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_unreachable", WaitingReason: fmt.Sprintf("Worker capabilities temporarily unavailable: %v", err)}
	}
	return StepResult{Status: StepFailed, Error: fmt.Sprintf("querying worker %s capability: %v (fail closed)", what, err)}
}

// sizeMain10Supported reports a worker that can encode and measure an 8-bit
// source as Main10 under size v3: x265, explicit Main10 benchmarks and the
// proven native 10-bit VMAF and CAMBI path. Otherwise the source stays 8-bit.
func sizeMain10Supported(caps transcode.WorkerCapabilities) bool {
	return caps.SizeModeV3 && caps.ExplicitMain10Benchmarks && caps.SupportsNative10BitQuality() && caps.Encoders[transcode.VideoCodecLibX265]
}

// setMain10 switches a resolved plan to 10-bit HEVC output.
func setMain10(plan *transcode.Plan) error {
	if plan.VideoCodec != transcode.VideoCodecLibX265 {
		return fmt.Errorf("size Main10 requires a libx265 plan (fail closed)")
	}
	plan.VideoProfile, plan.PixelFormat, plan.ExpectedBitDepth = "main10", "p010le", 10
	plan.PlanDigest = ""
	digest, err := transcode.DigestPlan(plan)
	plan.PlanDigest = digest
	return err
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
	plan.ModePolicyVersion = p.Version
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
	if json.Unmarshal([]byte(parent.InputsJSON), &inputs) != nil || json.Unmarshal([]byte(parent.StateJSON), &states) != nil {
		return fmt.Errorf("mode does not match frozen parent")
	}
	item, err := e.deps.Store.GetTranscodeBatchItem(parent.ID, getString(ec.Inputs, "batch_item_key"))
	if err != nil || item == nil || item.FilePath != getString(ec.Inputs, "path") || item.Decision != "transcode" {
		return fmt.Errorf("mode source is not a member of parent inventory")
	}
	mode, policy := getString(inputs, "mode"), states["mode_policy"]
	settings := stateObject(states, "batch_item_settings")
	if row, ok := settings[getString(ec.Inputs, "batch_item_key")].(map[string]any); ok && getString(row, "mode") != "" {
		mode = getString(row, "mode")
		policy = row["mode_policy"]
	}
	if mode != getString(ec.Inputs, "mode") {
		return fmt.Errorf("mode does not match frozen parent attempt")
	}
	ec.State["mode_policy"] = policy
	ec.State["policy_digest"] = getString(states, "policy_digest")
	ec.State["mode"] = mode
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
	// Reservations own stable child keys, even after a settings revision moves
	// the visible row to a new generation. Settle old attempts as well as current
	// ones, retaining the full reservation whenever execution cost is uncertain.
	for key, entry := range entries {
		if entry.Consumed != nil {
			continue
		}
		child, err := e.deps.Store.FindActionByIdempotencyKey("transcode_media", key)
		if err == nil && child == nil {
			for _, item := range items {
				if item.ChildActionID != "" && batchChildKey(ec, item) == key {
					child, err = e.deps.Store.GetActionInstance(item.ChildActionID)
					break
				}
			}
		}
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
		if value > entry.Reserved {
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
	if plan := getPlan(ec.State["plan"]); plan == nil || !request.Compact.Equal(plan.Compact) {
		return fmt.Errorf("mode benchmark compact plan does not match the frozen plan")
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

// compactSourcePlan classifies the source from stream and decoded-frame
// evidence and derives its frozen compact plan. A non-empty reason is a
// user-facing skip, never a hard failure: a source whose frames cannot be
// probed is skipped so one bad file does not fail a whole batch.
func compactSourcePlan(ctx context.Context, ffprobe, path string, rep *mediainspect.DetailedReport, p *ModePolicySnapshot) (*transcode.CompactPlan, string) {
	if p == nil || p.Compact == nil || p.Compact.ChainVersion != transcode.CompactChainVersion {
		return nil, "compact_policy_unavailable"
	}
	video := rep.MainVideo()
	if len(video) != 1 {
		return nil, "hdr_single_video_stream_required"
	}
	d, err := mediainspect.ClassifySource(ctx, ffprobe, path, rep)
	if err != nil {
		return nil, "compact_frame_evidence_unavailable"
	}
	plan, err := transcode.NewCompactPlan(d, video[0], p.Compact.MaxHeight)
	if err != nil {
		return nil, err.Error()
	}
	return plan, ""
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

// compactInventoryReason screens a batch source from stream evidence alone.
// SDR sources are decided now (for example compact_not_needed); HDR and Dolby
// Vision need decoded-frame evidence, which each child preflight collects.
func compactInventoryReason(ec *ExecutionContext, rep *mediainspect.DetailedReport) string {
	p, err := freezeModePolicy(ec)
	if err != nil || p == nil || p.Compact == nil {
		return "compact_policy_unavailable"
	}
	video := rep.MainVideo()
	if len(video) != 1 {
		return "hdr_single_video_stream_required"
	}
	d := mediainspect.ClassifyDynamicRange(rep, nil)
	if d.IsHDR() {
		return ""
	}
	if _, err := transcode.NewCompactPlan(d, video[0], p.Compact.MaxHeight); err != nil {
		return err.Error()
	}
	return ""
}
