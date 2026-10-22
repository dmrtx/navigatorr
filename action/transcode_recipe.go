package action

import (
	"context"
	"fmt"
	"slices"

	"github.com/jakenesler/navigatorr/transcode"
)

// Policy v4 replaces the CRF sample search with a fixed recipe for every goal.
// The search ended with nothing on most sources and overrated the final score;
// the full-file check is the one judge that held. So, like the size budget's
// VideoToolbox ladder (transcode_size_budget_ladder.go), every goal encodes the
// whole file at a short ladder of frozen settings and lets the final check
// decide:
//
//   - a size budget runs the budget ladder (VideoToolbox, media engine allowed)
//     or one two-pass x265 encode at the budget;
//   - every other goal starts at the most compressed rung and, when the final
//     check fails on quality, encodes again one rung better. The first rung
//     that passes is kept. A rung refused for its size stops the ladder: a
//     better rung only makes the file larger.
//
// A rung is a function of the frozen policy, the source and the record's Rung,
// so validation rebuilds it and needs no other evidence. The worker sees the
// plan under the version of the policy the content came from (v3 for size, v2
// for the rest), so it needs no change.

// ModePolicyV4 freezes new operations of every goal.
const ModePolicyV4 = "transcode-modes-v4"

const (
	// The two kinds of recipe a v4 snapshot freezes.
	RecipeQualityLadder = "quality_ladder"
	RecipeSizeBudget    = "size_budget"

	// qualityLadderKey holds the QualityLadderRecord; its plans carry
	// qualityLadderPlanSource.
	qualityLadderKey        = "quality_ladder"
	qualityLadderPlanSource = "quality_ladder"

	// What a quality ladder record ends with besides a worker reason code.
	QualityLadderEncoding = "encoding"
	QualityLadderPassed   = "passed"
)

// The rungs of each goal, most compressed first. x265 values are CRF (lower is
// better), VideoToolbox values constant quality (higher is better).
var (
	recipeSizeVTValues   = []int{60, 65, 70}
	recipeSizeX265Values = []int{22, 19, 16}
	recipeQualityValues  = []int{20, 17, 14}
	recipePreserveValues = []int{24, 21, 18}
	recipeCompactValues  = []int{18, 16, 14}
)

// recipeX265Preset is the ladders' x265 preset; size goals that opt in with
// allow_slow_preset use recipeX265SlowPreset.
const (
	recipeX265Preset     = "medium"
	recipeX265SlowPreset = "slow"
)

// ModeRecipe is the frozen recipe of a v4 snapshot.
type ModeRecipe struct {
	Kind       string `json:"kind"`
	VideoCodec string `json:"video_codec"`
	// Preset and QualityValues are a quality ladder's. A size budget's
	// encodes are frozen in the size policy's SizeBudget.
	Preset        string `json:"preset,omitempty"`
	QualityValues []int  `json:"quality_values,omitempty"`
	// Fallback is the x265 ladder a VideoToolbox ladder runs instead on a
	// worker that cannot encode with VideoToolbox, as a VideoToolbox size
	// budget falls back to its x265 encode.
	Fallback *ModeRecipe `json:"fallback,omitempty"`
}

// modeRecipeFor is the recipe a v4 snapshot of this content freezes.
func modeRecipeFor(p *ModePolicySnapshot, allowSlow bool) *ModeRecipe {
	preset := recipeX265Preset
	if allowSlow {
		preset = recipeX265SlowPreset
	}
	x265 := func(values []int) *ModeRecipe {
		return &ModeRecipe{Kind: RecipeQualityLadder, VideoCodec: transcode.VideoCodecLibX265, Preset: preset, QualityValues: slices.Clone(values)}
	}
	switch p.Mode {
	case "size":
		if p.Size == nil {
			return nil
		}
		if p.Size.SizeBudget != nil {
			codec := transcode.VideoCodecLibX265
			if len(p.Size.SizeBudget.VTLadder) > 0 && p.Size.Hardware != nil {
				codec = transcode.VideoCodecHEVCVideoToolbox
			}
			return &ModeRecipe{Kind: RecipeSizeBudget, VideoCodec: codec}
		}
		if p.Size.Hardware != nil {
			return &ModeRecipe{Kind: RecipeQualityLadder, VideoCodec: transcode.VideoCodecHEVCVideoToolbox, QualityValues: slices.Clone(recipeSizeVTValues), Fallback: x265(recipeSizeX265Values)}
		}
		return x265(recipeSizeX265Values)
	case "quality":
		return x265(recipeQualityValues)
	case "x265_preserve":
		return x265(recipePreserveValues)
	case "compact":
		return x265(recipeCompactValues)
	}
	return nil
}

