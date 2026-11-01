package action

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// Track selection by language for the size and compact goals.
//
// The policy is frozen with the goal (ModePolicySnapshot.Tracks): the languages
// to keep, from the job's keep_languages or else the global setting, and, for
// manual picking, the exact audio and subtitle streams. It is resolved once per
// source, at preflight, to the source stream indexes kept (transcode.SelectTracks
// is a pure function of the policy and the source's tracks), which the plan
// carries as Plan.Tracks and which turns the plan into policy v6. Every place
// that validates a plan derives the selection again and compares, and the
// worker follows the indexes and checks that the output has exactly those
// tracks.
//
// A snapshot frozen before this existed has no Tracks and keeps every track.

// errTrackSelectionWorker is why a plan that keeps only some tracks is not
// sent to a worker that predates the selection.
const errTrackSelectionWorker = "worker does not support keeping only some audio and subtitle tracks; upgrade the worker"

// errAudioObjectBedWorker is why a plan at one of the size goal's bed audio
// levels is not sent to a worker that predates them.
const errAudioObjectBedWorker = "worker does not support converting Atmos audio to its channel bed; upgrade the worker"

const (
	inputKeepLanguages       = "keep_languages"
	inputKeepAudioStreams    = "keep_audio_streams"
	inputKeepSubtitleStreams = "keep_subtitle_streams"
	trackSelectionKey        = "track_selection"
	trackReportKey           = "track_report"
	// trackLanguagesKey holds the languages a goal froze (state of the job or
	// batch that created it), kept even when the list is empty.
	trackLanguagesKey = "track_languages"
)

// KeepLanguages is the global list of languages the size and compact goals keep
// audio and subtitle tracks for: the operator's saved list, else the default.
// An empty saved list is a choice (keep every track) and is returned as such.
func (e *Engine) KeepLanguages() []string {
	if e != nil && e.deps.Store != nil {
		if row, err := e.deps.Store.GetKeepLanguages(); err == nil && row != nil {
			if list, err := transcode.LanguageList(row.Languages); err == nil {
				return list
			}
		}
	}
	list, _ := transcode.LanguageList(transcode.DefaultKeepLanguages())
	return list
}

// trackInput is the track-selection part of a goal's inputs, checked but not
// yet joined with the global list.
type trackInput struct {
	languages *[]string
	audio     *[]int
	subtitles *[]int
}

func (t trackInput) given() bool { return t.languages != nil || t.audio != nil || t.subtitles != nil }

