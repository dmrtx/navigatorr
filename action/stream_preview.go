package action

import (
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// PreviewAudioModes are the audio modes the stream preview plans each track
// under, keyed by the name the preview gives them: what a new size goal
// freezes (its level 1, and level 2, which a size budget may pick), and what
// compact freezes. Quality and x265_preserve copy every track.
var PreviewAudioModes = map[string]string{
	"size":    transcode.AudioModeSizeAACBed,
	"size_he": transcode.AudioModeSizeHEAACBed,
	"compact": "compact",
}

// StreamPreview is what a source's streams weigh and what each audio mode
// would make of its audio tracks, read from the probe alone: stream bitrates
// and Matroska statistics, the values the audio policy itself reads. Nothing
// here is measured, and an unknown size is zero.
type StreamPreview struct {
	SourceBytes int64
	DurationSec float64
	// VideoBytes is the programme video's size: its own statistics, else the
	// file less every audio and known subtitle track (VideoDerived), which
	// counts attachments and container overhead as video. Zero is unknown,
	// as it is when any audio track's size is.
	VideoBytes   int64
	VideoDerived bool
	Audio        map[int]AudioPreview
	Subtitles    map[int]int64
}

// AudioPreview is one source audio track: its size and bitrate, and its
// output under each of PreviewAudioModes.
type AudioPreview struct {
	Bytes   int64
	Kbps    int
	Outputs map[string]AudioPreviewOutput
}

// AudioPreviewOutput is a track under one audio mode: copied (Output "copy",
// Bytes the source's) or converted to "aac" or "he_aac" at Kbps, Bytes being
// that bitrate over the file's duration.
type AudioPreviewOutput struct {
	Output string
	Kbps   int
	Bytes  int64
}

// PreviewStreams sizes a probed source's streams for the stream preview.
func PreviewStreams(rep *mediainspect.DetailedReport) StreamPreview {
	p := StreamPreview{Audio: map[int]AudioPreview{}, Subtitles: map[int]int64{}}
	if rep == nil {
		return p
	}
	p.SourceBytes, p.DurationSec = rep.SizeBytes, rep.DurationSec
	dur, size := rep.DurationSec, rep.SizeBytes
	if dur <= 0 || size <= 0 {
		return p
	}
	rest, audioKnown := size, true
	for _, a := range rep.Audio {
		track := audioTrackOf(a, dur)
		ap := AudioPreview{Bytes: audioTrackBytes(a, dur, size), Kbps: track.BitrateKbps, Outputs: map[string]AudioPreviewOutput{}}
		for name, mode := range PreviewAudioModes {
			out := transcode.AudioOutputFor(mode, track)
			o := AudioPreviewOutput{Output: "copy", Bytes: ap.Bytes}
			if out.Codec != "copy" {
				o = AudioPreviewOutput{Output: "aac", Kbps: out.Kbps, Bytes: int64(out.Kbps) * 1000 * int64(dur) / 8}
				if out.HEAAC {
					o.Output = "he_aac"
				}
			}
			ap.Outputs[name] = o
		}
		if ap.Bytes <= 0 {
			audioKnown = false
		}
		rest -= ap.Bytes
		p.Audio[a.Index] = ap
	}
	for _, s := range rep.Subtitles {
		n := audioStreamBytes(s, dur, size)
		p.Subtitles[s.Index] = n
		rest -= n
	}
	if main := rep.MainVideo(); len(main) > 0 {
		if n := audioStreamBytes(main[0], dur, size); n > 0 {
			p.VideoBytes = n
			return p
		}
	}
	if audioKnown && rest > 0 {
		p.VideoBytes, p.VideoDerived = rest, true
	}
	return p
}
