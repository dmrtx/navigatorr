package transcode

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Track selection by language for the size and compact goals.
//
// A goal freezes a TrackPolicy: the languages to keep and, for manual picking,
// the exact audio and subtitle streams. Each source resolves that policy to a
// TrackSelection (SelectTracks), the source stream indexes kept, which the plan
// carries. The selection is a pure function of the policy and the source's
// tracks, so the coordinator re-derives it wherever it checks a plan, and the
// worker, which only follows the indexes, validates that the output has exactly
// those tracks.

// TrackPolicy is the frozen choice of which audio and subtitle tracks a goal
// keeps. Languages are canonical (see LanguageList); the explicit stream lists
// are source stream indexes. A list that is present replaces the language rule
// for its kind: an empty subtitle list drops every subtitle, an empty audio
// list is invalid (a file keeps its audio).
type TrackPolicy struct {
	Languages       []string `json:"languages,omitempty"`
	AudioStreams    *[]int   `json:"audio_streams,omitempty"`
	SubtitleStreams *[]int   `json:"subtitle_streams,omitempty"`
}

// Active reports whether the policy can remove any track.
func (p *TrackPolicy) Active() bool {
	return p != nil && (len(p.Languages) > 0 || p.AudioStreams != nil || p.SubtitleStreams != nil)
}

// Validate checks a frozen policy: canonical languages, well-formed indexes.
func (p *TrackPolicy) Validate() error {
	if p == nil {
		return nil
	}
	if !p.Active() {
		return fmt.Errorf("a track policy must keep languages or name streams")
	}
	canonical, err := LanguageList(p.Languages)
	if err != nil {
		return err
	}
	if !slices.Equal(canonical, p.Languages) {
		return fmt.Errorf("track policy languages must be canonical, sorted and unique")
	}
	if p.AudioStreams != nil && len(*p.AudioStreams) == 0 {
		return fmt.Errorf("keep_audio_streams must name at least one audio stream")
	}
	for _, list := range []*[]int{p.AudioStreams, p.SubtitleStreams} {
		if list != nil && !sortedUniqueNonNegative(*list) {
			return fmt.Errorf("stream indexes must be unique, ascending and not negative")
		}
	}
	return nil
}

// Clone returns an independent copy.
func (p *TrackPolicy) Clone() *TrackPolicy {
	if p == nil {
		return nil
	}
	c := &TrackPolicy{Languages: slices.Clone(p.Languages)}
	if p.AudioStreams != nil {
		v := slices.Clone(*p.AudioStreams)
		c.AudioStreams = &v
	}
	if p.SubtitleStreams != nil {
		v := slices.Clone(*p.SubtitleStreams)
		c.SubtitleStreams = &v
	}
	return c
}

// TrackInfo is a source audio or subtitle stream as selection reads it. Index
// is the stream's index in the file; Language is the tag as probed.
type TrackInfo struct {
	Index       int
	Kind        string // "audio" or "subtitle"
	Language    string
	Title       string
	Codec       string
	Disposition map[string]int
}

// TrackSelection is a source's resolved selection: the indexes of the audio and
// subtitle streams the output keeps, ascending. Both lists are complete (a
// stream not listed is dropped), so a plan with a selection always removes at
// least one track; a source that loses none carries no selection. Video,
// attachments and chapters are never part of it: they are always kept.
type TrackSelection struct {
	KeepAudio     []int `json:"keep_audio"`
	KeepSubtitles []int `json:"keep_subtitles"`
}

// Validate checks the shape of a selection from a plan.
func (s *TrackSelection) Validate() error {
	if s == nil {
		return nil
	}
	if !sortedUniqueNonNegative(s.KeepAudio) || !sortedUniqueNonNegative(s.KeepSubtitles) {
		return fmt.Errorf("track selection indexes must be unique, ascending and not negative")
	}
	for _, a := range s.KeepAudio {
		if slices.Contains(s.KeepSubtitles, a) {
			return fmt.Errorf("track selection keeps stream %d as both audio and subtitle", a)
		}
	}
	return nil
}

