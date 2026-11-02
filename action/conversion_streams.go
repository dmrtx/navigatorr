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
	BeforeBytes   int64  `json:"before_bytes,omitempty"`
	Fate          string `json:"fate"`
	AfterCodec    string `json:"after_codec,omitempty"`
	AfterChannels int    `json:"after_channels,omitempty"`
	AfterKbps     int    `json:"after_kbps,omitempty"`
	AfterBytes    int64  `json:"after_bytes,omitempty"`
}

type ConversionSubtitles struct {
	Total        int   `json:"total"`
	Kept         int   `json:"kept"`
	DroppedBytes int64 `json:"dropped_bytes,omitempty"`
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
	out := &ConversionStreams{DurationSec: dur, Audio: []ConversionAudioTrack{}, Video: ConversionVideo{BeforeBytes: p.VideoBytes}}
	if main := rep.MainVideo(); len(main) > 0 {
		out.Video.Codec = main[0].Codec
	}
	out.Video.AfterCodec = plan.VideoCodec
	if transcode.IsSizeAudioOnlyPlan(plan) {
		out.Video.AfterCodec = out.Video.Codec
	}
	rest, known := candidateBytes, candidateBytes > 0
	for _, a := range rep.Audio {
		t := ConversionAudioTrack{Index: a.Index, Language: transcode.TrackInfo{Language: a.Language}.LanguageLabel(), Title: a.Title, Codec: a.Codec, Channels: a.Channels, BeforeBytes: p.Audio[a.Index].Bytes}
		switch o := transcode.AudioOutputFor(plan.AudioMode, audioTrackOf(a, dur)); {
		case !plan.Tracks.Keeps("audio", a.Index):
			t.Fate = "dropped"
		case o.Codec == "copy" || o.Codec == "":
			t.Fate, t.AfterCodec, t.AfterChannels, t.AfterBytes = "copied", a.Codec, a.Channels, t.BeforeBytes
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
		if plan.Tracks.Keeps("subtitle", s.Index) {
			out.Subtitles.Kept++
			rest -= p.Subtitles[s.Index]
		} else {
			out.Subtitles.DroppedBytes += p.Subtitles[s.Index]
		}
	}
	if known && rest > 0 {
		out.Video.AfterBytes = rest
	}
	return out
}
