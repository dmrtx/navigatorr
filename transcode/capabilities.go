package transcode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
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

// WorkerCapabilities represents the versioned capability report of a transcode worker node.
type WorkerCapabilities struct {
	ProtocolVersion int                      `json:"protocol_version"`
	WorkerVersion   string                   `json:"worker_version,omitempty"`
	BuildGitCommit  string                   `json:"build_git_commit,omitempty"`
	FFmpegVersion   string                   `json:"ffmpeg_version"`
	FFmpegPath      string                   `json:"ffmpeg_path,omitempty"`
	Encoders        map[string]bool          `json:"encoders"`
	Filters         map[string]bool          `json:"filters"`
	VideoToolbox    VideoToolboxCapabilities `json:"video_toolbox"`
	Signature       string                   `json:"signature"`
}

// ComputeCapabilitySignature returns the deterministic sha256 digest of WorkerCapabilities.
func ComputeCapabilitySignature(caps WorkerCapabilities) (string, error) {
	cp := caps
	cp.Signature = ""
	if len(cp.VideoToolbox.Profiles) > 1 {
		cp.VideoToolbox.Profiles = append([]string(nil), cp.VideoToolbox.Profiles...)
		sort.Strings(cp.VideoToolbox.Profiles)
	}
	if len(cp.VideoToolbox.PixelFormats) > 1 {
		cp.VideoToolbox.PixelFormats = append([]string(nil), cp.VideoToolbox.PixelFormats...)
		sort.Strings(cp.VideoToolbox.PixelFormats)
	}
	if len(cp.VideoToolbox.Options) > 1 {
		cp.VideoToolbox.Options = append([]string(nil), cp.VideoToolbox.Options...)
		sort.Strings(cp.VideoToolbox.Options)
	}
	b, err := json.Marshal(cp)
	if err != nil {
		return "", fmt.Errorf("serializing worker capabilities for signature: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VerifyCapabilitySignature checks that the signature on WorkerCapabilities matches the computed digest.
func VerifyCapabilitySignature(caps WorkerCapabilities) error {
	if caps.Signature == "" {
		return fmt.Errorf("worker capabilities missing signature (fail closed)")
	}
	expected, err := ComputeCapabilitySignature(caps)
	if err != nil {
		return fmt.Errorf("failed computing capability signature: %w", err)
	}
	if caps.Signature != expected {
		return fmt.Errorf("worker capability signature mismatch: got %s, want %s (fail closed)", caps.Signature, expected)
	}
	return nil
}
