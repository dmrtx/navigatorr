package transcode

import "strings"

// AudioTarget implements opt-in audio policies without a second optimizer.
// he_aac explicitly converts 1-6 channels to HE-AAC via AudioToolbox.
// Preserve track count, channels and layout. Efficient lossy codecs (E-AC-3
// JOC included, so Atmos survives on them), >7.1 tracks and reported layouts
// AAC cannot carry unchanged are copied. DTS/lossless tracks become AAC (lossy).
func AudioTarget(mode, codec string, channels int, layout string) (string, int) {
	codec = strings.ToLower(strings.TrimSpace(codec))
	if mode == "he_aac" {
		if channels < 1 || channels > 6 {
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
