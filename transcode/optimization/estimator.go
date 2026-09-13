package optimization

import (
	"fmt"
	"math"
	"strings"
)

// DefaultContainerOverheadRate represents a typical 0.5% muxing overhead for MKV/MP4 containers.
const DefaultContainerOverheadRate = 0.005

// AudioStreamEstimate holds metadata needed to estimate copied audio stream size.
type AudioStreamEstimate struct {
	Index      int    `json:"index"`
	Codec      string `json:"codec"`
	Channels   int    `json:"channels,omitempty"`
	BitrateBps int64  `json:"bitrate_bps,omitempty"` // Declared bitrate in bits per second
	SizeBytes  int64  `json:"size_bytes,omitempty"`  // Known stream size in bytes if already probed
	Copied     bool   `json:"copied"`                // True if the stream is copied without re-encoding
}

// SubtitleStreamEstimate holds metadata needed to estimate subtitle stream overhead.
type SubtitleStreamEstimate struct {
	Index     int    `json:"index"`
	Codec     string `json:"codec"`
	SizeBytes int64  `json:"size_bytes,omitempty"` // Known stream size in bytes if already probed
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
// container overhead, total bytes/MB, estimated savings, and visible uncertainty reasons.
type EstimationResult struct {
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

// EstimateOutput calculates the expected output size by isolating the video stream
// from copied audio, subtitles, and attachments, never multiplying sample container size blindly.
func EstimateOutput(in OutputEstimateInput) EstimationResult {
	res := EstimationResult{
		Uncertainties: make([]string, 0),
	}

	if in.TotalDurationSeconds <= 0 {
		res.Uncertainties = append(res.Uncertainties, ReasonZeroDuration)
	}

	// 1. Video estimation
	if in.SampleVideoBytes > 0 && in.SampleDurationSeconds > 0 && in.TotalDurationSeconds > 0 {
		scale := in.TotalDurationSeconds / in.SampleDurationSeconds
		res.EstimatedVideoBytes = int64(math.Round(float64(in.SampleVideoBytes) * scale))
	} else if in.DeclaredVideoBitrateBps > 0 && in.TotalDurationSeconds > 0 {
		res.EstimatedVideoBytes = int64(math.Round(float64(in.DeclaredVideoBitrateBps) * in.TotalDurationSeconds / 8.0))
		res.Uncertainties = append(res.Uncertainties, ReasonVideoBitrateFallback)
	} else {
		res.EstimatedVideoBytes = 0
		res.Uncertainties = append(res.Uncertainties, ReasonVideoPayloadUncertain)
	}

	// 2. Audio estimation (copied streams)
	for _, a := range in.AudioStreams {
		var streamBytes int64
		if a.SizeBytes > 0 {
			streamBytes = a.SizeBytes
		} else if a.BitrateBps > 0 && in.TotalDurationSeconds > 0 {
			streamBytes = int64(math.Round(float64(a.BitrateBps) * in.TotalDurationSeconds / 8.0))
			res.Uncertainties = append(res.Uncertainties, fmt.Sprintf("%s:stream_%d", ReasonAudioBitrateFallback, a.Index))
		} else if in.TotalDurationSeconds > 0 {
			bps := heuristicAudioBitrate(a.Codec, a.Channels)
			streamBytes = int64(math.Round(float64(bps) * in.TotalDurationSeconds / 8.0))
			res.Uncertainties = append(res.Uncertainties, fmt.Sprintf("%s:stream_%d", ReasonAudioHeuristicFallback, a.Index))
		}
		res.EstimatedAudioBytes += streamBytes
	}

	// 3. Subtitle estimation (copied / passthrough)
	for _, s := range in.SubtitleStreams {
		var streamBytes int64
		if s.SizeBytes > 0 {
			streamBytes = s.SizeBytes
		} else {
			streamBytes = heuristicSubtitleSize(s.Codec)
			res.Uncertainties = append(res.Uncertainties, fmt.Sprintf("%s:stream_%d", ReasonSubtitleSizeEstimated, s.Index))
		}
		res.EstimatedSubtitleBytes += streamBytes
	}

	// 4. Attachments (fonts, cover art - copied 1:1, do not scale with duration)
	res.EstimatedAttachmentBytes = in.AttachmentBytes

	// 5. Mux overhead
	overheadRate := in.ContainerOverheadRate
	if overheadRate <= 0 {
		overheadRate = DefaultContainerOverheadRate
	}
	payloadBytes := res.EstimatedVideoBytes + res.EstimatedAudioBytes + res.EstimatedSubtitleBytes + res.EstimatedAttachmentBytes
	res.EstimatedMuxOverheadBytes = int64(math.Round(float64(payloadBytes) * overheadRate))

	// 6. Total bytes and MB
	res.EstimatedTotalBytes = payloadBytes + res.EstimatedMuxOverheadBytes
	res.EstimatedTotalMB = round2(float64(res.EstimatedTotalBytes) / (1024.0 * 1024.0))

	// 7. Savings calculation
	if in.SourceSizeBytes > 0 {
		res.SavingsBytes = in.SourceSizeBytes - res.EstimatedTotalBytes
		res.SavingsPercent = round2(float64(res.SavingsBytes) / float64(in.SourceSizeBytes) * 100.0)
	} else {
		res.Uncertainties = append(res.Uncertainties, ReasonMissingSourceSize)
	}

	return res
}

func heuristicAudioBitrate(codec string, channels int) int64 {
	c := strings.ToLower(strings.TrimSpace(codec))
	switch c {
	case "truehd", "dts-hd", "dts-hd ma", "flac", "pcm_s16le", "pcm_s24le":
		if channels >= 6 {
			return 3500000 // 3.5 Mbps for lossless 5.1/7.1
		}
		return 1500000 // 1.5 Mbps for lossless stereo
	case "ac3", "eac3", "dts", "aac", "opus", "mp3", "vorbis":
		if channels >= 6 {
			return 448000 // 448 kbps
		} else if channels == 1 {
			return 96000 // 96 kbps
		}
		return 192000 // 192 kbps stereo
	default:
		if channels >= 6 {
			return 448000
		}
		return 256000
	}
}

func heuristicSubtitleSize(codec string) int64 {
	c := strings.ToLower(strings.TrimSpace(codec))
	switch c {
	case "hdmv_pgs_subtitle", "pgs", "dvd_subtitle", "vobsub", "dvb_subtitle":
		return 20 * 1024 * 1024 // 20 MB typical for bitmap streams
	default:
		return 100 * 1024 // 100 KB typical for text streams (SRT, ASS)
	}
}

func round2(val float64) float64 {
	return math.Round(val*100.0) / 100.0
}
