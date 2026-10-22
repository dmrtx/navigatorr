package transcode

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// AudioModeCompactLossless is size mode's audio policy: like compact, but only
// lossless tracks are converted. Lossy tracks, DTS core and E-AC-3 JOC
// (Atmos) included, are copied. Lossless object audio (TrueHD Atmos,
// DTS-HD MA + DTS:X) is converted too: AAC keeps its channel bed and loses
// the object and height metadata.
const AudioModeCompactLossless = "compact_lossless"

// The size goal's audio levels, least compressed first. A size goal with a
// byte budget freezes both and picks one per source (see AudioLevelModes).
// Level 0, AudioModeCompactLossless, is what size operations froze before the
// levels existed and is never chosen for a new one.
//
//	level 1  AudioModeSizeAAC    lossless to AAC-LC (stereo 192, 5.1 384, 7.1 512
//	                             kbps); a lossy track above its AAC-LC target is
//	                             re-encoded at it
//	level 2  AudioModeSizeHEAAC  every track of up to 6 channels above its HE-AAC
//	                             target becomes HE-AAC (stereo 64, 3 to 6
//	                             channels 160 kbps); the rest as level 1
//
// At every level a track at or below its target is copied, and so are E-AC-3
// JOC (Atmos), layouts AAC cannot keep, more than 8 channels, and a lossy track
// whose source bitrate is unknown. Track count, order, languages, titles,
// dispositions and channel layouts never change.
const (
	AudioModeSizeAAC   = "size_aac"
	AudioModeSizeHEAAC = "size_he_aac"
)

// AudioLevelModes are the audio modes of the size goal's levels 1 and 2, least
// compressed first.
var AudioLevelModes = []string{AudioModeSizeAAC, AudioModeSizeHEAAC}

// IsAudioLevelMode reports a size goal audio level (1 or 2).
func IsAudioLevelMode(mode string) bool {
	return mode == AudioModeSizeAAC || mode == AudioModeSizeHEAAC
}

// AudioTrack is what the audio policy reads from a source track. Title and
// Handler (the stream's handler_name tag) are free text that may name object
// audio when the probed profile does not. BitrateKbps is the source track's
// bitrate (see SourceAudioKbps), zero when unknown; only the size goal's
// levels read it.
type AudioTrack struct {
	Codec       string
	Profile     string
	Title       string
	Handler     string
	Channels    int
	Layout      string
	BitrateKbps int
}

// SourceAudioKbps is a track's source bitrate in kbps from what the probe
// states: ffprobe's bit_rate (or the BPS tag, which mediainspect folds into
// it), else the exact NUMBER_OF_BYTES statistic over the file's duration. Zero
// is unknown. The coordinator and the worker both call it with the same probe
// values, so they agree on every track's bitrate.
func SourceAudioKbps(bitRate int64, tags map[string]string, durationSec float64) int {
	if bitRate > 0 {
		return int(bitRate / 1000)
	}
	if durationSec <= 0 {
		return 0
	}
	var exact, variants []string
	for key := range tags {
		if k := strings.ToUpper(key); k == "NUMBER_OF_BYTES" {
			exact = append(exact, key)
		} else if strings.HasPrefix(k, "NUMBER_OF_BYTES-") {
			variants = append(variants, key)
		}
	}
	sort.Strings(exact)
	sort.Strings(variants)
	for _, key := range append(exact, variants...) {
		if n, err := strconv.ParseInt(strings.TrimSpace(tags[key]), 10, 64); err == nil && n > 0 {
			return int(float64(n) * 8 / durationSec / 1000)
		}
	}
	return 0
}

// AudioBitrateCeilingBps is the most a converted track may measure for a
// nominal bitrate: AAC is variable bitrate, so only gross overshoot is refused,
// not silence or padding. The worker's packet check and the coordinator's
// output check share it.
func AudioBitrateCeilingBps(kbps int) int64 {
	return int64(kbps)*2000 + 16000
}

// AudioOutput is what the audio policy makes of one source track: copied, or
// AAC at Kbps (HE-AAC when HEAAC, else AAC-LC).
type AudioOutput struct {
	Codec string
	Kbps  int
	HEAAC bool
}

