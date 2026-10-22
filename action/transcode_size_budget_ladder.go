package action

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// With the media engine allowed, a size budget does not search. VideoToolbox
// encodes a whole episode in minutes and, at the budget, always cleared the
// quality floor in measurements, so the file is encoded in full at a short
// ladder of VideoToolbox settings, best picture first: constant quality at each
// of the frozen values, then the budget's average bitrate. Every rung's plan
// carries the budget as an absolute ceiling, so the worker refuses an
// over-budget file right after the encode, before it spends minutes scoring it.
// The first rung that fits is validated against the quality floor, and only
// that one is scored.
//
// A rung is a function of the frozen policy, the source and the record's Rung,
// so validation rebuilds it and needs no other evidence.

const (
	// The trigger and encoder choice a ladder record carries.
	sizeBudgetTriggerBudgetFirst = "budget_first"
	SizeBudgetChoiceLadder       = "videotoolbox_ladder"

	// sizeBudgetRungOverKind marks a rung that came out over the budget.
	sizeBudgetRungOverKind = "budget_rung_over"
	// sizeBudgetExceededReason is the worker's verdict on a candidate over the
	// plan's byte ceiling.
	sizeBudgetExceededReason = "size_budget_exceeded"
)

// sizeBudgetIsLadder reports a record planned by the ladder.
func sizeBudgetIsLadder(rec *SizeBudgetRecord) bool {
	return rec != nil && rec.Choice != nil && rec.Choice.Reason == SizeBudgetChoiceLadder
}

// sizeBudgetLadderOf is the ladder record in state, nil when the budget is not
// running a ladder.
func sizeBudgetLadderOf(state map[string]any) *SizeBudgetRecord {
	if rec := sizeBudgetOf(state); sizeBudgetIsLadder(rec) {
		return rec
	}
	return nil
}

// sizeBudgetLadderVideo is the video of one rung: VideoToolbox at the rung's
// constant quality, or, one past the last, at the budget's average bitrate.
func sizeBudgetLadderVideo(policy *SizeBudgetPolicy, hw *transcode.HardwareSearchRules, bitDepth int, enc sizeBudgetEncode, rung int) (sizeBudgetVideoSettings, error) {
	if policy == nil || hw == nil || len(policy.VTLadder) == 0 || rung < 0 || rung > len(policy.VTLadder) {
		return sizeBudgetVideoSettings{}, fmt.Errorf("the frozen policy has no VideoToolbox rung %d", rung)
	}
	if rung == len(policy.VTLadder) {
		return sizeBudgetVTVideo(hw, bitDepth, enc.X265.BitrateKbps), nil
	}
	v := sizeBudgetVTVideo(hw, bitDepth, 0)
	v.Quality = policy.VTLadder[rung]
	return v, nil
}

// sizeBudgetRungDescription names a rung's rate control in words.
func sizeBudgetRungDescription(p *transcode.Plan) string {
	if p.Quality > 0 {
		return fmt.Sprintf("quality %d", p.Quality)
	}
	return fmt.Sprintf("an average bitrate of %d kbps", p.AverageBitrateKbps)
}

// sizeBudgetLadderMessage says which rung is encoding.
func sizeBudgetLadderMessage(rec *SizeBudgetRecord, policy *SizeBudgetPolicy, plan *transcode.Plan) string {
	from := ""
	if rec.Siblings != nil {
		from = " " + rec.Siblings.Message
	}
	return fmt.Sprintf("Encoding with VideoToolbox at %s (step %d of %d) to fit the %s budget.%s%s", sizeBudgetRungDescription(plan), rec.Rung+1, len(policy.VTLadder)+1, formatBudgetBytes(rec.BudgetBytes), sizeBudgetAudioSentence(rec.Audio), from)
}