// validate checks a recipe against the snapshot that froze it. A nil recipe is
// valid here; the caller checks that v4, and only v4, has one.
func (r *ModeRecipe) validate(p *ModePolicySnapshot) error {
	if r == nil {
		return nil
	}
	if p.Version != ModePolicyV4 {
		return fmt.Errorf("only policy v4 freezes a recipe")
	}
	switch r.Kind {
	case RecipeSizeBudget:
		if p.Mode != "size" || p.Size == nil || p.Size.SizeBudget == nil || r.Preset != "" || len(r.QualityValues) != 0 || r.Fallback != nil {
			return fmt.Errorf("a size budget recipe needs a size budget and nothing else")
		}
		hardware := len(p.Size.SizeBudget.VTLadder) > 0 && p.Size.Hardware != nil
		if r.VideoCodec != transcode.VideoCodecLibX265 && r.VideoCodec != transcode.VideoCodecHEVCVideoToolbox || (r.VideoCodec == transcode.VideoCodecHEVCVideoToolbox) != hardware {
			return fmt.Errorf("the size budget recipe's encoder does not match the policy")
		}
		return nil
	case RecipeQualityLadder:
	default:
		return fmt.Errorf("unknown recipe kind %q", r.Kind)
	}
	if len(r.QualityValues) == 0 || (p.Mode == "size") != (p.Size != nil) || p.Size != nil && p.Size.SizeBudget != nil {
		return fmt.Errorf("a quality ladder needs rungs and no size budget")
	}
	switch r.VideoCodec {
	case transcode.VideoCodecHEVCVideoToolbox:
		if p.Mode != "size" || p.Size.Hardware == nil || r.Preset != "" {
			return fmt.Errorf("a VideoToolbox ladder needs a size goal that allows the media engine")
		}
		for i, q := range r.QualityValues {
			if q < 1 || q > 100 || i > 0 && q <= r.QualityValues[i-1] {
				return fmt.Errorf("VideoToolbox ladder values must rise within 1..100")
			}
		}
		if f := r.Fallback; f != nil {
			if f.Kind != RecipeQualityLadder || f.VideoCodec != transcode.VideoCodecLibX265 || f.Fallback != nil {
				return fmt.Errorf("a VideoToolbox ladder falls back only to one x265 ladder")
			}
			if err := f.validate(p); err != nil {
				return fmt.Errorf("fallback: %w", err)
			}
		}
	case transcode.VideoCodecLibX265:
		if r.Fallback != nil {
			return fmt.Errorf("an x265 ladder has no fallback")
		}
		if !transcode.IsValidLibX265Preset(r.Preset) || r.Preset == recipeX265SlowPreset && p.Mode != "size" {
			return fmt.Errorf("invalid x265 ladder preset %q", r.Preset)
		}
		for i, q := range r.QualityValues {
			if q < transcode.LibX265CRFMin || q > transcode.LibX265CRFMax || i > 0 && q >= r.QualityValues[i-1] {
				return fmt.Errorf("x265 ladder CRF values must fall within %d..%d", transcode.LibX265CRFMin, transcode.LibX265CRFMax)
			}
		}
	default:
		return fmt.Errorf("unknown ladder encoder %q", r.VideoCodec)
	}
	return nil
}

