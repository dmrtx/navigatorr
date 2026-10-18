package transcode

import (
	"regexp"
	"strings"
)

// AudioModeCompactLossless is size mode's audio policy: like compact, but only
// lossless tracks are converted. Lossy tracks, DTS core and E-AC-3 JOC
// (Atmos) included, are copied. Lossless object audio (TrueHD Atmos,
// DTS-HD MA + DTS:X) is converted too: AAC keeps its channel bed and loses
// the object and height metadata.
const AudioModeCompactLossless = "compact_lossless"

// AudioTrack is what the audio policy reads from a source track. Title and
// Handler (the stream's handler_name tag) are free text that may name object
// audio when the probed profile does not.
type AudioTrack struct {
	Codec    string
	Profile  string
	Title    string
	Handler  string
	Channels int
	Layout   string
}

// AudioHandler returns a stream's handler_name tag, matched case-insensitively.
func AudioHandler(tags map[string]string) string {
	for k, v := range tags {
		if strings.EqualFold(k, "handler_name") {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// DetailedAudioTrack builds an AudioTrack from a mediainspect stream's
// fields, so the coordinator reads the same title and handler as the worker.
func DetailedAudioTrack(codec, profile, title string, tags map[string]string, channels int, layout string) AudioTrack {
	return AudioTrack{Codec: codec, Profile: profile, Title: strings.TrimSpace(title), Handler: AudioHandler(tags), Channels: channels, Layout: layout}
}

// AudioTarget implements opt-in audio policies without a second optimizer.
// he_aac explicitly converts 1-6 channels to HE-AAC via AudioToolbox.
// Preserve track count, channels and layout: every mode copies a track whose
// reported layout AAC cannot carry unchanged. Efficient lossy codecs (E-AC-3
// JOC included, so Atmos survives on them), >7.1 tracks and reported layouts
// AAC cannot carry unchanged are copied. DTS/lossless tracks become AAC (lossy).
// compact_lossless needs the profile to tell DTS-HD MA from DTS core.
func AudioTarget(mode string, t AudioTrack) (string, int) {
	codec := strings.ToLower(strings.TrimSpace(t.Codec))
	channels, layout := t.Channels, t.Layout
	if mode == AudioModeCompactLossless {
		if !losslessAudio(codec, t.Profile) {
			return "copy", 0
		}
		mode = "compact"
	}
	if mode == "he_aac" {
		if channels < 1 || channels > 6 || !aacKeepsLayout(layout, channels) {
			return "copy", 0
		}
		if channels <= 2 {
			return "aac", 64
		}
		return "aac", 160
	}
	if mode != "compact" || channels < 1 || channels > 8 || !aacKeepsLayout(layout, channels) {
		return "copy", 0
	}
	switch codec {
	case "dts", "truehd", "mlp", "flac", "alac":
	default:
		if !strings.HasPrefix(codec, "pcm_") {
			return "copy", 0
		}
	}
	bitrate := 192
	if channels == 1 {
		bitrate = 128
	} else if channels > 2 {
		bitrate = 384
		if channels > 6 {
			bitrate = 512
		}
	}
	return "aac", bitrate
}

// losslessAudio reports a lossless track. DTS is lossless only with a
// reported DTS-HD MA profile; an unreported DTS profile is treated as core.
func losslessAudio(codec, profile string) bool {
	switch codec {
	case "truehd", "mlp", "flac", "alac":
		return true
	case "dts":
		return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(profile)), "DTS-HD MA")
	}
	return strings.HasPrefix(codec, "pcm_")
}

// ObjectAudio reports Dolby Atmos or DTS:X object metadata, which AAC cannot
// carry. A mux or an older ffprobe may report TrueHD Atmos as plain TrueHD,
// or DTS:X as DTS-HD MA, so a title or handler naming them counts too.
func ObjectAudio(t AudioTrack) bool {
	return ObjectAudioText(t.Profile) || ObjectAudioText(t.Title) || ObjectAudioText(t.Handler)
}

// ObjectAudioText reports free text naming Atmos or DTS:X as a word
// ("Atmosphere" does not count).
func ObjectAudioText(s string) bool {
	return objectAudioProfile.MatchString(s)
}

var objectAudioProfile = regexp.MustCompile(`(?i)\batmos\b|\bdts[:\s-]?x\b`)

// aacLayouts are the channel layouts FFmpeg's AAC encoder writes back
// unchanged, keyed to their channel count. AAC has one channel
// configuration for side and back surrounds, so the "(side)" variants decode
// as their back-surround equivalent (see SameAudioLayout); layouts it remaps
// (6.1, 6.0, 7.1(wide-side), the "(front)" variants) or rejects are absent.
var aacLayouts = map[string]int{
	"mono": 1, "stereo": 2, "2.1": 3, "3.0": 3, "3.0(back)": 3, "4.0": 4, "quad": 4, "quad(side)": 4, "3.1": 4,
	"5.0": 5, "5.0(side)": 5, "4.1": 5, "5.1": 6, "5.1(side)": 6, "hexagonal": 6, "6.1(back)": 7, "7.0": 7,
	"7.1": 8, "7.1(wide)": 8, "octagonal": 8, "5.1.2(back)": 8,
}

// aacKeepsLayout also admits an unspecified layout (PCM in Matroska has
// none): there is no layout to lose, and AAC writes the default one for the
// channel count.
func aacKeepsLayout(layout string, channels int) bool {
	layout = strings.ToLower(strings.TrimSpace(layout))
	if layout == "" {
		return true
	}
	n, ok := aacLayouts[layout]
	return ok && n == channels
}

// SameAudioLayout reports whether an output track kept the source layout,
// treating side and back surrounds as one (AAC cannot distinguish them). A
// source without a reported layout constrains only the channel count.
func SameAudioLayout(source, output string) bool {
	if strings.TrimSpace(source) == "" {
		return true
	}
	canon := func(l string) string {
		l = strings.ToLower(strings.TrimSpace(l))
		switch l {
		case "quad(side)":
			return "quad"
		case "5.0(side)":
			return "5.0"
		case "5.1(side)":
			return "5.1"
		}
		return l
	}
	return canon(source) == canon(output)
}
