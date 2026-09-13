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
	ProtocolVersion int                     `json:"protocol_version"`
	ID              string                  `json:"id"`
	SourcePath      string                  `json:"source_path"`
	SourceDuration  float64                 `json:"source_duration,omitempty"`
	Metric          string                  `json:"metric"` // "vmaf" or "ssim"
	Samples         []BenchmarkSampleWindow `json:"samples"`
	Candidates      []BenchmarkCandidate    `json:"candidates"`
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
	ProtocolVersion int       `json:"protocol_version"`
	ID              string    `json:"id"`
	Status          string    `json:"status"` // queued, running, completed, failed, cancelled
	SourcePath      string    `json:"source_path"`
	Metric          string    `json:"metric,omitempty"`
	Progress        float64   `json:"progress"`
	Error           string    `json:"error,omitempty"`
	SamplesPlanned  int       `json:"samples_planned"`
	CandidatesCount int       `json:"candidates_count"`
	Attempt         int       `json:"attempt,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	FinishedAt      time.Time `json:"finished_at,omitempty"`
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
	if normMetric != "vmaf" && normMetric != "ssim" {
		return fmt.Errorf("invalid metric %q: must be explicit enum 'vmaf' or 'ssim'", req.Metric)
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