// newSizeBudgetRecord is a budget record before any encoder is chosen: the
// numbers every budget encode shares.
func newSizeBudgetRecord(snapshot *ModePolicySnapshot, rep *mediainspect.DetailedReport, original, budget int64, siblings *SiblingBudgetEvidence, enc sizeBudgetEncode) *SizeBudgetRecord {
	policy := snapshot.Size.SizeBudget
	if policy.Siblings == nil {
		siblings = nil
	}
	rec := &SizeBudgetRecord{Trigger: sizeBudgetTriggerNoWinner, SourceBytes: original, BudgetBytes: budget, BudgetPercent: policy.Percent, Siblings: siblings, DurationSeconds: rep.DurationSec,
		Floor: QualityLimits{VMAFMean: &policy.Floor.VMAFMean, VMAFP5: &policy.Floor.VMAFP5, VMAFWorstWindow: &policy.Floor.VMAFWorstWindow}}
	if limits := qualityLimitsOf(ensureFinalQualityMetric(buildFinalQualityConfig(snapshot.Profile.Optimization.Quality), "vmaf")); limits != nil {
		rec.Target = *limits
		rec.Floor.WorstWindowSeconds, rec.Floor.CAMBIMean, rec.Floor.CAMBIPeak = limits.WorstWindowSeconds, limits.CAMBIMean, limits.CAMBIPeak
	}
	rec.NonVideoBytes, rec.TargetBytes, rec.VideoBitrateKbps, rec.Audio = enc.NonVideoBytes, enc.TargetBytes, enc.X265.BitrateKbps, enc.Audio
	return rec
}

