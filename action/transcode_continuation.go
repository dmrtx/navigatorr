package action

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

// A size search that picked a VideoToolbox result can still lose it at the
// full-file quality check, which sees every frame where the search saw five
// short windows. The action then continues once, in the same round, with the
// search repeated over x265 alone: the original is untouched, nothing is
// promoted, and a second failure ends the action as a quality rejection does
// without it.

const (
	// hardwareContinuationKey holds the HardwareContinuation record. Its
	// presence is the guard that lets the continuation happen once per action.
	hardwareContinuationKey = "hardware_continuation"
	// continuationBasePlanKey keeps the plan the search started from while a
	// VideoToolbox winner's plan replaces it, so a continuation can search again.
	continuationBasePlanKey       = "continuation_base_plan"
	continuationBasePlanSourceKey = "continuation_base_plan_source"
	// continuationPriorSecondsKey is the active search time of the searches
	// before the current one.
	continuationPriorSecondsKey = "benchmark_search_seconds_prior"

	continuationContinued  = "continued"
	continuationNotStarted = "not_started"

	reasonQualityNotMet = "quality_not_met"
)

// HardwareContinuation records a VideoToolbox result that failed the final
// quality check and what the action did about it. It keeps the failed attempt's
// validation numbers visible after the x265 continuation replaces the job.
type HardwareContinuation struct {
	// Outcome is "continued" (an x265 search follows) or "not_started".
	Outcome string `json:"outcome"`
	// Reason says why a continuation did not start.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message"`

	FailedCandidateID    string `json:"failed_candidate_id,omitempty"`
	FailedJobID          string `json:"failed_job_id,omitempty"`
	FailedBenchmarkJobID string `json:"failed_benchmark_job_id,omitempty"`
	// FailedBenchmarkDigest is the digest of the request the failed result was
	// searched under; the continued search names it so the worker may reuse
	// the measured x265 samples of that search.
	FailedBenchmarkDigest string `json:"failed_benchmark_digest,omitempty"`

	// FirstSearchSeconds is the active time the VideoToolbox search took and
	// RemainingSearchSeconds what the frozen search ceiling left for the
	// continuation.
	FirstSearchSeconds     float64 `json:"first_search_seconds,omitempty"`
	RemainingSearchSeconds float64 `json:"remaining_search_seconds,omitempty"`

	// Sampled is what the failed result scored on the search's sample windows
	// and Final what it scored on the whole file.
	Sampled *QualityNumbers      `json:"sampled,omitempty"`
	Final   *FinalQualitySummary `json:"final,omitempty"`
	// Windows are the failed result's search and final scores window by
	// window; the x265 continuation replaces the evidence they come from.
	Windows []transcode.WindowComparison `json:"windows,omitempty"`
}

// QualityNumbers are the bounded VMAF and CAMBI figures of one measurement.
type QualityNumbers struct {
	VMAFMean        *float64 `json:"vmaf_mean,omitempty"`
	VMAFP5          *float64 `json:"vmaf_p5,omitempty"`
	VMAFWorstWindow *float64 `json:"vmaf_worst_window,omitempty"`
	VMAFMin         *float64 `json:"vmaf_min,omitempty"`
	FramesBelow     *int     `json:"frames_below_threshold,omitempty"`
	CAMBIMean       *float64 `json:"cambi_mean,omitempty"`
	CAMBIPeak       *float64 `json:"cambi_peak,omitempty"`
	SSIMMean        *float64 `json:"ssim_mean,omitempty"`
}

// QualityLimits are the thresholds the numbers were held to.
type QualityLimits struct {
	VMAFMean           *float64 `json:"vmaf_mean_minimum,omitempty"`
	VMAFP5             *float64 `json:"vmaf_p5_minimum,omitempty"`
	VMAFWorstWindow    *float64 `json:"vmaf_worst_window_minimum,omitempty"`
	WorstWindowSeconds float64  `json:"worst_window_seconds,omitempty"`
	FrameThreshold     *float64 `json:"vmaf_frame_threshold,omitempty"`
	MaxFramesBelow     *int     `json:"max_frames_below_threshold,omitempty"`
	CAMBIMean          *float64 `json:"cambi_mean_maximum,omitempty"`
	CAMBIPeak          *float64 `json:"cambi_peak_maximum,omitempty"`
}

