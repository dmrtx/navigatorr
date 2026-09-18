package optimization

import (
	"fmt"
	"math"
	"strings"
)

// DefaultContainerOverheadRate represents a typical 0.5% muxing overhead for MKV/MP4 containers.
const DefaultContainerOverheadRate = 0.005

// DefaultTextSubtitleSizeBytes is a conservative 2 MiB allowance per text
// subtitle stream when ffprobe has no byte count. This is an estimate, not a
// bound: unusually large scripts remain visible as estimation uncertainty.
// Bitmap subtitles and unknown codecs require measured size or an explicit
// caller fallback because their payload can be significant.
const DefaultTextSubtitleSizeBytes int64 = 2 * 1024 * 1024

func isTextSubtitleCodec(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "srt", "subrip", "mov_text", "ass", "ssa", "webvtt":
		return true
	default:
		return false
	}
}

// AudioStreamEstimate holds stream metadata needed to estimate audio payload.
type AudioStreamEstimate struct {
	Index              int    `json:"index"`
	Codec              string `json:"codec,omitempty"`
	Channels           int    `json:"channels,omitempty"`
	BitrateBps         int64  `json:"bitrate_bps,omitempty"`          // Declared / probed bitrate in bits per second
	SizeBytes          int64  `json:"size_bytes,omitempty"`           // Known stream size in bytes if already probed
	FallbackBitrateBps int64  `json:"fallback_bitrate_bps,omitempty"` // Caller-supplied explicit fallback bitrate in bits per second
	Copied             bool   `json:"copied"`                         // True if copied; false if re-encoded
	Discarded          bool   `json:"discarded,omitempty"`            // True if this stream is discarded/omitted from output
}

// SubtitleStreamEstimate holds metadata needed to estimate subtitle stream overhead.
type SubtitleStreamEstimate struct {
	Index             int    `json:"index"`
	Codec             string `json:"codec,omitempty"`
	SizeBytes         int64  `json:"size_bytes,omitempty"`          // Known stream size in bytes if already probed
	FallbackSizeBytes int64  `json:"fallback_size_bytes,omitempty"` // Caller-supplied explicit fallback size in bytes
}

// OutputEstimateInput defines the inputs for computing an accurate output size and savings estimate.
type OutputEstimateInput struct {
	// SourceSizeBytes is the original media file size in bytes.
	SourceSizeBytes int64 `json:"source_size_bytes"`
	// TotalDurationSeconds is the total duration of the media file in seconds.
	TotalDurationSeconds float64 `json:"total_duration_seconds"`

	// Sample-based video estimation (preferred):
	SampleDurationSeconds float64 `json:"sample_duration_seconds,omitempty"`
	SampleVideoBytes      int64   `json:"sample_video_bytes,omitempty"`

	// Declared video bitrate fallback in bits per second (used only when sample video bytes are unavailable):
	DeclaredVideoBitrateBps int64 `json:"declared_video_bitrate_bps,omitempty"`

	// Audio, subtitle, and attachment streams:
	AudioStreams    []AudioStreamEstimate    `json:"audio_streams,omitempty"`
	SubtitleStreams []SubtitleStreamEstimate `json:"subtitle_streams,omitempty"`
	AttachmentBytes int64                    `json:"attachment_bytes,omitempty"`

	// ContainerOverheadRate is the estimated container packaging overhead (default 0.005 / 0.5%).
	ContainerOverheadRate float64 `json:"container_overhead_rate,omitempty"`
}

// EstimationResult provides a transparent breakdown of video, audio, subtitles,
// container overhead, total bytes/MB, estimated savings, and visible uncertainty/unusable reasons.
type EstimationResult struct {
	SuitableForSelection      bool     `json:"suitable_for_selection"`
	UnusableReason            string   `json:"unusable_reason,omitempty"`
	EstimatedVideoBytes       int64    `json:"estimated_video_bytes"`
	EstimatedAudioBytes       int64    `json:"estimated_audio_bytes"`
	EstimatedSubtitleBytes    int64    `json:"estimated_subtitle_bytes"`
	EstimatedAttachmentBytes  int64    `json:"estimated_attachment_bytes"`
	EstimatedMuxOverheadBytes int64    `json:"estimated_mux_overhead_bytes"`
	EstimatedTotalBytes       int64    `json:"estimated_total_bytes"`
	EstimatedTotalMB          float64  `json:"estimated_total_mb"` // Decimal MB (1,000,000 bytes)
	SavingsBytes              int64    `json:"savings_bytes"`
	SavingsPercent            float64  `json:"savings_percent"`
	Uncertainties             []string `json:"uncertainties,omitempty"`
}

