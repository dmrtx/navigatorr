package recipe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var safeToken = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

var allowedCopySubtitleCodecs = map[string]bool{
	"subrip": true, "srt": true, "ass": true, "ssa": true, "hdmv_pgs_subtitle": true, "pgs": true,
	"dvd_subtitle": true, "vobsub": true, "dvb_subtitle": true, "dvb_teletext": true, "webvtt": true, "text": true,
}

var allowedFailureClasses = map[string]bool{
	"worker_busy": true, "ssh_transient": true, "worker_unreachable": true, "encoder_temporarily_unavailable": true,
	"container_subtitle_incompatible": true, "container_audio_incompatible": true, "container_attachment_incompatible": true,
	"ffmpeg_input_corrupt": true, "ffmpeg_unknown": true, "validation_duration_mismatch": true,
	"validation_stream_loss": true, "validation_codec_mismatch": true, "source_changed": true,
}

var allowedFallbackActions = map[string]bool{"retry": true, "apply_container_conversion": true}

func Parse(data []byte) (*Snapshot, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var b Bundle
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("parsing recipe bundle: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("recipe bundle must contain exactly one YAML/JSON document")
	}
	if err := Validate(&b); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return &Snapshot{Bundle: b, Identity: Identity{Version: b.BundleVersion, Digest: "sha256:" + hex.EncodeToString(sum[:])}, Raw: append([]byte(nil), data...)}, nil
}

func Validate(b *Bundle) error {
	if b == nil {
		return fmt.Errorf("recipe bundle is nil")
	}
	if b.SchemaVersion != SupportedSchemaVersionV1 && b.SchemaVersion != SupportedSchemaVersionV2 {
		return fmt.Errorf("unsupported recipe schema_version %d (supported: %d, %d)", b.SchemaVersion, SupportedSchemaVersionV1, SupportedSchemaVersionV2)
	}
	if strings.TrimSpace(b.BundleVersion) == "" || !safeToken.MatchString(b.BundleVersion) {
		return fmt.Errorf("invalid bundle_version %q", b.BundleVersion)
	}
	if len(b.Containers) == 0 {
		return fmt.Errorf("recipe bundle has no containers")
	}
	if len(b.Profiles) == 0 {
		return fmt.Errorf("recipe bundle has no profiles")
	}

	for rawName, c := range b.Containers {
		name := normalizeContainer(rawName)
		if name != "mkv" {
			return fmt.Errorf("unsupported recipe container %q", rawName)
		}
		seen := map[string]bool{}
		for _, rawCodec := range c.SubtitleCopy {
			codec := normalizeCodec(rawCodec)
			if !safeToken.MatchString(codec) || !allowedCopySubtitleCodecs[codec] {
				return fmt.Errorf("container %s: unsafe or unsupported subtitle copy codec %q", rawName, rawCodec)
			}
			if seen[codec] {
				return fmt.Errorf("container %s: duplicate subtitle copy codec %q", rawName, rawCodec)
			}
			seen[codec] = true
		}
		for rawFrom, conv := range c.SubtitleConversions {
			from := normalizeCodec(rawFrom)
			target := normalizeCodec(conv.TargetCodec)
			if !safeToken.MatchString(from) {
				return fmt.Errorf("container %s: invalid source subtitle codec %q", rawName, rawFrom)
			}
			if target != "subrip" {
				return fmt.Errorf("container %s: unsupported subtitle conversion target %q", rawName, conv.TargetCodec)
			}
			if !safeToken.MatchString(conv.Reason) {
				return fmt.Errorf("container %s: invalid conversion reason %q", rawName, conv.Reason)
			}
			if seen[from] {
				return fmt.Errorf("container %s: codec %q is both copy-compatible and configured for conversion", rawName, rawFrom)
			}
		}
	}

	for name, p := range b.Profiles {
		if !safeToken.MatchString(name) {
			return fmt.Errorf("invalid profile name %q", name)
		}
		if p.Optimization != nil && b.SchemaVersion < SupportedSchemaVersionV2 {
			return fmt.Errorf("profile %q: optimization policy requires schema_version 2", name)
		}
		if err := ValidateProfile(name, p); err != nil {
			return err
		}
		if _, ok := b.Containers[normalizeContainer(p.Container)]; !ok {
			// permit matroska alias to resolve the canonical mkv rule
			if normalizeContainer(p.Container) != "mkv" {
				return fmt.Errorf("profile %q references undefined container %q", name, p.Container)
			}
			if _, ok = b.Containers["mkv"]; !ok {
				return fmt.Errorf("profile %q references undefined container %q", name, p.Container)
			}
		}
	}
	return nil
}