// workerVersion is the policy version the worker sees in plans and requests.
// A v4 snapshot keeps the content of the version it extends, so the worker
// applies the same rules to it.
func (p *ModePolicySnapshot) workerVersion() string {
	if p == nil || p.Version != ModePolicyV4 {
		if p == nil {
			return ""
		}
		return p.Version
	}
	if p.Mode == "size" {
		// A size budget that froze audio levels is policy v5; every other size
		// goal, and budgets frozen before the levels, stay v3.
		if p.Size != nil && p.Size.SizeBudget != nil && p.Size.SizeBudget.Audio != nil {
			return transcode.ModePolicyV5
		}
		return SizeModePolicyVersion
	}
	return ModePolicyVersion
}

// policyRecipe reads the recipe of a frozen policy without verifying its
// digest (that happens when the policy executes), nil when it has none.
func policyRecipe(policy any) *ModeRecipe {
	var snapshot ModePolicySnapshot
	if policy == nil || !reencode(policy, &snapshot) || snapshot.Version != ModePolicyV4 {
		return nil
	}
	return snapshot.Recipe
}

// QualityLadderRecord is a quality ladder in progress or finished: the frozen
// rungs, the one encoding, and what each earlier rung measured.
type QualityLadderRecord struct {
	VideoCodec string `json:"video_codec"`
	Preset     string `json:"preset,omitempty"`
	Values     []int  `json:"quality_values"`
	Rung       int    `json:"rung"`
	// Tried are the rungs that ran and failed, in order.
	Tried []QualityLadderRung `json:"tried,omitempty"`
	// Outcome is encoding, passed, or the reason the ladder stopped.
	Outcome string `json:"outcome"`
	Message string `json:"message"`
	// Final is the passing rung's full-file result.
	Final *FinalQualitySummary `json:"final,omitempty"`
	// Choice is set when the ladder runs the recipe's fallback encoder
	// instead of the one it froze, and says why.
	Choice *SizeBudgetChoice `json:"encoder_choice,omitempty"`
	// RetryWithoutHardware is set when every VideoToolbox rung missed the
	// quality limits: the file may be run again with the media engine off.
	RetryWithoutHardware bool `json:"retry_without_hardware,omitempty"`
}

// ladderRecipe is the ladder the record runs: the frozen recipe, or its
// fallback when the record chose it.
func ladderRecipe(snapshot *ModePolicySnapshot, rec *QualityLadderRecord) *ModeRecipe {
	r := snapshot.Recipe
	if r != nil && r.Fallback != nil && rec != nil && rec.Choice != nil && rec.VideoCodec == r.Fallback.VideoCodec && rec.VideoCodec != r.VideoCodec {
		return r.Fallback
	}
	return r
}

// QualityLadderRung is one rung's encode and why it did not pass.
type QualityLadderRung struct {
	Quality        int                  `json:"quality"`
	JobID          string               `json:"job_id,omitempty"`
	ReasonCode     string               `json:"reason_code"`
	CandidateBytes int64                `json:"candidate_bytes,omitempty"`
	Final          *FinalQualitySummary `json:"final,omitempty"`
}

// QualityLadderOf is the quality ladder record in state, nil when the action
// is not running one.
func QualityLadderOf(state map[string]any) *QualityLadderRecord {
	if state[qualityLadderKey] == nil {
		return nil
	}
	var rec QualityLadderRecord
	if !reencode(state[qualityLadderKey], &rec) || len(rec.Values) == 0 {
		return nil
	}
	return &rec
}

// recipeRungName names a rung: "x265 CRF 20" or "VideoToolbox quality 60".
func recipeRungName(codec string, q int) string {
	if codec == transcode.VideoCodecHEVCVideoToolbox {
		return fmt.Sprintf("VideoToolbox quality %d", q)
	}
	return fmt.Sprintf("x265 CRF %d", q)
}

