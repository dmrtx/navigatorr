package action

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// stepTranscodeValidate is the coordinator's LIGHTWEIGHT independent
// post-publish check. The expensive structural/media/policy validation already
// ran worker-local on the candidate BEFORE publish (see
// internal/transcodeworker.validateEncodedCandidateFull), and the worker
// performed its own stat/size check after publish. This step therefore only
// verifies cheap, independently observable facts:
//
//   - a filesystem candidate exists as a regular, non-empty file, and its size
//     matches the worker-attested accepted-candidate size when available;
//   - a direct-SMB logical candidate is never stat'd (its semantic path may not
//     be mounted) and relies on the worker's hash-verified exclusive publish;
//   - the resolved plan is present.
//
// It deliberately performs NO ffprobe/mediainspect and reads no candidate
// bytes. Full promotion-time inspection lives in validateCandidateDetailed.
func (e *Engine) stepTranscodeValidate(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if getBool(ec.State, "skip_transcode") {
		return StepResult{
			Status:  StepSkipped,
			Outputs: map[string]any{"skipped": true},
		}, nil
	}
	if ec.Decision != "" {
		if strings.EqualFold(ec.Decision, "reject") || strings.EqualFold(ec.Decision, "cancel") {
			return StepResult{Status: StepFailed, Error: "transcode candidate rejected by user decision; original file remains untouched"}, nil
		}
	}
	acceptValidationLoss := strings.EqualFold(ec.Decision, "accept_loss") || strings.EqualFold(ec.Decision, "approve")
	outputPath := getString(ec.State, "candidate_path")
	if outputPath == "" {
		outputPath = getString(ec.State, "output_path")
	}
	if outputPath == "" {
		return StepResult{Status: StepFailed, Error: "candidate path is missing (fail closed)"}, nil
	}
	plan := getPlan(ec.State["plan"])
	if plan == nil {
		return StepResult{Status: StepFailed, Error: "resolved plan missing during validation (fail closed)"}, nil
	}
	observedSize, err := e.verifyPublishedCandidateLight(ctx, ec, outputPath)
	if err != nil {
		return StepResult{Status: StepFailed, Error: err.Error()}, nil
	}
	candidateSize := observedSize
	if candidateSize <= 0 {
		candidateSize = getInt64(ec.State, "candidate_size_bytes")
	}
	if candidateSize <= 0 {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("accepted candidate size is unknown for %q (fail closed)", outputPath)}, nil
	}
	if ec.Decision != "" {
		ec.State["validation_decision_applied"] = ec.Decision
	}
	origMap, _ := ec.State["original"].(map[string]any)
	origSize := getInt64(origMap, "size_bytes")
	// Size guardrail: a cheap size-only policy check that needs no candidate
	// read. It preserves the historical wait/accept-loss behavior.
	_, maxInc := e.effectiveSizeGuardrails(ec)
	if !acceptValidationLoss && origSize > 0 && candidateSize > origSize {
		increasePct := float64(candidateSize-origSize) / float64(origSize) * 100
		if increasePct > maxInc {
			reason := fmt.Sprintf("Candidate file size (%d bytes) exceeds original (%d bytes) by %.1f%%, which is greater than max_size_increase_percent (%.1f%%)", candidateSize, origSize, increasePct, maxInc)
			return StepResult{Status: StepWaitingDecision, WaitingReason: reason, WaitingOptions: []WaitingOption{{Decision: "reject", Description: "Reject candidate and keep original"}, {Decision: "accept_loss", Description: "Accept candidate despite validation discrepancy"}}}, nil
		}
	}
	saved := origSize - candidateSize
	pct := float64(0)
	if origSize > 0 {
		pct = float64(saved) / float64(origSize) * 100
	}
	// Populate the result/validation maps from the immutable plan and the
	// original report. The candidate's own stream attributes were validated
	// worker-local before publish; the coordinator reports the expected target
	// attributes rather than re-probing over NAS.
	videoCodec := expectedVideoCodec(plan.VideoCodec)
	resolution := getString(origMap, "resolution")
	bitDepth := plan.ExpectedBitDepth
	if bitDepth == 0 {
		bitDepth = getInt(origMap, "bit_depth")
	}
	duration := getFloat(origMap, "duration_sec")
	result := map[string]any{
		"candidate_path": outputPath, "output_path": outputPath, "size_bytes": candidateSize, "duration_sec": duration,
		"video_codec": videoCodec, "resolution": resolution, "bit_depth": bitDepth,
		"profile": getString(ec.State, "profile"), "recipe_version": plan.RecipeVersion, "recipe_digest": plan.RecipeDigest, "plan_digest": plan.PlanDigest,
		"video_profile": plan.VideoProfile, "pixel_format": plan.PixelFormat, "prioritize_speed": plan.PrioritizeSpeed, "spatial_aq": plan.SpatialAQ, "realtime": plan.Realtime,
		"average_bitrate_kbps": plan.AverageBitrateKbps, "max_bitrate_kbps": plan.MaxBitrateKbps, "constant_bitrate": plan.ConstantBitrate,
		"qmin": plan.QMin, "qmax": plan.QMax, "gop_size": plan.GOPSize, "b_frames": plan.BFrames, "closed_gop": plan.ClosedGOP,
		"power_efficient": plan.PowerEfficient, "max_ref_frames": plan.MaxRefFrames, "expected_bit_depth": plan.ExpectedBitDepth,
		"attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count"), "fallback_count": getInt(ec.State, "fallback_count"), "applied_fallbacks": plan.AppliedFallbacks,
	}
	if c := ec.State["conversions"]; c != nil {
		result["conversions"] = c
	}
	bitDepthStatus := "not_requested"
	if plan.ExpectedBitDepth > 0 {
		bitDepthStatus = "ok"
	}
	validation := map[string]any{"duration": "ok", "video_streams": "ok", "resolution": "ok", "bit_depth": bitDepthStatus, "audio_streams": "ok", "subtitle_streams": "ok", "attachments": "ok", "chapters": "ok", "candidate_identity": "ok", "original_sha256_pending": "accept_result"}
	ec.State["validation"] = validation
	ec.State["result"] = result
	ec.State["size_saved_bytes"] = saved
	ec.State["size_saved_percent"] = pct
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"result": result, "validation": validation, "candidate_path": outputPath, "output_path": outputPath, "size_saved_bytes": saved, "size_saved_percent": pct, "profile": getString(ec.State, "profile"), "recipe_version": plan.RecipeVersion, "recipe_digest": plan.RecipeDigest}}, nil
}

