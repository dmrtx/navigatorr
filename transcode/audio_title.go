package transcode

import (
	"regexp"
	"strings"
)

// sourceFormatWords mark a track title that describes the source format,
// as whole words ("Atmosphere" names no format).
var sourceFormatWords = regexp.MustCompile(`(?i)\b(truehd|atmos|dts|flac|l?pcm|alac|mlp|lossless|master\s+audio|hd[\s-]?ma|dolby|uncompressed|hra)\b`)

// sourceFormatPhrases and sourceFormatTokens are the format, sample and
// channel words removed from a converted track's title; whatever else the
// title says ("Director's Commentary") is kept.
var (
	sourceFormatPhrases = regexp.MustCompile(`(?i)\bdts[-:\s]?(hd|x|es)\b([\s-]+(master\s+audio|ma|high\s+resolution(\s+audio)?|hra)\b)?|\bmaster\s+audio\b|\bhd[\s-]?ma\b|\bhigh\s+resolution(\s+audio)?\b|\bdolby\s+digital(\s+plus)?\b`)
	sourceFormatTokens  = regexp.MustCompile(`(?i)^(truehd|atmos|dts|flac|l?pcm|alac|mlp|lossless|dolby|uncompressed|hra|stereo|mono|surround|\d+ch|\d(\.\d){1,2}(ch)?|\d+-?bits?|\d+(\.\d+)?k?hz|\d+-?bits?/\d+(\.\d+)?(k?hz)?|\d+/\d+(\.\d+)?(k?hz)?)$`)
	// sourceBitrate is a bitrate claim ("@ 4000 kbps", "4.5Mbps"), which
	// describes the source track, not the AAC one.
	sourceBitrate = regexp.MustCompile(`(?i)@?\s*\b\d+(\.\d+)?\s*[km]?(bps|b/s|bit/s)\b`)
	// titleJoiners split compound words ("TrueHD/Atmos", "TrueHD+Atmos",
	// "TrueHD.Atmos.7.1", "English_TrueHD") into their parts.
	titleJoiners = regexp.MustCompile(`[/+._]+`)
	bareNumber   = regexp.MustCompile(`^\d+$`)
)

const (
	titleTrim     = "()[]{},;:|/+-·@"
	titleEdgeTrim = " ,;:|/+-·@"
)

// balanceBrackets drops closing brackets with no opener and closes brackets
// left open, so dropping format words never cuts "Español (Latino)" or
// leaves "Commentary (Director" behind.
func balanceBrackets(s string) string {
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}
	closers := map[rune]rune{'(': ')', '[': ']', '{': '}'}
	var out []rune
	var open []rune
	for _, r := range s {
		if _, ok := closers[r]; ok {
			open = append(open, r)
		} else if opener, ok := pairs[r]; ok {
			if len(open) == 0 || open[len(open)-1] != opener {
				continue
			}
			open = open[:len(open)-1]
		}
		out = append(out, r)
	}
	res := strings.TrimRight(string(out), titleEdgeTrim)
	for i := len(open) - 1; i >= 0; i-- {
		res += string(closers[open[i]])
	}
	res = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(res, "()", ""), "[]", ""), "{}", "")
	return strings.Trim(strings.Join(strings.Fields(res), " "), titleEdgeTrim)
}

// ConvertedAudioTitle is the title a track converted to AAC carries in the
// output (RetitleConvertedAudio), given the source track's title, channel
// layout and channel count: the source title when it names no source format.
// The worker writes it, and the worker and the coordinator both check it.
func ConvertedAudioTitle(title, layout string, channels int) string {
	if t, ok := RetitleConvertedAudio(title, layout, channels); ok {
		return t
	}
	return title
}

// RetitleConvertedAudio retitles a track converted to AAC when its title names
// the format it no longer has: the format, channel and bitrate words are
// replaced by the AAC layout ("TrueHD Atmos 7.1" becomes "AAC 7.1", "Director's
// Commentary DTS-HD MA 5.1" becomes "Director's Commentary AAC 5.1"), also
// inside compound words ("English TrueHD+Atmos" becomes "English AAC 7.1").
// Titles that name no source format ("Commentary") are kept; ok is false
// then. Without a reported layout (PCM in Matroska) the channel count names
// it. A label that would still name a source format is dropped (fail safe).
func RetitleConvertedAudio(title, layout string, channels int) (string, bool) {
	// "_" is a word character, so "English_TrueHD" is checked split.
	namesFormat := func(s string) bool {
		s = titleJoiners.ReplaceAllString(s, " ")
		return sourceFormatWords.MatchString(s) || ObjectAudioText(s)
	}
	if !namesFormat(title) {
		return "", false
	}
	l := strings.ToLower(strings.TrimSpace(layout))
	if i := strings.IndexByte(l, '('); i > 0 {
		l = l[:i]
	}
	if l == "" {
		l = map[int]string{1: "mono", 2: "stereo", 6: "5.1", 8: "7.1"}[channels]
	}
	format := "AAC " + l
	switch l {
	case "mono":
		format = "AAC Mono"
	case "stereo":
		format = "AAC Stereo"
	case "":
		format = "AAC"
	}
	cleaned := sourceFormatPhrases.ReplaceAllString(sourceBitrate.ReplaceAllString(title, " "), " ")
	var label []string
	for _, word := range strings.Fields(cleaned) {
		core := strings.Trim(word, titleTrim)
		if core == "" || sourceFormatTokens.MatchString(core) {
			continue
		}
		// A compound word loses its format and channel parts, and the bare
		// numbers next to them ("TrueHD.Atmos.7.1"); one with no format part
		// ("Track_01", "v1.5") is kept whole, like a plain word.
		parts := titleJoiners.Split(core, -1)
		var kept []string
		for _, part := range parts {
			if part = strings.Trim(part, titleTrim); part != "" && !sourceFormatTokens.MatchString(part) {
				kept = append(kept, part)
			}
		}
		if len(kept) == len(parts) {
			label = append(label, word)
			continue
		}
		for _, part := range kept {
			if !bareNumber.MatchString(part) {
				label = append(label, part)
			}
		}
	}
	rest := balanceBrackets(strings.Trim(strings.Join(label, " "), titleEdgeTrim))
	if rest == "" || namesFormat(rest) {
		return format, true
	}
	return rest + " " + format, true
}