// recipeRungShort names a later rung of the same ladder: "CRF 17".
func recipeRungShort(codec string, q int) string {
	if codec == transcode.VideoCodecHEVCVideoToolbox {
		return fmt.Sprintf("quality %d", q)
	}
	return fmt.Sprintf("CRF %d", q)
}

// qualityLadderVideo is the video of one rung.
func qualityLadderVideo(r *ModeRecipe, hw *transcode.HardwareSearchRules, bitDepth, rung int) (sizeBudgetVideoSettings, error) {
	if r == nil || r.Kind != RecipeQualityLadder || rung < 0 || rung >= len(r.QualityValues) {
		return sizeBudgetVideoSettings{}, fmt.Errorf("the frozen recipe has no rung %d", rung)
	}
	q := r.QualityValues[rung]
	if r.VideoCodec == transcode.VideoCodecHEVCVideoToolbox {
		if hw == nil {
			return sizeBudgetVideoSettings{}, fmt.Errorf("the frozen policy does not allow VideoToolbox")
		}
		v := sizeBudgetVTVideo(hw, bitDepth, 0)
		v.Quality = q
		return v, nil
	}
	return sizeBudgetVideoSettings{VideoCodec: transcode.VideoCodecLibX265, Quality: q, Preset: r.Preset}, nil
}

func snapshotHardware(p *ModePolicySnapshot) *transcode.HardwareSearchRules {
	if p.Size == nil {
		return nil
	}
	return p.Size.Hardware
}

// qualityLadderPlan is the action's current plan encoded with a rung's video,
// with final validation on the goal's windows held to its quality limits, as a
// search winner's would be.
func (e *Engine) qualityLadderPlan(ec *ExecutionContext, snapshot *ModePolicySnapshot, r *ModeRecipe, rung int) (*transcode.Plan, error) {
	base := getPlan(ec.State["plan"])
	if base == nil || base.Mode != snapshot.Mode || base.PolicyDigest != snapshot.Digest {
		return nil, fmt.Errorf("the frozen goal plan is missing")
	}
	video, err := qualityLadderVideo(r, snapshotHardware(snapshot), max(sourceVideoBitDepth(getSourceReport(ec.State["source_report"])), base.ExpectedBitDepth), rung)
	if err != nil {
		return nil, err
	}
	request, digest, samples, freshWindows, err := finalValidationInputs(ec)
	if err != nil {
		return nil, err
	}
	p := *base
	video.apply(&p)
	if base.SizePolicy != nil {
		sp := *base.SizePolicy
		sp.MaxCandidateBytes = 0
		p.SizePolicy = &sp
	}
	p.QualityValidation = &transcode.QualityValidationPlan{Metric: request.Metric, Samples: samples, FreshSamples: freshWindows, Quality: *request.Quality, BenchmarkRequestDigest: digest}
	p.PlanDigest = ""
	d, err := transcode.DigestPlan(&p)
	if err != nil {
		return nil, err
	}
	p.PlanDigest = d
	return &p, nil
}