func ValidateProfile(name string, p Profile) error {
	if normalizeContainer(p.Container) != "mkv" {
		return fmt.Errorf("profile %q: unsupported container %q", name, p.Container)
	}
	if normalizeCodec(p.Video.Codec) != "hevc_videotoolbox" {
		return fmt.Errorf("profile %q: unsupported video codec %q", name, p.Video.Codec)
	}
	if p.Video.Quality < 1 || p.Video.Quality > 100 {
		return fmt.Errorf("profile %q: video quality %d out of range 1-100", name, p.Video.Quality)
	}
	videoProfile := normalizeCodec(p.Video.Profile)
	pixelFormat := normalizeCodec(p.Video.PixelFormat)
	if videoProfile != "" && videoProfile != "main" && videoProfile != "main10" {
		return fmt.Errorf("profile %q: unsupported HEVC profile %q (allowed: main, main10)", name, p.Video.Profile)
	}
	if pixelFormat != "" && pixelFormat != "yuv420p" && pixelFormat != "p010le" {
		return fmt.Errorf("profile %q: unsupported pixel_format %q (allowed: yuv420p, p010le)", name, p.Video.PixelFormat)
	}
	if videoProfile == "main10" && pixelFormat != "p010le" {
		return fmt.Errorf("profile %q: HEVC main10 requires pixel_format p010le to guarantee 10-bit output", name)
	}
	if pixelFormat == "p010le" && videoProfile != "main10" {
		return fmt.Errorf("profile %q: pixel_format p010le requires HEVC profile main10", name)
	}
	if videoProfile == "main" && pixelFormat == "p010le" {
		return fmt.Errorf("profile %q: HEVC main is incompatible with pixel_format p010le", name)
	}
	if strings.ToLower(strings.TrimSpace(p.Audio.Mode)) != "copy" {
		return fmt.Errorf("profile %q: unsupported audio mode %q", name, p.Audio.Mode)
	}
	if strings.ToLower(strings.TrimSpace(p.Subtitles.Mode)) != "preserve" {
		return fmt.Errorf("profile %q: unsupported subtitles mode %q", name, p.Subtitles.Mode)
	}
	if !p.Preserve.Metadata || !p.Preserve.Chapters || !p.Preserve.Attachments {
		return fmt.Errorf("profile %q: metadata, chapters, and attachments must all be preserved by the current engine capability boundary", name)
	}
	r := p.Resilience
	if r.MaxAttempts <= 0 || r.MaxAttempts > 10 {
		return fmt.Errorf("profile %q: max_attempts must be 1-10", name)
	}
	if r.TransientRetries < 0 || r.TransientRetries >= r.MaxAttempts {
		return fmt.Errorf("profile %q: transient_retries must be >=0 and < max_attempts", name)
	}
	if len(r.RetryBackoffSeconds) < r.TransientRetries {
		return fmt.Errorf("profile %q: retry_backoff_seconds must cover transient_retries", name)
	}
	for _, s := range r.RetryBackoffSeconds {
		if s < 0 || s > 3600 {
			return fmt.Errorf("profile %q: retry backoff %d out of range", name, s)
		}
	}
	if r.MaxFallbacks < 0 || r.MaxFallbacks > 4 {
		return fmt.Errorf("profile %q: max_fallbacks out of range 0-4", name)
	}
	seen := map[string]bool{}
	for _, f := range r.Fallbacks {
		if !allowedFailureClasses[f.When] {
			return fmt.Errorf("profile %q: unknown fallback condition %q", name, f.When)
		}
		if !allowedFallbackActions[f.Action] {
			return fmt.Errorf("profile %q: unknown fallback action %q", name, f.Action)
		}
		key := f.When + ":" + f.Action
		if seen[key] {
			return fmt.Errorf("profile %q: duplicate fallback %q", name, key)
		}
		seen[key] = true
		if f.Action == "apply_container_conversion" && f.When != "container_subtitle_incompatible" {
			return fmt.Errorf("profile %q: container conversion fallback only valid for container_subtitle_incompatible", name)
		}
	}
	if p.Optimization != nil {
		if err := ValidateOptimizationPolicy(name, p.Optimization); err != nil {
			return err
		}
	}
	return nil
}

