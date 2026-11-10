package transcode

import "strings"

// MatroskaDispositions are the stream flags a Matroska output stores and
// reads back. A track that is converted keeps at least these.
var MatroskaDispositions = []string{"default", "forced", "hearing_impaired", "visual_impaired", "comment", "descriptions", "original"}

// DispositionChange compares the flags an output track must carry (want) with
// the ones it has (got) and returns the first Matroska flag one has and the
// other does not, or "" when they match. A flag missing from a map is unset.
// Flags Matroska cannot store (an MP4 source's dub or captions) have nothing
// to compare in the output.
//
// inferred allows got a default flag want does not have: ffmpeg's own default
// (InferredDefault).
func DispositionChange(want, got map[string]int, inferred bool) string {
	for _, name := range MatroskaDispositions {
		w, g := want[name] > 0, got[name] > 0
		if w == g || (name == "default" && inferred && g && !w) {
			continue
		}
		return name
	}
	return ""
}

// copyRenames are the codecs a Matroska output names differently from its
// source although a copy kept the bitstream. ffmpeg writes a plain-text track
// ("text": a NUT source, or an S_TEXT/ASCII one) as S_TEXT/UTF8, which reads
// back as subrip; an SSA track comes back as ass.
var copyRenames = map[string]string{"text": "subrip", "ssa": "ass"}

// SameCopiedCodec reports whether an output track copied from a source track
// carries its codec, as the probe names each: exactly (an E-AC-3 track is
// not an AC-3 one), save the renames above. A codec the probe did not name,
// on either side, is no match: nothing was measured.
func SameCopiedCodec(source, output string) bool {
	s, o := copiedCodecName(source), copiedCodecName(output)
	return s != "" && s == o
}

func copiedCodecName(codec string) string {
	c := strings.ToLower(strings.TrimSpace(codec))
	if r, ok := copyRenames[c]; ok {
		return r
	}
	return c
}

// SameLanguage reports whether an output track keeps its source track's
// language tag. A source with none has nothing to keep. "und" is the absence
// of one, which the muxer writes as no tag. Any other tag is kept as it is:
// Matroska writes and reads back a code outside ISO 639-2 ("pob", "en",
// "pt-BR") unchanged, so the tags are compared exactly, whatever their case.
// The worker's check and the coordinator's both use it.
func SameLanguage(source, output string) bool {
	s, o := strings.ToLower(strings.TrimSpace(source)), strings.ToLower(strings.TrimSpace(output))
	return s == "" || (s == "und" && o == "") || s == o
}

// LostDisposition returns the first of the given flags want has and got does
// not, or "".
func LostDisposition(want, got map[string]int, flags []string) string {
	for _, name := range flags {
		if want[name] > 0 && got[name] <= 0 {
			return name
		}
	}
	return ""
}

// WrittenDisposition is the flags the worker's command sets (-disposition) on
// an output track whose planned flags are planned and whose source track's
// are source, or nil when it sets none: an audio track's planned flags
// whenever they are known ("none set" included), a subtitle's only when the
// plan moved the default to it. Every other subtitle carries over the flags
// of its source track. An empty map is flags not known, like nil: a JSON round
// trip turns one into the other. The worker's plan builds its command from
// it, and the worker's and the coordinator's checks read it back.
func WrittenDisposition(kind string, planned, source map[string]int) map[string]int {
	if len(planned) == 0 {
		return nil
	}
	switch kind {
	case "audio":
		return planned
	case "subtitle":
		if planned["default"] > 0 && source["default"] == 0 {
			return planned
		}
	}
	return nil
}

// InferredDefault reports whether ffmpeg may have marked the output's track at
// position i of a kind default on its own. ffmpeg does so only when the
// command sets no output track's flags at all (written is false), and then
// only on the first of two or more tracks of a kind none of which is default
// (planned holds the flags they must carry, in output order).
func InferredDefault(i int, planned []map[string]int, written bool) bool {
	if written || i != 0 || len(planned) < 2 {
		return false
	}
	for _, d := range planned {
		if d["default"] > 0 {
			return false
		}
	}
	return true
}
