package transcode

import (
	"reflect"
	"strings"
	"testing"
)

func aud(i int, lang string, disp ...string) TrackInfo {
	return TrackInfo{Index: i, Kind: "audio", Language: lang, Codec: "eac3", Disposition: dispOf(disp)}
}

func sub(i int, lang string, disp ...string) TrackInfo {
	return TrackInfo{Index: i, Kind: "subtitle", Language: lang, Codec: "subrip", Disposition: dispOf(disp)}
}

func dispOf(names []string) map[string]int {
	d := map[string]int{}
	for _, n := range names {
		d[n] = 1
	}
	return d
}

func video(i int) TrackInfo { return TrackInfo{Index: i, Kind: "video", Codec: "hevc"} }

func TestLanguageListCanonicalisation(t *testing.T) {
	got, err := LanguageList([]string{"SPA", " eng ", "ger", "deu", "fre", "chi", "jpn", "eng"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"deu", "eng", "fra", "jpn", "spa", "zho"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	empty, err := LanguageList(nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("an empty list is a non-nil empty list: %v %v", empty, err)
	}
	for _, bad := range []string{"xx", "english", "en", "", "e1g", "qaa"} {
		if _, err := LanguageList([]string{"eng", bad}); err == nil {
			t.Errorf("%q must be rejected", bad)
		} else if !strings.Contains(err.Error(), "ISO 639-2") {
			t.Errorf("%q: error %q does not name the standard", bad, err)
		}
	}
}

func TestBibliographicAndTerminologyCodesAreOneLanguage(t *testing.T) {
	for _, p := range [][2]string{{"ger", "deu"}, {"fre", "fra"}, {"chi", "zho"}, {"dut", "nld"}, {"rum", "ron"}, {"gre", "ell"}, {"per", "fas"}, {"cze", "ces"}} {
		if !ValidLanguageCode(p[0]) || !ValidLanguageCode(p[1]) {
			t.Errorf("%v: both spellings are valid codes", p)
		}
		if CanonicalLanguage(p[0]) != CanonicalLanguage(p[1]) {
			t.Errorf("%v: spellings differ: %s vs %s", p, CanonicalLanguage(p[0]), CanonicalLanguage(p[1]))
		}
	}
	for tag, want := range map[string]string{"es-419": "spa", "pt_BR": "por", "EN": "eng", "": "und", "und": "und", "Unknown": "und", "xyz": "xyz"} {
		if got := CanonicalLanguage(tag); got != want {
			t.Errorf("CanonicalLanguage(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestDefaultKeepLanguages(t *testing.T) {
	d := DefaultKeepLanguages()
	canonical, err := LanguageList(d)
	if err != nil || !reflect.DeepEqual(canonical, d) {
		t.Fatalf("the default must already be canonical: %v %v", d, err)
	}
	if !reflect.DeepEqual(d, []string{"eng", "jpn", "spa"}) {
		t.Fatalf("default = %v", d)
	}
}

func policy(langs ...string) *TrackPolicy {
	l, err := LanguageList(langs)
	if err != nil {
		panic(err)
	}
	return &TrackPolicy{Languages: l}
}

func TestSelectTracksRules(t *testing.T) {
	cases := []struct {
		name       string
		policy     *TrackPolicy
		tracks     []TrackInfo
		wantAudio  []int
		wantSubs   []int
		wantNoop   bool
		wantFailed bool
	}{
		{
			name:      "keeps the languages and drops the rest",
			policy:    policy("spa", "eng", "jpn"),
			tracks:    []TrackInfo{video(0), aud(1, "eng"), aud(2, "spa"), aud(3, "fre"), aud(4, "jpn"), aud(5, "ger"), sub(6, "eng"), sub(7, "fre"), sub(8, "spa")},
			wantAudio: []int{1, 2, 4}, wantSubs: []int{6, 8},
		},
		{
			name:      "the original track is kept even when its language is not wanted",
			policy:    policy("spa"),
			tracks:    []TrackInfo{video(0), aud(1, "eng"), aud(2, "spa"), aud(3, "fre", "original"), aud(4, "ger")},
			wantAudio: []int{2, 3}, wantSubs: []int{},
		},
		{
			name:      "without an original disposition the first audio track is the original",
			policy:    policy("spa"),
			tracks:    []TrackInfo{video(0), aud(1, "jpn"), aud(2, "spa"), aud(3, "fre")},
			wantAudio: []int{1, 2}, wantSubs: []int{},
		},
		{
			name:      "no audio in the list keeps all audio",
			policy:    policy("spa"),
			tracks:    []TrackInfo{video(0), aud(1, "jpn"), aud(2, "fre"), sub(3, "jpn"), sub(4, "fre"), sub(5, "spa")},
			wantAudio: []int{1, 2}, wantSubs: []int{3, 5},
		},
		{
			name:      "untagged and und tracks are kept",
			policy:    policy("spa"),
			tracks:    []TrackInfo{video(0), aud(1, "spa"), aud(2, ""), aud(3, "und"), aud(4, "fre"), sub(5, ""), sub(6, "und"), sub(7, "fre")},
			wantAudio: []int{1, 2, 3}, wantSubs: []int{5, 6},
		},
		{
			name:      "bibliographic and terminology codes are one language",
			policy:    policy("deu", "fra"),
			tracks:    []TrackInfo{video(0), aud(1, "ger"), aud(2, "fre"), aud(3, "eng"), aud(4, "fra"), sub(5, "ger"), sub(6, "deu"), sub(7, "eng")},
			wantAudio: []int{1, 2, 4}, wantSubs: []int{5, 6},
		},
		{
			name:      "locale tags fold to their language",
			policy:    policy("spa", "eng"),
			tracks:    []TrackInfo{video(0), aud(1, "es-419"), aud(2, "pt-BR"), aud(3, "en")},
			wantAudio: []int{1, 3}, wantSubs: []int{},
		},
		{
			name:      "a subtitle in the original track's language is kept",
			policy:    policy("spa"),
			tracks:    []TrackInfo{video(0), aud(1, "jpn", "original"), aud(2, "spa"), sub(3, "jpn"), sub(4, "fre"), sub(5, "spa")},
			wantAudio: []int{1, 2}, wantSubs: []int{3, 5},
		},
		{
			name:      "forced subtitles follow the same rule",
			policy:    policy("spa"),
			tracks:    []TrackInfo{video(0), aud(1, "spa"), aud(2, "fre"), sub(3, "spa", "forced"), sub(4, "fre", "forced"), sub(5, "spa")},
			wantAudio: []int{1}, wantSubs: []int{3, 5},
		},
		{
			name:      "a forced subtitle of the original language is kept",
			policy:    policy("spa"),
			tracks:    []TrackInfo{video(0), aud(1, "fre", "original"), aud(2, "spa"), sub(3, "fre", "forced"), sub(4, "ger", "forced")},
			wantAudio: []int{1, 2}, wantSubs: []int{3},
		},
		{
			name:     "an empty list keeps everything",
			policy:   &TrackPolicy{},
			tracks:   []TrackInfo{video(0), aud(1, "fre"), sub(2, "ger")},
			wantNoop: true,
		},
		{
			name:     "nothing to remove is a no-op",
			policy:   policy("spa", "eng"),
			tracks:   []TrackInfo{video(0), aud(1, "spa"), aud(2, "eng"), sub(3, "eng")},
			wantNoop: true,
		},
		{
			name:     "a source without audio or subtitles is a no-op",
			policy:   policy("spa"),
			tracks:   []TrackInfo{video(0)},
			wantNoop: true,
		},
		{
			name:      "explicit streams replace the rule and are exact",
			policy:    &TrackPolicy{Languages: []string{"spa"}, AudioStreams: &[]int{3, 1}, SubtitleStreams: &[]int{}},
			tracks:    []TrackInfo{video(0), aud(1, "spa"), aud(2, "spa"), aud(3, "fre"), sub(4, "spa")},
			wantAudio: []int{1, 3}, wantSubs: []int{},
		},
		{
			name:       "an explicit index that is not an audio stream fails",
			policy:     &TrackPolicy{AudioStreams: &[]int{4}},
			tracks:     []TrackInfo{video(0), aud(1, "spa"), sub(4, "spa")},
			wantFailed: true,
		},
		{
			name:       "an explicit index that does not exist fails",
			policy:     &TrackPolicy{SubtitleStreams: &[]int{9}},
			tracks:     []TrackInfo{video(0), aud(1, "spa"), sub(2, "spa")},
			wantFailed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := SelectTracks(tc.policy, tc.tracks)
			if tc.wantFailed {
				if err == nil {
					t.Fatalf("want an error, got %+v", sel)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantNoop {
				if sel != nil {
					t.Fatalf("want no selection, got %+v", sel)
				}
				return
			}
			if sel == nil {
				t.Fatal("want a selection, got none")
			}
			if !reflect.DeepEqual(sel.KeepAudio, tc.wantAudio) || !reflect.DeepEqual(sel.KeepSubtitles, tc.wantSubs) {
				t.Fatalf("kept audio %v subtitles %v, want %v and %v", sel.KeepAudio, sel.KeepSubtitles, tc.wantAudio, tc.wantSubs)
			}
			if err := sel.Validate(); err != nil {
				t.Fatalf("a derived selection must validate: %v", err)
			}
		})
	}
}

// A selection is a function of the policy and the tracks alone: the same input
// always gives the same selection, and the policy is never changed by it.
func TestSelectTracksIsDeterministic(t *testing.T) {
	p := policy("spa", "eng")
	tracks := []TrackInfo{video(0), aud(1, "eng"), aud(2, "fre"), aud(3, "spa"), sub(4, "fre"), sub(5, "eng")}
	first, err := SelectTracks(p, tracks)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, _ := SelectTracks(p.Clone(), tracks)
		if !first.Equal(again) {
			t.Fatalf("run %d: %+v != %+v", i, again, first)
		}
	}
	if !reflect.DeepEqual(p.Languages, []string{"eng", "spa"}) {
		t.Fatalf("the policy changed: %v", p.Languages)
	}
}

func TestPartitionReassignsDefaults(t *testing.T) {
	tracks := []TrackInfo{
		video(0),
		aud(1, "fre", "default"), aud(2, "spa"), aud(3, "eng"),
		sub(4, "fre", "default"), sub(5, "spa"),
	}
	sel := &TrackSelection{KeepAudio: []int{2, 3}, KeepSubtitles: []int{5}}
	ka, ks, da, ds := sel.Partition(tracks)
	if len(ka) != 2 || len(ks) != 1 || len(da) != 1 || len(ds) != 1 {
		t.Fatalf("partition sizes %d %d %d %d", len(ka), len(ks), len(da), len(ds))
	}
	if ka[0].Disposition["default"] != 1 || ka[1].Disposition["default"] != 0 {
		t.Errorf("the first kept audio track must become the default: %v %v", ka[0].Disposition, ka[1].Disposition)
	}
	if ks[0].Disposition["default"] != 1 {
		t.Errorf("a default subtitle existed and was dropped: the first kept one becomes the default (%v)", ks[0].Disposition)
	}
	// The source tracks are not modified.
	if tracks[2].Disposition["default"] != 0 || tracks[5].Disposition["default"] != 0 {
		t.Error("Partition changed the source's dispositions")
	}
}

func TestPartitionLeavesDefaultsAlone(t *testing.T) {
	// The default audio track is kept: nothing changes.
	tracks := []TrackInfo{video(0), aud(1, "spa", "default"), aud(2, "fre"), sub(3, "spa"), sub(4, "fre")}
	sel := &TrackSelection{KeepAudio: []int{1}, KeepSubtitles: []int{3}}
	ka, ks, _, _ := sel.Partition(tracks)
	if ka[0].Disposition["default"] != 1 {
		t.Error("the kept default audio track lost its flag")
	}
	// No subtitle was default in the source, so none becomes one.
	if ks[0].Disposition["default"] != 0 {
		t.Errorf("a subtitle became default although the source had no default subtitle: %v", ks[0].Disposition)
	}
	// Without a selection nothing is dropped or changed.
	ka, ks, da, ds := (*TrackSelection)(nil).Partition(tracks)
	if len(ka) != 2 || len(ks) != 2 || len(da) != 0 || len(ds) != 0 {
		t.Errorf("a nil selection keeps everything: %d %d %d %d", len(ka), len(ks), len(da), len(ds))
	}
}

func TestOriginalAudioTrack(t *testing.T) {
	tracks := []TrackInfo{video(0), sub(1, "eng"), aud(2, "fre"), aud(3, "jpn", "original"), aud(4, "spa")}
	if o, ok := OriginalAudioTrack(tracks); !ok || o.Index != 3 {
		t.Fatalf("original = %+v %v", o, ok)
	}
	if o, ok := OriginalAudioTrack(tracks[:3]); !ok || o.Index != 2 {
		t.Fatalf("without a disposition the first audio track is the original: %+v %v", o, ok)
	}
	if _, ok := OriginalAudioTrack([]TrackInfo{video(0)}); ok {
		t.Fatal("no audio, no original")
	}
}

func TestTrackPolicyValidate(t *testing.T) {
	bad := []*TrackPolicy{
		{Languages: []string{"xx"}},
		{Languages: []string{"spa", "eng"}},        // not sorted
		{Languages: []string{"ger"}},               // not canonical
		{AudioStreams: &[]int{}},                   // an empty audio list
		{AudioStreams: &[]int{2, 1}},               // not ascending
		{SubtitleStreams: &[]int{1, 1}},            // duplicate
		{SubtitleStreams: &[]int{-1}},              // negative
		{Languages: []string{}, AudioStreams: nil}, // inactive
	}
	for i, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("case %d: %+v must be rejected", i, p)
		}
	}
	ok := []*TrackPolicy{nil, policy("spa"), {SubtitleStreams: &[]int{}}, {AudioStreams: &[]int{1, 4}}}
	for i, p := range ok {
		if err := p.Validate(); err != nil {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestTrackSelectionValidateAndEqual(t *testing.T) {
	if err := (&TrackSelection{KeepAudio: []int{2, 1}}).Validate(); err == nil {
		t.Error("descending indexes must be rejected")
	}
	if err := (&TrackSelection{KeepAudio: []int{1}, KeepSubtitles: []int{1}}).Validate(); err == nil {
		t.Error("a stream cannot be both audio and subtitle")
	}
	a := &TrackSelection{KeepAudio: []int{1}, KeepSubtitles: []int{}}
	if !a.Equal(&TrackSelection{KeepAudio: []int{1}, KeepSubtitles: nil}) {
		t.Error("equal selections")
	}
	if a.Equal(nil) || !(*TrackSelection)(nil).Equal(nil) {
		t.Error("nil handling")
	}
	if !a.Keeps("audio", 1) || a.Keeps("audio", 2) || !a.Keeps("video", 9) || !(*TrackSelection)(nil).Keeps("audio", 2) {
		t.Error("Keeps")
	}
}
