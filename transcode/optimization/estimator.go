package optimization

import (
	"fmt"
	"math"
)

// DefaultContainerOverheadRate represents a typical 0.5% muxing overhead for MKV/MP4 containers.
const DefaultContainerOverheadRate = 0.005

// AudioStreamEstimate holds stream metadata needed to estimate copied audio.
type AudioStreamEstimate struct {
	Index              int    `json:"index"`
	Codec              string `json:"codec,omitempty"`
	Channels           int    `json:"channels,omitempty"`
	BitrateBps         int64  `json:"bitrate_bps,omitempty"`          // Declared / probed bitrate in bits per second
	SizeBytes          int64  `json:"size_bytes,omitempty"`           // Known copied stream size in bytes if already probed
	FallbackBitrateBps int64  `json:"fallback_bitrate_bps,omitempty"` // Caller-supplied explicit fallback bitrate in bits per second
	Copied             bool   `json:"copied"`                         // Must be true to include in copied audio estimate
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

// EstimationResult provides a transparent breakdown of video, copied audio, subtitles,
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
// from copied audio, subtitles, and attachments. It validates inputs, rejects magic heuristics,
// and marks estimates lacking video or stream estimates unsuitable for automatic selection.
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

	// 2. Video estimation
	if in.SampleVideoBytes > 0 && in.SampleDurationSeconds > 0 {
		scale := in.TotalDurationSeconds / in.SampleDurationSeconds
		res.EstimatedVideoBytes = int64(math.Round(float64(in.SampleVideoBytes) * scale))
	} else if in.DeclaredVideoBitrateBps > 0 {
		res.EstimatedVideoBytes = int64(math.Round(float64(in.DeclaredVideoBitrateBps) * in.TotalDurationSeconds / 8.0))
		res.Uncertainties = append(res.Uncertainties, ReasonVideoBitrateFallback)
	} else {
		res.EstimatedVideoBytes = 0
		res.SuitableForSelection = false
		res.UnusableReason = ReasonInvalidVideoEstimate
		res.Uncertainties = append(res.Uncertainties, ReasonVideoPayloadUncertain)
	}

	// 3. Audio estimation (copied streams only)
	for _, a := range in.AudioStreams {
		if !a.Copied {
			// Correctly honor Copied: non-copied audio streams are excluded from copied audio output
			continue
		}

		if a.SizeBytes > 0 {
			res.EstimatedAudioBytes += a.SizeBytes
		} else if a.BitrateBps > 0 {
			streamBytes := int64(math.Round(float64(a.BitrateBps) * in.TotalDurationSeconds / 8.0))
			res.EstimatedAudioBytes += streamBytes
		} else if a.FallbackBitrateBps > 0 {
			streamBytes := int64(math.Round(float64(a.FallbackBitrateBps) * in.TotalDurationSeconds / 8.0))
			res.EstimatedAudioBytes += streamBytes
			res.Uncertainties = append(res.Uncertainties, fmt.Sprintf("%s:stream_%d", ReasonAudioBitrateFallback, a.Index))
		} else {
			// No measured size, declared bitrate, or explicit caller fallback exists! Do not invent size.
			res.SuitableForSelection = false
			if res.UnusableReason == "" {
				res.UnusableReason = ReasonMissingStreamBitrate
			}
			res.Uncertainties = append(res.Uncertainties, fmt.Sprintf("%s:stream_%d", ReasonMissingStreamBitrate, a.Index))
		}
	}

	// 4. Subtitle estimation
	for _, s := range in.SubtitleStreams {
		if s.SizeBytes > 0 {
			res.EstimatedSubtitleBytes += s.SizeBytes
		} else if s.FallbackSizeBytes > 0 {
			res.EstimatedSubtitleBytes += s.FallbackSizeBytes
			res.Uncertainties = append(res.Uncertainties, fmt.Sprintf("%s:stream_%d", ReasonSubtitleSizeEstimated, s.Index))
		} else {
			// No measured size or caller-supplied explicit fallback exists! Do not invent size.
			res.SuitableForSelection = false
			if res.UnusableReason == "" {
				res.UnusableReason = ReasonMissingSubtitleSize
			}
			res.Uncertainties = append(res.Uncertainties, fmt.Sprintf("%s:stream_%d", ReasonMissingSubtitleSize, s.Index))
		}
	}

	// 5. Attachments
	res.EstimatedAttachmentBytes = in.AttachmentBytes

	// 6. Mux overhead
	overheadRate := in.ContainerOverheadRate
	if overheadRate <= 0 {
		overheadRate = DefaultContainerOverheadRate
	}
	payloadBytes := res.EstimatedVideoBytes + res.EstimatedAudioBytes + res.EstimatedSubtitleBytes + res.EstimatedAttachmentBytes
	res.EstimatedMuxOverheadBytes = int64(math.Round(float64(payloadBytes) * overheadRate))

	// 7. Total bytes and decimal MB (1,000,000 bytes)
	res.EstimatedTotalBytes = payloadBytes + res.EstimatedMuxOverheadBytes
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
