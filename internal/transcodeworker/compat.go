package transcodeworker

import "strings"

// MatroskaCompatibleSubtitleCodecs lists subtitle codecs natively supported
// by Matroska container in FFmpeg for stream copy without re-encoding.
var matroskaCompatibleSubtitles = map[string]bool{
	"subrip":            true,
	"srt":               true,
	"ass":               true,
	"ssa":               true,
	"hdmv_pgs_subtitle": true,
	"pgs":               true,
	"dvd_subtitle":      true,
	"vobsub":            true,
	"dvb_subtitle":      true,
	"dvb_teletext":      true,
	"webvtt":            true,
	"text":              true,
}

// IsSubtitleCompatible checks if the subtitle codec can be directly copied into the destination container.
func IsSubtitleCompatible(container, codec string) bool {
	normCont := strings.ToLower(strings.TrimSpace(container))
	normCodec := strings.ToLower(strings.TrimSpace(codec))

	switch normCont {
	case "mkv", "matroska":
		return matroskaCompatibleSubtitles[normCodec]
	default:
		return false
	}
}

// ConvertibleSubtitle checks if an incompatible subtitle codec has a known, safe conversion
// for the target container.
func ConvertibleSubtitle(container, codec string) (targetCodec string, reason string, ok bool) {
	normCont := strings.ToLower(strings.TrimSpace(container))
	normCodec := strings.ToLower(strings.TrimSpace(codec))

	switch normCont {
	case "mkv", "matroska":
		if normCodec == "mov_text" {
			return "subrip", "matroska_compatibility", true
		}
	}

	return "", "", false
}