// startQualityLadder plans a v4 quality ladder's first rung in place of the
// search. It reports false when the action has no quality ladder recipe.
// Otherwise it never leaves the search to run: a ladder that cannot start
// asks for a decision or fails.
func (e *Engine) startQualityLadder(ctx context.Context, ec *ExecutionContext, cleanPath string) (StepResult, bool) {
	if QualityLadderOf(ec.State) != nil {
		return StepResult{Status: StepCompleted}, true // planned already; this step ran again
	}
	if ec.ActionName != "transcode_media" || getString(ec.Inputs, "mode") == "" {
		return StepResult{}, false
	}
	snapshot, err := freezeModePolicy(ec)
	if err != nil || snapshot == nil || snapshot.Recipe == nil || snapshot.Recipe.Kind != RecipeQualityLadder {
		return StepResult{}, false
	}
	r := snapshot.Recipe
	caps, err := e.deps.Transcode.Capabilities(ctx)
	if err != nil {
		return workerCapabilityLookupResult(err, "goal encoder"), true
	}
	var choice *SizeBudgetChoice
	if !caps.Encoders[r.VideoCodec] && r.Fallback != nil && caps.Encoders[r.Fallback.VideoCodec] {
		// As a VideoToolbox size budget does, a worker without VideoToolbox
		// runs the recipe's x265 ladder instead.
		f := r.Fallback
		later := make([]string, len(f.QualityValues))
		for i, q := range f.QualityValues {
			later[i] = fmt.Sprint(q)
		}
		choice = &SizeBudgetChoice{VideoCodec: f.VideoCodec, Reason: SizeBudgetChoiceWorkerUnsupported, Message: fmt.Sprintf("The worker cannot encode with %s, so this goal's %s ladder (CRF %s) runs instead.", recipeEncoderName(r.VideoCodec), recipeEncoderName(f.VideoCodec), joinWords(later))}
		r = f
	}
	if !caps.Encoders[r.VideoCodec] {
		return modeAttention(ec, "recipe_encoder_unavailable", fmt.Sprintf("This goal encodes with %s, which the worker cannot run; original preserved", recipeEncoderName(r.VideoCodec))), true
	}
	rep := getSourceReport(ec.State["source_report"])
	opt := getOptimizationPolicy(ec.State["optimization_policy"])
	if rep == nil || opt == nil || !opt.Enabled {
		return StepResult{Status: StepFailed, Error: "media inspection report or goal policy is missing from state (fail closed)"}, true
	}
	// The windows and limits final validation judges every rung on.
	req, err := buildFinalValidationRequest(ec, cleanPath, rep, opt, caps, r.VideoCodec)
	if err != nil {
		return modeAttention(ec, "quality_policy_unavailable", err.Error()+"; original preserved"), true
	}
	digest, err := transcode.DigestBenchmarkRequest(req)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("computing final validation request digest: %v", err)}, true
	}
	ec.State["benchmark_request"], ec.State["benchmark_request_digest"] = req, digest
	plan, err := e.qualityLadderPlan(ec, snapshot, r, 0)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("building the first encode of the goal: %v", err)}, true
	}
	rec := &QualityLadderRecord{VideoCodec: r.VideoCodec, Preset: r.Preset, Values: slices.Clone(r.QualityValues), Outcome: QualityLadderEncoding, Choice: choice}
	rec.Message = qualityLadderStartMessage(rec)
	if choice != nil {
		rec.Message = choice.Message + " " + rec.Message
	}
	ec.State[qualityLadderKey] = rec
	ec.State["plan"], ec.State["plan_source"], ec.State["plan_digest"] = plan, qualityLadderPlanSource, plan.PlanDigest
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"benchmark_request_digest": digest, qualityLadderKey: rec,
		"plan": plan, "plan_source": qualityLadderPlanSource, "plan_digest": plan.PlanDigest, "plan_note": rec.Message}}, true
}

func recipeEncoderName(codec string) string {
	if codec == transcode.VideoCodecHEVCVideoToolbox {
		return "VideoToolbox"
	}
	return "x265"
}

func qualityLadderStartMessage(rec *QualityLadderRecord) string {
	first := recipeRungName(rec.VideoCodec, rec.Values[0])
	if rec.Preset != "" {
		first += fmt.Sprintf(" (%s preset)", rec.Preset)
	}
	later := make([]string, 0, len(rec.Values)-1)
	for _, q := range rec.Values[1:] {
		later = append(later, recipeRungShort(rec.VideoCodec, q))
	}
	msg := fmt.Sprintf("Encoding the whole file with %s (step 1 of %d) and checking it against this goal's quality limits.", first, len(rec.Values))
	if len(later) > 0 {
		msg += fmt.Sprintf(" If it misses them, %s follow%s, one at a time.", joinWords(later), map[bool]string{true: "s", false: ""}[len(later) == 1])
	}
	return msg
}

