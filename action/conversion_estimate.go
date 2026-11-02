package action

import (
	"math"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// EstimateSettings are the goal settings a person is choosing, before a job
// exists: the goal's mode, whether only the audio is converted, the audio
// mode (a key of PreviewAudioModes, or "copy"), the tracks kept, the compact
// height and a target size.
type EstimateSettings struct {
	Mode          string
	AudioOnly     bool
	AudioMode     string
	Tracks        *transcode.TrackSelection
	MaxHeight     int
	BudgetPercent float64
	BudgetBytes   int64
}

// ConversionEstimate is what a goal would make of each stream, in the shape
// a finished job's streams have. Nothing is measured: the audio is the audio
// policy's bitrate over the duration, and the video is Basis: "copied" (Audio
// only), "size_budget" (the target less the audio and subtitles) or
// "heuristic" (a bitrate rule by codec and height).
type ConversionEstimate struct {
	Streams     *ConversionStreams `json:"streams"`
	AudioMode   string             `json:"audio_mode"`
	VideoBasis  string             `json:"video_basis"`
	TotalBefore int64              `json:"total_before"`
	TotalAfter  int64              `json:"total_after"`
	Estimate    bool               `json:"estimate"`
}

// estimateVideoKbps is the HEVC bitrate the heuristic aims a picture of this
// height at.
func estimateVideoKbps(height int) float64 {
	switch {
	case height <= 576:
		return 1200
	case height <= 720:
		return 1800
	case height <= 1080:
		return 3000
	case height <= 1440:
		return 6000
	}
	return 10000
}

// EstimateConversion estimates a goal on a probed source. It is nil without
// a programme video.
func EstimateConversion(rep *mediainspect.DetailedReport, s EstimateSettings) *ConversionEstimate {
	if rep == nil || len(rep.MainVideo()) == 0 {
		return nil
	}
	audioMode := ""
	switch {
	case s.AudioMode == "copy":
	case s.AudioMode != "":
		audioMode = PreviewAudioModes[s.AudioMode]
	case s.Mode == "size" || s.Mode == "compact":
		audioMode = PreviewAudioModes[s.Mode]
	}
	plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, AudioMode: audioMode, Tracks: s.Tracks}
	streams := conversionStreams(rep, plan, 0)
	out := &ConversionEstimate{Streams: streams, AudioMode: firstNonEmptyMode(audioMode), Estimate: true, TotalBefore: rep.SizeBytes}
	rest := streams.Totals.AudioAfter + streams.Totals.SubtitlesAfter
	video := streams.Video.BeforeBytes
	switch {
	case s.AudioOnly:
		out.VideoBasis = "copied"
		streams.Video.AfterCodec = streams.Video.Codec
	case s.BudgetBytes > 0 || s.BudgetPercent > 0:
		out.VideoBasis = "size_budget"
		target := s.BudgetBytes
		if target <= 0 {
			target = int64(float64(rep.SizeBytes) * s.BudgetPercent / 100)
		}
		video = max(target-rest, 0)
	default:
		out.VideoBasis = "heuristic"
		main := rep.MainVideo()[0]
		height := main.Height
		scale := 1.0
		if s.Mode == "compact" {
			limit := s.MaxHeight
			if limit <= 0 {
				limit = 1080
			}
			if height > limit {
				scale = math.Pow(float64(limit)/float64(height), 2)
				height = limit
			}
		}
		ratio := 0.5
		switch main.Codec {
		case "hevc", "av1", "vp9":
			ratio = 0.8
		}
		video = int64(float64(streams.Video.BeforeBytes) * ratio * scale)
		if rep.DurationSec > 0 {
			video = min(video, int64(estimateVideoKbps(height)*1000/8*rep.DurationSec))
		}
	}
	if streams.Video.BeforeBytes <= 0 && out.VideoBasis != "size_budget" {
		video = 0
	}
	streams.Video.AfterBytes, streams.Totals.VideoAfter = video, video
	if video > 0 {
		out.TotalAfter = video + rest
	}
	return out
}

func firstNonEmptyMode(mode string) string {
	if mode == "" {
		return "copy"
	}
	return mode
}