// Equal reports whether two selections keep the same streams. Two nil
// selections are equal.
func (s *TrackSelection) Equal(o *TrackSelection) bool {
	if s == nil || o == nil {
		return s == o
	}
	return slices.Equal(s.KeepAudio, o.KeepAudio) && slices.Equal(s.KeepSubtitles, o.KeepSubtitles)
}

// Keeps reports whether the stream at index is kept. Without a selection every
// track is kept.
func (s *TrackSelection) Keeps(kind string, index int) bool {
	if s == nil {
		return true
	}
	switch kind {
	case "audio":
		return slices.Contains(s.KeepAudio, index)
	case "subtitle":
		return slices.Contains(s.KeepSubtitles, index)
	}
	return true
}

func sortedUniqueNonNegative(v []int) bool {
	for i, n := range v {
		if n < 0 || i > 0 && n <= v[i-1] {
			return false
		}
	}
	return true
}

// OriginalAudioTrack is the track whose language the source was made in: the
// one marked with the "original" disposition, else the first audio track.
// The second result is false when the source has no audio.
func OriginalAudioTrack(tracks []TrackInfo) (TrackInfo, bool) {
	var first *TrackInfo
	for i := range tracks {
		t := &tracks[i]
		if t.Kind != "audio" {
			continue
		}
		if t.Disposition["original"] > 0 {
			return *t, true
		}
		if first == nil {
			first = t
		}
	}
	if first == nil {
		return TrackInfo{}, false
	}
	return *first, true
}

// SelectTracks resolves a policy against a source's audio and subtitle tracks
// (in source order). It returns nil when nothing would be removed. The rules,
// for the language list L of the policy:
//
//   - Audio: the original-language track is always kept, and so is any track
//     tagged in L, tagged in the original track's language (a commentary in
//     the original language stays with it) or untagged / "und". When no audio
//     track is tagged in L the file keeps all its audio; that test looks at L
//     only, never at the original language.
//   - Subtitles: a subtitle is kept when its language is in L, is the
//     original audio track's language, or is untagged / "und". Forced
//     subtitles follow the same rule.
//   - Explicit stream lists replace the rule for their kind, exactly.
//
// Languages are compared in canonical form, so ger matches deu. An explicit
// index that is not a track of that kind in this source is an error.
func SelectTracks(p *TrackPolicy, tracks []TrackInfo) (*TrackSelection, error) {
	if !p.Active() {
		return nil, nil
	}
	var audio, subs []TrackInfo
	for _, t := range tracks {
		switch t.Kind {
		case "audio":
			audio = append(audio, t)
		case "subtitle":
			subs = append(subs, t)
		}
	}
	wanted := map[string]bool{}
	for _, l := range p.Languages {
		wanted[CanonicalLanguage(l)] = true
	}
	originalLanguage := LanguageUndetermined
	if o, ok := OriginalAudioTrack(tracks); ok {
		originalLanguage = CanonicalLanguage(o.Language)
	}

	var keepAudio []int
	switch {
	case p.AudioStreams != nil:
		var err error
		if keepAudio, err = explicitStreams("audio", *p.AudioStreams, audio); err != nil {
			return nil, err
		}
	case len(wanted) == 0:
		keepAudio = indexesOf(audio)
	default:
		matched := false
		for _, t := range audio {
			matched = matched || wanted[CanonicalLanguage(t.Language)]
		}
		original, _ := OriginalAudioTrack(tracks)
		for _, t := range audio {
			lang := CanonicalLanguage(t.Language)
			if !matched || t.Index == original.Index || lang == originalLanguage || lang == LanguageUndetermined || wanted[lang] {
				keepAudio = append(keepAudio, t.Index)
			}
		}
	}

	var keepSubs []int
	switch {
	case p.SubtitleStreams != nil:
		var err error
		if keepSubs, err = explicitStreams("subtitle", *p.SubtitleStreams, subs); err != nil {
			return nil, err
		}
	case len(wanted) == 0:
		keepSubs = indexesOf(subs)
	default:
		for _, t := range subs {
			lang := CanonicalLanguage(t.Language)
			if lang == LanguageUndetermined || lang == originalLanguage || wanted[lang] {
				keepSubs = append(keepSubs, t.Index)
			}
		}
	}

	if len(keepAudio) == len(audio) && len(keepSubs) == len(subs) {
		return nil, nil
	}
	return &TrackSelection{KeepAudio: nonNil(keepAudio), KeepSubtitles: nonNil(keepSubs)}, nil
}

