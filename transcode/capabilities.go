package transcode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// WorkerProtocolVersion is the current version of the worker capability protocol.
const WorkerProtocolVersion = 1

// VideoToolboxCapabilities captures hardware encoder details for Apple Silicon VideoToolbox.
type VideoToolboxCapabilities struct {
	Encoder      string   `json:"encoder"`
	Available    bool     `json:"available"`
	Profiles     []string `json:"profiles,omitempty"`
	PixelFormats []string `json:"pixel_formats,omitempty"`
	Options      []string `json:"options,omitempty"`
}

// ProbeError records non-fatal probe warnings or errors encountered during capability discovery,
// allowing callers to differentiate between confirmed absence and probe failure.
type ProbeError struct {
	Component string `json:"component"` // e.g. "encoders", "filters", "videotoolbox"
	Message   string `json:"message"`   // bounded error description
}

// WorkerCapabilities represents the versioned capability report of a transcode worker node.
type WorkerCapabilities struct {
	ProtocolVersion       int                      `json:"protocol_version"`
	WorkerVersion         string                   `json:"worker_version,omitempty"`
	BuildGitCommit        string                   `json:"build_git_commit,omitempty"`
	FFmpegVersion         string                   `json:"ffmpeg_version"`
	FFmpegPath            string                   `json:"ffmpeg_path,omitempty"`
	Encoders              map[string]bool          `json:"encoders"`
	Filters               map[string]bool          `json:"filters"`
	VideoToolbox          VideoToolboxCapabilities `json:"video_toolbox"`
	ProbeErrors           []ProbeError             `json:"probe_errors,omitempty"`
	CapabilityFingerprint string                   `json:"capability_fingerprint,omitempty"`
	// CapabilitySignature is retained as an alias for backwards compatibility
	CapabilitySignature   string                   `json:"capability_signature,omitempty"`
	// Signature is retained as a JSON alias for backwards compatibility
	Signature             string                   `json:"signature,omitempty"`
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

// Fingerprint returns the capability fingerprint or signature alias.
func (c WorkerCapabilities) Fingerprint() string {
	if c.CapabilityFingerprint != "" {
		return c.CapabilityFingerprint
	}
	if c.CapabilitySignature != "" {
		return c.CapabilitySignature
	}
	return c.Signature
}

// fingerprintPayload defines the stable subset of capabilities used to compute
// the cache identity / capability fingerprint.
// Machine-specific paths like FFmpegPath are excluded to ensure capability equivalence
// across different worker nodes with identical capability sets.
type fingerprintPayload struct {
	ProtocolVersion int                      `json:"protocol_version"`
	WorkerVersion   string                   `json:"worker_version,omitempty"`
	BuildGitCommit  string                   `json:"build_git_commit,omitempty"`
	FFmpegVersion   string                   `json:"ffmpeg_version"`
	Encoders        map[string]bool          `json:"encoders"`
	Filters         map[string]bool          `json:"filters"`
	VideoToolbox    VideoToolboxCapabilities `json:"video_toolbox"`
	ProbeErrors     []ProbeError             `json:"probe_errors,omitempty"`
}

// ComputeCapabilityFingerprint returns a deterministic sha256 digest of WorkerCapabilities
// representing its capability equivalence for caching and validation.
// Note: This is a cache identity fingerprint, NOT a cryptographic authentication proof.
func ComputeCapabilityFingerprint(caps WorkerCapabilities) (string, error) {
	payload := fingerprintPayload{
		ProtocolVersion: caps.ProtocolVersion,
		WorkerVersion:   caps.WorkerVersion,
		BuildGitCommit:  caps.BuildGitCommit,
		FFmpegVersion:   caps.FFmpegVersion,
		Encoders:        caps.Encoders,
		Filters:         caps.Filters,
		VideoToolbox:    caps.VideoToolbox,
		ProbeErrors:     caps.ProbeErrors,
	}

	if len(payload.VideoToolbox.Profiles) > 1 {
		payload.VideoToolbox.Profiles = append([]string(nil), payload.VideoToolbox.Profiles...)
		sort.Strings(payload.VideoToolbox.Profiles)
	}
	if len(payload.VideoToolbox.PixelFormats) > 1 {
		payload.VideoToolbox.PixelFormats = append([]string(nil), payload.VideoToolbox.PixelFormats...)
		sort.Strings(payload.VideoToolbox.PixelFormats)
	}
	if len(payload.VideoToolbox.Options) > 1 {
		payload.VideoToolbox.Options = append([]string(nil), payload.VideoToolbox.Options...)
		sort.Strings(payload.VideoToolbox.Options)
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

// ComputeCapabilitySignature is an alias for ComputeCapabilityFingerprint for backwards compatibility.
func ComputeCapabilitySignature(caps WorkerCapabilities) (string, error) {
	return ComputeCapabilityFingerprint(caps)
}

// VerifyCapabilityFingerprint checks that the fingerprint on WorkerCapabilities matches the computed digest.
func VerifyCapabilityFingerprint(caps WorkerCapabilities) error {
	fp := caps.Fingerprint()
	if fp == "" {
		return fmt.Errorf("worker capabilities missing capability fingerprint (fail closed)")
	}
	expected, err := ComputeCapabilityFingerprint(caps)
	if err != nil {
		return fmt.Errorf("failed computing capability fingerprint: %w", err)
	}
	if fp != expected {
		return fmt.Errorf("worker capability fingerprint mismatch: got %s, want %s (fail closed)", fp, expected)
	}
	return nil
}

// VerifyCapabilitySignature is an alias for VerifyCapabilityFingerprint for backwards compatibility.
func VerifyCapabilitySignature(caps WorkerCapabilities) error {
	return VerifyCapabilityFingerprint(caps)
}
