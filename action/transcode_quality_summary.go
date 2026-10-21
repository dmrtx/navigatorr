package action

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

const (
	// hardwareContinuationKey holds the HardwareContinuation record of an
	// action that ran before the sample search was removed.
	hardwareContinuationKey = "hardware_continuation"

	reasonQualityNotMet = "quality_not_met"
)

// HardwareContinuation is the record an action of the removed sample search
// left when a VideoToolbox result failed the final quality check and the
// search went on with x265. It is read back from state to explain that history.
type HardwareContinuation struct {
	// Outcome is "continued" (an x265 search followed) or "not_started".
	Outcome string `json:"outcome"`
	// Reason says why a continuation did not start.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message"`

	FailedCandidateID     string `json:"failed_candidate_id,omitempty"`
	FailedJobID           string `json:"failed_job_id,omitempty"`
	FailedBenchmarkJobID  string `json:"failed_benchmark_job_id,omitempty"`
	FailedBenchmarkDigest string `json:"failed_benchmark_digest,omitempty"`

	FirstSearchSeconds     float64 `json:"first_search_seconds,omitempty"`
	RemainingSearchSeconds float64 `json:"remaining_search_seconds,omitempty"`

	// Sampled is what the failed result scored on the search's sample windows
	// and Final what it scored on the whole file.
	Sampled *QualityNumbers              `json:"sampled,omitempty"`
	Final   *FinalQualitySummary         `json:"final,omitempty"`
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
