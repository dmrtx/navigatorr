package transcodeworker

import (
	"slices"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// tracksSource is a synthetic file: programme video, four audio tracks (the
// default is the French AAC stereo one), three subtitles (the Spanish one
// forced), a font and a cover.
func tracksSource() []SourceStream {
	return []SourceStream{
		{Kind: "video", Index: 0, TypeIndex: 0, Codec: "h264", Width: 1920, Height: 1080},
		{Kind: "audio", Index: 1, TypeIndex: 0, Codec: "eac3", Channels: 6, Layout: "5.1(side)", Language: "eng", BitrateKbps: 640, Title: "English 5.1"},
		{Kind: "audio", Index: 2, TypeIndex: 1, Codec: "aac", Profile: "LC", Channels: 2, Layout: "stereo", Language: "fre", BitrateKbps: 128, Disposition: map[string]int{"default": 1}},
		{Kind: "audio", Index: 3, TypeIndex: 2, Codec: "aac", Profile: "LC", Channels: 2, Layout: "stereo", Language: "spa", BitrateKbps: 128},
		{Kind: "audio", Index: 4, TypeIndex: 3, Codec: "eac3", Channels: 6, Layout: "5.1(side)", Language: "jpn", BitrateKbps: 640},
		{Kind: "subtitle", Index: 5, TypeIndex: 0, Codec: "subrip", Language: "eng"},
		{Kind: "subtitle", Index: 6, TypeIndex: 1, Codec: "subrip", Language: "fre"},
		{Kind: "subtitle", Index: 7, TypeIndex: 2, Codec: "subrip", Language: "spa", Disposition: map[string]int{"forced": 1}},
	}
}

func tracksPlan(sel *transcode.TrackSelection) *transcode.Plan {
	p := sizeV5TestPlan(transcode.AudioModeSizeAAC)
	p.SubtitleActions = []transcode.SubtitleAction{
		{TypeIndex: 0, SourceStreamIndex: 5, SourceCodec: "subrip", Operation: "copy", Codec: "copy"},
		{TypeIndex: 1, SourceStreamIndex: 6, SourceCodec: "subrip", Operation: "copy", Codec: "copy"},
		{TypeIndex: 2, SourceStreamIndex: 7, SourceCodec: "subrip", Operation: "copy", Codec: "copy"},
	}
	if sel != nil {
		p.Tracks, p.ModePolicyVersion = sel, transcode.ModePolicyV6
		var kept []transcode.SubtitleAction
		for _, a := range p.SubtitleActions {
			if sel.Keeps("subtitle", a.SourceStreamIndex) {
				kept = append(kept, a)
			}
		}
		p.SubtitleActions = kept
	}
	p.PlanDigest, _ = transcode.DigestPlan(p)
	return p
}

func mapsOf(args []string) []string {
	var maps []string
	for i, a := range args {
		if a == "-map" && i+1 < len(args) {
			maps = append(maps, args[i+1])
		}
	}
	return maps
}

// Plan version 6 is exactly "a plan with a track selection": the selection and
// the version go together, only the size and compact goals carry it, and it
// extends whichever audio rule the goal froze.
func TestTrackSelectionPlanVersion(t *testing.T) {
	sel := func() *transcode.TrackSelection {
		return &transcode.TrackSelection{KeepAudio: []int{1, 3, 4}, KeepSubtitles: []int{5, 7}}
	}
	valid := tracksPlan(sel())
	if err := ValidatePlan(valid); err != nil {
		t.Fatalf("v6 size plan rejected: %v", err)
	}
	lossless := tracksPlan(sel())
	lossless.AudioMode = transcode.AudioModeCompactLossless
	lossless.PlanDigest, _ = transcode.DigestPlan(lossless)
	if err := ValidatePlan(lossless); err != nil {
		t.Fatalf("v6 size plan with lossless compaction rejected: %v", err)
	}
	for name, mutate := range map[string]func(*transcode.Plan){
		"v6 without a selection":           func(p *transcode.Plan) { p.Tracks = nil },
		"a selection on v5":                func(p *transcode.Plan) { p.ModePolicyVersion = transcode.ModePolicyV5 },
		"a selection on v3":                func(p *transcode.Plan) { p.ModePolicyVersion = transcode.ModePolicyV3 },
		"unsorted streams":                 func(p *transcode.Plan) { p.Tracks.KeepAudio = []int{3, 1} },
		"one stream as both":               func(p *transcode.Plan) { p.Tracks.KeepSubtitles = []int{1} },
		"a quality goal":                   func(p *transcode.Plan) { p.Mode = "quality" },
		"copied audio under v6":            func(p *transcode.Plan) { p.AudioMode = "copy" },
		"a changed selection, unsigned":    func(p *transcode.Plan) { p.Tracks.KeepAudio = []int{1, 2, 3, 4} },
		"lossy compact audio on a size v6": func(p *transcode.Plan) { p.AudioMode = "compact" },
	} {
		p := tracksPlan(sel())
		mutate(p)
		if name != "a changed selection, unsigned" {
			p.PlanDigest, _ = transcode.DigestPlan(p)
		}
		if err := ValidatePlan(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A plan without a selection is the plan it always was.
	if err := ValidatePlan(tracksPlan(nil)); err != nil {
		t.Fatalf("a plan without a selection rejected: %v", err)
	}
	// Compact: v6 admits compact's own audio mode and nothing else.
	if c, ok := transcode.ModeMinimumSavings("compact", transcode.ModePolicyV6); !ok || c != 30 {
		t.Fatal("v6 compact minimum savings", c, ok)
	}
	if c, ok := transcode.ModeMinimumSavings("size", transcode.ModePolicyV6); !ok || c != 15 {
		t.Fatal("v6 size minimum savings", c, ok)
	}
	if !modeAudioModeAllowed("compact", transcode.ModePolicyV6, "compact") || modeAudioModeAllowed("compact", transcode.ModePolicyV6, "copy") || modeAudioModeAllowed("quality", transcode.ModePolicyV6, "copy") {
		t.Fatal("v6 audio rules")
	}
}

// The execution plan keeps only the selected streams, and the arguments map
// them by index and address the output's streams: the first kept subtitle is
// the output's subtitle 0 whatever its place in the source, and the converted
// tracks and the copied one are told apart by their output position.
func TestTrackSelectionExecutionPlanAndArguments(t *testing.T) {
	sel := &transcode.TrackSelection{KeepAudio: []int{1, 3, 4}, KeepSubtitles: []int{5, 7}}
	ep, err := BuildExecutionPlan(tracksPlan(sel), tracksSource(), 1800)
	if err != nil {
		t.Fatal(err)
	}
	type got struct {
		kind           string
		source, output int
		target         string
	}
	var actions []got
	for _, s := range ep.Streams {
		if s.Kind == "audio" || s.Kind == "subtitle" {
			actions = append(actions, got{s.Kind, s.SourceIndex, s.OutputIndex, s.TargetCodec})
		}
	}
	want := []got{{"audio", 1, 0, "aac"}, {"audio", 3, 1, "copy"}, {"audio", 4, 2, "aac"}, {"subtitle", 5, 0, "copy"}, {"subtitle", 7, 1, "copy"}}
	if !slices.Equal(actions, want) {
		t.Fatalf("actions %+v, want %+v", actions, want)
	}
	args, err := BuildFFmpegArgs(ep, "source.mkv", "candidate.mkv", "progress")
	if err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(args, " ") + " "
	// Video first, the selected audio and subtitles by index, attachments last;
	// never the wildcard that would bring everything back.
	if maps := mapsOf(args); !slices.Equal(maps, []string{"0:v?", "0:1", "0:3", "0:4", "0:5", "0:7", "0:t?"}) {
		t.Fatalf("maps %v", maps)
	}
	for _, wildcard := range []string{" 0:a? ", " 0:s? "} {
		if strings.Contains(joined, wildcard) {
			t.Fatalf("the selection maps %q: %s", wildcard, joined)
		}
	}
	// Converted tracks are addressed by output position, 0 and 2; the copied one
	// (output 1) has no encoder of its own.
	for _, must := range []string{" -c:a:0 aac_at ", " -b:a:0 384k ", " -c:a:2 aac_at ", " -b:a:2 384k ", " -profile:a:0 2 ", " -profile:a:2 2 ", " -c:s:0 copy ", " -c:s:1 copy "} {
		if !strings.Contains(joined, must) {
			t.Errorf("missing %q in %s", must, joined)
		}
	}
	for _, mustNot := range []string{" -c:a:1 ", " -b:a:1 ", " -c:a:3 ", " -b:a:3 ", " -c:s:2 ", " -profile:a:1 ", " -profile:a:3 "} {
		if strings.Contains(joined, mustNot) {
			t.Errorf("unexpected %q in %s", mustNot, joined)
		}
	}
	// The French track was the default and is gone: the first kept track, the
	// English one, is the output's default. The others carry no flag of their
	// own in the source and none is written for them.
	if !strings.Contains(joined, " -disposition:a:0 default ") {
		t.Errorf("the first kept audio track is not the default: %s", joined)
	}
	for _, mustNot := range []string{" -disposition:a:1 ", " -disposition:a:2 "} {
		if strings.Contains(joined, mustNot) {
			t.Errorf("unexpected %q in %s", mustNot, joined)
		}
	}
	// A subtitle keeps its own flags with the stream: nothing is written for the
	// forced Spanish one, and no subtitle becomes default, for none was.
	if strings.Contains(joined, " -disposition:s:") {
		t.Errorf("subtitle dispositions written without a reason: %s", joined)
	}

	// Without a selection nothing changes: the wildcards and the source's own
	// positions.
	whole, err := BuildExecutionPlan(tracksPlan(nil), tracksSource(), 1800)
	if err != nil {
		t.Fatal(err)
	}
	wargs, err := BuildFFmpegArgs(whole, "source.mkv", "candidate.mkv", "progress")
	if err != nil {
		t.Fatal(err)
	}
	if maps := mapsOf(wargs); !slices.Equal(maps, []string{"0:v?", "0:a?", "0:s?", "0:t?"}) {
		t.Fatalf("maps without a selection %v", maps)
	}
	wjoined := " " + strings.Join(wargs, " ") + " "
	for _, must := range []string{" -c:a:0 aac_at ", " -c:a:3 aac_at ", " -c:s:2 copy "} {
		if !strings.Contains(wjoined, must) {
			t.Errorf("without a selection: missing %q in %s", must, wjoined)
		}
	}
	if strings.Contains(wjoined, " -disposition:s:") {
		t.Errorf("without a selection a subtitle disposition was written: %s", wjoined)
	}
}

// A default subtitle that is dropped passes to the first kept subtitle; a
// source without one gets none.
func TestTrackSelectionMovesTheDefaultSubtitle(t *testing.T) {
	src := tracksSource()
	src[6].Disposition = map[string]int{"default": 1} // the French subtitle
	sel := &transcode.TrackSelection{KeepAudio: []int{1, 4}, KeepSubtitles: []int{5, 7}}
	p := tracksPlan(sel)
	ep, err := BuildExecutionPlan(p, src, 1800)
	if err != nil {
		t.Fatal(err)
	}
	args, err := BuildFFmpegArgs(ep, "source.mkv", "candidate.mkv", "progress")
	if err != nil {
		t.Fatal(err)
	}
	if joined := " " + strings.Join(args, " ") + " "; !strings.Contains(joined, " -disposition:s:0 default ") {
		t.Fatalf("the first kept subtitle is not the default: %s", joined)
	}
}

// A dropped default subtitle that was forced never hands the flag to a full
// subtitle: a kept forced track takes it, and with none there is no default.
func TestTrackSelectionForcedDefaultSubtitle(t *testing.T) {
	build := func(src []SourceStream, sel *transcode.TrackSelection) string {
		ep, err := BuildExecutionPlan(tracksPlan(sel), src, 1800)
		if err != nil {
			t.Fatal(err)
		}
		args, err := BuildFFmpegArgs(ep, "source.mkv", "candidate.mkv", "progress")
		if err != nil {
			t.Fatal(err)
		}
		return " " + strings.Join(args, " ") + " "
	}
	// The French subtitle is a forced default; the Spanish one is forced too.
	src := tracksSource()
	src[6].Disposition = map[string]int{"default": 1, "forced": 1}
	kept := &transcode.TrackSelection{KeepAudio: []int{1, 4}, KeepSubtitles: []int{5, 7}}
	if joined := build(src, kept); !strings.Contains(joined, " -disposition:s:1 default+forced ") || strings.Contains(joined, " -disposition:s:0 ") {
		t.Fatalf("the kept forced subtitle did not take the default: %s", joined)
	}
	// Only full subtitles are kept: nothing becomes the default.
	src = tracksSource()
	src[6].Disposition = map[string]int{"default": 1, "forced": 1}
	src[7].Disposition = nil
	full := &transcode.TrackSelection{KeepAudio: []int{1, 4}, KeepSubtitles: []int{5, 7}}
	if joined := build(src, full); strings.Contains(joined, " -disposition:s:") {
		t.Fatalf("a full subtitle became the default of a dropped forced one: %s", joined)
	}
}

// A source with no audio can still lose subtitles: its plan keeps no audio and
// builds. A source that has audio can never lose all of it.
func TestTrackSelectionOnASourceWithoutAudio(t *testing.T) {
	var silent []SourceStream
	for _, s := range tracksSource() {
		if s.Kind != "audio" {
			silent = append(silent, s)
		}
	}
	sel := &transcode.TrackSelection{KeepAudio: []int{}, KeepSubtitles: []int{5, 7}}
	plan := tracksPlan(sel)
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("a plan that keeps subtitles of a silent source was rejected: %v", err)
	}
	ep, err := BuildExecutionPlan(plan, silent, 1800)
	if err != nil {
		t.Fatalf("silent source: %v", err)
	}
	args, err := BuildFFmpegArgs(ep, "source.mkv", "candidate.mkv", "progress")
	if err != nil {
		t.Fatal(err)
	}
	if maps := mapsOf(args); !slices.Equal(maps, []string{"0:v?", "0:5", "0:7", "0:t?"}) {
		t.Fatalf("maps %v", maps)
	}
	if _, err := BuildExecutionPlan(plan, tracksSource(), 1800); err == nil {
		t.Fatal("a plan that keeps no audio was built for a source that has audio")
	}
}

// A plan that no longer describes the source fails before anything is encoded.
func TestTrackSelectionRefusesAChangedSource(t *testing.T) {
	sel := &transcode.TrackSelection{KeepAudio: []int{1, 3, 4}, KeepSubtitles: []int{5, 7}}
	for name, mutate := range map[string]func([]SourceStream) []SourceStream{
		"a kept audio stream is gone":    func(s []SourceStream) []SourceStream { return slices.Delete(slices.Clone(s), 4, 5) },
		"a kept stream is another kind":  func(s []SourceStream) []SourceStream { c := slices.Clone(s); c[3].Kind = "subtitle"; return c },
		"a kept subtitle is gone":        func(s []SourceStream) []SourceStream { return slices.Delete(slices.Clone(s), 7, 8) },
		"a subtitle became an audio one": func(s []SourceStream) []SourceStream { c := slices.Clone(s); c[7].Kind = "audio"; return c },
	} {
		if _, err := BuildExecutionPlan(tracksPlan(sel), mutate(tracksSource()), 1800); err == nil {
			t.Errorf("%s: the plan was built", name)
		}
	}
	// An action for a subtitle the plan drops means the plan is not the one the
	// server signed.
	p := tracksPlan(sel)
	p.SubtitleActions = append(p.SubtitleActions, transcode.SubtitleAction{TypeIndex: 1, SourceStreamIndex: 6, SourceCodec: "subrip", Operation: "copy", Codec: "copy"})
	if _, err := BuildExecutionPlan(p, tracksSource(), 1800); err == nil {
		t.Error("an action for a dropped subtitle was accepted")
	}
}

// The candidate must have exactly the planned tracks: count, order, language,
// codec rules and channels.
func TestTrackSelectionCandidateValidation(t *testing.T) {
	sel := &transcode.TrackSelection{KeepAudio: []int{1, 3, 4}, KeepSubtitles: []int{5, 7}}
	plan := tracksPlan(sel)
	src := tracksSource()
	audio := func(codec, profile, lang string, channels int, layout string, def bool) SourceStream {
		s := SourceStream{Kind: "audio", Codec: codec, Profile: profile, Language: lang, Channels: channels, Layout: layout}
		if def {
			s.Disposition = map[string]int{"default": 1}
		}
		return s
	}
	good := func() []SourceStream {
		return []SourceStream{
			audio("aac", "LC", "eng", 6, "5.1", true),
			audio("aac", "LC", "spa", 2, "stereo", false),
			audio("aac", "LC", "jpn", 6, "5.1", false),
			{Kind: "subtitle", Codec: "subrip", Language: "eng"},
			{Kind: "subtitle", Codec: "subrip", Language: "spa", Disposition: map[string]int{"forced": 1}},
		}
	}
	check := func(cand []SourceStream) (audioErr, subErr error) {
		return validateAudioPreservation(plan, src, cand), validateSubtitlePreservation(plan, src, cand)
	}
	if a, s := check(good()); a != nil || s != nil {
		t.Fatalf("the planned output was rejected: %v %v", a, s)
	}
	type mutation func([]SourceStream) []SourceStream
	for name, tc := range map[string]struct {
		mutate mutation
		audio  bool // the audio check must fail
		sub    bool // the subtitle check must fail
	}{
		"every audio track kept": {func(c []SourceStream) []SourceStream {
			return slices.Insert(c, 1, audio("aac", "LC", "fre", 2, "stereo", false))
		}, true, false},
		"a kept audio track missing": {func(c []SourceStream) []SourceStream { return slices.Delete(c, 1, 2) }, true, false},
		"audio languages out of order": {func(c []SourceStream) []SourceStream {
			c[1], c[2] = c[2], c[1]
			return c
		}, true, false},
		"channels reduced": {func(c []SourceStream) []SourceStream { c[0].Channels, c[0].Layout = 2, "stereo"; return c }, true, false},
		"a dropped subtitle kept": {func(c []SourceStream) []SourceStream {
			return slices.Insert(c, 4, SourceStream{Kind: "subtitle", Codec: "subrip", Language: "fre"})
		}, false, true},
		"a kept subtitle missing": {func(c []SourceStream) []SourceStream { return c[:4] }, false, true},
		"subtitle language wrong": {func(c []SourceStream) []SourceStream { c[3].Language = "ger"; return c }, false, true},
		"the default flag lost":   {func(c []SourceStream) []SourceStream { c[0].Disposition = nil; return c }, true, false},
		"the forced flag lost":    {func(c []SourceStream) []SourceStream { c[4].Disposition = nil; return c }, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			a, s := check(tc.mutate(good()))
			if (a != nil) != tc.audio || (s != nil) != tc.sub {
				t.Fatalf("audio error %v (want one: %v), subtitle error %v (want one: %v)", a, tc.audio, s, tc.sub)
			}
		})
	}
	// The same candidate against a plan with no selection has too few tracks.
	whole := tracksPlan(nil)
	if err := validateAudioPreservation(whole, src, good()); err == nil {
		t.Error("a candidate of the kept tracks passed for a plan that keeps all")
	}
}