// verifyPublishedCandidateLight performs only stat-level checks. It returns the
// observed regular-file size for filesystem candidates (0 for direct-SMB).
//
//   - Filesystem: existence, regular-file identity, non-empty size, and (when
//     the worker attested one) an exact size match.
//   - Direct SMB: the semantic destination is a logical, possibly-unmounted
//     name. It is never stat'd; the worker's hash-verified exclusive publish is
//     the verification. The worker must have attested a positive size.
func (e *Engine) verifyPublishedCandidateLight(ctx context.Context, ec *ExecutionContext, candidatePath string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	p := strings.TrimSpace(candidatePath)
	if p == "" {
		return 0, fmt.Errorf("candidate path is missing (fail closed)")
	}
	expected := getInt64(ec.State, "candidate_size_bytes")
	if strings.EqualFold(getString(ec.State, "storage_backend"), "smb_direct") {
		if expected <= 0 {
			return 0, fmt.Errorf("smb_direct candidate %q has no worker-attested size after verified publish (fail closed)", p)
		}
		return 0, nil
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Size() == 0 {
		return 0, fmt.Errorf("transcoded candidate file %q not accessible or has 0 bytes: %v", p, err)
	}
	if !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("transcoded candidate file %q is not a regular file (fail closed)", p)
	}
	if expected > 0 && fi.Size() != expected {
		return 0, fmt.Errorf("transcoded candidate file %q size %d != worker-attested size %d (fail closed)", p, fi.Size(), expected)
	}
	return fi.Size(), nil
}