var audioCopy = AudioOutput{Codec: "copy"}

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

// DetailedAudioTrackAt is DetailedAudioTrack with the source bitrate, which the
// size goal's levels need.
func DetailedAudioTrackAt(codec, profile, title string, tags map[string]string, channels int, layout string, bitRate int64, durationSec float64) AudioTrack {
	t := DetailedAudioTrack(codec, profile, title, tags, channels, layout)
	t.BitrateKbps = SourceAudioKbps(bitRate, tags, durationSec)
	return t
}

// AudioTarget is AudioOutputFor without the HE-AAC flag.
func AudioTarget(mode string, t AudioTrack) (string, int) {
	out := AudioOutputFor(mode, t)
	return out.Codec, out.Kbps
}

// AudioOutputFor implements opt-in audio policies without a second optimizer.
// he_aac explicitly converts 1-6 channels to HE-AAC via AudioToolbox.
// Preserve track count, channels and layout: every mode copies a track whose
// reported layout AAC cannot carry unchanged. Efficient lossy codecs (E-AC-3
// JOC included, so Atmos survives on them), >7.1 tracks and reported layouts
// AAC cannot carry unchanged are copied. DTS/lossless tracks become AAC (lossy).
// compact_lossless needs the profile to tell DTS-HD MA from DTS core. The size
// goal's levels (AudioModeSizeAAC, AudioModeSizeHEAAC) are in sizeLevelOutput.
func AudioOutputFor(mode string, t AudioTrack) AudioOutput {
	codec := strings.ToLower(strings.TrimSpace(t.Codec))
	channels, layout := t.Channels, t.Layout
	if IsAudioLevelMode(mode) {
		return sizeLevelOutput(mode, t)
	}
	if mode == AudioModeCompactLossless {
		if !losslessAudio(codec, t.Profile) {
			return audioCopy
		}
		mode = "compact"
	}
	if mode == "he_aac" {
		if channels < 1 || channels > 6 || !aacKeepsLayout(layout, channels) {
			return audioCopy
		}
		return AudioOutput{Codec: "aac", Kbps: heAACKbps(channels), HEAAC: true}
	}
	if mode != "compact" || channels < 1 || channels > 8 || !aacKeepsLayout(layout, channels) {
		return audioCopy
	}
	switch codec {
	case "dts", "truehd", "mlp", "flac", "alac":
	default:
		if !strings.HasPrefix(codec, "pcm_") {
			return audioCopy
		}
	}
	return AudioOutput{Codec: "aac", Kbps: aacLCKbps(channels)}
}

// aacLCKbps is the AAC-LC bitrate for a track of this many channels.
func aacLCKbps(channels int) int {
	switch {
	case channels == 1:
		return 128
	case channels == 2:
		return 192
	case channels <= 6:
		return 384
	}
	return 512
}

// heAACKbps is the HE-AAC bitrate for a track of 1 to 6 channels.
func heAACKbps(channels int) int {
	if channels <= 2 {
		return 64
	}
	return 160
}

// sizeLevelOutput is one track under the size goal's audio levels. A track is
// converted only when it is above the target of the level, so a conversion
// never raises its bitrate, and only when its bitrate is known: a lossy track
// of unknown bitrate is copied, never guessed. Lossless is always larger than
// any target, so it converts even when its bitrate is unknown.
func sizeLevelOutput(mode string, t AudioTrack) AudioOutput {
	channels := t.Channels
	if channels < 1 || channels > 8 || !aacKeepsLayout(t.Layout, channels) {
		return audioCopy
	}
	lossless := losslessAudio(strings.ToLower(strings.TrimSpace(t.Codec)), t.Profile)
	if !lossless && (t.BitrateKbps <= 0 || ObjectAudio(t)) {
		return audioCopy
	}
	out := AudioOutput{Codec: "aac", Kbps: aacLCKbps(channels)}
	if mode == AudioModeSizeHEAAC && channels <= 6 {
		out.Kbps, out.HEAAC = heAACKbps(channels), true
	}
	if t.BitrateKbps > 0 && t.BitrateKbps <= out.Kbps {
		return audioCopy
	}
	return out
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
