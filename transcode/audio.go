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
// byte budget freezes both and picks one per source (see AudioLevelModes); one
// without a budget plans level 1. Level 0, AudioModeCompactLossless, is what
// size operations froze before the levels existed and is never chosen for a
// new one.
//
//	level 1  AudioModeSizeAAC    a track at least 1.25 times its AAC-LC target
//	                             (stereo 192, mono 128, 5.1 384, 7.1 512 kbps)
//	                             becomes AAC-LC at it
//	level 2  AudioModeSizeHEAAC  a stereo, quad or 5.1 track (heAACLayouts) at
//	                             least 1.25 times its HE-AAC target becomes
//	                             HE-AAC (stereo 64, quad and 5.1 160 kbps); any
//	                             other track as level 1
//
// At every level any other track is copied: one below 1.25 times its target
// (lossless included, when it states its bitrate), E-AC-3 JOC (Atmos), layouts
// AAC cannot keep, more than 8 channels, and a lossy track whose source bitrate
// is unknown. A lossless track of unknown bitrate is converted. Track count,
// order, languages, titles, dispositions and channel layouts never change.
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
// AAC at Kbps (HE-AAC when HEAAC, else AAC-LC). AudioToolbox says the track is
// encoded with aac_at rather than FFmpeg's native encoder: always for HE-AAC,
// and for AAC-LC only in the size goal's levels.
type AudioOutput struct {
	Codec        string
	Kbps         int
	HEAAC        bool
	AudioToolbox bool
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

// sizeLevelMinGain is how much larger than the level's target a track must be
// to be re-encoded, as a ratio (5/4): a second lossy generation is not worth a
// saving smaller than a fifth of the track (AC-3 5.1 at 448 kbps against the
// 384 kbps target stays as it is).
const sizeLevelMinGainNum, sizeLevelMinGainDen = 5, 4

// sizeLevelOutput is one track under the size goal's audio levels. A track is
// converted only when its bitrate is known and at least 1.25 times the target
// of the level for its layout, so a conversion never raises a bitrate and
// always saves a meaningful amount; any other track with a known bitrate,
// lossless included (a quiet or mono FLAC can be smaller than the target), is
// copied. A lossy track of unknown bitrate is copied, never guessed. A
// lossless track of unknown bitrate is converted: PCM and FLAC in a remux
// often state none and are nearly always far above any target.
//
// AAC-LC is encoded with AudioToolbox (aac_at) where it keeps the track's
// layout, else with FFmpeg's native encoder. HE-AAC (level 2) is aac_at too and
// only for the few layouts it encodes correctly (heAACLayouts); any other track
// keeps its level 1 output at level 2, with the same minimum gain against the
// AAC-LC target.
//
// This function, its targets and the layout tables below are the contract of
// plan version transcode-modes-v5 (see TestSizeAudioV5Contract): a change to
// what it returns needs a new policy and plan version.
func sizeLevelOutput(mode string, t AudioTrack) AudioOutput {
	channels := t.Channels
	if channels < 1 || channels > 8 || !aacKeepsLayout(t.Layout, channels) {
		return audioCopy
	}
	lossless := losslessAudio(strings.ToLower(strings.TrimSpace(t.Codec)), t.Profile)
	if !lossless && (t.BitrateKbps <= 0 || ObjectAudio(t)) {
		return audioCopy
	}
	toolbox := audioToolboxKeepsLayout(t.Layout, channels)
	out := AudioOutput{Codec: "aac", Kbps: aacLCKbps(channels), AudioToolbox: toolbox}
	if mode == AudioModeSizeHEAAC && heAACKeepsLayout(t.Layout, channels) {
		out.Kbps, out.HEAAC = heAACKbps(channels), true
	}
	if t.BitrateKbps > 0 && t.BitrateKbps*sizeLevelMinGainDen < out.Kbps*sizeLevelMinGainNum {
		return audioCopy
	}
	return out
}

// audioToolboxLayouts are the layouts AudioToolbox's AAC encoder (aac_at)
// writes back unchanged as AAC-LC, keyed to their channel count (checked with a
// real encode: 2.1, 3.0(back), 3.1, 4.1, hexagonal, 7.1, 7.1(wide) and
// 5.1.2(back) come out with a different layout or channel count, 7.1 without
// its LFE).
var audioToolboxLayouts = map[string]int{
	"mono": 1, "stereo": 2, "3.0": 3, "4.0": 4, "quad": 4, "quad(side)": 4, "5.0": 5, "5.0(side)": 5,
	"5.1": 6, "5.1(side)": 6, "6.1(back)": 7, "7.0": 7, "octagonal": 8,
}

// audioToolboxKeepsLayout reports a track aac_at encodes without changing its
// layout. An unspecified layout is the channel count's default one.
func audioToolboxKeepsLayout(layout string, channels int) bool {
	layout = strings.ToLower(strings.TrimSpace(layout))
	if layout == "" {
		switch channels {
		case 1, 2, 4, 5, 6, 7:
			return true
		}
		return false
	}
	n, ok := audioToolboxLayouts[layout]
	return ok && n == channels
}

// heAACLayouts are the layouts aac_at encodes as HE-AAC at the level 2 bitrates
// and writes back unchanged, keyed to their channel count. Checked with a real
// encode: mono comes out as stereo (the bitrate is clamped), and 3.0, 4.0, 5.0,
// 5.0(side) and every layout above 6 channels do not open the encoder.
var heAACLayouts = map[string]int{
	"stereo": 2, "quad": 4, "quad(side)": 4, "5.1": 6, "5.1(side)": 6,
}

// heAACKeepsLayout reports a track aac_at encodes as HE-AAC without changing
// its layout. An unspecified layout is the channel count's default one, which
// works for 2 and 6 channels.
func heAACKeepsLayout(layout string, channels int) bool {
	layout = strings.ToLower(strings.TrimSpace(layout))
	if layout == "" {
		return channels == 2 || channels == 6
	}
	n, ok := heAACLayouts[layout]
	return ok && n == channels
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
