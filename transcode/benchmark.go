package transcode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

var (
	// validBenchmarkJobIDRegex enforces the distinct benchmark namespace and safe filesystem identifier:
	// must start with "bench-" followed by an alphanumeric character, and then alphanumeric, underscores, hyphens, or dots.
	validBenchmarkJobIDRegex = regexp.MustCompile(`^bench-[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	validCandidateIDRegex    = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
)

const (
	// MaxBenchmarkCandidates defines the conservative upper limit for candidates in a single benchmark.
	MaxBenchmarkCandidates = 32
	// MaxBenchmarkSamples defines the conservative upper limit for sample windows in a single benchmark.
	MaxBenchmarkSamples = 32
	// MaxBenchmarkIDLength bounds the length of benchmark job and candidate IDs.
	MaxBenchmarkIDLength = 128
	// DefaultEncodeConcurrency bounds simultaneous candidate sample encodes.
	DefaultEncodeConcurrency = 2
	// DefaultMetricConcurrency bounds simultaneous metric (VMAF/SSIM) evaluations.
	DefaultMetricConcurrency = 2
	// MaxBenchmarkConcurrency caps explicit encode/metric concurrency settings.
	MaxBenchmarkConcurrency = 4
)

// ValidateBenchmarkJobID validates that a job ID belongs to the benchmark namespace,
// has bounded length (7..128), and contains only safe filesystem characters without traversal.
func ValidateBenchmarkJobID(id string) error {
	trimmedID := strings.TrimSpace(id)
	if trimmedID == "" {
		return errors.New("benchmark job id is required")
	}
	if len(trimmedID) < 7 || len(trimmedID) > MaxBenchmarkIDLength {
		return fmt.Errorf("benchmark job id %q must be between 7 and %d characters", trimmedID, MaxBenchmarkIDLength)
	}
	if !strings.HasPrefix(trimmedID, "bench-") {
		return fmt.Errorf("invalid benchmark job id %q: must begin with required prefix 'bench-'", trimmedID)
	}
	if strings.ContainsAny(trimmedID, "/\\:\x00") {
		return fmt.Errorf("invalid benchmark job id %q: contains illegal characters or path separators", trimmedID)
	}
	if strings.Contains(trimmedID, "..") {
		return fmt.Errorf("invalid benchmark job id %q: path traversal attempt detected", trimmedID)
	}
	lowerID := strings.ToLower(trimmedID)
	if strings.Contains(lowerID, "%2f") || strings.Contains(lowerID, "%5c") {
		return fmt.Errorf("invalid benchmark job id %q: encoded path traversal detected", trimmedID)
	}
	if !validBenchmarkJobIDRegex.MatchString(trimmedID) {
		return fmt.Errorf("invalid benchmark job id %q: must match regex format ^bench-[a-zA-Z0-9][a-zA-Z0-9_.-]*$", trimmedID)
	}

	reservedNames := map[string]bool{
		"bench-samples": true,
		"bench-scratch": true,
		"bench-lock":    true,
		"bench-con":     true,
		"bench-prn":     true,
		"bench-aux":     true,
		"bench-nul":     true,
		"bench-com1":    true,
		"bench-com2":    true,
		"bench-lpt1":    true,
	}
	if reservedNames[lowerID] {
		return fmt.Errorf("invalid benchmark job id %q: reserved filesystem identifier", trimmedID)
	}
	return nil
}

// BenchmarkQualityThresholds specifies target, minimum acceptable score, and optional marginal tolerance.
type BenchmarkQualityThresholds struct {
	Target                  float64  `json:"target,omitempty"`
	Minimum                 float64  `json:"minimum,omitempty"`
	MarginalTolerance       *float64 `json:"marginal_tolerance,omitempty"`
	Model                   string   `json:"model,omitempty"`
	GuardrailEnforcement    string   `json:"guardrail_enforcement,omitempty"`
	P5Minimum               *float64 `json:"p5_minimum,omitempty"`
	WorstWindowMinimum      *float64 `json:"worst_window_minimum,omitempty"`
	WorstWindowSeconds      float64  `json:"worst_window_seconds,omitempty"`
	FrameThreshold          *float64 `json:"frame_threshold,omitempty"`
	MaxFramesBelowThreshold *int     `json:"max_frames_below_threshold,omitempty"`
}

type BenchmarkBandingConfig struct {
	Enabled     bool     `json:"enabled"`
	Metric      string   `json:"metric,omitempty"`
	Mode        string   `json:"mode,omitempty"`
	Enforcement string   `json:"enforcement,omitempty"`
	MaxMean     *float64 `json:"max_mean,omitempty"`
	MaxPeak     *float64 `json:"max_peak,omitempty"`
}

type BenchmarkFinalValidationConfig struct {
	Mode string `json:"mode,omitempty"`
}

type BenchmarkCandidateQualityEvidence struct {
	CandidateID           string              `json:"candidate_id"`
	CapabilityFingerprint string              `json:"capability_fingerprint,omitempty"`
	VMAF                  *quality.VMAFStats  `json:"vmaf,omitempty"`
	CAMBI                 *quality.CAMBIStats `json:"cambi,omitempty"`
	Verdict               string              `json:"verdict"`
	ReasonCodes           []string            `json:"reason_codes,omitempty"`
}

// BenchmarkQualityConfig configures quality evaluation policies for a benchmark run.
type BenchmarkQualityConfig struct {
	PreferredMetric string                          `json:"preferred_metric,omitempty"`
	VMAF            *BenchmarkQualityThresholds     `json:"vmaf,omitempty"`
	SSIM            *BenchmarkQualityThresholds     `json:"ssim,omitempty"`
	Banding         *BenchmarkBandingConfig         `json:"banding,omitempty"`
	FinalValidation *BenchmarkFinalValidationConfig `json:"final_validation,omitempty"`
}

// BenchmarkAdaptiveConfig selects the candidate evaluation strategy.
// A nil Adaptive field preserves the existing exhaustive path unchanged.
// Mode "adaptive" enables ordered probing starting near InitialQuality with
// exhaustive fallback; any other/empty mode is exhaustive.
type BenchmarkAdaptiveConfig struct {
	Mode           string `json:"mode,omitempty"`
	InitialQuality int    `json:"initial_quality,omitempty"`
}

// BenchmarkConcurrencyConfig bounds concurrent benchmark execution.
// A nil Concurrency field selects conservative defaults (2 encoders, 2 metrics).
// Zero values inside select the corresponding default; explicit values are
// bounded to MaxBenchmarkConcurrency to avoid VideoToolbox/CPU contention.
type BenchmarkConcurrencyConfig struct {
	EncodeConcurrency int `json:"encode_concurrency,omitempty"`
	MetricConcurrency int `json:"metric_concurrency,omitempty"`
}

// BenchmarkWinner records the selected winning candidate and its concrete parameters.
type BenchmarkWinner struct {
	X265                      *X265Settings `json:"x265,omitempty"`
	CandidateID               string        `json:"candidate_id"`
	CandidateIndex            int           `json:"candidate_index"`
	VideoCodec                string        `json:"video_codec,omitempty"`
	Quality                   int           `json:"quality"`
	Preset                    string        `json:"preset,omitempty"`
	Tune                      string        `json:"tune,omitempty"`
	AverageBitrateKbps        int           `json:"average_bitrate_kbps,omitempty"`
	MaxBitrateKbps            int           `json:"max_bitrate_kbps,omitempty"`
	ConstantBitrate           *bool         `json:"constant_bitrate,omitempty"`
	QMin                      *int          `json:"qmin,omitempty"`
	QMax                      *int          `json:"qmax,omitempty"`
	GOPSize                   *int          `json:"gop_size,omitempty"`
	BFrames                   *int          `json:"b_frames,omitempty"`
	ClosedGOP                 *bool         `json:"closed_gop,omitempty"`
	PowerEfficient            *bool         `json:"power_efficient,omitempty"`
	MaxRefFrames              *int          `json:"max_ref_frames,omitempty"`
	PrioritizeSpeed           *bool         `json:"prioritize_speed,omitempty"`
	SpatialAQ                 *bool         `json:"spatial_aq,omitempty"`
	Realtime                  *bool         `json:"realtime,omitempty"`
	VideoProfile              string        `json:"video_profile,omitempty"`
	PixelFormat               string        `json:"pixel_format,omitempty"`
	ExpectedBitDepth          int           `json:"expected_bit_depth"`
	MetricType                string        `json:"metric_type"`
	Score                     float64       `json:"score"`
	TargetReached             bool          `json:"target_reached"`
	MinimumMet                bool          `json:"minimum_met"`
	EstimatedVideoBytes       int64         `json:"estimated_video_bytes"`
	EstimatedAudioBytes       int64         `json:"estimated_audio_bytes"`
	EstimatedSubtitleBytes    int64         `json:"estimated_subtitle_bytes"`
	EstimatedAttachmentBytes  int64         `json:"estimated_attachment_bytes"`
	EstimatedMuxOverheadBytes int64         `json:"estimated_mux_overhead_bytes"`
	EstimatedTotalBytes       int64         `json:"estimated_total_bytes"`
	EstimatedTotalMB          float64       `json:"estimated_total_mb"`
	SavingsBytes              int64         `json:"savings_bytes"`
	SavingsPercent            float64       `json:"savings_percent"`
	Uncertainties             []string      `json:"uncertainties,omitempty"`
}

// BenchmarkCandidateEvaluation records the evaluation summary for one candidate.
type BenchmarkCandidateEvaluation struct {
	MeasuredSeconds    *float64                 `json:"measured_seconds,omitempty"`
	FailedSamples      []BenchmarkSampleFailure `json:"failed_samples,omitempty"`
	CandidateID        string                   `json:"candidate_id"`
	CandidateIndex     int                      `json:"candidate_index"`
	VideoCodec         string                   `json:"video_codec,omitempty"`
	Quality            int                      `json:"quality"`
	Preset             string                   `json:"preset,omitempty"`
	Tune               string                   `json:"tune,omitempty"`
	AverageBitrateKbps int                      `json:"average_bitrate_kbps,omitempty"`
	VideoProfile       string                   `json:"video_profile,omitempty"`
	PixelFormat        string                   `json:"pixel_format,omitempty"`
	ExpectedBitDepth   int                      `json:"expected_bit_depth"`
	Score              float64                  `json:"score"`
	MetricType         string                   `json:"metric_type"`
	Eligible           bool                     `json:"eligible"`
	TargetReached      bool                     `json:"target_reached"`
	MinimumMet         bool                     `json:"minimum_met"`
	EvaluationReason   string                   `json:"evaluation_reason"`
	EstimatedBytes     int64                    `json:"estimated_bytes"`
	EstimatedMB        float64                  `json:"estimated_mb"`
	SavingsPercent     float64                  `json:"savings_percent"`
	Uncertainties      []string                 `json:"uncertainties,omitempty"`
	// VMAF is the measured distribution behind Score, kept with the decision
	// so later reviews can compare rounds without the transient worker status.
	VMAF *BenchmarkVMAFSummary `json:"vmaf,omitempty"`
	// Reuse is set only when some of this candidate's sample measurements were
	// replayed from the worker's sample cache instead of measured in this run.
	// Replayed samples count toward selection exactly like measured ones.
	Reuse *BenchmarkSampleReuse `json:"reuse,omitempty"`
}

// BenchmarkSampleReuse counts a candidate's sample windows replayed from an
// earlier measurement of the same source bytes, out of all its sample windows.
type BenchmarkSampleReuse struct {
	Reused int `json:"reused"`
	Total  int `json:"total"`
}

// BenchmarkVMAFSummary is one candidate's pooled VMAF over its sampled frames.
type BenchmarkVMAFSummary struct {
	Model              string  `json:"model,omitempty"`
	Mean               float64 `json:"mean"`
	P5                 float64 `json:"p5"`
	Min                float64 `json:"min"`
	WorstWindowMean    float64 `json:"worst_window_mean,omitempty"`
	WorstWindowSeconds float64 `json:"worst_window_seconds,omitempty"`
	FramesTotal        int     `json:"frames_total,omitempty"`
	TemporalDecimation int     `json:"temporal_decimation,omitempty"`
}

// SummarizeVMAF keeps the reviewable part of a candidate's VMAF statistics.
func SummarizeVMAF(stats *quality.VMAFStats) *BenchmarkVMAFSummary {
	if stats == nil || stats.FramesTotal <= 0 {
		return nil
	}
	return &BenchmarkVMAFSummary{Model: stats.ModelID, Mean: stats.Mean, P5: stats.P5, Min: stats.Min, WorstWindowMean: stats.WorstWindowMean, WorstWindowSeconds: stats.WorstWindowSeconds, FramesTotal: stats.FramesTotal, TemporalDecimation: stats.TemporalDecimation}
}

// AttachVMAFEvidence copies each candidate's measured VMAF summary from the
// quality evidence into its evaluation, without replacing one already set.
func AttachVMAFEvidence(decision *BenchmarkDecision, evidence []BenchmarkCandidateQualityEvidence) {
	if decision == nil {
		return
	}
	for i := range decision.Evaluations {
		ev := &decision.Evaluations[i]
		if ev.VMAF != nil {
			continue
		}
		for _, q := range evidence {
			if q.CandidateID == ev.CandidateID && q.VMAF != nil {
				ev.VMAF = SummarizeVMAF(q.VMAF)
				break
			}
		}
	}
}

// BenchmarkDecision records the explainable Phase 6 candidate selection outcome.
type BenchmarkDecision struct {
	Selection      *optimization.SelectionExplanation `json:"selection,omitempty"`
	Winner         *BenchmarkWinner                   `json:"winner,omitempty"`
	DecisionReason string                             `json:"decision_reason"`
	Evaluations    []BenchmarkCandidateEvaluation     `json:"evaluations"`
}

type BenchmarkSampleFailure struct {
	SampleIndex   int     `json:"sample_index"`
	SourceSeconds float64 `json:"source_seconds"`
	Metric        string  `json:"metric"`
	Actual        float64 `json:"actual"`
	Minimum       float64 `json:"minimum"`
}

// BenchmarkCandidate specifies one encoder candidate to evaluate during a benchmark.
// Arbitrary ffmpeg arguments are strictly forbidden.
type BenchmarkCandidate struct {
	X265 *X265Settings `json:"x265,omitempty"`
	ID   string        `json:"id"`
	// VideoCodec selects the encoder. Empty means hevc_videotoolbox for
	// backwards compatibility. libx265 is also accepted.
	VideoCodec string `json:"video_codec,omitempty"`
	// Quality is -q:v for VideoToolbox (1..100, higher = better) and CRF for
	// libx265 (1..51, lower = better), or zero for explicit X265 ABR settings.
	// For VideoToolbox average-bitrate mode,
	// Quality must be 0 and AverageBitrateKbps carries the -b:v target.
	Quality int `json:"quality"`
	// Preset is the libx265 preset; it must be empty for hevc_videotoolbox.
	Preset string `json:"preset,omitempty"`
	// Tune is a bounded libx265 tune and must be empty for VideoToolbox.
	Tune string `json:"tune,omitempty"`
	// Bounded typed hevc_videotoolbox rate-control/offline knobs. All must be
	// unset for libx265. All other knobs besides the swept rate dimension are
	// inherited from the profile's resolved base plan and held fixed.
	AverageBitrateKbps int    `json:"average_bitrate_kbps,omitempty"`
	MaxBitrateKbps     int    `json:"max_bitrate_kbps,omitempty"`
	ConstantBitrate    *bool  `json:"constant_bitrate,omitempty"`
	QMin               *int   `json:"qmin,omitempty"`
	QMax               *int   `json:"qmax,omitempty"`
	GOPSize            *int   `json:"gop_size,omitempty"`
	BFrames            *int   `json:"b_frames,omitempty"`
	ClosedGOP          *bool  `json:"closed_gop,omitempty"`
	PowerEfficient     *bool  `json:"power_efficient,omitempty"`
	MaxRefFrames       *int   `json:"max_ref_frames,omitempty"`
	PrioritizeSpeed    *bool  `json:"prioritize_speed,omitempty"`
	SpatialAQ          *bool  `json:"spatial_aq,omitempty"`
	Realtime           *bool  `json:"realtime,omitempty"`
	VideoProfile       string `json:"video_profile,omitempty"`
	PixelFormat        string `json:"pixel_format,omitempty"`
}

// BenchmarkCandidateVideoCodec resolves the effective encoder for a candidate.
func BenchmarkCandidateVideoCodec(c BenchmarkCandidate) string {
	codec := NormalizeVideoCodec(c.VideoCodec)
	if codec == "" {
		return VideoCodecHEVCVideoToolbox
	}
	return codec
}

// BenchmarkSampleWindow specifies one temporal window to sample from the source.
type BenchmarkSampleWindow struct {
	Index           int     `json:"index"`
	StartSeconds    float64 `json:"start_seconds"`
	DurationSeconds float64 `json:"duration_seconds"`
	CenterSeconds   float64 `json:"center_seconds,omitempty"`
}

// Search extension bounds. The longest a search can run, verification included,
// never exceeds the largest finite search budget a request may carry.
const (
	MaxSearchBudgetSeconds = 3600
	MinSearchSteadySeconds = 30
	MaxSearchSteadySeconds = 600
)

// SearchExtension is the frozen rule for running a finite search past its base
// budget. After the base budget the worker keeps going only while a unit of
// work (sample extraction, encode or metric pass) finished within the last
// SteadySeconds, and stops at MaxSeconds from the search start. A search that
// has begun verifying its winner on holdout windows may run VerifySeconds
// beyond that, so a winner found just before the cap is not thrown away
// unverified.
type SearchExtension struct {
	MaxSeconds    int `json:"max_seconds"`
	SteadySeconds int `json:"steady_seconds"`
	VerifySeconds int `json:"verify_seconds,omitempty"`
}

// TotalSeconds is the longest a search can run under the rule, whatever its
// progress: MaxSeconds plus the verification allowance.
func (e *SearchExtension) TotalSeconds() int {
	if e == nil {
		return 0
	}
	return e.MaxSeconds + e.VerifySeconds
}

// Validate reports whether the extension is a bounded rule over base.
func (e *SearchExtension) Validate(base int) error {
	if e == nil {
		return nil
	}
	// Each field is bounded before they are summed, so the sum cannot wrap.
	if base <= 0 || e.MaxSeconds <= base || e.MaxSeconds > MaxSearchBudgetSeconds || e.VerifySeconds < 0 || e.VerifySeconds > MaxSearchBudgetSeconds || e.TotalSeconds() > MaxSearchBudgetSeconds {
		return fmt.Errorf("search extension max_seconds must be above the base budget and, with verify_seconds, at most %d", MaxSearchBudgetSeconds)
	}
	if e.SteadySeconds < MinSearchSteadySeconds || e.SteadySeconds > MaxSearchSteadySeconds {
		return fmt.Errorf("search extension steady_seconds must be in [%d,%d]", MinSearchSteadySeconds, MaxSearchSteadySeconds)
	}
	return nil
}

// Clone returns an independent copy.
func (e *SearchExtension) Clone() *SearchExtension {
	if e == nil {
		return nil
	}
	c := *e
	return &c
}

// Automatic search bounds. A size search under AutomaticSearchRules adds a
// medium-preset quality fallback beside the slow one, so it may carry more
// candidates; every other automatic search keeps the original twelve.
const (
	MaxAutomaticCandidates      = 12
	MaxAutomaticRulesCandidates = 15
	// MaxAutomaticHardwareCandidates leaves room for the VideoToolbox
	// quality ladder and its higher-quality fallback rungs beside the rules
	// candidates (15 + 4 + 3).
	MaxAutomaticHardwareCandidates = 22
	// MaxHardwareMarginPercent bounds how much larger than the x265 winner a
	// hardware candidate may be and still win.
	MaxHardwareMarginPercent = 50
	// MaxHardwareQualityValues bounds the VideoToolbox quality ladder.
	MaxHardwareQualityValues = 4
	// MaxHardwareFallbackQualityValues bounds the higher-quality VideoToolbox
	// rungs tried when the whole ladder fails quality.
	MaxHardwareFallbackQualityValues = 3
	// HardwareFallbackCandidatePrefix begins the ID of a higher-quality
	// VideoToolbox fallback candidate. It starts with "fallback-", so the
	// worker keeps it out of the initial stage.
	HardwareFallbackCandidatePrefix = "fallback-hardware-"
	MaxSelectionMarginVMAF          = 10
	// HoldoutWindowsFinalGridEdges centres the two holdout windows on the
	// first and last positions of the final validation grid.
	HoldoutWindowsFinalGridEdges = "final_grid_edges"
)

// AutomaticSearchRules are the size-mode search rules frozen with a policy and
// sent to workers that advertise automatic_search_rules. A request without
// them keeps the rules automatic search had before they existed.
//
// With rules, a holdout that fails on quality blocks that setting and every
// more-compressed setting (higher CRF), the next verification goes to a safer
// (lower CRF) setting, and the fallback family is the quality one.
type AutomaticSearchRules struct {
	// SelectionMarginVMAF: a candidate can be selected only when its VMAF p5
	// and worst-window mean clear their minimums by at least this much. The
	// minimums are unchanged; a thinner pass is a quality result
	// (quality_vmaf_selection_margin_not_met), not a pass.
	SelectionMarginVMAF float64 `json:"selection_margin_vmaf"`
	// HoldoutWindows places the two holdout windows. HoldoutWindowsFinalGridEdges
	// covers the start and end of the source that final validation checks;
	// empty keeps 35% and 65%.
	HoldoutWindows string `json:"holdout_windows,omitempty"`
	// QualityFallbackPresets orders the quality fallback family's presets: a
	// later preset is measured only when the earlier ones found nothing.
	QualityFallbackPresets []string `json:"quality_fallback_presets,omitempty"`
}

// Validate reports whether the rules are bounded and well formed.
func (r *AutomaticSearchRules) Validate() error {
	if r == nil {
		return nil
	}
	if math.IsNaN(r.SelectionMarginVMAF) || r.SelectionMarginVMAF < 0 || r.SelectionMarginVMAF > MaxSelectionMarginVMAF {
		return fmt.Errorf("automatic search selection_margin_vmaf must be in [0,%d]", MaxSelectionMarginVMAF)
	}
	if r.HoldoutWindows != "" && r.HoldoutWindows != HoldoutWindowsFinalGridEdges {
		return fmt.Errorf("unsupported automatic search holdout_windows %q", r.HoldoutWindows)
	}
	if len(r.QualityFallbackPresets) > 3 {
		return errors.New("automatic search allows at most three quality fallback presets")
	}
	seen := map[string]bool{}
	for _, preset := range r.QualityFallbackPresets {
		if preset == "" || preset != strings.ToLower(strings.TrimSpace(preset)) || seen[preset] {
			return fmt.Errorf("invalid automatic search quality fallback preset %q", preset)
		}
		seen[preset] = true
	}
	return nil
}

// Clone returns an independent copy.
func (r *AutomaticSearchRules) Clone() *AutomaticSearchRules {
	if r == nil {
		return nil
	}
	c := *r
	c.QualityFallbackPresets = append([]string(nil), r.QualityFallbackPresets...)
	return &c
}

// HardwareSearchRules are the frozen rules of a size search that allows the
// media engine. VideoToolbox candidates are measured beside the x265 ones and
// pass the same quality gates and selection margin. The smallest qualifying
// one wins when it is at most MarginPercent larger than the smallest
// qualifying x265 candidate, or when no x265 candidate qualifies; otherwise
// x265 wins as without them. Absent rules mean the search is x265 only.
type HardwareSearchRules struct {
	// MarginPercent is how much larger (percent) than the best qualifying x265
	// candidate a hardware candidate may be and still win.
	MarginPercent float64 `json:"margin_percent"`
	// QualityValues are the VideoToolbox -q:v values measured, ascending.
	QualityValues []int `json:"quality_values"`
	// GOPSize is the keyframe interval of every hardware candidate. The
	// encoder's own default (12 frames) makes files about three times larger.
	GOPSize int `json:"gop_size"`
	// BFrames is 0 or 1 (the encoder only knows whether B-frames are on).
	BFrames int `json:"b_frames"`
	// FallbackQualityValues are higher-quality -q:v rungs (ascending, all above
	// QualityValues) measured, with the same GOP and B-frames, only when every
	// ladder rung failed the quality policy. They are tried beside the first
	// x265 quality fallback stage, so a fast hardware result can win within
	// the margin before the slow x265 stage runs. Absent means the ladder has
	// no fallback, which is what snapshots frozen before it carry.
	FallbackQualityValues []int `json:"fallback_quality_values,omitempty"`
}

// Validate reports whether the rules are bounded and well formed.
func (r *HardwareSearchRules) Validate() error {
	if r == nil {
		return nil
	}
	if math.IsNaN(r.MarginPercent) || r.MarginPercent < 0 || r.MarginPercent > MaxHardwareMarginPercent {
		return fmt.Errorf("hardware search margin_percent must be in [0,%d]", MaxHardwareMarginPercent)
	}
	if len(r.QualityValues) == 0 || len(r.QualityValues) > MaxHardwareQualityValues {
		return fmt.Errorf("hardware search needs 1 to %d quality values", MaxHardwareQualityValues)
	}
	prev := 0
	for _, q := range r.QualityValues {
		if q < 1 || q > 100 || q <= prev {
			return errors.New("hardware search quality values must be ascending and in 1..100")
		}
		prev = q
	}
	if len(r.FallbackQualityValues) > MaxHardwareFallbackQualityValues {
		return fmt.Errorf("hardware search needs at most %d fallback quality values", MaxHardwareFallbackQualityValues)
	}
	prev = r.QualityValues[len(r.QualityValues)-1]
	for _, q := range r.FallbackQualityValues {
		if q > 100 || q <= prev {
			return errors.New("hardware fallback quality values must be ascending, above the ladder and at most 100")
		}
		prev = q
	}
	if r.GOPSize < 1 || r.GOPSize > MaxBenchmarkGOPSize {
		return fmt.Errorf("hardware search gop_size must be in 1..%d", MaxBenchmarkGOPSize)
	}
	if r.BFrames < 0 || r.BFrames > MaxBenchmarkBFrames {
		return fmt.Errorf("hardware search b_frames must be in 0..%d", MaxBenchmarkBFrames)
	}
	return nil
}

// Clone returns an independent copy.
func (r *HardwareSearchRules) Clone() *HardwareSearchRules {
	if r == nil {
		return nil
	}
	c := *r
	c.QualityValues = append([]int(nil), r.QualityValues...)
	c.FallbackQualityValues = append([]int(nil), r.FallbackQualityValues...)
	return &c
}

// BenchmarkRequest defines the parameters sent from the coordinator to the worker to execute a benchmark.
// It is strictly versioned and does NOT accept candidate output paths or replace_original parameters,
// making original media mutation completely impossible.
type BenchmarkRequest struct {
	Automatic bool `json:"automatic,omitempty"`
	// QualityPruning lets automatic search skip the rest of a rate-control
	// family once its highest-quality setting fails the quality policy.
	QualityPruning bool `json:"quality_pruning,omitempty"`
	// MinSavingsPercent lets the worker stop measuring once every encoded
	// candidate is already too large for the caller's size guardrail.
	MinSavingsPercent   float64   `json:"min_savings_percent,omitempty"`
	ModePolicyVersion   string    `json:"mode_policy_version,omitempty"`
	SearchBudgetSeconds int       `json:"search_budget_seconds,omitempty"`
	Mode                string    `json:"mode,omitempty"`
	SearchDeadline      time.Time `json:"search_deadline,omitzero"`
	// SearchExtension lets a mode search run past SearchBudgetSeconds while it
	// keeps completing work, up to a hard cap. Unset keeps the fixed budget.
	SearchExtension *SearchExtension `json:"search_extension,omitempty"`
	// AutomaticSearch carries size-mode search rules to workers that
	// advertise automatic_search_rules. Unset keeps the earlier rules.
	AutomaticSearch *AutomaticSearchRules `json:"automatic_search,omitempty"`
	// HardwareSearch carries the frozen rules of a size search that allows the
	// media engine to workers that advertise hardware_search. Unset keeps the
	// search x265 only.
	HardwareSearch *HardwareSearchRules `json:"hardware_search,omitempty"`
	// Allow8BitTo10Bit is an explicit experiment opt-in, never an automatic
	// optimization. Quality measurement must use an explicit native-depth model.
	Allow8BitTo10Bit          bool                        `json:"allow_8bit_to_10bit,omitempty"`
	AudioMode                 string                      `json:"audio_mode,omitempty"`
	ProtocolVersion           int                         `json:"protocol_version"`
	ID                        string                      `json:"id"`
	SourcePath                string                      `json:"source_path"`
	SourceDuration            float64                     `json:"source_duration,omitempty"`
	Metric                    string                      `json:"metric"` // "vmaf", "ssim", "both"
	Samples                   []BenchmarkSampleWindow     `json:"samples"`
	Candidates                []BenchmarkCandidate        `json:"candidates"`
	Quality                   *BenchmarkQualityConfig     `json:"quality,omitempty"`
	Adaptive                  *BenchmarkAdaptiveConfig    `json:"adaptive,omitempty"`
	Concurrency               *BenchmarkConcurrencyConfig `json:"concurrency,omitempty"`
	FallbackAudioBitrateBps   int64                       `json:"fallback_audio_bitrate_bps,omitempty"`
	FallbackSubtitleSizeBytes int64                       `json:"fallback_subtitle_size_bytes,omitempty"`
	DeclaredVideoBitrateBps   int64                       `json:"declared_video_bitrate_bps,omitempty"`
	AttachmentBytes           int64                       `json:"attachment_bytes,omitempty"`
	// SourceSHA256 is the coordinator preflight's SHA-256 of the original source
	// bytes. Optional for legacy callers; it binds the shared source cache entry
	// to content identity so a same path/size/mtime source with changed content
	// can never be reused.
	SourceSHA256 string `json:"source_sha256,omitempty"`
	// Compact applies the frozen compact transform to every candidate and to
	// the metric reference. Workers without the compact_transcode capability
	// reject the field as unknown.
	Compact *CompactPlan `json:"compact,omitempty"`
}

// BenchmarkJob is the receipt returned upon successful submission of a benchmark request.
type BenchmarkJob struct {
	ID string `json:"id"`
}

// BenchmarkSubmitResponse defines the worker's JSON response for benchmark_submit.
type BenchmarkSubmitResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	ID              string `json:"id"`
	Status          string `json:"status"` // "queued", "running", "completed"
	Error           string `json:"error,omitempty"`
}

// BenchmarkCancelResponse defines the worker's JSON response for benchmark_cancel.
type BenchmarkCancelResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	ID              string `json:"id"`
	Status          string `json:"status"` // "cancelled"
	Error           string `json:"error,omitempty"`
}

// BenchmarkProgressDetails describes the latest started unit and resolved work
// count. Encodes and metrics can overlap; this is not an exclusive active slot.
// SampleNumber and CandidateNumber are one-based positions in the original plan.
type BenchmarkProgressDetails struct {
	SampleNumber    int    `json:"sample_number,omitempty"`
	CandidateNumber int    `json:"candidate_number,omitempty"`
	CandidateID     string `json:"candidate_id,omitempty"`
	Metric          string `json:"metric,omitempty"`
	CompletedUnits  int    `json:"completed_units"`
	TotalUnits      int    `json:"total_units"`
}

// Automatic evidence explains one bounded search; discovery is not final validation.
type BenchmarkAutomaticStage struct {
	Stage      string   `json:"stage"`
	Candidates []string `json:"candidates"`
	// Skipped lists candidates not measured because a higher-quality setting
	// of the same family already failed the quality policy.
	Skipped []string `json:"skipped,omitempty"`
	Outcome string   `json:"outcome"`
}
type BenchmarkAutomaticEvidence struct {
	Stages          []BenchmarkAutomaticStage `json:"stages"`
	Discovery       *BenchmarkDecision        `json:"discovery,omitempty"`
	VerifiedSamples int                       `json:"verified_samples"`
}

// BenchmarkStatus captures the current execution status and metadata of a benchmark job.
// SearchWorkerBusy is set when a search stopped at its budget and another job
// held a worker slot for most of it, so the shortfall says nothing about the
// source.
type BenchmarkStatus struct {
	Automatic           *BenchmarkAutomaticEvidence         `json:"automatic,omitempty"`
	SearchSeconds       *float64                            `json:"search_seconds,omitempty"`
	PhaseCosts          map[string]PhaseCost                `json:"phase_costs,omitempty"`
	ReasonCode          string                              `json:"reason_code,omitempty"`
	SearchStartedAt     time.Time                           `json:"search_started_at,omitzero"`
	SearchBudgetSeconds int                                 `json:"search_budget_seconds,omitempty"`
	SearchExtension     *SearchExtension                    `json:"search_extension,omitempty"`
	SearchWorkerBusy    bool                                `json:"search_worker_busy,omitempty"`
	ComparisonAvailable bool                                `json:"comparison_available,omitempty"`
	ProtocolVersion     int                                 `json:"protocol_version"`
	ID                  string                              `json:"id"`
	Status              string                              `json:"status"` // queued, running, completed, failed, cancelled
	SourcePath          string                              `json:"source_path"`
	Metric              string                              `json:"metric,omitempty"`
	Progress            float64                             `json:"progress"`
	Phase               string                              `json:"phase,omitempty"`
	HeartbeatAt         time.Time                           `json:"heartbeat_at,omitempty"`
	LastProgressAt      time.Time                           `json:"last_progress_at,omitempty"`
	ProgressIsStale     bool                                `json:"progress_is_stale"`
	ProgressDetails     *BenchmarkProgressDetails           `json:"progress_details,omitempty"`
	Error               string                              `json:"error,omitempty"`
	SamplesPlanned      int                                 `json:"samples_planned"`
	CandidatesCount     int                                 `json:"candidates_count"`
	Attempt             int                                 `json:"attempt,omitempty"`
	CreatedAt           time.Time                           `json:"created_at"`
	StartedAt           time.Time                           `json:"started_at,omitempty"`
	FinishedAt          time.Time                           `json:"finished_at,omitempty"`
	Decision            *BenchmarkDecision                  `json:"decision,omitempty"`
	Quality             []BenchmarkCandidateQualityEvidence `json:"quality,omitempty"`
}

// DigestBenchmarkRequest computes a deterministic sha256 digest of the benchmark request payload.
func DigestBenchmarkRequest(req *BenchmarkRequest) (string, error) {
	if req == nil {
		return "", errors.New("benchmark request is nil")
	}
	cp := *req
	data, err := json.Marshal(cp)
	if err != nil {
		return "", fmt.Errorf("serializing benchmark request for digest: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ValidateBenchmarkRequest enforces strict validation on the benchmark request before submission.
func ValidateBenchmarkRequest(req *BenchmarkRequest) error {
	if req == nil {
		return errors.New("benchmark request cannot be nil")
	}
	if req.ModePolicyVersion != "" && req.ModePolicyVersion != "transcode-modes-v1" && req.ModePolicyVersion != optimization.ModePolicyV2 && req.ModePolicyVersion != optimization.ModePolicyV3 {
		return errors.New("unsupported mode policy version")
	}
	sizeV3 := req.ModePolicyVersion == optimization.ModePolicyV3
	if sizeV3 && req.Mode != "size" {
		return errors.New("mode policy v3 is only defined for size mode")
	}
	maxAutomatic := MaxAutomaticCandidates
	if req.HardwareSearch != nil {
		if req.AutomaticSearch == nil || !req.Automatic || !sizeV3 {
			return errors.New("hardware search requires an automatic size v3 search with automatic search rules")
		}
		if err := req.HardwareSearch.Validate(); err != nil {
			return err
		}
	}
	fallbackCandidates := 0
	for _, c := range req.Candidates {
		if !strings.HasPrefix(c.ID, HardwareFallbackCandidatePrefix) {
			continue
		}
		fallbackCandidates++
		if req.HardwareSearch == nil || len(req.HardwareSearch.FallbackQualityValues) == 0 || BenchmarkCandidateVideoCodec(c) != VideoCodecHEVCVideoToolbox {
			return errors.New("hardware fallback candidates require hardware search rules with fallback quality values and a VideoToolbox encoder")
		}
		// A fallback candidate is one of the frozen rungs, encoded the way the
		// rules say; anything else is not the search the policy froze.
		rules := req.HardwareSearch
		if !slices.Contains(rules.FallbackQualityValues, c.Quality) || c.GOPSize == nil || *c.GOPSize != rules.GOPSize || c.BFrames == nil || *c.BFrames != rules.BFrames || c.Preset != "" || c.Tune != "" || c.AverageBitrateKbps != 0 {
			return fmt.Errorf("hardware fallback candidate %q does not match the frozen hardware search rules", c.ID)
		}
	}
	if fallbackCandidates > MaxHardwareFallbackQualityValues {
		return fmt.Errorf("at most %d hardware fallback candidates", MaxHardwareFallbackQualityValues)
	}
	if req.AutomaticSearch != nil {
		if !req.Automatic || !sizeV3 {
			return errors.New("automatic search rules require an automatic size v3 search")
		}
		if err := req.AutomaticSearch.Validate(); err != nil {
			return err
		}
		maxAutomatic = MaxAutomaticRulesCandidates
		if req.HardwareSearch != nil {
			maxAutomatic = MaxAutomaticHardwareCandidates
		}
	}
	if req.Automatic && (req.Mode == "" || req.ModePolicyVersion != optimization.ModePolicyV2 && !sizeV3 || len(req.Samples) != 5 || len(req.Candidates) > maxAutomatic) {
		return fmt.Errorf("automatic search requires v2 or v3 mode, five windows and at most %d candidates", maxAutomatic)
	}
	if req.QualityPruning && !req.Automatic {
		return errors.New("quality pruning requires automatic search")
	}
	if req.SearchExtension != nil && req.Mode == "" {
		return errors.New("search extension requires a mode search budget")
	}
	if req.Compact != nil {
		if err := req.Compact.Validate(); err != nil {
			return err
		}
		if req.Mode != "compact" || req.Metric != "vmaf" || req.Quality == nil || req.Quality.VMAF == nil || req.Quality.VMAF.Model == "" {
			return errors.New("compact benchmark requires compact mode with an explicit VMAF model")
		}
	} else if req.Mode == "compact" {
		return errors.New("compact mode benchmark requires a compact plan")
	}
	if math.IsNaN(req.MinSavingsPercent) || req.MinSavingsPercent < 0 || req.MinSavingsPercent >= 100 || (req.MinSavingsPercent > 0 && (req.Automatic || req.Mode != "")) {
		return errors.New("min_savings_percent must be in [0,100) and only for profile benchmarks")
	}
	if req.Automatic {
		if req.Metric != "vmaf" || req.Quality == nil || req.Quality.VMAF == nil || req.Quality.VMAF.Model == "" {
			return errors.New("automatic search requires explicit VMAF quality policy")
		}
		for i, a := range req.Samples {
			for _, b := range req.Samples[i+1:] {
				if a.StartSeconds < b.StartSeconds+b.DurationSeconds && b.StartSeconds < a.StartSeconds+a.DurationSeconds {
					return errors.New("automatic quality windows must not overlap")
				}
			}
		}
	}
	if req.Mode != "" {
		if req.Mode != "size" && req.Mode != "quality" && req.Mode != "x265_preserve" && req.Mode != "compact" {
			return errors.New("unsupported processing mode")
		}
		if req.SearchBudgetSeconds <= 0 || req.SearchBudgetSeconds > MaxSearchBudgetSeconds {
			return errors.New("mode search requires finite search budget (1..3600 seconds)")
		}
		if err := req.SearchExtension.Validate(req.SearchBudgetSeconds); err != nil {
			return err
		}
		// Size v3 encodes 8-bit sources as Main10; no other mode converts.
		if req.Allow8BitTo10Bit && !sizeV3 {
			return errors.New("mode cannot opt into a bit-depth conversion")
		}
		if req.Mode == "x265_preserve" {
			for _, candidate := range req.Candidates {
				if BenchmarkCandidateVideoCodec(candidate) != VideoCodecLibX265 {
					return errors.New("x265_preserve requires libx265 candidates only")
				}
			}
		}
	}

	if req.ProtocolVersion != WorkerProtocolVersion {
		return fmt.Errorf("benchmark request protocol_version %d does not match expected %d (fail closed)",
			req.ProtocolVersion, WorkerProtocolVersion)
	}

	if err := ValidateBenchmarkJobID(req.ID); err != nil {
		return err
	}

	trimmedSource := strings.TrimSpace(req.SourcePath)
	if trimmedSource == "" {
		return errors.New("source_path is required")
	}
	if strings.ContainsRune(trimmedSource, '\x00') {
		return errors.New("source_path contains null bytes")
	}

	if req.SourceDuration < 0 || math.IsNaN(req.SourceDuration) || math.IsInf(req.SourceDuration, 0) {
		return fmt.Errorf("invalid source_duration %v: must be non-negative finite number", req.SourceDuration)
	}

	normMetric := strings.ToLower(strings.TrimSpace(req.Metric))
	if req.Allow8BitTo10Bit && (normMetric != "vmaf" || req.Quality == nil || req.Quality.VMAF == nil || req.Quality.VMAF.Model == "") {
		return errors.New("explicit 8-bit to Main10 benchmark requires VMAF with an explicit native-depth model")
	}
	if normMetric != "vmaf" && normMetric != "ssim" && normMetric != "both" && normMetric != "vmaf+ssim" {
		return fmt.Errorf("invalid metric %q: must be explicit enum 'vmaf', 'ssim', or 'both'", req.Metric)
	}

	if req.AudioMode != "" && req.AudioMode != "copy" && req.AudioMode != "compact" && req.AudioMode != "he_aac" && req.AudioMode != AudioModeCompactLossless {
		return fmt.Errorf("invalid audio_mode %q", req.AudioMode)
	}
	if (req.AudioMode == AudioModeCompactLossless) != sizeV3 {
		return errors.New("compact_lossless audio is size mode v3's policy and only that")
	}
	if req.FallbackAudioBitrateBps < 0 {
		return fmt.Errorf("invalid fallback_audio_bitrate_bps %d: cannot be negative", req.FallbackAudioBitrateBps)
	}
	if req.FallbackSubtitleSizeBytes < 0 {
		return fmt.Errorf("invalid fallback_subtitle_size_bytes %d: cannot be negative", req.FallbackSubtitleSizeBytes)
	}
	if req.DeclaredVideoBitrateBps < 0 {
		return fmt.Errorf("invalid declared_video_bitrate_bps %d: cannot be negative", req.DeclaredVideoBitrateBps)
	}
	if req.AttachmentBytes < 0 {
		return fmt.Errorf("invalid attachment_bytes %d: cannot be negative", req.AttachmentBytes)
	}

	if err := ValidateSourceSHA256(req.SourceSHA256); err != nil {
		return err
	}
	if norm, err := NormalizeSourceSHA256(req.SourceSHA256); err == nil {
		req.SourceSHA256 = norm
	}

	if req.Quality != nil {
		if req.Quality.PreferredMetric != "" {
			normPref := strings.ToLower(strings.TrimSpace(req.Quality.PreferredMetric))
			if normPref != "vmaf" && normPref != "ssim" {
				return fmt.Errorf("invalid preferred_metric %q: must be 'vmaf' or 'ssim'", req.Quality.PreferredMetric)
			}
			req.Quality.PreferredMetric = normPref
		}

		if req.Quality.VMAF != nil {
			v := req.Quality.VMAF
			if v.Model != "" {
				if v.WorstWindowSeconds == 0 {
					v.WorstWindowSeconds = 5
				}
				if req.Quality.FinalValidation == nil {
					req.Quality.FinalValidation = &BenchmarkFinalValidationConfig{Mode: "sampled"}
				}
				model, err := quality.Model(v.Model)
				if err != nil {
					return err
				}
				if v.WorstWindowSeconds <= 0 || v.WorstWindowSeconds > 120 || math.IsNaN(v.WorstWindowSeconds) || math.IsInf(v.WorstWindowSeconds, 0) {
					return fmt.Errorf("invalid VMAF worst_window_seconds")
				}
				for _, value := range []*float64{v.P5Minimum, v.WorstWindowMinimum, v.FrameThreshold} {
					if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < model.MinScore || *value > model.MaxScore) {
						return fmt.Errorf("VMAF guardrail outside model score range")
					}
				}
			} else if v.P5Minimum != nil || v.WorstWindowMinimum != nil || v.FrameThreshold != nil || v.MaxFramesBelowThreshold != nil {
				return fmt.Errorf("VMAF frame guardrails require explicit model")
			}
			if v.GuardrailEnforcement != "" && v.GuardrailEnforcement != "observe" && v.GuardrailEnforcement != "reject" {
				return fmt.Errorf("invalid VMAF guardrail_enforcement")
			}
			if v.MaxFramesBelowThreshold != nil && (v.FrameThreshold == nil || *v.MaxFramesBelowThreshold < 0) {
				return fmt.Errorf("max_frames_below_threshold requires frame_threshold")
			}
			tol := optimization.DefaultVMAFPolicy().Tolerance()
			if req.Quality.VMAF.MarginalTolerance != nil {
				tol = *req.Quality.VMAF.MarginalTolerance
			}
			p := optimization.NewVMAFPolicy(req.Quality.VMAF.Target, req.Quality.VMAF.Minimum, tol)
			if err := optimization.ValidatePolicy(p); err != nil {
				return fmt.Errorf("invalid vmaf quality policy: %w", err)
			}
		}
		if req.Quality.Banding != nil && req.Quality.Banding.Enabled {
			b := req.Quality.Banding
			if req.Quality.FinalValidation == nil {
				req.Quality.FinalValidation = &BenchmarkFinalValidationConfig{Mode: "sampled"}
			}
			if b.Enforcement == "" {
				b.Enforcement = "observe"
			}
			if b.Metric != "cambi" || b.Mode != "full_ref" || (b.Enforcement != "observe" && b.Enforcement != "reject") {
				return fmt.Errorf("invalid CAMBI full-reference policy")
			}
			for _, value := range []*float64{b.MaxMean, b.MaxPeak} {
				if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0) {
					return fmt.Errorf("invalid CAMBI limit")
				}
			}
			if b.Enforcement == "reject" && b.MaxMean == nil && b.MaxPeak == nil {
				return fmt.Errorf("CAMBI reject policy requires a limit")
			}
		}
		if req.Quality.FinalValidation != nil && req.Quality.FinalValidation.Mode != "sampled" {
			return fmt.Errorf("final validation mode must be sampled")
		}
		if req.Quality.FinalValidation != nil && (normMetric == "vmaf" || normMetric == "both" || normMetric == "vmaf+ssim") && (req.Quality.VMAF == nil || req.Quality.VMAF.Model == "") {
			return fmt.Errorf("sampled final VMAF validation requires an explicit model")
		}
		if req.Quality.SSIM != nil {
			tol := optimization.DefaultSSIMPolicy().Tolerance()
			if req.Quality.SSIM.MarginalTolerance != nil {
				tol = *req.Quality.SSIM.MarginalTolerance
			}
			p := optimization.NewSSIMPolicy(req.Quality.SSIM.Target, req.Quality.SSIM.Minimum, tol)
			if err := optimization.ValidatePolicy(p); err != nil {
				return fmt.Errorf("invalid ssim quality policy: %w", err)
			}
		}
	}

	if req.Adaptive != nil {
		mode := strings.ToLower(strings.TrimSpace(req.Adaptive.Mode))
		if mode == "" {
			mode = optimization.AdaptiveModeExhaustive
			req.Adaptive.Mode = mode
		}
		if mode != optimization.AdaptiveModeExhaustive && mode != optimization.AdaptiveModeAdaptive {
			return fmt.Errorf("invalid adaptive mode %q: must be 'exhaustive' or 'adaptive'", req.Adaptive.Mode)
		}
		req.Adaptive.Mode = mode
		if req.Adaptive.InitialQuality != 0 && (req.Adaptive.InitialQuality < 1 || req.Adaptive.InitialQuality > 100) {
			return fmt.Errorf("invalid adaptive initial_quality %d: must be in 1..100", req.Adaptive.InitialQuality)
		}
		if req.Adaptive.InitialQuality == 0 {
			req.Adaptive.InitialQuality = optimization.DefaultAdaptiveInitialQuality
		}
	}

	if req.Concurrency != nil {
		if req.Concurrency.EncodeConcurrency < 0 || req.Concurrency.EncodeConcurrency > MaxBenchmarkConcurrency {
			return fmt.Errorf("invalid concurrency encode_concurrency %d: must be in 0..%d (0 selects default %d)",
				req.Concurrency.EncodeConcurrency, MaxBenchmarkConcurrency, DefaultEncodeConcurrency)
		}
		if req.Concurrency.MetricConcurrency < 0 || req.Concurrency.MetricConcurrency > MaxBenchmarkConcurrency {
			return fmt.Errorf("invalid concurrency metric_concurrency %d: must be in 0..%d (0 selects default %d)",
				req.Concurrency.MetricConcurrency, MaxBenchmarkConcurrency, DefaultMetricConcurrency)
		}
		if req.Concurrency.EncodeConcurrency == 0 {
			req.Concurrency.EncodeConcurrency = DefaultEncodeConcurrency
		}
		if req.Concurrency.MetricConcurrency == 0 {
			req.Concurrency.MetricConcurrency = DefaultMetricConcurrency
		}
	}

	if len(req.Samples) == 0 {
		return errors.New("benchmark samples cannot be empty")
	}
	if len(req.Samples) > MaxBenchmarkSamples {
		return fmt.Errorf("benchmark samples count %d exceeds maximum allowed (%d)", len(req.Samples), MaxBenchmarkSamples)
	}

	seenSampleIndices := make(map[int]bool)
	for i, s := range req.Samples {
		if s.Index < 0 {
			return fmt.Errorf("sample window at index %d has negative sample index %d", i, s.Index)
		}
		if seenSampleIndices[s.Index] {
			return fmt.Errorf("duplicate sample window index %d", s.Index)
		}
		seenSampleIndices[s.Index] = true

		if !isFiniteFloat(s.StartSeconds) || s.StartSeconds < 0 {
			return fmt.Errorf("sample window %d start_seconds (%v) must be non-negative finite float", s.Index, s.StartSeconds)
		}
		if !isFiniteFloat(s.DurationSeconds) || s.DurationSeconds <= 0 {
			return fmt.Errorf("sample window %d duration_seconds (%v) must be positive finite float", s.Index, s.DurationSeconds)
		}
		if s.CenterSeconds != 0 && (!isFiniteFloat(s.CenterSeconds) || s.CenterSeconds < 0) {
			return fmt.Errorf("sample window %d center_seconds (%v) must be non-negative finite float", s.Index, s.CenterSeconds)
		}

		if req.SourceDuration > 0 {
			end := s.StartSeconds + s.DurationSeconds
			// Allow tiny float rounding tolerance (0.01s)
			if end > req.SourceDuration+0.01 {
				return fmt.Errorf("sample window %d end time (%.3fs) exceeds source duration (%.3fs)",
					s.Index, end, req.SourceDuration)
			}
		}
	}

	if len(req.Candidates) == 0 {
		return errors.New("benchmark candidates cannot be empty")
	}
	if len(req.Candidates) > MaxBenchmarkCandidates {
		return fmt.Errorf("benchmark candidates count %d exceeds maximum allowed (%d)", len(req.Candidates), MaxBenchmarkCandidates)
	}

	seenCandidateIDs := make(map[string]bool)
	seenQualities := make(map[string]bool)
	vtRateMode := ""
	for i, c := range req.Candidates {
		cID := strings.TrimSpace(c.ID)
		if cID == "" {
			return fmt.Errorf("candidate at index %d has empty id", i)
		}
		if len(cID) > MaxBenchmarkIDLength {
			return fmt.Errorf("candidate %q exceeds maximum id length (%d)", cID, MaxBenchmarkIDLength)
		}
		if !validCandidateIDRegex.MatchString(cID) {
			return fmt.Errorf("candidate id %q contains invalid characters (allowed: alphanumeric, dash, dot, underscore)", cID)
		}
		if seenCandidateIDs[cID] {
			return fmt.Errorf("duplicate candidate id %q", cID)
		}
		seenCandidateIDs[cID] = true

		codec := BenchmarkCandidateVideoCodec(c)
		if err := c.X265.Validate(codec, c.Quality); err != nil {
			return err
		}
		preset := strings.ToLower(strings.TrimSpace(c.Preset))
		tune := strings.ToLower(strings.TrimSpace(c.Tune))
		switch codec {
		case VideoCodecHEVCVideoToolbox:
			if preset != "" {
				return fmt.Errorf("candidate %q: preset is only supported for libx265, got %q for %s", cID, c.Preset, VideoCodecHEVCVideoToolbox)
			}
			if tune != "" {
				return fmt.Errorf("candidate %q: tune is only supported for libx265, got %q for %s", cID, c.Tune, VideoCodecHEVCVideoToolbox)
			}
			hasQuality := c.Quality != 0
			hasBitrate := c.AverageBitrateKbps != 0
			if hasQuality && hasBitrate {
				return fmt.Errorf("candidate %q specifies both quality (%d) and average_bitrate_kbps (%d): rate-control modes are mutually exclusive (fail closed)", cID, c.Quality, c.AverageBitrateKbps)
			}
			if !hasQuality && !hasBitrate {
				return fmt.Errorf("candidate %q must specify either quality (1..100) or average_bitrate_kbps (>0) (fail closed)", cID)
			}
			mode := "quality"
			if hasBitrate {
				mode = "bitrate"
			}
			if vtRateMode == "" {
				vtRateMode = mode
			} else if vtRateMode != mode {
				return fmt.Errorf("candidate %q uses %s mode but the benchmark already uses %s mode for hevc_videotoolbox: mixed rate-control configurations are forbidden (fail closed)", cID, mode, vtRateMode)
			}
			if hasQuality {
				if c.Quality < 1 || c.Quality > 100 {
					return fmt.Errorf("candidate %q quality %d out of valid range 1..100", cID, c.Quality)
				}
			} else {
				if c.AverageBitrateKbps < 1 || c.AverageBitrateKbps > MaxVideoBitrateKbps {
					return fmt.Errorf("candidate %q average_bitrate_kbps %d out of valid range 1..%d", cID, c.AverageBitrateKbps, MaxVideoBitrateKbps)
				}
			}
			if c.MaxBitrateKbps != 0 {
				if !hasBitrate {
					return fmt.Errorf("candidate %q specifies max_bitrate_kbps without average_bitrate_kbps (fail closed)", cID)
				}
				if c.MaxBitrateKbps < 1 || c.MaxBitrateKbps > MaxVideoBitrateKbps {
					return fmt.Errorf("candidate %q max_bitrate_kbps %d out of valid range 1..%d", cID, c.MaxBitrateKbps, MaxVideoBitrateKbps)
				}
				if c.MaxBitrateKbps < c.AverageBitrateKbps {
					return fmt.Errorf("candidate %q max_bitrate_kbps (%d) must be >= average_bitrate_kbps (%d) (fail closed)", cID, c.MaxBitrateKbps, c.AverageBitrateKbps)
				}
			}
			if c.ConstantBitrate != nil && *c.ConstantBitrate && !hasBitrate {
				return fmt.Errorf("candidate %q enables constant_bitrate without average_bitrate_kbps (fail closed)", cID)
			}
			if err := validateBenchmarkIntKnob(cID, "qmin", c.QMin, 0, MaxQPBound); err != nil {
				return err
			}
			if err := validateBenchmarkIntKnob(cID, "qmax", c.QMax, 0, MaxQPBound); err != nil {
				return err
			}
			if c.QMin != nil && c.QMax != nil && *c.QMin > *c.QMax {
				return fmt.Errorf("candidate %q qmin (%d) must be <= qmax (%d) (fail closed)", cID, *c.QMin, *c.QMax)
			}
			if err := validateBenchmarkIntKnob(cID, "gop_size", c.GOPSize, 1, MaxBenchmarkGOPSize); err != nil {
				return err
			}
			if err := validateBenchmarkIntKnob(cID, "b_frames", c.BFrames, 0, MaxBenchmarkBFrames); err != nil {
				return err
			}
			if err := validateBenchmarkIntKnob(cID, "max_ref_frames", c.MaxRefFrames, 1, MaxBenchmarkRefFrames); err != nil {
				return err
			}
		case VideoCodecLibX265:
			if preset != "" && !IsValidLibX265Preset(preset) {
				return fmt.Errorf("candidate %q has unsupported libx265 preset %q", cID, c.Preset)
			}
			if tune != "" && !IsValidLibX265Tune(tune) {
				return fmt.Errorf("candidate %q has unsupported libx265 tune %q", cID, c.Tune)
			}
			if c.X265 == nil && (c.Quality < LibX265CRFMin || c.Quality > LibX265CRFMax) {
				return fmt.Errorf("candidate %q crf %d out of valid range %d..%d for libx265", cID, c.Quality, LibX265CRFMin, LibX265CRFMax)
			}
			if err := rejectLibX265VideoToolboxKnobs(cID, c); err != nil {
				return err
			}
		default:
			return fmt.Errorf("candidate %q has unsupported video codec %q", cID, c.VideoCodec)
		}
		qualityKey := codec + ":" + preset + ":" + tune + ":" + strconv.Itoa(c.Quality) + ":br" + strconv.Itoa(c.AverageBitrateKbps)
		if c.X265 != nil {
			b, _ := json.Marshal(c.X265)
			qualityKey += string(b)
		}
		if seenQualities[qualityKey] {
			return fmt.Errorf("duplicate candidate quality %d (bitrate %d) for codec %s (candidate %q)", c.Quality, c.AverageBitrateKbps, codec, cID)
		}
		seenQualities[qualityKey] = true
	}

	return nil
}

// rejectLibX265VideoToolboxKnobs fail-closes when a libx265 candidate carries
// any hevc_videotoolbox-only control. The families never share knobs.
func rejectLibX265VideoToolboxKnobs(candidateID string, c BenchmarkCandidate) error {
	if c.AverageBitrateKbps != 0 {
		return fmt.Errorf("candidate %q: average_bitrate_kbps is only supported for hevc_videotoolbox, not libx265 (fail closed)", candidateID)
	}
	if c.MaxBitrateKbps != 0 {
		return fmt.Errorf("candidate %q: max_bitrate_kbps is only supported for hevc_videotoolbox, not libx265 (fail closed)", candidateID)
	}
	if c.ConstantBitrate != nil {
		return fmt.Errorf("candidate %q: constant_bitrate is only supported for hevc_videotoolbox, not libx265 (fail closed)", candidateID)
	}
	if c.QMin != nil || c.QMax != nil {
		return fmt.Errorf("candidate %q: qmin/qmax are only supported for hevc_videotoolbox, not libx265 (fail closed)", candidateID)
	}
	if c.GOPSize != nil {
		return fmt.Errorf("candidate %q: gop_size is only supported for hevc_videotoolbox, not libx265 (fail closed)", candidateID)
	}
	if c.BFrames != nil {
		return fmt.Errorf("candidate %q: b_frames is only supported for hevc_videotoolbox, not libx265 (fail closed)", candidateID)
	}
	if c.ClosedGOP != nil {
		return fmt.Errorf("candidate %q: closed_gop is only supported for hevc_videotoolbox, not libx265 (fail closed)", candidateID)
	}
	if c.PowerEfficient != nil {
		return fmt.Errorf("candidate %q: power_efficient is only supported for hevc_videotoolbox, not libx265 (fail closed)", candidateID)
	}
	if c.MaxRefFrames != nil {
		return fmt.Errorf("candidate %q: max_ref_frames is only supported for hevc_videotoolbox, not libx265 (fail closed)", candidateID)
	}
	if c.PrioritizeSpeed != nil || c.SpatialAQ != nil || c.Realtime != nil {
		return fmt.Errorf("candidate %q: VideoToolbox-only options (prio_speed/spatial_aq/realtime) are not supported for libx265 (fail closed)", candidateID)
	}
	return nil
}

// validateBenchmarkIntKnob fail-closes on out-of-range explicit integer knobs.
// A nil pointer means "emit nothing" and is always valid.
func validateBenchmarkIntKnob(candidateID, knob string, v *int, min, max int) error {
	if v == nil {
		return nil
	}
	if *v < min || *v > max {
		return fmt.Errorf("candidate %q %s %d out of valid range %d..%d (fail closed)", candidateID, knob, *v, min, max)
	}
	return nil
}

func isFiniteFloat(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
