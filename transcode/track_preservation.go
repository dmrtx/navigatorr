package transcode

// MatroskaDispositions are the stream flags a Matroska output stores and
// reads back. A track that is converted keeps at least these.
var MatroskaDispositions = []string{"default", "forced", "hearing_impaired", "visual_impaired", "comment", "descriptions", "original"}

// DispositionChange compares the flags an output track must carry (want) with
// the ones it has (got) and returns the first Matroska flag one has and the
// other does not, or "" when they match. A flag missing from a map is unset.
// Flags Matroska cannot store (an MP4 source's dub or captions) have nothing
// to compare in the output.
//
// autoDefault allows got a default flag want does not have: ffmpeg marks the
// first track of a kind default when none of the output's tracks of that kind
// is, unless the command sets their flags. The worker's command sets a
// subtitle's flags only when the plan moved the default to it, and an audio
// track's whenever the source's probe reported them (AutoDefaultAudio).
func DispositionChange(want, got map[string]int, autoDefault bool) string {
	for _, name := range MatroskaDispositions {
		w, g := want[name] > 0, got[name] > 0
		if w == g || (name == "default" && autoDefault && g && !w) {
			continue
		}
		return name
	}
	return ""
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

// AutoDefault reports whether ffmpeg may have marked the output's track at
// position i default on its own: it is the first of its kind, and none of the
// tracks of that kind the output must carry is default.
func AutoDefault(i int, planned []map[string]int) bool {
	if i != 0 {
		return false
	}
	for _, d := range planned {
		if d["default"] > 0 {
			return false
		}
	}
	return true
}

// AutoDefaultAudio is AutoDefault for audio: the worker's command writes every
// kept audio track's flags (none set when the track has none), which stops
// ffmpeg's inference, unless the probe reported no flags for the track (want
// is nil) and the command wrote none.
func AutoDefaultAudio(i int, want map[string]int, planned []map[string]int) bool {
	return want == nil && AutoDefault(i, planned)
}