// ValidateOptimizationPolicy verifies that typed optimization models contain safe, bounded values.
func ValidateOptimizationPolicy(name string, opt *OptimizationPolicy) error {
	if opt == nil {
		return nil
	}
	if opt.Sampling != nil {
		s := opt.Sampling
		if s.SegmentDurationSec < 0 || s.SegmentDurationSec > 300 {
			return fmt.Errorf("profile %q: sampling segment_duration_sec must be between 0 and 300 seconds", name)
		}
		if s.SegmentCount < 0 || s.SegmentCount > 20 {
			return fmt.Errorf("profile %q: sampling segment_count out of range (allowed: 1-20)", name)
		}
		if s.MinSourceDurationSec < 0 {
			return fmt.Errorf("profile %q: sampling min_source_duration_sec must be non-negative", name)
		}
	}
	if opt.Thresholds != nil {
		t := opt.Thresholds
		if t.MinVMAF < 0 || t.MinVMAF > 100 {
			return fmt.Errorf("profile %q: min_vmaf %v out of range 0-100", name, t.MinVMAF)
		}
		if t.TargetVMAF < 0 || t.TargetVMAF > 100 {
			return fmt.Errorf("profile %q: target_vmaf %v out of range 0-100", name, t.TargetVMAF)
		}
		if t.MinVMAF > 0 && t.TargetVMAF > 0 && t.TargetVMAF < t.MinVMAF {
			return fmt.Errorf("profile %q: target_vmaf (%v) must be >= min_vmaf (%v)", name, t.TargetVMAF, t.MinVMAF)
		}
		if t.MinSSIM < 0 || t.MinSSIM > 1.0 {
			return fmt.Errorf("profile %q: min_ssim %v out of range 0.0-1.0", name, t.MinSSIM)
		}
		if t.TargetSSIM < 0 || t.TargetSSIM > 1.0 {
			return fmt.Errorf("profile %q: target_ssim %v out of range 0.0-1.0", name, t.TargetSSIM)
		}
		if t.MinSSIM > 0 && t.TargetSSIM > 0 && t.TargetSSIM < t.MinSSIM {
			return fmt.Errorf("profile %q: target_ssim (%v) must be >= min_ssim (%v)", name, t.TargetSSIM, t.MinSSIM)
		}
	}
	if len(opt.QualityCandidates) > 0 {
		if len(opt.QualityCandidates) > 10 {
			return fmt.Errorf("profile %q: maximum 10 quality candidates allowed", name)
		}
		seen := map[int]bool{}
		for _, q := range opt.QualityCandidates {
			if q < 1 || q > 100 {
				return fmt.Errorf("profile %q: quality candidate %d out of range 1-100", name, q)
			}
			if seen[q] {
				return fmt.Errorf("profile %q: duplicate quality candidate %d", name, q)
			}
			seen[q] = true
		}
	}
	if opt.BitrateGuidance != nil {
		bg := opt.BitrateGuidance
		if bg.PreferredBitrate < 0 {
			return fmt.Errorf("profile %q: preferred_bitrate must be >= 0", name)
		}
		if bg.SoftMaxBitrate < 0 {
			return fmt.Errorf("profile %q: soft_max_bitrate must be >= 0", name)
		}
		if bg.PreferredBitrate > 0 && bg.SoftMaxBitrate > 0 && bg.SoftMaxBitrate < bg.PreferredBitrate {
			return fmt.Errorf("profile %q: soft_max_bitrate (%d) must be >= preferred_bitrate (%d)", name, bg.SoftMaxBitrate, bg.PreferredBitrate)
		}
	}
	return nil
}

func normalizeCodec(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
func normalizeContainer(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "matroska" {
		return "mkv"
	}
	return s
}