// qualityLadderFailures says why a rung missed: the failed checks, or its
// scores when the worker named none.
func qualityLadderFailures(f *FinalQualitySummary) string {
	if f == nil {
		return "no scores reported"
	}
	if len(f.Failures) > 0 {
		return joinWords(f.Failures)
	}
	if m := f.Measured; m.VMAFMean != nil && m.VMAFP5 != nil {
		return fmt.Sprintf("VMAF %.1f, p5 %.1f", *m.VMAFMean, *m.VMAFP5)
	}
	return "no scores reported"
}

// advanceQualityLadder handles the worker's refusal of a quality ladder rung.
// A rung that missed the quality limits is encoded again one rung better; one
// refused for its size, or the last rung, ends the ladder. It reports false
// when the action is not running a quality ladder or the refusal is not one
// the ladder answers.
func (e *Engine) advanceQualityLadder(ctx context.Context, ec *ExecutionContext, st transcode.JobStatus) (StepResult, bool) {
	rec := QualityLadderOf(ec.State)
	plan := getPlan(ec.State["plan"])
	if rec == nil || plan == nil || getString(ec.State, "plan_source") != qualityLadderPlanSource {
		return StepResult{}, false
	}
	switch st.ReasonCode {
	case reasonQualityNotMet, "insufficient_savings", "size_growth", "size_growth_exceeded":
	default:
		return StepResult{}, false
	}
	snapshot, err := freezeModePolicy(ec)
	if err != nil || snapshot == nil || snapshot.Recipe == nil || snapshot.Recipe.Kind != RecipeQualityLadder {
		return StepResult{Status: StepFailed, Error: "frozen goal recipe is missing or does not match the encode (fail closed)"}, true
	}
	r := ladderRecipe(snapshot, rec)
	if rec.VideoCodec != r.VideoCodec || !slices.Equal(rec.Values, r.QualityValues) || rec.Rung >= len(r.QualityValues) {
		return StepResult{Status: StepFailed, Error: "frozen goal recipe is missing or does not match the encode (fail closed)"}, true
	}
	reason := st.ReasonCode
	if reason == "size_growth_exceeded" {
		reason = "size_growth"
	}
	tried := QualityLadderRung{Quality: rec.Values[rec.Rung], JobID: getString(ec.State, "job_id"), ReasonCode: reason, CandidateBytes: st.CandidateSizeBytes, Final: summarizeFinalQuality(st.QualityEvidence, stateQualityLimits(ec.State))}
	// The audio-only fallback may wait and ask again about the same refusal:
	// that rung is already on the record.
	if n := len(rec.Tried); n == 0 || tried.JobID == "" || rec.Tried[n-1].JobID != tried.JobID {
		rec.Tried = append(rec.Tried, tried)
	}
	was := recipeRungName(rec.VideoCodec, tried.Quality)
	if reason != reasonQualityNotMet {
		size := ""
		if st.CandidateSizeBytes > 0 {
			size = fmt.Sprintf(" at %s", formatBudgetBytes(st.CandidateSizeBytes))
		}
		why := fmt.Sprintf("does not save the %.0f%% this goal needs", snapshot.MinSavingsPercent)
		if reason == "size_growth" || snapshot.MinSavingsPercent <= 0 {
			why = "is larger than the original"
		}
		msg := fmt.Sprintf("%s came out%s, which %s. A better setting would only make it larger, so the goal stops here. Original preserved.", was, size, why)
		return e.endQualityLadder(ctx, ec, snapshot, st, rec, reason, msg), true
	}
	if st.QualityEvidence == nil || rec.Rung+1 >= len(rec.Values) {
		parts := make([]string, len(rec.Tried))
		for i, t := range rec.Tried {
			parts[i] = fmt.Sprintf("%s (%s)", recipeRungName(rec.VideoCodec, t.Quality), qualityLadderFailures(t.Final))
		}
		msg := fmt.Sprintf("Every step missed this goal's quality limits: %s. No candidate is offered; original preserved.", joinWords(parts))
		if rec.Rung+1 < len(rec.Values) {
			msg = fmt.Sprintf("%s missed this goal's quality limits, and the worker reported no scores to go on. No candidate is offered; original preserved.", was)
		}
		return e.endQualityLadder(ctx, ec, snapshot, st, rec, reasonQualityNotMet, msg), true
	}
	rec.Rung++
	next, err := e.qualityLadderPlan(ec, snapshot, r, rec.Rung)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("building the next encode of the goal: %v", err)}, true
	}
	rec.Message = fmt.Sprintf("%s missed the quality limits (%s); encoding at %s (step %d of %d).", was, qualityLadderFailures(tried.Final), recipeRungShort(rec.VideoCodec, rec.Values[rec.Rung]), rec.Rung+1, len(rec.Values))
	return e.rewindQualityLadder(ec, rec, next, fmt.Sprintf("job-%s-recipe-r%d", ec.InstanceID, rec.Rung)), true
}