// FinalQualitySummary is the full-file validation result in a form small
// enough for status responses: the verdict, which checks failed and by how
// much, and the numbers behind them.
type FinalQualitySummary struct {
	Verdict      string         `json:"verdict"`
	FailedChecks []string       `json:"failed_checks,omitempty"`
	Failures     []string       `json:"failures,omitempty"`
	Measured     QualityNumbers `json:"measured"`
	Limits       *QualityLimits `json:"limits,omitempty"`
}

func reencode(from, to any) bool {
	b, err := json.Marshal(from)
	return err == nil && json.Unmarshal(b, to) == nil
}

func qualityNumbersOf(v *quality.VMAFStats, c *quality.CAMBIStats, ssim *float64) QualityNumbers {
	var n QualityNumbers
	if v != nil {
		n.VMAFMean, n.VMAFP5, n.VMAFWorstWindow, n.VMAFMin = &v.Mean, &v.P5, &v.WorstWindowMean, &v.Min
		if v.FrameThreshold != nil {
			below := v.SourceEquivalentFramesBelowThreshold()
			n.FramesBelow = &below
		}
	}
	if c != nil {
		n.CAMBIMean, n.CAMBIPeak = &c.Mean, &c.Max
	}
	n.SSIMMean = ssim
	return n
}

func qualityLimitsOf(cfg *transcode.BenchmarkQualityConfig) *QualityLimits {
	if cfg == nil {
		return nil
	}
	var l QualityLimits
	if v := cfg.VMAF; v != nil {
		minimum := v.Minimum
		l.VMAFMean, l.VMAFP5, l.VMAFWorstWindow = &minimum, v.P5Minimum, v.WorstWindowMinimum
		l.WorstWindowSeconds, l.FrameThreshold, l.MaxFramesBelow = v.WorstWindowSeconds, v.FrameThreshold, v.MaxFramesBelowThreshold
	}
	if b := cfg.Banding; b != nil && b.Enabled {
		l.CAMBIMean, l.CAMBIPeak = b.MaxMean, b.MaxPeak
	}
	return &l
}

// summarizeFinalQuality projects a full-file validation result. limits may be nil.
func summarizeFinalQuality(ev *transcode.FinalQualityEvidence, limits *QualityLimits) *FinalQualitySummary {
	if ev == nil {
		return nil
	}
	s := &FinalQualitySummary{Verdict: ev.Verdict, FailedChecks: append([]string(nil), ev.ReasonCodes...), Measured: qualityNumbersOf(ev.VMAF, ev.CAMBI, ev.SSIMMean), Limits: limits}
	for _, code := range ev.ReasonCodes {
		s.Failures = append(s.Failures, describeQualityFailure(code, s.Measured, limits))
	}
	return s
}

func describeQualityFailure(code string, m QualityNumbers, l *QualityLimits) string {
	if l == nil {
		l = &QualityLimits{}
	}
	value := func(v *float64) string {
		if v == nil {
			return "unknown"
		}
		return fmt.Sprintf("%.1f", *v)
	}
	switch code {
	case "quality_vmaf_p5_below_minimum":
		return fmt.Sprintf("VMAF p5 %s is below the minimum %s", value(m.VMAFP5), value(l.VMAFP5))
	case "quality_vmaf_worst_window_below_minimum":
		return fmt.Sprintf("VMAF worst %.0f-second window %s is below the minimum %s", l.WorstWindowSeconds, value(m.VMAFWorstWindow), value(l.VMAFWorstWindow))
	case "sample_below_minimum_quality", "below_minimum_quality":
		return fmt.Sprintf("VMAF mean %s is below the minimum %s", value(m.VMAFMean), value(l.VMAFMean))
	case "quality_ssim_mean_below_minimum":
		return fmt.Sprintf("SSIM mean %s is below the minimum", value(m.SSIMMean))
	case "quality_vmaf_frames_below_limit_exceeded":
		below := "unknown"
		if m.FramesBelow != nil {
			below = fmt.Sprint(*m.FramesBelow)
		}
		limit := "unknown"
		if l.MaxFramesBelow != nil {
			limit = fmt.Sprint(*l.MaxFramesBelow)
		}
		return fmt.Sprintf("%s frames score below VMAF %s, more than the %s allowed", below, value(l.FrameThreshold), limit)
	case "quality_cambi_mean_above_maximum":
		return fmt.Sprintf("CAMBI mean %s is above the maximum %s", value(m.CAMBIMean), value(l.CAMBIMean))
	case "quality_cambi_peak_above_maximum":
		return fmt.Sprintf("CAMBI peak %s is above the maximum %s", value(m.CAMBIPeak), value(l.CAMBIPeak))
	}
	return code
}

