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
	"time"

	"github.com/jakenesler/navigatorr/transcode/optimization"
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
	Target            float64  `json:"target,omitempty"`
	Minimum           float64  `json:"minimum,omitempty"`
	MarginalTolerance *float64 `json:"marginal_tolerance,omitempty"`
}

// BenchmarkQualityConfig configures quality evaluation policies for a benchmark run.
type BenchmarkQualityConfig struct {
	PreferredMetric string                      `json:"preferred_metric,omitempty"`
	VMAF            *BenchmarkQualityThresholds `json:"vmaf,omitempty"`
	SSIM            *BenchmarkQualityThresholds `json:"ssim,omitempty"`
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
	CandidateID               string   `json:"candidate_id"`
	CandidateIndex            int      `json:"candidate_index"`
	Quality                   int      `json:"quality"`
	VideoProfile              string   `json:"video_profile,omitempty"`
	PixelFormat               string   `json:"pixel_format,omitempty"`
	ExpectedBitDepth          int      `json:"expected_bit_depth"`
	MetricType                string   `json:"metric_type"`
	Score                     float64  `json:"score"`
	TargetReached             bool     `json:"target_reached"`
	MinimumMet                bool     `json:"minimum_met"`
	EstimatedVideoBytes       int64    `json:"estimated_video_bytes"`
	EstimatedAudioBytes       int64    `json:"estimated_audio_bytes"`
	EstimatedSubtitleBytes    int64    `json:"estimated_subtitle_bytes"`
	EstimatedAttachmentBytes  int64    `json:"estimated_attachment_bytes"`
	EstimatedMuxOverheadBytes int64    `json:"estimated_mux_overhead_bytes"`
	EstimatedTotalBytes       int64    `json:"estimated_total_bytes"`
	EstimatedTotalMB          float64  `json:"estimated_total_mb"`
	SavingsBytes              int64    `json:"savings_bytes"`
	SavingsPercent            float64  `json:"savings_percent"`
	Uncertainties             []string `json:"uncertainties,omitempty"`
}

// BenchmarkCandidateEvaluation records the evaluation summary for one candidate.
type BenchmarkCandidateEvaluation struct {
	CandidateID      string   `json:"candidate_id"`
	CandidateIndex   int      `json:"candidate_index"`
	Quality          int      `json:"quality"`
	VideoProfile     string   `json:"video_profile,omitempty"`
	PixelFormat      string   `json:"pixel_format,omitempty"`
	ExpectedBitDepth int      `json:"expected_bit_depth"`
	Score            float64  `json:"score"`
	MetricType       string   `json:"metric_type"`
	Eligible         bool     `json:"eligible"`
	TargetReached    bool     `json:"target_reached"`
	MinimumMet       bool     `json:"minimum_met"`
	EvaluationReason string   `json:"evaluation_reason"`
	EstimatedBytes   int64    `json:"estimated_bytes"`
	EstimatedMB      float64  `json:"estimated_mb"`
	SavingsPercent   float64  `json:"savings_percent"`
	Uncertainties    []string `json:"uncertainties,omitempty"`
}

// BenchmarkDecision records the explainable Phase 6 candidate selection outcome.
type BenchmarkDecision struct {
	Winner         *BenchmarkWinner               `json:"winner,omitempty"`
	DecisionReason string                         `json:"decision_reason"`
	Evaluations    []BenchmarkCandidateEvaluation `json:"evaluations"`
}

// BenchmarkCandidate specifies one encoder candidate to evaluate during a benchmark.
// Arbitrary ffmpeg arguments are strictly forbidden.
type BenchmarkCandidate struct {
	ID           string `json:"id"`
	Quality      int    `json:"quality"`
	VideoProfile string `json:"video_profile,omitempty"`
	PixelFormat  string `json:"pixel_format,omitempty"`
}

// BenchmarkSampleWindow specifies one temporal window to sample from the source.
type BenchmarkSampleWindow struct {
	Index           int     `json:"index"`
	StartSeconds    float64 `json:"start_seconds"`
	DurationSeconds float64 `json:"duration_seconds"`
	CenterSeconds   float64 `json:"center_seconds,omitempty"`
}

// BenchmarkRequest defines the parameters sent from the coordinator to the worker to execute a benchmark.
// It is strictly versioned and does NOT accept candidate output paths or replace_original parameters,
// making original media mutation completely impossible.
type BenchmarkRequest struct {
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

// BenchmarkStatus captures the current execution status and metadata of a benchmark job.
type BenchmarkStatus struct {
	ProtocolVersion int                `json:"protocol_version"`
	ID              string             `json:"id"`
	Status          string             `json:"status"` // queued, running, completed, failed, cancelled
	SourcePath      string             `json:"source_path"`
	Metric          string             `json:"metric,omitempty"`
	Progress        float64            `json:"progress"`
	Phase           string             `json:"phase,omitempty"`
	HeartbeatAt     time.Time          `json:"heartbeat_at,omitempty"`
	Error           string             `json:"error,omitempty"`
	SamplesPlanned  int                `json:"samples_planned"`
	CandidatesCount int                `json:"candidates_count"`
	Attempt         int                `json:"attempt,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
	StartedAt       time.Time          `json:"started_at,omitempty"`
	FinishedAt      time.Time          `json:"finished_at,omitempty"`
	Decision        *BenchmarkDecision `json:"decision,omitempty"`
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
	if normMetric != "vmaf" && normMetric != "ssim" && normMetric != "both" && normMetric != "vmaf+ssim" {
		return fmt.Errorf("invalid metric %q: must be explicit enum 'vmaf', 'ssim', or 'both'", req.Metric)
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

	if req.Quality != nil {
		if req.Quality.PreferredMetric != "" {
			normPref := strings.ToLower(strings.TrimSpace(req.Quality.PreferredMetric))
			if normPref != "vmaf" && normPref != "ssim" {
				return fmt.Errorf("invalid preferred_metric %q: must be 'vmaf' or 'ssim'", req.Quality.PreferredMetric)
			}
			req.Quality.PreferredMetric = normPref
		}

		if req.Quality.VMAF != nil {
			tol := optimization.DefaultVMAFPolicy().Tolerance()
			if req.Quality.VMAF.MarginalTolerance != nil {
				tol = *req.Quality.VMAF.MarginalTolerance
			}
			p := optimization.NewVMAFPolicy(req.Quality.VMAF.Target, req.Quality.VMAF.Minimum, tol)
			if err := optimization.ValidatePolicy(p); err != nil {
				return fmt.Errorf("invalid vmaf quality policy: %w", err)
			}
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
	seenQualities := make(map[int]bool)
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

		if c.Quality < 1 || c.Quality > 100 {
			return fmt.Errorf("candidate %q quality %d out of valid range 1..100", cID, c.Quality)
		}
		if seenQualities[c.Quality] {
			return fmt.Errorf("duplicate candidate quality %d for candidate %q", c.Quality, cID)
		}
		seenQualities[c.Quality] = true
	}

	return nil
}

func isFiniteFloat(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