// modeTrackInput reads keep_languages, keep_audio_streams and
// keep_subtitle_streams. All three belong to the size and compact goals; the
// stream lists name source streams of one file, so they are for a single
// transcode_media job only.
func modeTrackInput(mode string, inputs map[string]any) (trackInput, error) {
	var in trackInput
	present := ""
	for _, key := range []string{inputKeepLanguages, inputKeepAudioStreams, inputKeepSubtitleStreams} {
		if raw, has := inputs[key]; has && raw != nil {
			present = firstNonEmpty(present, key)
		}
	}
	if present == "" {
		return in, nil
	}
	if mode != "size" && mode != "compact" {
		return in, inputErr(present, "%s is only supported with mode=size or mode=compact", present)
	}
	if raw, has := inputs[inputKeepLanguages]; has && raw != nil {
		list, ok := stringList(raw)
		if !ok {
			return in, inputErr(inputKeepLanguages, "keep_languages must be a list of ISO 639-2 language codes such as [\"eng\", \"spa\", \"jpn\"]")
		}
		canonical, err := transcode.LanguageList(list)
		if err != nil {
			return in, inputErr(inputKeepLanguages, "%v", err)
		}
		in.languages = &canonical
	}
	for _, key := range []string{inputKeepAudioStreams, inputKeepSubtitleStreams} {
		raw, has := inputs[key]
		if !has || raw == nil {
			continue
		}
		if getString(inputs, "parent_action_id") != "" {
			return in, inputErr(key, "%s names streams of one file and cannot be used on a batch", key)
		}
		list, ok := intList(raw)
		if !ok {
			return in, inputErr(key, "%s must be a list of source stream indexes", key)
		}
		sort.Ints(list)
		list = slices.Compact(list)
		if key == inputKeepAudioStreams {
			if len(list) == 0 {
				return in, inputErr(key, "keep_audio_streams must name at least one audio stream; a file keeps its audio")
			}
			in.audio = &list
		} else {
			in.subtitles = &list
		}
	}
	return in, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// stringList reads a JSON list of strings (or a Go one).
func stringList(raw any) ([]string, bool) {
	switch v := raw.(type) {
	case []string:
		return slices.Clone(v), true
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// intList reads a JSON list of whole, non-negative numbers (or a Go one).
func intList(raw any) ([]int, bool) {
	var items []any
	switch v := raw.(type) {
	case []int:
		items = make([]any, len(v))
		for i, n := range v {
			items[i] = float64(n)
		}
	case []float64:
		items = make([]any, len(v))
		for i, n := range v {
			items[i] = n
		}
	case []any:
		items = v
	default:
		return nil, false
	}
	out := make([]int, 0, len(items))
	for _, item := range items {
		n, ok := item.(float64)
		if !ok || n != float64(int(n)) || n < 0 || n > 1<<20 {
			return nil, false
		}
		out = append(out, int(n))
	}
	return out, true
}

// trackPolicyFor is the policy a goal freezes: the job's own languages when it
// gives them, else the global list, plus any manual picks. It is nil for goals
// without track selection and when nothing could be removed (an empty list
// keeps every track).
func trackPolicyFor(mode string, in trackInput, global []string) (*transcode.TrackPolicy, error) {
	if mode != "size" && mode != "compact" {
		return nil, nil
	}
	languages := global
	if in.languages != nil {
		languages = *in.languages
	}
	canonical, err := transcode.LanguageList(languages)
	if err != nil {
		return nil, err
	}
	p := &transcode.TrackPolicy{Languages: canonical, AudioStreams: in.audio, SubtitleStreams: in.subtitles}
	if !p.Active() {
		return nil, nil
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// trackPolicyOf is the policy a new goal freezes for these inputs.
func trackPolicyOf(ec *ExecutionContext, mode string) (*transcode.TrackPolicy, error) {
	in, err := modeTrackInput(mode, ec.Inputs)
	if err != nil {
		return nil, err
	}
	var engine *Engine
	if ec != nil {
		engine = ec.Engine
	}
	policy, err := trackPolicyFor(mode, in, engine.KeepLanguages())
	if err == nil && ec != nil && ec.State != nil && (mode == "size" || mode == "compact") {
		// Remember the languages this goal froze, an empty list included (it
		// leaves no policy in the snapshot), so that a later goal change of a
		// batch keeps them instead of reading the global list again.
		languages := engine.KeepLanguages()
		if in.languages != nil {
			languages = *in.languages
		}
		ec.State[trackLanguagesKey] = append([]string{}, languages...)
	}
	return policy, err
}

// reconfiguredTrackPolicy is the policy a batch freezes when its goal changes.
// The batch keeps its own choice of languages: its keep_languages input if it
// has one ([] included), else the languages it froze when it was created,
// else (a batch frozen before this setting existed) the global list as it is
// now.
func (e *Engine) reconfiguredTrackPolicy(mode string, ec *ExecutionContext) (*transcode.TrackPolicy, error) {
	if mode != "size" && mode != "compact" {
		return nil, nil
	}
	in, err := modeTrackInput(mode, ec.Inputs)
	if err != nil {
		return nil, err
	}
	if in.languages == nil {
		if frozen, ok := stringList(ec.State[trackLanguagesKey]); ok && ec.State[trackLanguagesKey] != nil {
			in.languages = &frozen
		} else if snapshot := stateModePolicy(ec.State); snapshot != nil && snapshot.Tracks != nil {
			languages := slices.Clone(snapshot.Tracks.Languages)
			in.languages = &languages
		}
	}
	return trackPolicyFor(mode, in, e.KeepLanguages())
}

// stateModePolicy is the goal snapshot stored in an action's state, if any.
func stateModePolicy(state map[string]any) *ModePolicySnapshot {
	raw := state["mode_policy"]
	if raw == nil {
		return nil
	}
	if p, ok := raw.(*ModePolicySnapshot); ok {
		return p
	}
	if v, ok := raw.(ModePolicySnapshot); ok {
		return &v
	}
	var p ModePolicySnapshot
	if !reencode(raw, &p) || p.Mode == "" {
		return nil
	}
	return &p
}

// TrackInfosOf lists a source's audio and subtitle streams for selection, in
// source order (audio first, then subtitles, each by stream index). The job and
// the console's track picker both read a file through it, so the boxes the
// picker ticks are what the job keeps.
func TrackInfosOf(rep *mediainspect.DetailedReport) []transcode.TrackInfo {
	if rep == nil {
		return nil
	}
	return trackInfosOfStreams(rep.Audio, rep.Subtitles)
}

func trackInfosOfStreams(audio, subtitles []mediainspect.DetailedStream) []transcode.TrackInfo {
	out := make([]transcode.TrackInfo, 0, len(audio)+len(subtitles))
	add := func(kind string, streams []mediainspect.DetailedStream) {
		for _, s := range streams {
			out = append(out, transcode.TrackInfo{Index: s.Index, Kind: kind, Language: s.Language, Title: s.Title, Codec: s.Codec, Disposition: s.Disposition})
		}
	}
	add("audio", audio)
	add("subtitle", subtitles)
	return out
}

// resolveTrackSelection resolves the frozen policy against a probed source. It
// is nil when the goal has no policy or nothing would be removed.
func resolveTrackSelection(snapshot *ModePolicySnapshot, rep *mediainspect.DetailedReport) (*transcode.TrackSelection, error) {
	if snapshot == nil || !snapshot.Tracks.Active() || rep == nil {
		return nil, nil
	}
	return transcode.SelectTracks(snapshot.Tracks, TrackInfosOf(rep))
}

// trackSelectionOf is the selection preflight stored for this source.
func trackSelectionOf(state map[string]any) *transcode.TrackSelection {
	raw := state[trackSelectionKey]
	if raw == nil {
		return nil
	}
	if sel, ok := raw.(*transcode.TrackSelection); ok {
		return sel
	}
	var sel transcode.TrackSelection
	if !reencode(raw, &sel) || sel.Validate() != nil {
		return nil
	}
	return &sel
}

// applyTrackSelection puts the selection on a plan: the kept indexes, policy
// v6 (which an older worker refuses; v7, v8 and v9 carry a selection as they
// are), and subtitle actions for the kept subtitles only. A nil selection
// leaves the plan as it is.
func applyTrackSelection(plan *transcode.Plan, sel *transcode.TrackSelection) error {
	if sel == nil {
		return nil
	}
	plan.Tracks = &transcode.TrackSelection{KeepAudio: slices.Clone(sel.KeepAudio), KeepSubtitles: slices.Clone(sel.KeepSubtitles)}
	if !transcode.SelectsTracksItself(plan.ModePolicyVersion) {
		plan.ModePolicyVersion = transcode.ModePolicyV6
	}
	plan.SubtitleActions = keptSubtitleActions(plan.SubtitleActions, sel)
	plan.PlanDigest = ""
	digest, err := transcode.DigestPlan(plan)
	plan.PlanDigest = digest
	return err
}

// keptSubtitleActions are the actions of the subtitles the selection keeps. An
// action keeps its TypeIndex, the subtitle's position among all of the
// source's subtitles, so it still names the source subtitle after the others
// are removed.
func keptSubtitleActions(actions []transcode.SubtitleAction, sel *transcode.TrackSelection) []transcode.SubtitleAction {
	out := make([]transcode.SubtitleAction, 0, len(actions))
	for _, a := range actions {
		if sel.Keeps("subtitle", a.SourceStreamIndex) {
			out = append(out, a)
		}
	}
	return out
}

// trackSelectionMatches reports whether a plan carries exactly the selection
// the frozen policy gives the source: the same kept streams, policy v6 if and
// only if streams are removed, and the subtitle actions of the kept subtitles.
// A plan, a stored selection or a snapshot that disagree with each other fail.
func trackSelectionMatches(snapshot *ModePolicySnapshot, state map[string]any, plan *transcode.Plan) bool {
	if plan == nil || snapshot == nil {
		return false
	}
	if snapshot.Tracks != nil && (snapshot.Mode != "size" && snapshot.Mode != "compact" || snapshot.Tracks.Validate() != nil) {
		return false
	}
	want, err := resolveTrackSelection(snapshot, getSourceReport(state["source_report"]))
	if err != nil || !plan.Tracks.Equal(want) || !trackSelectionOf(state).Equal(want) {
		return false
	}
	// Plans of v7, v8 and v9 keep their version with or without a selection;
	// every other plan is v6 exactly when streams are removed.
	version := snapshot.workerVersion()
	if own := transcode.SelectsTracksItself(version); own && plan.ModePolicyVersion != version || !own && (transcode.SelectsTracksItself(plan.ModePolicyVersion) || (want != nil) != (plan.ModePolicyVersion == transcode.ModePolicyV6)) {
		return false
	}
	if want == nil {
		return true
	}
	// Every subtitle action belongs to a kept subtitle, and every kept
	// subtitle has one.
	have := map[int]bool{}
	for _, a := range plan.SubtitleActions {
		if !want.Keeps("subtitle", a.SourceStreamIndex) || have[a.SourceStreamIndex] {
			return false
		}
		have[a.SourceStreamIndex] = true
	}
	return len(have) == len(want.KeepSubtitles)
}

// keptView is the source as the output will have it: the report with only the
// kept audio and subtitle streams, and the file size less what the dropped
// streams state they take. A stream that states no size is not subtracted,
// which leaves the other streams' share larger and so plans a smaller video,
// never a larger one. Without a selection it is the source as it is.
func keptView(rep *mediainspect.DetailedReport, sel *transcode.TrackSelection, sourceBytes int64) (*mediainspect.DetailedReport, int64) {
	if rep == nil || sel == nil {
		return rep, sourceBytes
	}
	view := *rep
	view.Audio, view.Subtitles = nil, nil
	dropped := int64(0)
	for _, a := range rep.Audio {
		if sel.Keeps("audio", a.Index) {
			view.Audio = append(view.Audio, a)
		} else {
			dropped += audioTrackBytes(a, rep.DurationSec, sourceBytes)
		}
	}
	for _, s := range rep.Subtitles {
		if sel.Keeps("subtitle", s.Index) {
			view.Subtitles = append(view.Subtitles, s)
		} else {
			dropped += audioStreamBytes(s, rep.DurationSec, sourceBytes)
		}
	}
	return &view, max(sourceBytes-dropped, 1)
}

// keptViewOf reads the stored selection and applies it (see keptView).
func keptViewOf(state map[string]any, rep *mediainspect.DetailedReport, sourceBytes int64) (*mediainspect.DetailedReport, int64) {
	return keptView(rep, trackSelectionOf(state), sourceBytes)
}

// keptStreams are the source streams of one kind the selection keeps, each with
// the disposition the output must carry: when the default audio track is
// dropped the first kept one becomes the default, and likewise for subtitles
// only if the source had a default subtitle (transcode.TrackSelection.Partition).
// Without a selection they are all of the streams, unchanged.
func keptStreams(audio, subtitles []mediainspect.DetailedStream, sel *transcode.TrackSelection) (keptAudio, keptSubtitles []mediainspect.DetailedStream) {
	if sel == nil {
		return audio, subtitles
	}
	ka, ks, _, _ := sel.Partition(trackInfosOfStreams(audio, subtitles))
	pick := func(streams []mediainspect.DetailedStream, kept []transcode.TrackInfo) []mediainspect.DetailedStream {
		out := make([]mediainspect.DetailedStream, 0, len(kept))
		for _, k := range kept {
			for _, s := range streams {
				if s.Index == k.Index {
					s.Disposition = k.Disposition
					out = append(out, s)
					break
				}
			}
		}
		return out
	}
	return pick(audio, ka), pick(subtitles, ks)
}

// TrackEntry is one audio or subtitle track in a selection report.
type TrackEntry struct {
	Index    int    `json:"index"`
	Language string `json:"language"`
	Title    string `json:"title,omitempty"`
	Codec    string `json:"codec"`
	// Bytes is what the stream states it takes; 0 when it states nothing.
	Bytes int64 `json:"bytes,omitempty"`
}

// TrackSplit is a kind of track, split into those kept and those dropped, in
// source order.
type TrackSplit struct {
	Total   int          `json:"total"`
	Kept    []TrackEntry `json:"kept"`
	Dropped []TrackEntry `json:"dropped"`
}

// TrackSelectionReport says what a plan keeps of a source's audio and
// subtitles and what dropping the rest saves. BytesSaved sums the sizes the
// dropped streams state, so it is a lower bound.
type TrackSelectionReport struct {
	Audio      TrackSplit `json:"audio"`
	Subtitles  TrackSplit `json:"subtitles"`
	BytesSaved int64      `json:"bytes_saved"`
	// Languages is the list the selection followed, empty for a manual pick.
	Languages []string `json:"languages,omitempty"`
	Manual    bool     `json:"manual,omitempty"`
	Message   string   `json:"message"`
}

// trackSelectionReport describes a selection against its source. It is nil
// without a selection.
func trackSelectionReport(policy *transcode.TrackPolicy, sel *transcode.TrackSelection, rep *mediainspect.DetailedReport, sourceBytes int64) *TrackSelectionReport {
	if sel == nil || rep == nil {
		return nil
	}
	out := &TrackSelectionReport{}
	if policy != nil {
		out.Languages = slices.Clone(policy.Languages)
		out.Manual = policy.AudioStreams != nil || policy.SubtitleStreams != nil
	}
	split := func(kind string, streams []mediainspect.DetailedStream) TrackSplit {
		s := TrackSplit{Total: len(streams), Kept: []TrackEntry{}, Dropped: []TrackEntry{}}
		for _, st := range streams {
			bytes := int64(0)
			if kind == "audio" {
				bytes = audioTrackBytes(st, rep.DurationSec, sourceBytes)
			} else {
				bytes = audioStreamBytes(st, rep.DurationSec, sourceBytes)
			}
			entry := TrackEntry{Index: st.Index, Language: transcode.TrackInfo{Language: st.Language}.LanguageLabel(), Title: st.Title, Codec: st.Codec, Bytes: bytes}
			if sel.Keeps(kind, st.Index) {
				s.Kept = append(s.Kept, entry)
			} else {
				s.Dropped = append(s.Dropped, entry)
				out.BytesSaved += bytes
			}
		}
		return s
	}
	out.Audio, out.Subtitles = split("audio", rep.Audio), split("subtitle", rep.Subtitles)
	out.Message = trackSelectionMessage(out)
	return out
}

// trackSelectionMessage is the sentence the job shows, for example "Kept 4 of
// 8 audio tracks (spa, spa, eng, jpn) and 6 of 32 subtitles".
func trackSelectionMessage(r *TrackSelectionReport) string {
	part := func(s TrackSplit, noun, nouns string, languages bool) string {
		switch {
		case s.Total == 0:
			return ""
		case len(s.Dropped) == 0:
			if s.Total == 1 {
				return "the only " + noun
			}
			return fmt.Sprintf("all %d %s", s.Total, nouns)
		}
		labels := ""
		if languages {
			names := make([]string, len(s.Kept))
			for i, k := range s.Kept {
				names[i] = k.Language
			}
			labels = " (" + strings.Join(names, ", ") + ")"
		}
		return fmt.Sprintf("%d of %d %s%s", len(s.Kept), s.Total, nouns, labels)
	}
	audio, subs := part(r.Audio, "audio track", "audio tracks", true), part(r.Subtitles, "subtitle", "subtitles", false)
	msg := "Kept " + audio
	switch {
	case audio == "":
		msg = "Kept " + subs
	case subs != "":
		msg += " and " + subs
	}
	if r.BytesSaved > 0 {
		msg += fmt.Sprintf("; dropping the rest saves about %s", formatBudgetBytes(r.BytesSaved))
	}
	return msg
}

// addTrackReport puts what the plan kept of the source's tracks in a result:
// the lists, the bytes saved and the sentence to show.
func addTrackReport(result map[string]any, state map[string]any) {
	if r := trackReportOf(state); r != nil {
		result["tracks"], result["tracks_message"] = r, r.Message
	}
}

// trackReportOf is the report preflight stored for this job.
func trackReportOf(state map[string]any) *TrackSelectionReport {
	raw := state[trackReportKey]
	if raw == nil {
		return nil
	}
	if r, ok := raw.(*TrackSelectionReport); ok {
		return r
	}
	var r TrackSelectionReport
	if !reencode(raw, &r) || r.Message == "" {
		return nil
	}
	return &r
}