// stateQualityLimits reads the limits the current search's final validation
// holds a result to.
func stateQualityLimits(state map[string]any) *QualityLimits {
	var req struct {
		Quality *transcode.BenchmarkQualityConfig `json:"quality"`
	}
	if reencode(state["benchmark_request"], &req) {
		return qualityLimitsOf(req.Quality)
	}
	return nil
}

// continuationOf returns the record of a continuation that is running or ran.
func continuationOf(state map[string]any) *HardwareContinuation {
	if state[hardwareContinuationKey] == nil {
		return nil
	}
	var c HardwareContinuation
	if !reencode(state[hardwareContinuationKey], &c) {
		return nil
	}
	return &c
}

// continuationActive reports an x265 search that follows a failed hardware result.
func continuationActive(state map[string]any) bool {
	c := continuationOf(state)
	return c != nil && c.Outcome == continuationContinued
}

// rememberContinuationBase keeps the plan a size search started from when a
// VideoToolbox result replaces it, which is the only winner a continuation
// can follow.
func rememberContinuationBase(ec *ExecutionContext, base *transcode.Plan, winner *transcode.BenchmarkWinner) {
	if base == nil || winner == nil || ec.ActionName != "transcode_media" || getString(ec.Inputs, "mode") != "size" {
		return
	}
	if transcode.BenchmarkCandidateVideoCodec(transcode.BenchmarkCandidate{VideoCodec: winner.VideoCodec}) != transcode.VideoCodecHEVCVideoToolbox {
		return
	}
	ec.State[continuationBasePlanKey] = base
	ec.State[continuationBasePlanSourceKey] = getString(ec.State, "plan_source")
}

// minContinuationSeconds is the least search time worth starting an x265
// search with: the frozen base budget of one search.
func minContinuationSeconds(ec *ExecutionContext) float64 {
	return float64(getInt(ec.State, "search_budget_seconds"))
}

// searchSecondsRemaining is what the frozen ceiling of one source's search
// leaves after the searches already run. known is false when the time the
// earlier search used is not recorded.
func searchSecondsRemaining(ec *ExecutionContext) (used, remaining float64, known bool) {
	raw, ok := ec.State["benchmark_search_seconds"]
	if !ok {
		return 0, 0, false
	}
	used, valid := measuredSearchSeconds(raw)
	if !valid {
		return 0, 0, false
	}
	used += getFloat(ec.State, continuationPriorSecondsKey)
	ceiling := searchCeilingSeconds(ec.State["mode_policy"], getInt(ec.State, "search_budget_seconds"))
	return used, float64(ceiling) - used, true
}

// continuationExtension fits a frozen search extension inside the time the
// ceiling left. Without room for an extension the search keeps its base
// budget, which the caller has already checked fits.
func continuationExtension(ext *transcode.SearchExtension, base int, remaining float64) *transcode.SearchExtension {
	if ext == nil {
		return nil
	}
	fitted := ext.Clone()
	limit := int(remaining) - fitted.VerifySeconds
	if limit <= base {
		return nil
	}
	fitted.MaxSeconds = min(fitted.MaxSeconds, limit)
	if fitted.Validate(base) != nil {
		return nil
	}
	return fitted
}

