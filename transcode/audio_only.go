package transcode

import (
	"fmt"
	"strings"
)

// VideoCodecCopy is the video codec of a size v3 audio-only plan: the
// programme video is stream-copied and only lossless audio is compacted.
const VideoCodecCopy = "copy"

// VideoCopyMethod names the bit-identical video proof: ffprobe's demuxed
// packet list of the main video, each payload hashed with SHA-256, compared
// packet by packet (count, order, size, keyframe flag, payload, timing).
const VideoCopyMethod = "ffprobe_packets_sha256"

// emptyPacketDigest is the digest of an empty packet list, never a proof.
const emptyPacketDigest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// VideoCopyEvidence proves that an audio-only candidate carries the source's
// main video unchanged. The hashes digest each file's demuxed packet list,
// so a remux into a new container still matches while any re-encode, drop
// (leading non-key packets included) or edit does not.
type VideoCopyEvidence struct {
	Method           string   `json:"method"`
	Verdict          string   `json:"verdict"`
	SourceHash       string   `json:"source_hash"`
	CandidateHash    string   `json:"candidate_hash"`
	SourcePackets    int      `json:"source_packets"`
	CandidatePackets int      `json:"candidate_packets"`
	SourceCodec      string   `json:"source_codec"`
	ReasonCodes      []string `json:"reason_codes,omitempty"`
}

// IsSizeAudioOnlyPlan reports a size plan (v3 or v5) that copies the video
// stream.
func IsSizeAudioOnlyPlan(p *Plan) bool {
	return p != nil && p.Mode == "size" && IsSizePolicyVersion(p.ModePolicyVersion) && strings.EqualFold(strings.TrimSpace(p.VideoCodec), VideoCodecCopy)
}

// SizeAudioMode reports the audio mode a size plan of this policy version
// carries: compact_lossless under v3, one of the audio levels under v5, either
// under v6 (which version the goal's snapshot extends is the coordinator's to
// check).
func SizeAudioMode(version, audioMode string) bool {
	switch version {
	case ModePolicyV3:
		return audioMode == AudioModeCompactLossless
	case ModePolicyV5:
		return IsAudioLevelMode(audioMode)
	case ModePolicyV6:
		return audioMode == AudioModeCompactLossless || IsAudioLevelMode(audioMode)
	}
	return false
}

// NewSizeAudioOnlyPlan derives the audio-only plan from a frozen size v3
// plan: the same audio, subtitle, preservation and size policy, with every
// video encoder setting cleared. No perceptual validation applies because
// the video is not re-encoded; the final check is VideoCopyEvidence.
func NewSizeAudioOnlyPlan(base *Plan, sourceBitDepth int) (*Plan, error) {
	if base == nil || base.Mode != "size" || !SizeAudioMode(base.ModePolicyVersion, base.AudioMode) || base.Compact != nil || base.SizePolicy == nil || base.PolicyDigest == "" {
		return nil, fmt.Errorf("audio-only plan requires a frozen size plan with its audio policy (fail closed)")
	}
	if sourceBitDepth != 8 && sourceBitDepth != 10 {
		return nil, fmt.Errorf("audio-only plan requires an 8- or 10-bit source (fail closed)")
	}
	p := Plan{
		ModePolicyVersion:            base.ModePolicyVersion,
		Mode:                         base.Mode,
		PolicyDigest:                 base.PolicyDigest,
		SizePolicy:                   &SizePolicy{MinSavingsPercent: base.SizePolicy.MinSavingsPercent, MaxSizeIncreasePercent: base.SizePolicy.MaxSizeIncreasePercent},
		Container:                    base.Container,
		VideoCodec:                   VideoCodecCopy,
		ExpectedBitDepth:             sourceBitDepth,
		AudioMode:                    base.AudioMode,
		SubtitleMode:                 base.SubtitleMode,
		ConvertIncompatibleSubtitles: base.ConvertIncompatibleSubtitles,
		PreserveMetadata:             base.PreserveMetadata,
		PreserveChapters:             base.PreserveChapters,
		PreserveAttachments:          base.PreserveAttachments,
		SubtitleActions:              append([]SubtitleAction(nil), base.SubtitleActions...),
		Tracks:                       base.Tracks,
		RecipeVersion:                base.RecipeVersion,
		RecipeDigest:                 base.RecipeDigest,
		Resilience:                   base.Resilience,
	}
	p.Resilience.RetryBackoffSeconds = append([]int(nil), base.Resilience.RetryBackoffSeconds...)
	p.Resilience.RetryOn = append([]string(nil), base.Resilience.RetryOn...)
	digest, err := DigestPlan(&p)
	if err != nil {
		return nil, err
	}
	p.PlanDigest = digest
	return &p, ValidateSizeAudioOnlyShape(&p)
}

// ValidateSizeAudioOnlyShape admits exactly the plan NewSizeAudioOnlyPlan
// builds: no encoder knob, compact chain or perceptual plan may ride along
// with a copied video stream.
func ValidateSizeAudioOnlyShape(p *Plan) error {
	if !IsSizeAudioOnlyPlan(p) {
		return fmt.Errorf("not a size v3 audio-only plan (fail closed)")
	}
	encoderKnobs := p.X265 != nil || p.Quality != 0 || p.VideoProfile != "" || p.PixelFormat != "" || p.Preset != "" || p.Tune != "" ||
		p.PrioritizeSpeed != nil || p.SpatialAQ != nil || p.Realtime != nil || p.AverageBitrateKbps != 0 || p.MaxBitrateKbps != 0 ||
		p.ConstantBitrate != nil || p.QMin != nil || p.QMax != nil || p.GOPSize != nil || p.BFrames != nil || p.ClosedGOP != nil ||
		p.PowerEfficient != nil || p.MaxRefFrames != nil
	if encoderKnobs || p.Compact != nil || p.Podcast != nil || p.QualityValidation != nil || len(p.AppliedFallbacks) > 0 {
		return fmt.Errorf("audio-only plan carries video encoder or perceptual settings (fail closed)")
	}
	if !SizeAudioMode(p.ModePolicyVersion, p.AudioMode) || p.SizePolicy == nil || p.PolicyDigest == "" || (p.ExpectedBitDepth != 8 && p.ExpectedBitDepth != 10) {
		return fmt.Errorf("audio-only plan requires its size audio policy, a frozen size policy and the source bit depth (fail closed)")
	}
	return nil
}

// Check reports whether the evidence proves an unchanged video stream.
func (e *VideoCopyEvidence) Check() error {
	if e == nil || e.Method != VideoCopyMethod || e.Verdict != "pass" || e.SourceHash == "" || e.SourceHash == emptyPacketDigest || e.SourceHash != e.CandidateHash ||
		e.SourcePackets <= 0 || e.SourcePackets != e.CandidatePackets {
		return fmt.Errorf("video copy evidence is missing or the video stream changed")
	}
	return nil
}
