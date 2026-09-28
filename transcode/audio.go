package transcode

import "strings"

// AudioTarget implements the opt-in compact policy without a second optimizer.
// Preserve track count and channels. Efficient lossy codecs, unknown layouts
// and >7.1 tracks are copied. DTS/lossless tracks become AAC (lossy).
func AudioTarget(mode, codec string, channels int) (string, int) {
	codec = strings.ToLower(strings.TrimSpace(codec))
	if mode != "compact" || channels < 1 || channels > 8 {
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
