package action

import (
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// ConversionStreams is what a finished conversion did to each of the
// source's streams, before and after, for a client to draw. Before is read
// from the probe the job planned from, after from the plan it ran: a copied
// track keeps its size, a converted one takes its bitrate over the duration,
// and the video is the new file less its audio and subtitles. Sizes are
// estimates; zero is unknown.
type ConversionStreams struct {
	DurationSec float64                `json:"duration_sec"`
	Video       ConversionVideo        `json:"video"`
	Audio       []ConversionAudioTrack `json:"audio"`
	Subtitles   ConversionSubtitles    `json:"subtitles"`
	Totals      ConversionTotals       `json:"totals"`
}

// ConversionTotals adds up each kind of stream before and after, and what the
// dropped tracks weighed. Zero is unknown, as for the video's size after
// before the new file exists.
type ConversionTotals struct {
	VideoBefore     int64 `json:"video_before"`
	VideoAfter      int64 `json:"video_after"`
	AudioBefore     int64 `json:"audio_before"`
	AudioAfter      int64 `json:"audio_after"`
	SubtitlesBefore int64 `json:"subtitles_before"`
	SubtitlesAfter  int64 `json:"subtitles_after"`
	RemovedBytes    int64 `json:"removed_bytes"`
}

type ConversionVideo struct {
	Codec       string `json:"codec"`
	BeforeBytes int64  `json:"before_bytes,omitempty"`
	AfterCodec  string `json:"after_codec,omitempty"`
	AfterBytes  int64  `json:"after_bytes,omitempty"`
}

// ConversionAudioTrack is one source audio track and its fate: copied,
// converted or dropped.
type ConversionAudioTrack struct {
	Index         int    `json:"index"`
	Language      string `json:"language"`
	Title         string `json:"title,omitempty"`
	Codec         string `json:"codec"`
	Channels      int    `json:"channels,omitempty"`
	BeforeKbps    int    `json:"before_kbps,omitempty"`
	BeforeBytes   int64  `json:"before_bytes,omitempty"`
	Fate          string `json:"fate"`
	AfterCodec    string `json:"after_codec,omitempty"`
	AfterChannels int    `json:"after_channels,omitempty"`
	AfterKbps     int    `json:"after_kbps,omitempty"`
	AfterBytes    int64  `json:"after_bytes,omitempty"`
}

type ConversionSubtitles struct {
	Total        int                       `json:"total"`
	Kept         int                       `json:"kept"`
	DroppedBytes int64                     `json:"dropped_bytes,omitempty"`
	Tracks       []ConversionSubtitleTrack `json:"tracks"`
}

// ConversionSubtitleTrack is one source subtitle and whether the new file
// keeps it. A kept subtitle is copied, so its size is the same after.
type ConversionSubtitleTrack struct {
	Index    int    `json:"index"`
	Language string `json:"language"`
	Title    string `json:"title,omitempty"`
	Codec    string `json:"codec"`
	Bytes    int64  `json:"bytes,omitempty"`
	Fate     string `json:"fate"`
}

// ConversionStreamsOf reads a transcode job's state. It is nil without the
// source probe or the plan.
func ConversionStreamsOf(state map[string]any, candidateBytes int64) *ConversionStreams {
	plan := getPlan(state["plan"])
	var rep mediainspect.DetailedReport
	if plan == nil || state["source_report"] == nil || !reencode(state["source_report"], &rep) || len(rep.Video) == 0 {
		return nil
	}
	return conversionStreams(&rep, plan, candidateBytes)
}

func conversionStreams(rep *mediainspect.DetailedReport, plan *transcode.Plan, candidateBytes int64) *ConversionStreams {
	p := PreviewStreams(rep)
	dur := rep.DurationSec
	out := &ConversionStreams{DurationSec: dur, Audio: []ConversionAudioTrack{}, Video: ConversionVideo{BeforeBytes: p.VideoBytes}, Subtitles: ConversionSubtitles{Tracks: []ConversionSubtitleTrack{}}}
	if main := rep.MainVideo(); len(main) > 0 {
		out.Video.Codec = main[0].Codec
	}
	out.Video.AfterCodec = plan.VideoCodec
	if transcode.IsSizeAudioOnlyPlan(plan) {
		out.Video.AfterCodec = out.Video.Codec
	}
	rest, known := candidateBytes, candidateBytes > 0
	for _, a := range rep.Audio {
		t := ConversionAudioTrack{Index: a.Index, Language: transcode.TrackInfo{Language: a.Language}.LanguageLabel(), Title: a.Title, Codec: a.Codec, Channels: a.Channels, BeforeKbps: p.Audio[a.Index].Kbps, BeforeBytes: p.Audio[a.Index].Bytes}
		switch o := transcode.AudioOutputFor(plan.AudioMode, audioTrackOf(a, dur)); {
		case !plan.Tracks.Keeps("audio", a.Index):
			t.Fate = "dropped"
		case o.Codec == "copy" || o.Codec == "":
			t.Fate, t.AfterCodec, t.AfterChannels, t.AfterKbps, t.AfterBytes = "copied", a.Codec, a.Channels, t.BeforeKbps, t.BeforeBytes
		default:
			t.Fate, t.AfterCodec, t.AfterChannels, t.AfterKbps = "converted", "aac", a.Channels, o.Kbps
			if o.HEAAC {
				t.AfterCodec = "he_aac"
			}
			if dur > 0 && o.Kbps > 0 {
				t.AfterBytes = int64(o.Kbps) * 1000 * int64(dur) / 8
			}
		}
		if t.Fate != "dropped" {
			rest -= t.AfterBytes
			known = known && t.AfterBytes > 0
		}
		out.Audio = append(out.Audio, t)
	}
	out.Subtitles.Total = len(rep.Subtitles)
	for _, s := range rep.Subtitles {
		t := ConversionSubtitleTrack{Index: s.Index, Language: transcode.TrackInfo{Language: s.Language}.LanguageLabel(), Title: s.Title, Codec: s.Codec, Bytes: p.Subtitles[s.Index], Fate: "dropped"}
		out.Totals.SubtitlesBefore += t.Bytes
		if plan.Tracks.Keeps("subtitle", s.Index) {
			t.Fate = "kept"
			out.Subtitles.Kept++
			out.Totals.SubtitlesAfter += t.Bytes
			rest -= t.Bytes
		} else {
			out.Subtitles.DroppedBytes += t.Bytes
		}
		out.Subtitles.Tracks = append(out.Subtitles.Tracks, t)
	}
	if known && rest > 0 {
		out.Video.AfterBytes = rest
	}
	out.Totals.VideoBefore, out.Totals.VideoAfter = out.Video.BeforeBytes, out.Video.AfterBytes
	out.Totals.RemovedBytes = out.Subtitles.DroppedBytes
	for _, a := range out.Audio {
		out.Totals.AudioBefore += a.BeforeBytes
		out.Totals.AudioAfter += a.AfterBytes
		if a.Fate == "dropped" {
			out.Totals.RemovedBytes += a.BeforeBytes
		}
	}
	return out
}