// continueWithoutHardware ends a size action's rejection of a VideoToolbox
// result by rewinding to the search with hardware excluded. It reports false,
// leaving the action to finish as a quality rejection, when the failure is not
// a hardware result's final quality check, the action already continued, or
// nothing about the action allows another search; in the last case it records
// why, so the rejection explains itself.
func (e *Engine) continueWithoutHardware(ctx context.Context, ec *ExecutionContext, st transcode.JobStatus) (StepResult, bool, error) {
	if ec.ActionName != "transcode_media" || getString(ec.Inputs, "mode") != "size" || st.ReasonCode != reasonQualityNotMet || st.QualityEvidence == nil || ec.State[hardwareContinuationKey] != nil {
		return StepResult{}, false, nil
	}
	if sizeBudgetLadderOf(ec.State) != nil || policyRecipe(ec.State["mode_policy"]) != nil {
		// A ladder or recipe has no search to continue; a ladder's failure
		// offers x265 to the user instead (failSizeBudget).
		return StepResult{}, false, nil
	}
	var winner transcode.BenchmarkWinner
	if ec.State["benchmark_winner"] == nil || !reencode(ec.State["benchmark_winner"], &winner) ||
		transcode.BenchmarkCandidateVideoCodec(transcode.BenchmarkCandidate{VideoCodec: winner.VideoCodec}) != transcode.VideoCodecHEVCVideoToolbox {
		return StepResult{}, false, nil
	}
	var base *transcode.Plan
	if ec.State[continuationBasePlanKey] != nil {
		base = getPlan(ec.State[continuationBasePlanKey])
	}
	if base == nil {
		return StepResult{}, false, nil
	}
	rec := &HardwareContinuation{
		FailedCandidateID:     winner.CandidateID,
		FailedJobID:           getString(ec.State, "job_id"),
		FailedBenchmarkJobID:  getString(ec.State, "benchmark_job_id"),
		FailedBenchmarkDigest: getString(ec.State, "benchmark_request_digest"),
		Final:                 summarizeFinalQuality(st.QualityEvidence, stateQualityLimits(ec.State)),
	}
	var searched transcode.BenchmarkAutomaticEvidence
	if ec.State["automatic_search"] != nil && reencode(ec.State["automatic_search"], &searched) {
		rec.Windows = transcode.CompareWindows(searched.WinnerWindows, st.QualityEvidence.Windows)
	}
	var sampled []transcode.BenchmarkCandidateQualityEvidence
	if reencode(ec.State["benchmark_quality"], &sampled) {
		for _, q := range sampled {
			if q.CandidateID == winner.CandidateID {
				n := qualityNumbersOf(q.VMAF, q.CAMBI, nil)
				rec.Sampled = &n
			}
		}
	}
	used, remaining, known := searchSecondsRemaining(ec)
	rec.FirstSearchSeconds, rec.RemainingSearchSeconds = used, remaining
	finish := func(outcome, reason, message string) {
		rec.Outcome, rec.Reason, rec.Message = outcome, reason, message
		ec.State[hardwareContinuationKey] = rec
		ec.Outputs[hardwareContinuationKey] = rec
	}
	workerHasX265, capabilitiesUnknown := true, false
	if e.deps.Transcode != nil {
		caps, err := e.deps.Transcode.Capabilities(ctx)
		if err != nil {
			if isRetryableWorkerPollError(err) {
				return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_unreachable", WaitingReason: fmt.Sprintf("Worker capabilities temporarily unavailable; the VideoToolbox result's quality failure will be handled when they return: %v", err)}, true, nil
			}
			workerHasX265, capabilitiesUnknown = false, true
		} else {
			workerHasX265 = caps.Encoders[transcode.VideoCodecLibX265]
		}
	}
	benchID, jobID := fmt.Sprintf("bench-%s-x265", ec.InstanceID), fmt.Sprintf("job-%s-x265", ec.InstanceID)
	switch {
	case !known:
		finish(continuationNotStarted, "search_time_unknown", "The VideoToolbox result failed the final quality check. The x265 continuation did not start because the time the first search used was not recorded.")
		return StepResult{}, false, nil
	case remaining < minContinuationSeconds(ec):
		rec.RemainingSearchSeconds = max(remaining, 0)
		finish(continuationNotStarted, "search_budget_exhausted", "The VideoToolbox result failed the final quality check. The x265 continuation did not start because the search time allowed for this file was used up.")
		return StepResult{}, false, nil
	case capabilitiesUnknown:
		finish(continuationNotStarted, "worker_capabilities_unavailable", "The VideoToolbox result failed the final quality check. The x265 continuation did not start because the worker's capabilities could not be read.")
		return StepResult{}, false, nil
	case !workerHasX265:
		finish(continuationNotStarted, "x265_unavailable", "The VideoToolbox result failed the final quality check. The x265 continuation did not start because the worker cannot encode with x265.")
		return StepResult{}, false, nil
	case transcode.ValidateBenchmarkJobID(benchID) != nil:
		finish(continuationNotStarted, "identity_unavailable", "The VideoToolbox result failed the final quality check. The x265 continuation could not be given its own worker identity.")
		return StepResult{}, false, nil
	}
	finish(continuationContinued, "", "The VideoToolbox result failed the final quality check; continued with x265.")
	appendFailureHistory(ec, "hardware_quality", "policy_rejected", fmt.Sprintf("VideoToolbox result %s failed final quality validation; continuing the search with x265 only", winner.CandidateID))

	for _, key := range continuationClearedKeys {
		delete(ec.State, key)
		delete(ec.Outputs, key)
	}
	ec.State[continuationPriorSecondsKey] = used
	ec.Outputs[continuationPriorSecondsKey] = used
	ec.State["previous_job_id"], ec.State["previous_benchmark_job_id"] = rec.FailedJobID, rec.FailedBenchmarkJobID
	ec.State["benchmark_job_id"], ec.State["job_id"], ec.State["external_reference"] = benchID, jobID, jobID
	ec.State["benchmark_attempt"], ec.State["benchmark_retry_count"] = 1, 0
	ec.State["attempt"], ec.State["retry_count"], ec.State["fallback_count"] = 1, 0, 0
	// The search starts again from the plan it started from, not the
	// VideoToolbox winner's.
	ec.State["plan"] = base
	ec.State["plan_source"] = getString(ec.State, continuationBasePlanSourceKey)
	ec.State["plan_digest"] = base.PlanDigest
	ec.State["recipe_version"], ec.State["recipe_digest"] = base.RecipeVersion, base.RecipeDigest
	mergeMap(ec.Outputs, map[string]any{"plan": base, "plan_source": ec.State["plan_source"], "plan_digest": base.PlanDigest, "recipe_version": base.RecipeVersion, "recipe_digest": base.RecipeDigest,
		"benchmark_job_id": benchID, "job_id": jobID, "external_reference": jobID, "previous_job_id": rec.FailedJobID, "previous_benchmark_job_id": rec.FailedBenchmarkJobID})
	return StepResult{Status: StepCompleted, RewindTo: "submit_benchmark", Outputs: map[string]any{hardwareContinuationKey: rec}}, true, nil
}

