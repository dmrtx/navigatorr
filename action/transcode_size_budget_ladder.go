package action

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
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
	return fmt.Sprintf("Encoding with VideoToolbox at %s (step %d of %d) to fit the %s budget.%s", sizeBudgetRungDescription(plan), rec.Rung+1, len(policy.VTLadder)+1, formatBudgetBytes(rec.BudgetBytes), from)
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
	if limits := qualityLimitsOf(ensureBenchmarkQualityMetric(buildBenchmarkQualityConfig(snapshot.Profile.Optimization.Quality), "vmaf")); limits != nil {
		rec.Target = *limits
		rec.Floor.WorstWindowSeconds, rec.Floor.CAMBIMean, rec.Floor.CAMBIPeak = limits.WorstWindowSeconds, limits.CAMBIMean, limits.CAMBIPeak
	}
	rec.NonVideoBytes, rec.TargetBytes, rec.VideoBitrateKbps = enc.NonVideoBytes, enc.TargetBytes, enc.X265.BitrateKbps
	return rec
}

// startSizeBudgetLadder replaces the search with the ladder when the action is
// a size goal with a budget that allows the media engine and the worker can
// encode with it. It plans the first rung and reports handled; the wait step
// then has no benchmark to wait for. It reports false, leaving the search to
// run as it does without a ladder, when any condition fails or no encode at the
// budget can be planned; in the last case the reason is recorded beside the
// search's own outcome.
//
// Under policy v4 the budget's recipe always replaces the search, which it no
// longer has to fall back to: without the media engine (or a worker that has
// it) the one encode is two-pass x265 at the budget, an audio-only result that
// fits the budget is planned directly, and a budget that cannot be encoded ends
// the action with the reason.
func (e *Engine) startSizeBudgetLadder(ctx context.Context, ec *ExecutionContext, cleanPath string) (StepResult, bool) {
	if sizeBudgetLadderOf(ec.State) != nil {
		return StepResult{Status: StepCompleted}, true // planned already; this step ran again
	}
	if ec.ActionName != "transcode_media" || getString(ec.Inputs, "mode") != "size" || getString(ec.State, "benchmark_job_id") != "" {
		return StepResult{}, false
	}
	snapshot, err := freezeModePolicy(ec)
	if err != nil || snapshot == nil || snapshot.Size == nil || snapshot.Size.SizeBudget == nil {
		return StepResult{}, false
	}
	recipe := snapshot.Recipe != nil && snapshot.Recipe.Kind == RecipeSizeBudget
	if recipe && sizeBudgetOf(ec.State) != nil {
		return StepResult{Status: StepCompleted}, true // planned already; this step ran again
	}
	if !recipe && (snapshot.Version != optimization.ModePolicyV3 || len(snapshot.Size.SizeBudget.VTLadder) == 0 || snapshot.Size.Hardware == nil) {
		return StepResult{}, false
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
	if !hardware && !recipe {
		return StepResult{}, false
	}
	// Planning, advancing and validating a rung all read source_report.
	rep := getSourceReport(ec.State["source_report"])
	opt := getOptimizationPolicy(ec.State["optimization_policy"])
	if rep == nil || opt == nil || !opt.Enabled {
		if recipe {
			return StepResult{Status: StepFailed, Error: "media inspection report or goal policy is missing from state (fail closed)"}, true
		}
		return StepResult{}, false
	}
	// decline leaves the budget to the search under v3. A recipe has no
	// search: the action ends with the reason instead.
	decline := func(reason, message string) (StepResult, bool) {
		if recipe {
			message += " Original preserved."
			ec.State[sizeBudgetSkipReasonKey], ec.State[sizeBudgetSkipMessageKey] = reason, message
			ec.State["skip_transcode"], ec.State["reason_code"] = true, reason
			return StepResult{Status: StepCompleted, Outputs: map[string]any{"skipped": true, "reason_code": reason, "mode": snapshot.Mode, "policy_digest": snapshot.Digest,
				sizeBudgetSkipReasonKey: reason, sizeBudgetSkipMessageKey: message, "plan_note": message}}, true
		}
		message += " The search runs as usual."
		ec.State[sizeBudgetSkipReasonKey], ec.State[sizeBudgetSkipMessageKey] = reason, message
		return StepResult{}, false
	}
	original := getInt64(ec.State, "original_size")
	siblings := siblingEvidenceOf(ec.State)
	budget := policy.BudgetBytes(original, siblings)
	if budget <= 0 {
		if recipe {
			message := "The other episodes gave no size budget for this file."
			if siblings != nil && siblings.Message != "" {
				message = siblings.Message
			}
			return decline("size_budget_unavailable", message)
		}
		return StepResult{}, false // the siblings gave no budget; the evidence says why
	}
	audioFits := false
	if snapshot.Size.AudioOnlyFallback && caps.SizeAudioOnly {
		est := estimateAudioOnly(rep, original)
		audioFits = est.Compacted > 0 && est.CandidateBytes <= budget
	}
	if recipe && audioFits {
		// Copying the video loses no picture quality, and also answers a
		// budget too small for any video bitrate.
		return e.startSizeBudgetAudioOnly(ctx, ec)
	}
	enc := planSizeBudgetEncode(policy, rep, original, budget)
	if enc.Reason != "" {
		return decline(enc.Reason, enc.Message)
	}
	if audioFits {
		return decline("size_budget_audio_only_fits", "Copying the video and compacting the lossless audio alone fits the budget.")
	}
	if !hardware && (!caps.X265TwoPass || !caps.Encoders[transcode.VideoCodecLibX265]) {
		return decline("size_budget_worker_unsupported", "The size budget needs a worker that can encode two-pass x265; upgrade the worker to use it.")
	}
	// The windows final validation measures come from the request the search
	// would have made; it is never submitted.
	req, err := buildBenchmarkRequest(ec, cleanPath, rep, opt, caps)
	if err != nil {
		if recipe {
			return modeAttention(ec, "quality_policy_unavailable", err.Error()+"; original preserved"), true
		}
		return StepResult{}, false
	}
	digest, err := transcode.DigestBenchmarkRequest(req)
	if err != nil {
		if recipe {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("computing final validation request digest: %v", err)}, true
		}
		return StepResult{}, false
	}
	ec.State["benchmark_request"], ec.State["benchmark_request_digest"] = req, digest

	rec := newSizeBudgetRecord(snapshot, rep, original, budget, siblings, enc)
	rec.Trigger = sizeBudgetTriggerBudgetFirst
	rec.Rung = 0
	switch {
	case hardware:
		rec.Choice = &SizeBudgetChoice{VideoCodec: transcode.VideoCodecHEVCVideoToolbox, Reason: SizeBudgetChoiceLadder,
			Message: fmt.Sprintf("The search is skipped: VideoToolbox encodes the whole file at quality %s, then at the budget's average bitrate, and the first result within the budget is checked against the quality floor.", joinLadder(policy.VTLadder))}
	case snapshot.Size.Hardware == nil:
		rec.Choice = &SizeBudgetChoice{VideoCodec: transcode.VideoCodecLibX265, Reason: SizeBudgetChoiceHardwareOff, Message: "This goal does not allow the media engine (allow_hardware=false), so the whole file is encoded once with two-pass x265 at the budget."}
	default:
		rec.Choice = &SizeBudgetChoice{VideoCodec: transcode.VideoCodecLibX265, Reason: SizeBudgetChoiceWorkerUnsupported, Message: "The worker cannot encode with VideoToolbox, so the whole file is encoded once with two-pass x265 at the budget."}
	}
	outputs := map[string]any{"benchmark_done": true, "has_winner": false, "benchmark_request_digest": digest}
	ec.State["benchmark_done"], ec.State["has_winner"] = true, false
	return e.commitSizeBudgetPlan(ec, snapshot, enc, rec, outputs)
}

// startSizeBudgetAudioOnly plans a v4 budget whose lossless audio alone fits:
// the video is copied and only the audio is compacted.
func (e *Engine) startSizeBudgetAudioOnly(ctx context.Context, ec *ExecutionContext) (StepResult, bool) {
	outputs := map[string]any{"benchmark_done": true, "has_winner": false}
	ec.State["benchmark_done"], ec.State["has_winner"] = true, false
	res, handled := e.sizeAudioOnlyFallback(ctx, ec, outputs)
	if !handled {
		return StepResult{Status: StepFailed, Error: "audio-only encode could not be planned for the size budget (fail closed)"}, true
	}
	if res.Status == StepCompleted && getString(ec.State, "plan_source") == "audio_only" {
		res.Outputs["plan_note"] = "Copying the video and compacting the lossless audio alone fits the budget, so the video is copied unchanged and only the lossless audio is compacted."
	}
	return res, true
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