// EstimateOutput calculates the expected output size by isolating the video stream
// alongside audio, subtitles, and attachments. It validates inputs, guards against
// overflow and non-finite numbers, and marks estimates lacking video or material
// stream estimates unsuitable for selection. Known text subtitle codecs may use
// a conservative default with visible uncertainty when their size is unknown.
func EstimateOutput(in OutputEstimateInput) (EstimationResult, error) {
	res := EstimationResult{
		SuitableForSelection: true,
		Uncertainties:        make([]string, 0),
	}

	// 1. Validate estimator inputs
	if !isFinite(in.TotalDurationSeconds) || in.TotalDurationSeconds <= 0 {
		res.SuitableForSelection = false
		res.UnusableReason = ReasonInvalidEstimatorInput
		return res, fmt.Errorf("%s: total_duration_seconds must be finite and positive", ReasonInvalidEstimatorInput)
	}

	if !isFinite(in.SampleDurationSeconds) || in.SampleDurationSeconds < 0 {
		res.SuitableForSelection = false
		res.UnusableReason = ReasonInvalidEstimatorInput
		return res, fmt.Errorf("%s: sample_duration_seconds must be finite and non-negative", ReasonInvalidEstimatorInput)
	}

	if in.SampleVideoBytes < 0 || in.DeclaredVideoBitrateBps < 0 || in.SourceSizeBytes < 0 || in.AttachmentBytes < 0 {
		res.SuitableForSelection = false
		res.UnusableReason = ReasonInvalidEstimatorInput
		return res, fmt.Errorf("%s: byte counts and bitrates cannot be negative", ReasonInvalidEstimatorInput)
	}

	if !isFinite(in.ContainerOverheadRate) || in.ContainerOverheadRate < 0 || in.ContainerOverheadRate >= 1.0 {
		res.SuitableForSelection = false
		res.UnusableReason = ReasonInvalidEstimatorInput
		return res, fmt.Errorf("%s: container_overhead_rate must be finite and in [0, 1)", ReasonInvalidEstimatorInput)
	}

	for _, a := range in.AudioStreams {
		if a.BitrateBps < 0 || a.SizeBytes < 0 || a.FallbackBitrateBps < 0 {
			res.SuitableForSelection = false
			res.UnusableReason = ReasonInvalidEstimatorInput
			return res, fmt.Errorf("%s: audio stream bitrate and size cannot be negative", ReasonInvalidEstimatorInput)
		}
	}

	for _, s := range in.SubtitleStreams {
		if s.SizeBytes < 0 || s.FallbackSizeBytes < 0 {
			res.SuitableForSelection = false
			res.UnusableReason = ReasonInvalidEstimatorInput
			return res, fmt.Errorf("%s: subtitle stream size cannot be negative", ReasonInvalidEstimatorInput)
		}
	}

	// 2. Video estimation guarded against overflow
	if in.SampleVideoBytes > 0 && in.SampleDurationSeconds > 0 {
		scale := in.TotalDurationSeconds / in.SampleDurationSeconds
		videoFloat := float64(in.SampleVideoBytes) * scale
		vb, err := safeFloatToInt64(videoFloat)
		if err != nil {
			res.SuitableForSelection = false
			res.UnusableReason = ReasonIntegerOverflow
			return res, err
		}
		res.EstimatedVideoBytes = vb
	} else if in.DeclaredVideoBitrateBps > 0 {
		videoFloat := float64(in.DeclaredVideoBitrateBps) * in.TotalDurationSeconds / 8.0
		vb, err := safeFloatToInt64(videoFloat)
		if err != nil {
			res.SuitableForSelection = false
			res.UnusableReason = ReasonIntegerOverflow
			return res, err
		}
		res.EstimatedVideoBytes = vb
		res.Uncertainties = append(res.Uncertainties, ReasonVideoBitrateFallback)
	} else {
		res.EstimatedVideoBytes = 0
		res.SuitableForSelection = false
		res.UnusableReason = ReasonInvalidVideoEstimate
		res.Uncertainties = append(res.Uncertainties, ReasonVideoPayloadUncertain)
	}

	// 3. Audio estimation (copied or re-encoded streams in output)
	for _, a := range in.AudioStreams {
		if a.Discarded {
			// Stream is explicitly discarded from output container.
			continue
		}

		var streamBytes int64
		if a.SizeBytes > 0 {
			streamBytes = a.SizeBytes
		} else if a.BitrateBps > 0 {
			raw := float64(a.BitrateBps) * in.TotalDurationSeconds / 8.0
			sb, err := safeFloatToInt64(raw)
			if err != nil {
				res.SuitableForSelection = false
				res.UnusableReason = ReasonIntegerOverflow
				return res, err
			}
			streamBytes = sb
			if !a.Copied {
				res.Uncertainties = append(res.Uncertainties, fmt.Sprintf("%s:stream_%d", ReasonAudioBitrateFallback, a.Index))
			}
		} else if a.FallbackBitrateBps > 0 {
			raw := float64(a.FallbackBitrateBps) * in.TotalDurationSeconds / 8.0
			sb, err := safeFloatToInt64(raw)
			if err != nil {
				res.SuitableForSelection = false
				res.UnusableReason = ReasonIntegerOverflow
				return res, err
			}
			streamBytes = sb
			res.Uncertainties = append(res.Uncertainties, fmt.Sprintf("%s:stream_%d", ReasonAudioBitrateFallback, a.Index))
		} else {
			// Output audio stream lacks an explicit measured size, declared bitrate, or fallback.
			// We may NOT silently ignore non-copied or copied audio, which would underestimate size.
			res.SuitableForSelection = false
			if res.UnusableReason == "" {
				res.UnusableReason = ReasonMissingStreamBitrate
			}
			res.Uncertainties = append(res.Uncertainties, fmt.Sprintf("%s:stream_%d", ReasonMissingStreamBitrate, a.Index))
		}

		var err error
		res.EstimatedAudioBytes, err = safeAddInt64(res.EstimatedAudioBytes, streamBytes)
		if err != nil {
			res.SuitableForSelection = false
			res.UnusableReason = ReasonIntegerOverflow
			return res, err
		}
	}

	// 4. Subtitle estimation
	for _, s := range in.SubtitleStreams {
		var streamBytes int64
		if s.SizeBytes > 0 {
			streamBytes = s.SizeBytes
		} else if s.FallbackSizeBytes > 0 {
			streamBytes = s.FallbackSizeBytes
			res.Uncertainties = append(res.Uncertainties, fmt.Sprintf("%s:stream_%d", ReasonSubtitleSizeEstimated, s.Index))
		} else if isTextSubtitleCodec(s.Codec) {
			streamBytes = DefaultTextSubtitleSizeBytes
			res.Uncertainties = append(res.Uncertainties, fmt.Sprintf("%s:stream_%d", ReasonSubtitleSizeEstimated, s.Index))
		} else {
			// Bitmap and unknown subtitles cannot use the small text allowance.
			res.SuitableForSelection = false
			if res.UnusableReason == "" {
				res.UnusableReason = ReasonMissingSubtitleSize
			}
			res.Uncertainties = append(res.Uncertainties, fmt.Sprintf("%s:stream_%d", ReasonMissingSubtitleSize, s.Index))
		}

		var err error
		res.EstimatedSubtitleBytes, err = safeAddInt64(res.EstimatedSubtitleBytes, streamBytes)
		if err != nil {
			res.SuitableForSelection = false
			res.UnusableReason = ReasonIntegerOverflow
			return res, err
		}
	}

	// 5. Attachments
	res.EstimatedAttachmentBytes = in.AttachmentBytes

	// 6. Mux overhead guarded against overflow
	overheadRate := in.ContainerOverheadRate
	if overheadRate <= 0 {
		overheadRate = DefaultContainerOverheadRate
	}

	payloadBytes, err := safeAddInt64(res.EstimatedVideoBytes, res.EstimatedAudioBytes)
	if err != nil {
		res.SuitableForSelection = false
		res.UnusableReason = ReasonIntegerOverflow
		return res, err
	}
	payloadBytes, err = safeAddInt64(payloadBytes, res.EstimatedSubtitleBytes)
	if err != nil {
		res.SuitableForSelection = false
		res.UnusableReason = ReasonIntegerOverflow
		return res, err
	}
	payloadBytes, err = safeAddInt64(payloadBytes, res.EstimatedAttachmentBytes)
	if err != nil {
		res.SuitableForSelection = false
		res.UnusableReason = ReasonIntegerOverflow
		return res, err
	}

	overheadFloat := float64(payloadBytes) * overheadRate
	muxBytes, err := safeFloatToInt64(overheadFloat)
	if err != nil {
		res.SuitableForSelection = false
		res.UnusableReason = ReasonIntegerOverflow
		return res, err
	}
	res.EstimatedMuxOverheadBytes = muxBytes

	// 7. Total bytes and decimal MB (1,000,000 bytes)
	totalBytes, err := safeAddInt64(payloadBytes, muxBytes)
	if err != nil {
		res.SuitableForSelection = false
		res.UnusableReason = ReasonIntegerOverflow
		return res, err
	}
	res.EstimatedTotalBytes = totalBytes
	res.EstimatedTotalMB = round2(float64(res.EstimatedTotalBytes) / 1000000.0)

	// 8. Savings calculation
	if in.SourceSizeBytes > 0 {
		res.SavingsBytes = in.SourceSizeBytes - res.EstimatedTotalBytes
		res.SavingsPercent = round2(float64(res.SavingsBytes) / float64(in.SourceSizeBytes) * 100.0)
	} else {
		res.Uncertainties = append(res.Uncertainties, ReasonMissingSourceSize)
	}

	// 9. Final usability gate
	if res.EstimatedVideoBytes <= 0 || res.EstimatedTotalBytes <= 0 {
		res.SuitableForSelection = false
		if res.UnusableReason == "" {
			res.UnusableReason = ReasonUnusableEstimate
		}
	}

	return res, nil
}

func round2(val float64) float64 {
	return math.Round(val*100.0) / 100.0
}