// rewindQualityLadder makes plan the action's encode and sends the action back
// to submit it, as a size budget rung does.
func (e *Engine) rewindQualityLadder(ec *ExecutionContext, rec *QualityLadderRecord, plan *transcode.Plan, jobID string) StepResult {
	appendFailureHistory(ec, "quality_ladder", "policy_rejected", rec.Message)
	for _, key := range transcodeAttemptKeys {
		delete(ec.State, key)
		delete(ec.Outputs, key)
	}
	delete(ec.State, "skip_transcode")
	delete(ec.State, "reason_code")
	previous := getString(ec.State, "job_id")
	ec.State[qualityLadderKey] = rec
	ec.State["plan"], ec.State["plan_digest"] = plan, plan.PlanDigest
	ec.State["previous_job_id"], ec.State["job_id"], ec.State["external_reference"] = previous, jobID, jobID
	ec.State["attempt"], ec.State["retry_count"], ec.State["fallback_count"] = 1, 0, 0
	mergeMap(ec.Outputs, map[string]any{"plan": plan, "plan_digest": plan.PlanDigest, "job_id": jobID, "external_reference": jobID, "previous_job_id": previous})
	return StepResult{Status: StepCompleted, RewindTo: "submit_transcode", Outputs: map[string]any{qualityLadderKey: rec, "plan_note": rec.Message}}
}

// endQualityLadder ends a ladder with no candidate. A size goal whose policy
// allows it still tries to compact the lossless audio alone, with the video
// copied, as it did after a search without a winner.
func (e *Engine) endQualityLadder(ctx context.Context, ec *ExecutionContext, snapshot *ModePolicySnapshot, st transcode.JobStatus, rec *QualityLadderRecord, reason, message string) StepResult {
	rec.Outcome, rec.Message = reason, message
	// Every VideoToolbox rung missed: x265 at the same goal usually keeps
	// more quality, so the file may be tried again with the media engine off.
	rec.RetryWithoutHardware = reason == reasonQualityNotMet && rec.VideoCodec == transcode.VideoCodecHEVCVideoToolbox
	// The ladder has ended however the rest goes: the record says so before
	// anything else can stop this step.
	ec.State[qualityLadderKey] = rec
	if snapshot.Mode == "size" && snapshot.Size != nil && snapshot.Size.AudioOnlyFallback && !getBool(ec.State, "size_audio_only") {
		scratch := map[string]any{}
		state := map[string]any{"skip_transcode": ec.State["skip_transcode"], "reason_code": ec.State["reason_code"]}
		if res, handled := e.sizeAudioOnlyFallback(ctx, ec, scratch, ""); handled {
			if res.Status == StepCompleted && getString(ec.State, "plan_source") == "audio_only" {
				plan := getPlan(ec.State["plan"])
				rec.Message = message + " The video is copied unchanged and only the lossless audio is compacted instead."
				out := e.rewindQualityLadder(ec, rec, plan, fmt.Sprintf("job-%s-recipe-audio", ec.InstanceID))
				ec.State["plan_source"] = "audio_only"
				mergeMap(out.Outputs, map[string]any{"plan": plan, "plan_source": "audio_only", "plan_digest": plan.PlanDigest})
				return out
			}
			if res.Status != StepCompleted {
				if res.Outputs == nil {
					res.Outputs = map[string]any{}
				}
				res.Outputs[qualityLadderKey] = rec
				return res
			}
		}
		// Audio alone does not do it either: the ladder's own reason stands.
		for k, v := range state {
			if v == nil {
				delete(ec.State, k)
			} else {
				ec.State[k] = v
			}
		}
	}
	ec.State[qualityLadderKey] = rec
	ec.State["skip_transcode"] = true
	ec.State["reason_code"] = reason
	out := transcodeWorkerMetadataOutputs(ec, st)
	out["skipped"], out["reason_code"], out[qualityLadderKey], out["plan_note"] = true, reason, rec, rec.Message
	return StepResult{Status: StepCompleted, Outputs: out}
}

