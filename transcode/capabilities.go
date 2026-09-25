package transcode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// WorkerProtocolVersion is the current version of the worker capability
// protocol.
//
// Version 2 is a breaking wire-schema change: normal transcode submit and
// BenchmarkRequest payloads now carry `source_sha256`. Worker HTTP decoders use
// DisallowUnknownFields, so an older (v1) worker rejects those payloads. New
// coordinators therefore reject a stale v1 worker during the capability
// handshake (before any submit) instead of deferring the failure. Server and
// worker must be upgraded together.
const WorkerProtocolVersion = 2

// EncoderCapabilities describes detailed supported profiles, pixel formats, and options for a specific encoder.
type EncoderCapabilities struct {
	Encoder      string   `json:"encoder"`
	Available    bool     `json:"available"`
	Profiles     []string `json:"profiles,omitempty"`
	PixelFormats []string `json:"pixel_formats,omitempty"`
	Options      []string `json:"options,omitempty"`
}

// VideoToolboxCapabilities is an alias to EncoderCapabilities for convenience and compatibility.
type VideoToolboxCapabilities = EncoderCapabilities

// ProbeError records non-fatal probe warnings or errors encountered during capability discovery,
// allowing callers to differentiate between confirmed absence and probe failure.
type ProbeError struct {
	Component string `json:"component"` // e.g. "encoders", "filters", "videotoolbox"
	Message   string `json:"message"`   // bounded error description
}

// QualityModelCapability is the result of executing an actual model and JSON
// measurement path, not merely finding the libvmaf filter in -filters output.
type QualityModelCapability struct {
	Available           bool    `json:"available"`
	MeasurementBitDepth int     `json:"measurement_bit_depth"`
	ScoreMin            float64 `json:"score_min"`
	ScoreMax            float64 `json:"score_max"`
	Reason              string  `json:"reason,omitempty"`
}

type QualityCapabilities struct {
	LibvmafVersion string                            `json:"libvmaf_version,omitempty"`
	Models         map[string]QualityModelCapability `json:"models,omitempty"`
	CAMBIFullRef   bool                              `json:"cambi_full_ref"`
	CAMBIOutput    string                            `json:"cambi_output,omitempty"`
	ProbeError     string                            `json:"probe_error,omitempty"`
}

// WorkerCapabilities represents the versioned capability report of a transcode worker node.
type WorkerCapabilities struct {
	ProtocolVersion       int                            `json:"protocol_version"`
	WorkerVersion         string                         `json:"worker_version,omitempty"`
	BuildGitCommit        string                         `json:"build_git_commit,omitempty"`
	FFmpegVersion         string                         `json:"ffmpeg_version"`
	FFmpegPath            string                         `json:"ffmpeg_path,omitempty"`
	Encoders              map[string]bool                `json:"encoders"`
	EncoderDetails        map[string]EncoderCapabilities `json:"encoder_details,omitempty"`
	Filters               map[string]bool                `json:"filters"`
	Quality               *QualityCapabilities           `json:"quality,omitempty"`
	ProbeErrors           []ProbeError                   `json:"probe_errors,omitempty"`
	CapabilityFingerprint string                         `json:"capability_fingerprint,omitempty"`
}

// Fingerprint returns the canonical capability fingerprint.
func (c WorkerCapabilities) Fingerprint() string {
	return c.CapabilityFingerprint
}

// HasProbeErrors returns true if any non-fatal probe errors were reported during capability discovery.
func (c WorkerCapabilities) HasProbeErrors() bool {
	return len(c.ProbeErrors) > 0
}

// HasComponentError returns true if a probe error exists for the given component.
func (c WorkerCapabilities) HasComponentError(component string) bool {
	for _, pe := range c.ProbeErrors {
		if strings.EqualFold(pe.Component, component) {
			return true
		}
	}
	return false
}

// fingerprintPayload defines the stable subset of capabilities used to compute
// the cache identity / capability fingerprint.
// Machine-specific paths like FFmpegPath are excluded to ensure capability equivalence
// across different worker nodes with identical capability sets.
type fingerprintPayload struct {
	ProtocolVersion int                            `json:"protocol_version"`
	WorkerVersion   string                         `json:"worker_version,omitempty"`
	BuildGitCommit  string                         `json:"build_git_commit,omitempty"`
	FFmpegVersion   string                         `json:"ffmpeg_version"`
	Encoders        map[string]bool                `json:"encoders"`
	EncoderDetails  map[string]EncoderCapabilities `json:"encoder_details,omitempty"`
	Filters         map[string]bool                `json:"filters"`
	Quality         *QualityCapabilities           `json:"quality,omitempty"`
	ProbeErrors     []ProbeError                   `json:"probe_errors,omitempty"`
}

// ComputeCapabilityFingerprint returns a deterministic sha256 digest of WorkerCapabilities
// representing its capability equivalence for caching and validation.
// Note: This is a cache identity fingerprint, NOT a cryptographic authentication proof.
func ComputeCapabilityFingerprint(caps WorkerCapabilities) (string, error) {
	// Deep-clone and sort encoder details
	detCopy := make(map[string]EncoderCapabilities, len(caps.EncoderDetails))
	for k, v := range caps.EncoderDetails {
		ec := v
		if len(ec.Profiles) > 1 {
			ec.Profiles = append([]string(nil), ec.Profiles...)
			sort.Strings(ec.Profiles)
		}
		if len(ec.PixelFormats) > 1 {
			ec.PixelFormats = append([]string(nil), ec.PixelFormats...)
			sort.Strings(ec.PixelFormats)
		}
		if len(ec.Options) > 1 {
			ec.Options = append([]string(nil), ec.Options...)
			sort.Strings(ec.Options)
		}
		detCopy[k] = ec
	}

	payload := fingerprintPayload{
		ProtocolVersion: caps.ProtocolVersion,
		WorkerVersion:   caps.WorkerVersion,
		BuildGitCommit:  caps.BuildGitCommit,
		FFmpegVersion:   caps.FFmpegVersion,
		Encoders:        caps.Encoders,
		EncoderDetails:  detCopy,
		Filters:         caps.Filters,
		Quality:         caps.Quality,
		ProbeErrors:     caps.ProbeErrors,
	}

	if len(payload.ProbeErrors) > 1 {
		payload.ProbeErrors = append([]ProbeError(nil), payload.ProbeErrors...)
		sort.Slice(payload.ProbeErrors, func(i, j int) bool {
			if payload.ProbeErrors[i].Component != payload.ProbeErrors[j].Component {
				return payload.ProbeErrors[i].Component < payload.ProbeErrors[j].Component
			}
			return payload.ProbeErrors[i].Message < payload.ProbeErrors[j].Message
		})
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("serializing worker capabilities for fingerprint: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VerifyCapabilityFingerprint checks that the fingerprint on WorkerCapabilities matches the computed digest.
func VerifyCapabilityFingerprint(caps WorkerCapabilities) error {
	if caps.CapabilityFingerprint == "" {
		return fmt.Errorf("worker capabilities missing capability fingerprint (fail closed)")
	}
	expected, err := ComputeCapabilityFingerprint(caps)
	if err != nil {
		return fmt.Errorf("failed computing capability fingerprint: %w", err)
	}
	if caps.CapabilityFingerprint != expected {
		return fmt.Errorf("worker capability fingerprint mismatch: got %s, want %s (fail closed)", caps.CapabilityFingerprint, expected)
	}
	return nil
}
