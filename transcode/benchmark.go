package transcode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

var (
	// validBenchmarkJobIDRegex enforces the distinct benchmark namespace and safe filesystem identifier:
	// must start with "bench-" followed by an alphanumeric character, and then alphanumeric, underscores, hyphens, or dots.
	validBenchmarkJobIDRegex = regexp.MustCompile(`^bench-[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
)

const (
	// MaxBenchmarkSamples defines the conservative upper limit for sample windows in a single benchmark.
	MaxBenchmarkSamples = 32
	// MaxBenchmarkIDLength bounds the length of benchmark job and candidate IDs.
	MaxBenchmarkIDLength = 128
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

// BenchmarkQualityConfig configures quality evaluation policies for a benchmark run.
type BenchmarkQualityConfig struct {
	PreferredMetric string                          `json:"preferred_metric,omitempty"`
	VMAF            *BenchmarkQualityThresholds     `json:"vmaf,omitempty"`
	SSIM            *BenchmarkQualityThresholds     `json:"ssim,omitempty"`
	Banding         *BenchmarkBandingConfig         `json:"banding,omitempty"`
	FinalValidation *BenchmarkFinalValidationConfig `json:"final_validation,omitempty"`
}

// BenchmarkCandidate is one entry of a request's candidates. The final gate
// measures a finished encode and carries none, so only the encoder is kept for
// reading the stored decisions of earlier sample tests.
type BenchmarkCandidate struct {
	ID string `json:"id"`
	// VideoCodec selects the encoder. Empty means hevc_videotoolbox.
	VideoCodec string `json:"video_codec,omitempty"`
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

// Bounds the frozen search rules carry. The capability payload reports the two
// candidate limits.
const (
	// MaxAutomaticWalkCandidates and MaxAutomaticWalkHardwareCandidates are the
	// limits of a search whose rules walk the quality fallback or refine the
	// ladder. Its x265 candidates are the five ladder rungs, four refinement
	// rungs between them, eight quality fallback rungs for each of the two
	// presets (medium, and slow when the operation opts in) and three savings
	// rungs: 28, and 35 beside the seven VideoToolbox rungs.
	MaxAutomaticWalkCandidates         = 28
	MaxAutomaticWalkHardwareCandidates = 35
	// MaxRefinementQualityValues bounds the refinement rungs of a search.
	MaxRefinementQualityValues = 6
	// MaxHardwareMarginPercent bounds how much larger than the x265 winner a
	// hardware candidate may be and still win.
	MaxHardwareMarginPercent = 50
	// MaxHardwareQualityValues bounds the VideoToolbox quality ladder.
	MaxHardwareQualityValues = 4
	// MaxHardwareFallbackQualityValues bounds the higher-quality VideoToolbox
	// rungs tried when the whole ladder fails quality.
	MaxHardwareFallbackQualityValues = 3
	MaxSelectionMarginVMAF           = 10
	// HoldoutWindowsFinalGridEdges centres the two holdout windows on the
	// first and last positions of the final validation grid.
	HoldoutWindowsFinalGridEdges = "final_grid_edges"
	// HoldoutWindowsFinalGrid verifies a candidate on every window of the final
	// validation grid (GridVerificationWindows), so verification measures the
	// windows the final check re-measures on the full encode.
	HoldoutWindowsFinalGrid = "final_grid"
	// MaxFinalFreshWindows bounds the out-of-sample windows added to final
	// validation.
	MaxFinalFreshWindows = 2
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
	// HoldoutWindows places the verification windows.
	// HoldoutWindowsFinalGridEdges puts two on the start and end of the source
	// that final validation checks; HoldoutWindowsFinalGrid verifies on the whole
	// final validation grid; empty keeps 35% and 65%.
	HoldoutWindows string `json:"holdout_windows,omitempty"`
	// QualityFallbackPresets orders the quality fallback family's presets: a
	// later preset is measured only when the earlier ones found nothing.
	QualityFallbackPresets []string `json:"quality_fallback_presets,omitempty"`
	// VerifyUntilExhausted keeps verifying (smallest eligible setting, then the
	// safest, then a bisection between them) until one passes or none is
	// eligible, instead of stopping after two attempts. Only the search's time
	// limit ends it earlier, and the result says so. Absent keeps two attempts.
	VerifyUntilExhausted bool `json:"verify_until_exhausted,omitempty"`
	// FinalFreshWindows is how many windows no search window covers final
	// validation also measures, reported apart and never deciding its verdict.
	// It is read by the server when it plans final validation.
	FinalFreshWindows int `json:"final_fresh_windows,omitempty"`
	// QualityWalk measures the libx265 rungs of the quality fallback after the
	// family's highest-quality rung, one at a time, most compressed first. The
	// walk stops at the first rung that is not a quality failure (a lower CRF
	// can only be larger); the rungs below it are measured only if a verification
	// of that rung fails on quality. Absent measures each fallback stage at once.
	QualityWalk bool `json:"quality_walk,omitempty"`
	// RefinementQualityValues are x265 CRF rungs between the ladder's, measured
	// only after the ladder, and only the one that lies between the most
	// compressed rung that passed and the next, more compressed, rung that did
	// not. They narrow the result by one CRF step without measuring every
	// in-between rung of an easy source.
	RefinementQualityValues []int `json:"refinement_quality_values,omitempty"`
	// DeferBanding measures banding (CAMBI) only for a candidate whose VMAF
	// result has not already been rejected by a VMAF limit. A candidate that
	// failed VMAF is rejected whatever CAMBI says, so its CAMBI is not measured.
	DeferBanding bool `json:"defer_banding,omitempty"`
}

// Validate reports whether the rules are bounded and well formed.
func (r *AutomaticSearchRules) Validate() error {
	if r == nil {
		return nil
	}
	if math.IsNaN(r.SelectionMarginVMAF) || r.SelectionMarginVMAF < 0 || r.SelectionMarginVMAF > MaxSelectionMarginVMAF {
		return fmt.Errorf("automatic search selection_margin_vmaf must be in [0,%d]", MaxSelectionMarginVMAF)
	}
	if r.HoldoutWindows != "" && r.HoldoutWindows != HoldoutWindowsFinalGridEdges && r.HoldoutWindows != HoldoutWindowsFinalGrid {
		return fmt.Errorf("unsupported automatic search holdout_windows %q", r.HoldoutWindows)
	}
	if r.FinalFreshWindows < 0 || r.FinalFreshWindows > MaxFinalFreshWindows {
		return fmt.Errorf("automatic search final_fresh_windows must be in [0,%d]", MaxFinalFreshWindows)
	}
	if len(r.QualityFallbackPresets) > 3 {
		return errors.New("automatic search allows at most three quality fallback presets")
	}
	if len(r.RefinementQualityValues) > MaxRefinementQualityValues {
		return fmt.Errorf("automatic search allows at most %d refinement rungs", MaxRefinementQualityValues)
	}
	for i, q := range r.RefinementQualityValues {
		if q < 1 || q > 51 || i > 0 && q <= r.RefinementQualityValues[i-1] {
			return errors.New("automatic search refinement_quality_values must be CRF values in 1-51, ascending and distinct")
		}
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
	c.RefinementQualityValues = append([]int(nil), r.RefinementQualityValues...)
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
	// ExtraSelectionMarginVMAF is added to the automatic search's selection
	// margin for hardware candidates only: a hardware result must clear the VMAF
	// p5 and worst-window minimums by that much more than an x265 one. A fast
	// encoder that only just passes on three sampled windows has failed the
	// full-file check, so its samples must show headroom. Absent means no extra
	// margin, which is what snapshots frozen before it carry.
	ExtraSelectionMarginVMAF float64 `json:"extra_selection_margin_vmaf,omitempty"`
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
	if math.IsNaN(r.ExtraSelectionMarginVMAF) || r.ExtraSelectionMarginVMAF < 0 || r.ExtraSelectionMarginVMAF > MaxSelectionMarginVMAF {
		return fmt.Errorf("hardware search extra_selection_margin_vmaf must be in [0,%d]", MaxSelectionMarginVMAF)
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

// BenchmarkRequest is what the coordinator freezes for the final quality gate
// of a finished candidate: the windows to measure, the limits to judge them by
// and the mode policy the encode ran under. It does NOT accept candidate output
// paths or replace_original parameters, making original media mutation
// completely impossible. Its JSON is digested into benchmark_request_digest,
// so a field leaves it only when the final gate never sets it.
type BenchmarkRequest struct {
	ModePolicyVersion string                  `json:"mode_policy_version,omitempty"`
	Mode              string                  `json:"mode,omitempty"`
	ProtocolVersion   int                     `json:"protocol_version"`
	ID                string                  `json:"id"`
	SourcePath        string                  `json:"source_path"`
	SourceDuration    float64                 `json:"source_duration,omitempty"`
	Metric            string                  `json:"metric"` // "vmaf", "ssim", "both"
	Samples           []BenchmarkSampleWindow `json:"samples"`
	// Candidates is always empty now. The field keeps its place so the digest
	// of a request frozen before the sample search was removed stays equal.
	Candidates []BenchmarkCandidate    `json:"candidates"`
	Quality    *BenchmarkQualityConfig `json:"quality,omitempty"`
	// SourceSHA256 is the coordinator preflight's SHA-256 of the original source
	// bytes. Optional for legacy callers; it binds the shared source cache entry
	// to content identity so a same path/size/mtime source with changed content
	// can never be reused.
	SourceSHA256 string `json:"source_sha256,omitempty"`
}

// Automatic evidence explains one bounded search; discovery is not final validation.
type BenchmarkAutomaticStage struct {
	Stage      string   `json:"stage"`
	Candidates []string `json:"candidates"`
	// Skipped lists candidates not measured because a higher-quality setting
	// of the same family already failed the quality policy.
	Skipped []string `json:"skipped,omitempty"`
	// Reused lists candidates whose sample measurements came from the earlier
	// search a continuation follows instead of being encoded again.
	Reused  []string `json:"reused,omitempty"`
	Outcome string   `json:"outcome"`
}
type BenchmarkAutomaticEvidence struct {
	Stages          []BenchmarkAutomaticStage `json:"stages"`
	VerifiedSamples int                       `json:"verified_samples"`
	// VerificationWindows names where the verification windows were placed:
	// HoldoutWindowsFinalGrid (every window final validation re-measures),
	// HoldoutWindowsFinalGridEdges, or empty for the earlier 35%/65% holdouts.
	VerificationWindows string `json:"verification_windows,omitempty"`
	// Verification lists the verification attempts in order and how the
	// verification phase ended.
	Verification *BenchmarkVerificationEvidence `json:"verification,omitempty"`
	// WinnerWindows are the winner's per-window sample scores, the numbers the
	// final validation windows are compared with.
	WinnerWindows []BenchmarkWindowScore `json:"winner_windows,omitempty"`
}

// Verification attempt roles and outcomes.
const (
	VerificationRoleSmallest = "smallest"
	VerificationRoleSafest   = "safest"
	VerificationRoleBisect   = "bisect"
	VerificationRoleNext     = "next"

	VerificationPassed        = "passed"
	VerificationQualityFailed = "quality_failed"
	VerificationRejected      = "rejected"

	// VerificationStoppedTimeCap marks a verification phase that ended because
	// the search's time limit left no room for another attempt.
	VerificationStoppedTimeCap = "time_cap"
)

// BenchmarkVerificationAttempt is one candidate checked on the verification
// windows.
type BenchmarkVerificationAttempt struct {
	CandidateID string `json:"candidate_id"`
	// Role says why this candidate was chosen: the smallest eligible setting,
	// the safest after a quality failure, a bisection between a failed and a
	// passed setting, or the next eligible after a non-quality rejection.
	Role    string `json:"role"`
	Windows int    `json:"windows"`
	Outcome string `json:"outcome"`
	// Reason is the evaluation reason of a failed attempt.
	Reason          string   `json:"reason,omitempty"`
	VMAFMean        *float64 `json:"vmaf_mean,omitempty"`
	VMAFP5          *float64 `json:"vmaf_p5,omitempty"`
	WorstWindowMean *float64 `json:"worst_window_mean,omitempty"`
}

// BenchmarkVerificationEvidence records the verification phase. When the time
// limit ends it, StoppedBy says so and Unverified lists the candidates that
// passed discovery and were never checked.
type BenchmarkVerificationEvidence struct {
	Attempts   []BenchmarkVerificationAttempt `json:"attempts"`
	StoppedBy  string                         `json:"stopped_by,omitempty"`
	Unverified []string                       `json:"unverified,omitempty"`
}

// BenchmarkWindowScore is one window's VMAF measurement. Role is "discovery"
// for the windows the search selected on and "verification" for the rest.
type BenchmarkWindowScore struct {
	StartSeconds    float64 `json:"start_seconds"`
	DurationSeconds float64 `json:"duration_seconds"`
	Role            string  `json:"role,omitempty"`
	VMAFMean        float64 `json:"vmaf_mean"`
	VMAFP5          float64 `json:"vmaf_p5"`
	VMAFMin         float64 `json:"vmaf_min"`
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
