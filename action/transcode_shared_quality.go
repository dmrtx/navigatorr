package action

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// applySharedBatchCalibration replaces the expensive candidate sweep with the
// parent's immutable selection. The worker still measures the completed file
// against this source before publishing the candidate.
func (e *Engine) applySharedBatchCalibration(ec *ExecutionContext, plan *transcode.Plan, source *mediainspect.DetailedReport, profile recipe.Profile, sourceSHA, profileDigest, cleanPath string) error {
	if ec.ActionName != "transcode_media" || e.deps.Store == nil {
		return fmt.Errorf("fixed quality is only supported for a persisted batch child")
	}
	parentID := strings.TrimSpace(getString(ec.Inputs, "parent_action_id"))
	itemKey := strings.TrimSpace(getString(ec.Inputs, "batch_item_key"))
	if parentID == "" || itemKey == "" || !strings.HasPrefix(itemKey, "epfile-") || profileDigest == "" {
		return fmt.Errorf("batch parent, item key, and frozen profile are required")
	}
	parent, err := e.deps.Store.GetActionInstance(parentID)
	if err != nil || parent == nil || parent.ActionName != "transcode_batch" || parent.Status == StatusFailed || parent.Status == StatusCancelled {
		return fmt.Errorf("calibrated parent batch is unavailable")
	}
	var parentInputs, parentState map[string]any
	if json.Unmarshal([]byte(parent.InputsJSON), &parentInputs) != nil || json.Unmarshal([]byte(parent.StateJSON), &parentState) != nil || !batchCalibrationEnabled(parentInputs) {
		return fmt.Errorf("parent batch does not own shared calibration")
	}
	calibration := getBatchCalibrationResult(parentState["shared_calibration_result"])
	if calibration == nil || calibration.Digest != getString(ec.Inputs, "batch_calibration_digest") || calibration.ProfileDigest != profileDigest || calibration.ProfileDigest != getString(parentState, "shared_calibration_profile_digest") || calibration.qualityFor(itemKey) != getInt(ec.Inputs, "batch_fixed_quality") {
		return fmt.Errorf("fixed quality does not match persisted parent calibration")
	}
	item, err := e.deps.Store.GetTranscodeBatchItem(parentID, itemKey)
	if err != nil || item == nil || item.FilePath != cleanPath || item.Decision != "transcode" {
		return fmt.Errorf("media file is not a member of the calibrated batch")
	}
	if plan == nil || plan.VideoCodec != transcode.VideoCodecLibX265 || profile.Optimization == nil || !profile.Optimization.Enabled || profile.Optimization.Search == nil || profile.Optimization.Quality == nil {
		return fmt.Errorf("frozen profile has no supported x265 quality policy")
	}
	allowed := false
	for _, quality := range profile.Optimization.Search.QualityValues {
		if quality == calibration.qualityFor(itemKey) {
			allowed = true
			break
		}
	}
	if !allowed || source == nil || len(source.Video) != 1 || source.Video[0].BitDepth != 8 || isSourceHDRorDV(source) || source.Video[0].FPS <= 0 || source.Video[0].FPS >= 45 {
		return fmt.Errorf("source is outside the calibrated 8-bit SDR video class")
	}
	policy := profile.Optimization.Quality
	if policy.VMAF == nil || policy.VMAF.Model == "" || policy.VMAF.GuardrailEnforcement != "reject" || policy.Banding == nil || !policy.Banding.Enabled || policy.Banding.Enforcement != "reject" || policy.FinalValidation == nil || policy.FinalValidation.Mode != "sampled" {
		return fmt.Errorf("frozen profile lacks enforced final VMAF/CAMBI validation")
	}
	sampling := profile.Optimization.Sampling
	if sampling == nil {
		return fmt.Errorf("frozen profile has no sample policy")
	}
	samplePlan, err := optimization.PlanSamples(optimization.SamplePlanConfig{Duration: source.DurationSec, SampleSeconds: sampling.SampleSeconds, SampleCount: sampling.SampleCount, Positions: sampling.Positions, RelativePositions: true})
	if err != nil || len(samplePlan.Samples) == 0 {
		return fmt.Errorf("planning final quality samples: %v", err)
	}
	windows := make([]transcode.BenchmarkSampleWindow, 0, len(samplePlan.Samples))
	for _, window := range samplePlan.Samples {
		windows = append(windows, transcode.BenchmarkSampleWindow{Index: window.Index, StartSeconds: window.StartSeconds, DurationSeconds: window.DurationSeconds, CenterSeconds: window.CenterSeconds})
	}
	qualityConfig := buildBenchmarkQualityConfig(policy)
	if qualityConfig == nil {
		return fmt.Errorf("final quality policy is unavailable")
	}
	provenance, err := json.Marshal(struct {
		CalibrationDigest string                            `json:"calibration_digest"`
		SourceSHA         string                            `json:"source_sha256"`
		Quality           int                               `json:"quality"`
		Samples           []transcode.BenchmarkSampleWindow `json:"samples"`
		Policy            *transcode.BenchmarkQualityConfig `json:"policy"`
	}{calibration.Digest, sourceSHA, calibration.qualityFor(itemKey), windows, qualityConfig})
	if err != nil {
		return fmt.Errorf("encoding validation provenance: %w", err)
	}
	sum := sha256.Sum256(provenance)
	plan.Quality = calibration.qualityFor(itemKey)
	plan.AverageBitrateKbps = 0
	plan.QualityValidation = &transcode.QualityValidationPlan{Metric: "vmaf", Samples: windows, Quality: *qualityConfig, BenchmarkRequestDigest: "sha256:" + hex.EncodeToString(sum[:])}
	plan.PlanDigest = ""
	digest, err := transcode.DigestPlan(plan)
	if err != nil {
		return fmt.Errorf("digesting calibrated plan: %w", err)
	}
	plan.PlanDigest = digest
	ec.State["batch_shared_validation"] = true
	ec.State["batch_calibration_digest"] = calibration.Digest
	ec.State["batch_fixed_quality"] = calibration.qualityFor(itemKey)
	return nil
}
