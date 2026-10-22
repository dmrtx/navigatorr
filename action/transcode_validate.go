package action

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
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
			return rejectedCandidateResult(), nil
		}
	}
	if getString(ec.Inputs, "mode") != "" && (strings.EqualFold(ec.Decision, "accept_loss") || strings.EqualFold(ec.Decision, "approve")) {
		return StepResult{Status: StepFailed, Error: "mode policy cannot accept validation loss; original preserved"}, nil
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
	if getString(ec.Inputs, "mode") != "" {
		snapshot, err := freezeModePolicy(ec)
		audioOnly := transcode.IsSizeAudioOnlyPlan(plan)
		if err != nil || snapshot == nil || plan.Mode != snapshot.Mode || plan.PolicyDigest != snapshot.Digest || (plan.QualityValidation == nil) != audioOnly {
			return StepResult{Status: StepFailed, Error: "mode final quality policy is missing or mismatched; original preserved"}, nil
		}
		if audioOnly {
			// The video was copied, so the proof is bit-identical video, not
			// a perceptual score; only a policy that allows it may use it.
			var evidence transcode.FinalQualityEvidence
			encoded, err := json.Marshal(ec.State["quality_evidence"])
			if snapshot.Size == nil || !snapshot.Size.AudioOnlyFallback || transcode.ValidateSizeAudioOnlyShape(plan) != nil || err != nil || json.Unmarshal(encoded, &evidence) != nil || evidence.Verdict != "pass" || evidence.CandidateSHA256 == "" || evidence.CandidateSHA256 != getString(ec.State, "candidate_sha256") || evidence.PlanDigest != plan.PlanDigest || evidence.VideoCopy.Check() != nil {
				return StepResult{Status: StepFailed, Error: "video copy evidence is missing or does not match the candidate and plan; original preserved"}, nil
			}
			// A v4 budget planned audio-only because it fits carries the
			// budget as its ceiling; nothing else carries one.
			if snapshot.Version == ModePolicyV4 {
				if budget, ok := sizeAudioOnlyBudget(snapshot, ec.State); !ok || plan.SizePolicy.MaxCandidateBytes != budget {
					return StepResult{Status: StepFailed, Error: "audio-only plan does not carry the frozen size budget; original preserved"}, nil
				}
			}
		} else {
			expectedQuality := ensureFinalQualityMetric(buildFinalQualityConfig(snapshot.Profile.Optimization.Quality), "vmaf")
			if getString(ec.State, "plan_source") == sizeBudgetPlanSource {
				// A budget encode is held to the frozen floor, and only a plan
				// the budget recipe built may be. The encode is rebuilt from
				// the frozen policy, the source and the record, not read back
				// from the plan, and must match exactly.
				expectedQuality = sizeBudgetFloorQuality(snapshot)
				if expectedQuality == nil || !sizeBudgetPlanMatches(snapshot, ec.State, plan, sizeBudgetOf(ec.State)) {
					return StepResult{Status: StepFailed, Error: "size budget plan does not match the frozen budget; original preserved"}, nil
				}
			} else {
				// An encode is a rung of the frozen recipe, rebuilt from the
				// policy and the source.
				if snapshot.Recipe == nil || getString(ec.State, "plan_source") != qualityLadderPlanSource || !qualityLadderPlanMatches(snapshot, ec.State, plan, QualityLadderOf(ec.State)) {
					return StepResult{Status: StepFailed, Error: "encode does not match the goal's frozen recipe; original preserved"}, nil
				}
			}
			expected, _ := json.Marshal(expectedQuality)
			actual, _ := json.Marshal(plan.QualityValidation.Quality)
			if plan.QualityValidation.Metric != "vmaf" || string(expected) != string(actual) {
				return StepResult{Status: StepFailed, Error: "final quality validation does not enforce frozen mode policy; original preserved"}, nil
			}
		}
	}
	if plan.QualityValidation != nil {
		var evidence struct {
			Verdict                string `json:"verdict"`
			CandidateSHA256        string `json:"candidate_sha256"`
			PlanDigest             string `json:"plan_digest"`
			BenchmarkRequestDigest string `json:"benchmark_request_digest"`
			CompactChain           *struct {
				ChainVersion string `json:"chain_version"`
				TargetWidth  int    `json:"target_width"`
				TargetHeight int    `json:"target_height"`
			} `json:"compact_chain"`
			CompactOutput *struct {
				Verdict string `json:"verdict"`
			} `json:"compact_output"`
		}
		encoded, err := json.Marshal(ec.State["quality_evidence"])
		if err != nil || json.Unmarshal(encoded, &evidence) != nil || evidence.Verdict != "pass" || evidence.CandidateSHA256 == "" || evidence.CandidateSHA256 != getString(ec.State, "candidate_sha256") || evidence.PlanDigest != plan.PlanDigest || evidence.BenchmarkRequestDigest != plan.QualityValidation.BenchmarkRequestDigest {
			return StepResult{Status: StepFailed, Error: "final quality validation evidence is missing or does not match the candidate and plan; original preserved"}, nil
		}
		if c := plan.Compact; c != nil && (evidence.CompactChain == nil || evidence.CompactChain.ChainVersion != c.ChainVersion || evidence.CompactChain.TargetWidth != c.TargetWidth || evidence.CompactChain.TargetHeight != c.TargetHeight || evidence.CompactOutput == nil || evidence.CompactOutput.Verdict != "pass") {
			return StepResult{Status: StepFailed, Error: "compact conversion evidence (reference chain and SDR output check) is missing; original preserved"}, nil
		}
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
	var budget *SizeBudgetRecord
	if plan.Mode != "" && plan.SizePolicy != nil && plan.SizePolicy.MaxCandidateBytes > 0 && candidateSize > plan.SizePolicy.MaxCandidateBytes && !sizeBudgetActive(ec.State) {
		// The worker holds an encode to its ceiling; a file over it is
		// never offered, whoever let it through.
		ec.State["skip_transcode"] = true
		ec.State["reason_code"] = SizeBudgetExceeded
		return StepResult{Status: StepCompleted, Outputs: map[string]any{"skipped": true, "reason_code": SizeBudgetExceeded, "candidate_size_bytes": candidateSize}}, nil
	}
	if plan.Mode != "" {
		original := getInt64(ec.State, "original_size")
		if original <= 0 {
			original = getInt64(stateObject(ec.State, "original"), "size_bytes")
		}
		minimum, maximum := e.effectiveSizeGuardrails(ec)
		if original <= 0 || plan.SizePolicy == nil || plan.Mode != getString(ec.Inputs, "mode") || plan.PolicyDigest != getString(ec.State, "policy_digest") || plan.SizePolicy.MinSavingsPercent != minimum || plan.SizePolicy.MaxSizeIncreasePercent != maximum {
			return StepResult{Status: StepFailed, Error: "mode size policy or source identity missing; original preserved"}, nil
		}
		// A budget encode is checked against its budget first: every budget
		// sits under the 15% minimum saving, so one that misses the saving is
		// over the budget too, and is reported as that.
		if sizeBudgetActive(ec.State) {
			rec, fits := finishSizeBudget(ec, candidateSize)
			if !fits {
				ec.State["skip_transcode"] = true
				ec.State["reason_code"] = SizeBudgetExceeded
				return StepResult{Status: StepCompleted, Outputs: map[string]any{"skipped": true, "reason_code": SizeBudgetExceeded, "candidate_size_bytes": candidateSize, sizeBudgetKey: rec}}, nil
			}
			budget = rec
		}
		if !transcode.ModeSizeAllowedVersion(plan.Mode, original, candidateSize, plan.ModePolicyVersion) {
			ec.State["skip_transcode"] = true
			ec.State["reason_code"] = "insufficient_savings"
			out := map[string]any{"skipped": true, "reason_code": "insufficient_savings", "candidate_size_bytes": candidateSize}
			if getString(ec.State, "plan_source") == qualityLadderPlanSource {
				out[qualityLadderKey] = stopQualityLadder(ec, "insufficient_savings", candidateSize)
			}
			return StepResult{Status: StepCompleted, Outputs: out}, nil
		}
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
			if getBool(ec.State, "batch_shared_validation") {
				return StepResult{Status: StepFailed, Error: reason + "; original preserved"}, nil
			}
			return StepResult{Status: StepWaitingDecision, WaitingReason: reason, WaitingOptions: []WaitingOption{{Decision: "reject", Description: "Reject candidate and keep original"}, {Decision: "accept_loss", Description: "Accept candidate despite validation discrepancy"}}}, nil
		}
	}
	saved := origSize - candidateSize
	pct := float64(0)
	if origSize > 0 {
		pct = float64(saved) / float64(origSize) * 100
	}
	if getBool(ec.State, "batch_shared_validation") {
		minSavings, _ := e.effectiveSizeGuardrails(ec)
		if pct < minSavings {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("calibrated candidate saved %.1f%%, below required %.1f%%; original preserved", pct, minSavings)}, nil
		}
	}
	// Populate the result/validation maps from the immutable plan and the
	// original report. The candidate's own stream attributes were validated
	// worker-local before publish; the coordinator reports the expected target
	// attributes rather than re-probing over NAS.
	videoCodec := expectedVideoCodec(plan.VideoCodec)
	if transcode.IsSizeAudioOnlyPlan(plan) {
		videoCodec = normCodec(getString(origMap, "video_codec"))
	}
	resolution := getString(origMap, "resolution")
	if c := plan.Compact; c != nil {
		resolution = fmt.Sprintf("%dx%d", c.TargetWidth, c.TargetHeight)
	}
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
	outputs := map[string]any{"result": result, "validation": validation, "candidate_path": outputPath, "output_path": outputPath, "size_saved_bytes": saved, "size_saved_percent": pct, "profile": getString(ec.State, "profile"), "recipe_version": plan.RecipeVersion, "recipe_digest": plan.RecipeDigest}
	if budget != nil {
		result["size_budget_outcome"] = budget.Outcome
		outputs[sizeBudgetKey] = budget
	}
	if getString(ec.State, "plan_source") == qualityLadderPlanSource {
		if rec := passQualityLadder(ec, candidateSize); rec != nil {
			outputs[qualityLadderKey] = rec
		}
	}
	return StepResult{Status: StepCompleted, Outputs: outputs}, nil
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