// startSizeBudgetLadder plans a size budget goal's first encode: the ladder's
// first rung when the media engine is allowed and the worker has it, otherwise
// one two-pass x265 encode at the budget. An audio-only result that fits the
// budget is planned directly, and a budget that cannot be encoded ends the
// action with the reason. It reports false when the goal has no size budget
// recipe.
func (e *Engine) startSizeBudgetLadder(ctx context.Context, ec *ExecutionContext, cleanPath string) (StepResult, bool) {
	if ec.ActionName != "transcode_media" || getString(ec.Inputs, "mode") != "size" {
		return StepResult{}, false
	}
	snapshot, err := freezeModePolicy(ec)
	if err != nil || snapshot == nil || snapshot.Size == nil || snapshot.Size.SizeBudget == nil || snapshot.Recipe == nil || snapshot.Recipe.Kind != RecipeSizeBudget {
		return StepResult{}, false
	}
	if sizeBudgetOf(ec.State) != nil {
		return StepResult{Status: StepCompleted}, true // planned already; this step ran again
	}
	policy := snapshot.Size.SizeBudget
	if policy.Validate() != nil {
		return StepResult{Status: StepFailed, Error: "frozen size budget is invalid (fail closed)"}, true
	}
	caps, err := e.deps.Transcode.Capabilities(ctx)
	if err != nil {
		return workerCapabilityLookupResult(err, "size budget VideoToolbox"), true
	}
	hardware := len(policy.VTLadder) > 0 && snapshot.Size.Hardware != nil && caps.Encoders[transcode.VideoCodecHEVCVideoToolbox]
	// Planning, advancing and validating a rung all read source_report.
	rep := getSourceReport(ec.State["source_report"])
	opt := getOptimizationPolicy(ec.State["optimization_policy"])
	if rep == nil || opt == nil || !opt.Enabled {
		return StepResult{Status: StepFailed, Error: "media inspection report or goal policy is missing from state (fail closed)"}, true
	}
	// decline ends the action with the reason: there is nothing else to run.
	decline := func(reason, message string) (StepResult, bool) {
		message += " Original preserved."
		ec.State[sizeBudgetSkipReasonKey], ec.State[sizeBudgetSkipMessageKey] = reason, message
		ec.State["skip_transcode"], ec.State["reason_code"] = true, reason
		return StepResult{Status: StepCompleted, Outputs: map[string]any{"skipped": true, "reason_code": reason, "mode": snapshot.Mode, "policy_digest": snapshot.Digest,
			sizeBudgetSkipReasonKey: reason, sizeBudgetSkipMessageKey: message, "plan_note": message}}, true
	}
	original := getInt64(ec.State, "original_size")
	siblings := siblingEvidenceOf(ec.State)
	budget := policy.BudgetBytes(original, siblings)
	if budget <= 0 {
		message := "The other episodes gave no size budget for this file."
		if siblings != nil && siblings.Message != "" {
			message = siblings.Message
		}
		return decline("size_budget_unavailable", message)
	}
	if policy.Audio != nil && !caps.SizeAudioLevels {
		return decline("size_budget_worker_unsupported", "The size budget needs a worker that converts audio per track to AAC and HE-AAC; upgrade the worker to use it.")
	}
	if snapshot.Size.AudioOnlyFallback && caps.SizeAudioOnly {
		if audioMode, _, fits := sizeBudgetAudioOnlyLevel(policy, rep, original, budget); fits {
			// Copying the video loses no picture quality, and also answers a
			// budget too small for any video bitrate.
			return e.startSizeBudgetAudioOnly(ctx, ec, budget, audioMode)
		}
	}
	enc := planSizeBudgetEncode(policy, rep, original, budget)
	if enc.Reason != "" {
		return decline(enc.Reason, enc.Message)
	}
	if !hardware && (!caps.X265TwoPass || !caps.Encoders[transcode.VideoCodecLibX265]) {
		return decline("size_budget_worker_unsupported", "The size budget needs a worker that can encode two-pass x265; upgrade the worker to use it.")
	}
	// The windows and limits final validation judges every encode on.
	codec := transcode.VideoCodecLibX265
	if hardware {
		codec = transcode.VideoCodecHEVCVideoToolbox
	}
	req, err := buildFinalValidationRequest(ec, cleanPath, rep, opt, caps, codec)
	if err != nil {
		return modeAttention(ec, "quality_policy_unavailable", err.Error()+"; original preserved"), true
	}
	digest, err := transcode.DigestBenchmarkRequest(req)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("computing final validation request digest: %v", err)}, true
	}
	ec.State["benchmark_request"], ec.State["benchmark_request_digest"] = req, digest

	rec := newSizeBudgetRecord(snapshot, rep, original, budget, siblings, enc)
	rec.Trigger = sizeBudgetTriggerBudgetFirst
	rec.Rung = 0
	switch {
	case hardware:
		rec.Choice = &SizeBudgetChoice{VideoCodec: transcode.VideoCodecHEVCVideoToolbox, Reason: SizeBudgetChoiceLadder,
			Message: fmt.Sprintf("VideoToolbox encodes the whole file at quality %s, then at the budget's average bitrate, and the first result within the budget is checked against the quality floor.", joinLadder(policy.VTLadder))}
	case snapshot.Size.Hardware == nil:
		rec.Choice = &SizeBudgetChoice{VideoCodec: transcode.VideoCodecLibX265, Reason: SizeBudgetChoiceHardwareOff, Message: "This goal does not allow the media engine (allow_hardware=false), so the whole file is encoded once with two-pass x265 at the budget."}
	default:
		rec.Choice = &SizeBudgetChoice{VideoCodec: transcode.VideoCodecLibX265, Reason: SizeBudgetChoiceWorkerUnsupported, Message: "The worker cannot encode with VideoToolbox, so the whole file is encoded once with two-pass x265 at the budget."}
	}
	outputs := map[string]any{"benchmark_request_digest": digest}
	return e.commitSizeBudgetPlan(ec, snapshot, enc, rec, outputs)
}

