package transcode

import (
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/transcode/optimization"
)

// VideoCodecCopy is the video codec of a size v3 audio-only plan: the
// programme video is stream-copied and only lossless audio is compacted.
const VideoCodecCopy = "copy"

// VideoCopyMethod names the bit-identical video proof: ffmpeg's streamhash
// muxer (SHA-256) over the stream-copied packets of the main video.
const VideoCopyMethod = "ffmpeg_streamhash_sha256"

// VideoCopyEvidence proves that an audio-only candidate carries the source's
// main video unchanged. Hashes are of packet payloads, so a remux into a new
// container still matches while any re-encode, drop or edit does not.
type VideoCopyEvidence struct {
	Method        string   `json:"method"`
	Verdict       string   `json:"verdict"`
	SourceHash    string   `json:"source_hash"`
	CandidateHash string   `json:"candidate_hash"`
	SourceCodec   string   `json:"source_codec"`
	ReasonCodes   []string `json:"reason_codes,omitempty"`
}

// IsSizeAudioOnlyPlan reports a size v3 plan that copies the video stream.
func IsSizeAudioOnlyPlan(p *Plan) bool {
	return p != nil && p.Mode == "size" && p.ModePolicyVersion == optimization.ModePolicyV3 && strings.EqualFold(strings.TrimSpace(p.VideoCodec), VideoCodecCopy)
}

// NewSizeAudioOnlyPlan derives the audio-only plan from a frozen size v3
// plan: the same audio, subtitle, preservation and size policy, with every
// video encoder setting cleared. No perceptual validation applies because
// the video is not re-encoded; the final check is VideoCopyEvidence.
func NewSizeAudioOnlyPlan(base *Plan, sourceBitDepth int) (*Plan, error) {
	if base == nil || base.Mode != "size" || base.ModePolicyVersion != optimization.ModePolicyV3 || base.AudioMode != AudioModeCompactLossless || base.Compact != nil || base.SizePolicy == nil || base.PolicyDigest == "" {
		return nil, fmt.Errorf("audio-only plan requires a frozen size v3 plan with lossless audio compaction (fail closed)")
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
	if p.AudioMode != AudioModeCompactLossless || p.SizePolicy == nil || p.PolicyDigest == "" || (p.ExpectedBitDepth != 8 && p.ExpectedBitDepth != 10) {
		return fmt.Errorf("audio-only plan requires lossless audio compaction, a frozen size policy and the source bit depth (fail closed)")
	}
	return nil
}

// Check reports whether the evidence proves an unchanged video stream.
func (e *VideoCopyEvidence) Check() error {
	if e == nil || e.Method != VideoCopyMethod || e.Verdict != "pass" || e.SourceHash == "" || e.SourceHash != e.CandidateHash {
		return fmt.Errorf("video copy evidence is missing or the video stream changed")
	}
	return nil
}