func nonNil(v []int) []int {
	if v == nil {
		return []int{}
	}
	return v
}

func indexesOf(tracks []TrackInfo) []int {
	out := make([]int, 0, len(tracks))
	for _, t := range tracks {
		out = append(out, t.Index)
	}
	return out
}

// explicitStreams checks a manual pick against the source's tracks of that
// kind. Every index must be one of them, so a pick made for another file (or
// a wrong kind) fails instead of silently keeping something else.
func explicitStreams(kind string, picked []int, tracks []TrackInfo) ([]int, error) {
	have := map[int]bool{}
	for _, t := range tracks {
		have[t.Index] = true
	}
	var out []int
	for _, i := range picked {
		if !have[i] {
			return nil, fmt.Errorf("keep_%s_streams names stream %d, which is not an %s stream of this file", kind, i, kind)
		}
		out = append(out, i)
	}
	sort.Ints(out)
	return slices.Compact(out), nil
}

// Partition splits the source's audio and subtitle tracks (in source order) into
// the tracks the selection keeps and drops. A kept track carries the
// disposition the output must have: when the default audio track is dropped,
// the first kept audio track becomes the default; the same holds for
// subtitles, but only when the source had a default subtitle. Without a
// selection every track is kept unchanged.
func (s *TrackSelection) Partition(tracks []TrackInfo) (keptAudio, keptSubtitles, droppedAudio, droppedSubtitles []TrackInfo) {
	for _, t := range tracks {
		switch t.Kind {
		case "audio":
			if s.Keeps("audio", t.Index) {
				keptAudio = append(keptAudio, t)
			} else {
				droppedAudio = append(droppedAudio, t)
			}
		case "subtitle":
			if s.Keeps("subtitle", t.Index) {
				keptSubtitles = append(keptSubtitles, t)
			} else {
				droppedSubtitles = append(droppedSubtitles, t)
			}
		}
	}
	if s != nil {
		reassignDefault(keptAudio, hasDefault(keptAudio) || hasDefault(droppedAudio), false)
		// A default subtitle that was forced is a track for foreign-language
		// parts only; a full subtitle never takes its place.
		reassignDefault(keptSubtitles, hasDefault(keptSubtitles) || hasDefault(droppedSubtitles), droppedDefaultForced(keptSubtitles, droppedSubtitles))
	}
	return keptAudio, keptSubtitles, droppedAudio, droppedSubtitles
}

func hasDefault(tracks []TrackInfo) bool {
	for _, t := range tracks {
		if t.Disposition["default"] > 0 {
			return true
		}
	}
	return false
}

// droppedDefaultForced reports whether the default subtitle the selection
// removed was a forced one (and no kept track is a default).
func droppedDefaultForced(kept, dropped []TrackInfo) bool {
	if hasDefault(kept) {
		return false
	}
	for _, t := range dropped {
		if t.Disposition["default"] > 0 && t.Disposition["forced"] > 0 {
			return true
		}
	}
	return false
}

// reassignDefault marks a kept track default when the source had a default of
// this kind and none of the kept tracks is: the first kept track, or, when
// forcedOnly, the first kept forced track (none at all when there is no such
// track, so the output has no default of this kind). The disposition map is
// copied, never changed in place.
func reassignDefault(kept []TrackInfo, sourceHadDefault, forcedOnly bool) {
	if !sourceHadDefault || len(kept) == 0 || hasDefault(kept) {
		return
	}
	target := -1
	for i := range kept {
		if !forcedOnly || kept[i].Disposition["forced"] > 0 {
			target = i
			break
		}
	}
	if target < 0 {
		return
	}
	d := make(map[string]int, len(kept[target].Disposition)+1)
	for k, v := range kept[target].Disposition {
		d[k] = v
	}
	d["default"] = 1
	kept[target].Disposition = d
}

// LanguageLabel is a track's language as shown to people: the tag, or "und".
func (t TrackInfo) LanguageLabel() string {
	l := strings.ToLower(strings.TrimSpace(t.Language))
	if l == "" {
		return LanguageUndetermined
	}
	return l
}