// startSizeBudgetAudioOnly plans a v4 budget whose lossless audio alone fits:
// the video is copied and only the audio is compacted. The fit is an
// estimate, so the plan carries the budget as its byte ceiling, as a ladder
// rung does, and the worker refuses a larger file.
func (e *Engine) startSizeBudgetAudioOnly(ctx context.Context, ec *ExecutionContext, budget int64, audioMode string) (StepResult, bool) {
	outputs := map[string]any{}
	res, handled := e.sizeAudioOnlyFallback(ctx, ec, outputs, audioMode)
	if !handled {
		return StepResult{Status: StepFailed, Error: "audio-only encode could not be planned for the size budget (fail closed)"}, true
	}
	if res.Status == StepCompleted && getString(ec.State, "plan_source") == "audio_only" {
		plan, err := sizeAudioOnlyBudgetPlan(getPlan(ec.State["plan"]), budget)
		if err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("audio-only encode for the size budget: %v", err)}, true
		}
		ec.State["plan"], ec.State["plan_digest"] = plan, plan.PlanDigest
		res.Outputs["plan"], res.Outputs["plan_digest"] = plan, plan.PlanDigest
		res.Outputs["plan_note"] = sizeBudgetAudioOnlyNote(plan)
	}
	return res, true
}

// sizeAudioOnlyBudgetPlan is an audio-only plan held to a size budget.
func sizeAudioOnlyBudgetPlan(base *transcode.Plan, budget int64) (*transcode.Plan, error) {
	if base == nil || base.SizePolicy == nil || budget <= 0 {
		return nil, fmt.Errorf("the audio-only plan or the budget is missing (fail closed)")
	}
	p := *base
	sp := *base.SizePolicy
	sp.MaxCandidateBytes = budget
	p.SizePolicy = &sp
	p.PlanDigest = ""
	d, err := transcode.DigestPlan(&p)
	if err != nil {
		return nil, err
	}
	p.PlanDigest = d
	return &p, transcode.ValidateSizeAudioOnlyShape(&p)
}

// sizeAudioOnlyBudget is the byte ceiling a v4 audio-only encode must carry:
// the frozen budget for a size budget goal, none otherwise. ok is false when
// the budget cannot be worked out again.
func sizeAudioOnlyBudget(snapshot *ModePolicySnapshot, state map[string]any) (int64, bool) {
	if snapshot.Recipe == nil || snapshot.Recipe.Kind != RecipeSizeBudget || snapshot.Size == nil || snapshot.Size.SizeBudget == nil {
		return 0, true
	}
	budget := snapshot.Size.SizeBudget.BudgetBytes(getInt64(state, "original_size"), siblingEvidenceOf(state))
	return budget, budget > 0
}

func joinLadder(values []int) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprint(v)
	}
	return joinWords(parts)
}