// continuationClearedKeys are the search and encode results of the failed
// VideoToolbox attempt. The continuation allocates a fresh benchmark and job
// identity, so nothing of the first attempt may be mistaken for its own.
var continuationClearedKeys = append(append(append([]string(nil), continuationSearchKeys...), sizeBudgetRoundKey), transcodeAttemptKeys...)

// continuationSearchKeys are the benchmark's results.
var continuationSearchKeys = []string{
	"benchmark_submitted", "benchmark_reconcile", "benchmark_done", "benchmark_status", "benchmark_decision", "benchmark_request", "benchmark_request_digest",
	"benchmark_retry_not_before", benchmarkRejectedKey, "benchmark_quality", "benchmark_winner", "benchmark_search_seconds", "benchmark_phase", "benchmark_progress_details",
	"benchmark_comparison_available", "benchmark_reconciled_at", "benchmark_worker_completed_at", "benchmark_reconcile_lag_ms", "benchmark_search_reviewed", "benchmark_search_review_required",
	"benchmark_search_review", "automatic_search", "has_winner", "decision_reason", "search_worker_busy",
	"winner", "winner_candidate_id", "winner_video_codec", "winner_preset", "winner_quality", "winner_average_bitrate_kbps", "winner_video_profile", "winner_pixel_format",
	"winner_bit_depth", "winner_score", "winner_metric", "estimated_total_mb", "estimated_savings_percent", "predicted_savings_percent", "effective_min_savings_percent",
	"effective_max_size_increase_percent", "skip_transcode", "skipped", "reason_code", sizeBudgetKey, sizeBudgetSkipReasonKey, sizeBudgetSkipMessageKey,
}

// transcodeAttemptKeys are an encode's results and its validation.
var transcodeAttemptKeys = []string{
	"recovery_required", "worker_error", "finalization_retry_count", "next_finalization_at", "transcode_submitted", "transcode_reconcile", "transcode_done", "transcode_status",
	"transcode_phase", "candidate_path", "output_path", "candidate_sha256", "candidate_size_bytes", "idempotency_key", "worker_completed_at", "reconciled_at", "reconcile_lag_ms",
	"next_poll_at", "retry_not_before", workerBusyNotBeforeKey, "worker_busy", slotWaitLogKey, workerSlotWaitSinceKey, workerSlotPositionKey, workerSlotPolledAtKey, transcodeRejectedKey,
	"validation", "result", "original_intact", "original_integrity", "progress", "speed", "fps", "last_known_progress", "last_progress_at", "worker_heartbeat_at",
	"failure_classification", "error_class", "queue_duration_ms", "encode_duration_ms", "validation_duration_ms", "worker_validation_duration_ms", "coordinator_validation_duration_ms",
	"worker_wall_duration_ms", "quality_evidence", "conversions", "applied_fallbacks", "phase", "progress_is_stale",
}

func continuationNote(c *HardwareContinuation) string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.Message)
}
