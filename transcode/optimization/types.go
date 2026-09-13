package optimization

import (
	"fmt"
	"math"
	"strings"
)

// MetricType identifies the perceptual quality metric used for evaluation.
type MetricType string

const (
	// MetricTypeVMAF indicates the Video Multi-Method Assessment Fusion metric (0-100 scale).
	MetricTypeVMAF MetricType = "vmaf"
	// MetricTypeSSIM indicates the Structural Similarity Index metric (0-1 scale).
	MetricTypeSSIM MetricType = "ssim"
)

// MaxSampleCount defines the conservative upper limit for planned sample windows.
// Prevents excessive loop iterations, memory allocations, and probe overhead.
const MaxSampleCount = 32

// Deterministic reason codes for sample planning, metric evaluation, estimation, and candidate selection.
const (
	// Candidate selection reasons
	ReasonTargetReachedSmallestSize    = "target_reached_smallest_size"
	ReasonMinimumMetHighestQuality     = "minimum_met_highest_quality"
	ReasonNoCandidateMetMinimumQuality = "no_candidate_met_minimum_quality"
	ReasonAllCandidatesInvalid         = "all_candidates_invalid"
	ReasonNoCandidatesProvided         = "no_candidates_provided"

	// Per-candidate policy evaluation status reasons
	ReasonTargetReached              = "target_reached"
	ReasonMinimumMet                 = "minimum_met"
	ReasonBelowMinimumQuality        = "below_minimum_quality"
	ReasonSampleBelowMinimum         = "sample_below_minimum_quality"
	ReasonScoreOutOfBounds           = "score_out_of_bounds"
	ReasonHDRIneligibleForSDRScoring = "hdr_ineligible_for_sdr_scoring"
	ReasonInvalidMetric              = "invalid_metric"
	ReasonNoValidMetric              = "no_valid_metric"
	ReasonMetricMissing              = "metric_missing"
	ReasonCandidateEncodeFailed      = "candidate_encode_failed"
	ReasonIncompleteSampleScores     = "incomplete_sample_scores"
	ReasonNoValidSampleScores        = "no_valid_sample_scores"

	// Sample planning reasons
	ReasonInvalidDuration          = "invalid_duration"
	ReasonInvalidSampleConfig      = "invalid_sample_config"
	ReasonInvalidPosition          = "invalid_sample_position"
	ReasonContradictorySampleCount = "contradictory_sample_count"
	ReasonExcessiveSampleCount     = "excessive_sample_count"
	ReasonReducedSampleCount       = "reduced_sample_count_to_avoid_overlap"
	ReasonFullDurationSample       = "full_duration_used_for_short_video"

	// Output estimation uncertainty / fallback reasons
	ReasonVideoBitrateFallback  = "video_bitrate_fallback_used"
	ReasonAudioBitrateFallback  = "audio_bitrate_fallback_used"
	ReasonSubtitleSizeEstimated = "subtitle_size_estimated"
	ReasonMissingSourceSize     = "missing_source_size"
	ReasonZeroDuration          = "zero_duration"
	ReasonVideoPayloadUncertain = "video_payload_uncertain"
	ReasonMissingStreamBitrate  = "missing_stream_bitrate"
	ReasonMissingSubtitleSize   = "missing_subtitle_size"
	ReasonUnusableEstimate      = "unusable_estimate"
	ReasonInvalidVideoEstimate  = "invalid_video_estimate"
	ReasonInvalidEstimatorInput = "invalid_estimator_input"
	ReasonIntegerOverflow       = "integer_overflow"
)

// ColorInfo captures the color space, transfer characteristics, and HDR metadata of a stream.
type ColorInfo struct {
	ColorPrimaries string `json:"color_primaries,omitempty"`
	ColorTransfer  string `json:"color_trc,omitempty"`
	ColorSpace     string `json:"color_space,omitempty"`
	PixelFormat    string `json:"pixel_format,omitempty"`
	BitDepth       int    `json:"bit_depth,omitempty"`
	HDRFormat      string `json:"hdr_format,omitempty"` // e.g. "hdr10", "hdr10+", "dovi", "hlg"
}

// IsHDR returns true if the stream color characteristics indicate HDR (High Dynamic Range)
// or wide color gamut that is ineligible for automatic standard SDR scoring models.
func IsHDR(c ColorInfo) bool {
	if strings.TrimSpace(c.HDRFormat) != "" {
		return true
	}

	transfer := strings.ToLower(strings.TrimSpace(c.ColorTransfer))
	switch transfer {
	case "smpte2084", "arib-std-b67", "arib_std_b67", "hlg", "pq", "smpte428", "bt2020-10", "bt2020-12":
		return true
	}

	primaries := strings.ToLower(strings.TrimSpace(c.ColorPrimaries))
	switch primaries {
	case "bt2020", "bt2020nc", "bt2020c", "dci-p3":
		return true
	}

	cs := strings.ToLower(strings.TrimSpace(c.ColorSpace))
	switch cs {
	case "bt2020nc", "bt2020c":
		return true
	}

	return false
}

// isFinite reports whether f is neither NaN nor an infinity.
func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// safeFloatToInt64 safely rounds a finite, non-negative float64 to int64, returning an error on overflow.
func safeFloatToInt64(f float64) (int64, error) {
	if !isFinite(f) {
		return 0, fmt.Errorf("%s: non-finite float value %f", ReasonInvalidEstimatorInput, f)
	}
	if f < 0 {
		return 0, fmt.Errorf("%s: negative value %f", ReasonInvalidEstimatorInput, f)
	}
	// math.MaxInt64 is 9223372036854775807 (~9.223372e18).
	if f > 9.223372036854774e18 {
		return 0, fmt.Errorf("%s: float value %e exceeds maximum int64", ReasonIntegerOverflow, f)
	}
	return int64(math.Round(f)), nil
}

// safeAddInt64 safely adds two non-negative int64 values, guarding against integer overflow.
func safeAddInt64(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, fmt.Errorf("%s: negative operand in safe addition (%d, %d)", ReasonInvalidEstimatorInput, a, b)
	}
	if a > math.MaxInt64-b {
		return 0, fmt.Errorf("%s: integer overflow adding %d and %d", ReasonIntegerOverflow, a, b)
	}
	return a + b, nil
}