// advanceSizeBudgetRung moves a ladder to its next rung after the worker
// refused the current rung's file as over the budget. It reports false when the
// action is not running a ladder or the last rung was the one refused; the
// caller then ends the budget as exceeded.
func (e *Engine) advanceSizeBudgetRung(ec *ExecutionContext, candidateBytes int64) (StepResult, bool) {
	rec := sizeBudgetLadderOf(ec.State)
	plan := getPlan(ec.State["plan"])
	if rec == nil || plan == nil {
		return StepResult{}, false
	}
	snapshot, err := freezeModePolicy(ec)
	if err != nil || snapshot == nil || snapshot.Size == nil || snapshot.Size.SizeBudget == nil || rec.Rung >= len(snapshot.Size.SizeBudget.VTLadder) {
		return StepResult{}, false
	}
	policy := snapshot.Size.SizeBudget
	rep := getSourceReport(ec.State["source_report"])
	enc := planSizeBudgetEncode(policy, rep, getInt64(ec.State, "original_size"), rec.BudgetBytes)
	if enc.Reason != "" {
		return StepResult{Status: StepFailed, Error: "size budget encode can no longer be planned for the next VideoToolbox step (fail closed)"}, true
	}
	over := SizeBudgetPoint{Kind: sizeBudgetRungOverKind, VideoCodec: transcode.VideoCodecHEVCVideoToolbox, Quality: plan.Quality, BitrateKbps: plan.AverageBitrateKbps, Bytes: candidateBytes, Measured: true}
	was := sizeBudgetRungDescription(plan)
	rec.Rung++
	video, err := sizeBudgetVideo(snapshot, rep, plan, enc, rec)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("building the next size budget step: %v", err)}, true
	}
	next, err := e.sizeBudgetPlan(ec, snapshot, video, rec)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("building the next size budget step: %v", err)}, true
	}
	curve := rec.Curve[:0:0]
	for _, p := range rec.Curve {
		if p.Kind != "budget_fallback" {
			curve = append(curve, p)
		}
	}
	rec.Curve = append(curve, over, SizeBudgetPoint{Kind: "budget_fallback", VideoCodec: transcode.VideoCodecHEVCVideoToolbox, Quality: next.Quality, BitrateKbps: next.AverageBitrateKbps, Bytes: rec.TargetBytes, WithinBudget: true})
	size := "over the budget"
	if candidateBytes > 0 {
		size = fmt.Sprintf("%s, over the %s budget", formatBudgetBytes(candidateBytes), formatBudgetBytes(rec.BudgetBytes))
	}
	rec.Message = fmt.Sprintf("VideoToolbox at %s came out %s; encoding at %s.", was, size, sizeBudgetRungDescription(next))
	appendFailureHistory(ec, "size_budget", "policy_rejected", rec.Message)
	for _, key := range transcodeAttemptKeys {
		delete(ec.State, key)
		delete(ec.Outputs, key)
	}
	delete(ec.State, "skip_transcode")
	delete(ec.State, "reason_code")
	previous := getString(ec.State, "job_id")
	jobID := fmt.Sprintf("job-%s-budget-r%d", ec.InstanceID, rec.Rung)
	ec.State[sizeBudgetKey] = rec
	ec.State["plan"], ec.State["plan_digest"] = next, next.PlanDigest
	ec.State["previous_job_id"], ec.State["job_id"], ec.State["external_reference"] = previous, jobID, jobID
	ec.State["attempt"], ec.State["retry_count"], ec.State["fallback_count"] = 1, 0, 0
	mergeMap(ec.Outputs, map[string]any{"plan": next, "plan_digest": next.PlanDigest, "job_id": jobID, "external_reference": jobID, "previous_job_id": previous})
	return StepResult{Status: StepCompleted, RewindTo: "submit_transcode", Outputs: map[string]any{sizeBudgetKey: rec, "plan_note": rec.Message}}, true
}

// sizeBudgetLadderPlanMatches reports whether plan is exactly the ladder rung
// the record names, rebuilt from the frozen policy and the source, and carries
// the budget as its byte ceiling.
func sizeBudgetLadderPlanMatches(snapshot *ModePolicySnapshot, rep *mediainspect.DetailedReport, plan *transcode.Plan, rec *SizeBudgetRecord, enc sizeBudgetEncode, budget int64) bool {
	policy := snapshot.Size.SizeBudget
	if len(policy.VTLadder) == 0 || policy.Comparison != nil || snapshot.Size.Hardware == nil || rec.Correction != nil || rec.Choice.VideoCodec != transcode.VideoCodecHEVCVideoToolbox {
		return false
	}
	if plan.SizePolicy == nil || plan.SizePolicy.MaxCandidateBytes != budget {
		return false
	}
	want, err := sizeBudgetLadderVideo(policy, snapshot.Size.Hardware, max(sourceVideoBitDepth(rep), plan.ExpectedBitDepth), enc, rec.Rung)
	if err != nil {
		return false
	}
	return sizeBudgetVideoJSON(want) == sizeBudgetVideoJSON(sizeBudgetVideoOf(plan))
}

func sizeBudgetVideoJSON(v sizeBudgetVideoSettings) string {
	b, _ := json.Marshal(v)
	return string(b)
}