// qualityLadderPlanMatches reports whether plan is exactly the rung the record
// names, rebuilt from the frozen recipe and the source, with no byte ceiling
// and final validation pinned to the stored request.
func qualityLadderPlanMatches(snapshot *ModePolicySnapshot, state map[string]any, plan *transcode.Plan, rec *QualityLadderRecord) bool {
	r := ladderRecipe(snapshot, rec)
	if r == nil || r.Kind != RecipeQualityLadder || rec == nil || plan == nil || rec.VideoCodec != r.VideoCodec || rec.Preset != r.Preset || !slices.Equal(rec.Values, r.QualityValues) {
		return false
	}
	if plan.SizePolicy == nil || plan.SizePolicy.MaxCandidateBytes != 0 || plan.QualityValidation == nil || plan.QualityValidation.BenchmarkRequestDigest != getString(state, "benchmark_request_digest") {
		return false
	}
	want, err := qualityLadderVideo(r, snapshotHardware(snapshot), max(sourceVideoBitDepth(getSourceReport(state["source_report"])), plan.ExpectedBitDepth), rec.Rung)
	if err != nil {
		return false
	}
	return sizeBudgetVideoJSON(want) == sizeBudgetVideoJSON(sizeBudgetVideoOf(plan))
}

// passQualityLadder records the rung that passed final validation.
func passQualityLadder(ec *ExecutionContext, candidateBytes int64) *QualityLadderRecord {
	rec := QualityLadderOf(ec.State)
	if rec == nil {
		return nil
	}
	var evidence transcode.FinalQualityEvidence
	if reencode(ec.State["quality_evidence"], &evidence) && evidence.Verdict != "" {
		rec.Final = summarizeFinalQuality(&evidence, stateQualityLimits(ec.State))
	}
	scores := ""
	if rec.Final != nil && rec.Final.Measured.VMAFMean != nil && rec.Final.Measured.VMAFP5 != nil {
		scores = fmt.Sprintf(" (VMAF %.1f, p5 %.1f)", *rec.Final.Measured.VMAFMean, *rec.Final.Measured.VMAFP5)
	}
	rec.Outcome = QualityLadderPassed
	rec.Message = fmt.Sprintf("%s passed this goal's quality limits on the whole file%s at %s (step %d of %d).", recipeRungName(rec.VideoCodec, rec.Values[rec.Rung]), scores, formatBudgetBytes(candidateBytes), rec.Rung+1, len(rec.Values))
	ec.State[qualityLadderKey] = rec
	return rec
}

// stopQualityLadder records a rung the coordinator refused for its size.
func stopQualityLadder(ec *ExecutionContext, reason string, candidateBytes int64) *QualityLadderRecord {
	rec := QualityLadderOf(ec.State)
	if rec == nil {
		return nil
	}
	rec.Outcome = reason
	rec.Message = fmt.Sprintf("%s came out at %s, which does not save enough for this goal. Original preserved.", recipeRungName(rec.VideoCodec, rec.Values[rec.Rung]), formatBudgetBytes(candidateBytes))
	ec.State[qualityLadderKey] = rec
	return rec
}
