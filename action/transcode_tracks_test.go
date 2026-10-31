package action

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// trackFixtureReport is the episode of levelReport with languages: eight E-AC-3
// 640 kbps 5.1 tracks (eng, two Spanish, fre, ger, ita, por, jpn) and 32 text
// subtitles, six of them in the languages kept (eng, spa and jpn twice each,
// one of the Spanish ones forced). The names are synthetic.
func trackFixtureReport() *mediainspect.DetailedReport {
	rep := levelReport()
	langs := []string{"eng", "spa", "spa", "fre", "ger", "ita", "por", "jpn"}
	titles := []string{"English", "Spanish (Castilian)", "Spanish (Latin America)", "French", "German", "Italian", "Portuguese", "Japanese"}
	for i := range rep.Audio {
		rep.Audio[i].Language, rep.Audio[i].Title = langs[i], titles[i]
	}
	rep.Audio[0].Disposition = map[string]int{"default": 1}
	subLangs := []string{"eng", "eng", "spa", "spa", "jpn", "jpn"}
	others := []string{"fre", "ger", "ita", "por", "rus", "kor"}
	rep.Subtitles = nil
	for i := 0; i < 32; i++ {
		lang := ""
		if i < len(subLangs) {
			lang = subLangs[i]
		} else {
			lang = others[(i-len(subLangs))%len(others)]
		}
		s := mediainspect.DetailedStream{Index: 9 + i, Codec: "subrip", Language: lang, Tags: map[string]string{"NUMBER_OF_BYTES": "4000000"}}
		if i == 3 {
			s.Disposition = map[string]int{"forced": 1}
		}
		rep.Subtitles = append(rep.Subtitles, s)
	}
	return rep
}

func TestKeepLanguagesInputs(t *testing.T) {
	for name, tc := range map[string]struct {
		mode   string
		inputs map[string]any
		ok     bool
	}{
		"codes":                      {"size", map[string]any{"keep_languages": []any{"eng", "ger"}}, true},
		"empty list keeps all":       {"compact", map[string]any{"keep_languages": []any{}}, true},
		"go slice":                   {"size", map[string]any{"keep_languages": []string{"spa"}}, true},
		"manual picks":               {"size", map[string]any{"keep_audio_streams": []any{1.0, 3.0}, "keep_subtitle_streams": []any{}}, true},
		"not a code":                 {"size", map[string]any{"keep_languages": []any{"english"}}, false},
		"two letters":                {"size", map[string]any{"keep_languages": []any{"en"}}, false},
		"not a list":                 {"size", map[string]any{"keep_languages": "eng"}, false},
		"not strings":                {"size", map[string]any{"keep_languages": []any{1.0}}, false},
		"empty audio pick":           {"size", map[string]any{"keep_audio_streams": []any{}}, false},
		"negative stream":            {"size", map[string]any{"keep_subtitle_streams": []any{-1.0}}, false},
		"fractional stream":          {"size", map[string]any{"keep_audio_streams": []any{1.5}}, false},
		"streams on a batch child":   {"size", map[string]any{"keep_audio_streams": []any{1.0}, "parent_action_id": "batch-1"}, false},
		"languages on a batch child": {"size", map[string]any{"keep_languages": []any{"eng"}, "parent_action_id": "batch-1"}, true},
		"quality goal":               {"quality", map[string]any{"keep_languages": []any{"eng"}}, false},
		"x265_preserve goal":         {"x265_preserve", map[string]any{"keep_languages": []any{"eng"}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			inputs := map[string]any{"mode": tc.mode}
			for k, v := range tc.inputs {
				inputs[k] = v
			}
			err := validateModeInputs(inputs)
			if (err == nil) != tc.ok {
				t.Fatalf("validateModeInputs: %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestTrackPolicyFreezesWithTheGoal(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	engine := NewEngine(EngineDeps{Store: st})
	freeze := func(inputs map[string]any, state map[string]any) *ModePolicySnapshot {
		t.Helper()
		ec := &ExecutionContext{Engine: engine, InstanceID: "tracks", Inputs: inputs, State: state}
		p, err := freezeModePolicy(ec)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// The global default applies to size and compact, not to the other goals.
	size := freeze(map[string]any{"mode": "size"}, map[string]any{})
	if size.Tracks == nil || !slices.Equal(size.Tracks.Languages, []string{"eng", "jpn", "spa"}) || size.Version != ModePolicyV4 {
		t.Fatalf("default size policy %+v", size.Tracks)
	}
	if compact := freeze(map[string]any{"mode": "compact"}, map[string]any{}); compact.Tracks == nil {
		t.Fatal("compact must freeze the list too")
	}
	for _, mode := range []string{"quality", "x265_preserve"} {
		if p := freeze(map[string]any{"mode": mode}, map[string]any{}); p.Tracks != nil {
			t.Fatalf("%s froze a track policy", mode)
		}
	}

	// The job's own list overrides the global one; [] turns the selection off.
	over := freeze(map[string]any{"mode": "size", "keep_languages": []any{"ger", "fre"}}, map[string]any{})
	if !slices.Equal(over.Tracks.Languages, []string{"deu", "fra"}) {
		t.Fatalf("override %+v", over.Tracks)
	}
	if off := freeze(map[string]any{"mode": "size", "keep_languages": []any{}}, map[string]any{}); off.Tracks != nil {
		t.Fatalf("an empty override froze %+v", off.Tracks)
	}
	manual := freeze(map[string]any{"mode": "size", "keep_audio_streams": []any{3.0, 1.0}, "keep_subtitle_streams": []any{}}, map[string]any{})
	if manual.Tracks.AudioStreams == nil || !slices.Equal(*manual.Tracks.AudioStreams, []int{1, 3}) || manual.Tracks.SubtitleStreams == nil || len(*manual.Tracks.SubtitleStreams) != 0 || !slices.Equal(manual.Tracks.Languages, []string{"eng", "jpn", "spa"}) {
		t.Fatalf("manual policy %+v", manual.Tracks)
	}

	// An empty saved global list is a choice: no selection.
	if _, err := st.SetKeepLanguages([]string{}); err != nil {
		t.Fatal(err)
	}
	if p := freeze(map[string]any{"mode": "size"}, map[string]any{}); p.Tracks != nil {
		t.Fatalf("an empty global list froze %+v", p.Tracks)
	}

	// The snapshot is frozen: a later change of the global list never reaches
	// a job that has its own, and it reads back with the same digest.
	if _, err := st.SetKeepLanguages([]string{"deu"}); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{}
	first := freeze(map[string]any{"mode": "size"}, state)
	if !slices.Equal(first.Tracks.Languages, []string{"deu"}) {
		t.Fatalf("first %+v", first.Tracks)
	}
	if _, err := st.SetKeepLanguages([]string{"eng", "jpn"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	var restored map[string]any
	_ = json.Unmarshal(raw, &restored)
	again := freeze(map[string]any{"mode": "size"}, restored)
	if again.Digest != first.Digest || !slices.Equal(again.Tracks.Languages, []string{"deu"}) {
		t.Fatalf("a later change reached a frozen job: %+v", again.Tracks)
	}
	// The selection is part of the digest: tampering fails closed.
	tampered := restored["mode_policy"].(map[string]any)
	tampered["tracks"] = map[string]any{"languages": []any{"eng"}}
	if _, err := freezeModePolicy(&ExecutionContext{Engine: engine, Inputs: map[string]any{"mode": "size"}, State: restored}); err == nil {
		t.Fatal("a changed track policy kept its digest")
	}
}

// Snapshots and plans made before the selection existed hash exactly as they
// did, read back with no selection and keep every track. The digests were
// computed with the code of the commit before this feature.
func TestSnapshotsBeforeTrackSelectionKeepTheirDigestAndEveryTrack(t *testing.T) {
	budget, err := modeSizeBudgetInput("size", map[string]any{"size_budget_bytes": 1000000000.0})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		mode   string
		budget *SizeBudgetPolicy
		digest string
	}{
		"size":        {"size", nil, "sha256:4fd41a1c54c6224707e0639f01c01acc0d14673f8ff2bc260b6e3f2ad9ec0192"},
		"compact":     {"compact", nil, "sha256:c5393ccefe357b233249ce39261bf292a7fe093b9e1014f7110122733d5b35db"},
		"quality":     {"quality", nil, "sha256:5e39369ea4c3f2f0469a74e1f5b2e909495cad724da3e92e872dff37516adede"},
		"size budget": {"size", budget, "sha256:eb9335cee51de282049f16d1c5f6601319625474ed83ff90ca456f66306ac869"},
	} {
		t.Run(name, func(t *testing.T) {
			old, err := modePolicyWithBudget(tc.mode, false, 0, nil, false, tc.budget)
			if err != nil {
				t.Fatal(err)
			}
			if tc.mode == "compact" {
				// Frozen before Shrink took the size goal's audio levels.
				old.Profile.Audio.Mode = "compact"
				if err := digestModePolicy(&old); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mode == "size" && tc.budget == nil {
				// Frozen before level 1 became the default too.
				old = preLevelSizePolicy(t, old)
			}
			if tc.budget != nil {
				// And before the bed audio levels.
				old = objectCopySizePolicy(t, old)
			}
			if old.Digest != tc.digest || old.Tracks != nil {
				t.Fatalf("digest %s tracks %+v, want %s", old.Digest, old.Tracks, tc.digest)
			}
			stored := map[string]any{}
			raw, _ := json.Marshal(old)
			_ = json.Unmarshal(raw, &stored)
			if _, has := stored["tracks"]; has {
				t.Fatal("a snapshot without a selection serialises a tracks field")
			}
			// Frozen under the old digest, the snapshot is read as it is, with
			// the global default ignored: it keeps every track.
			ec := &ExecutionContext{Engine: NewEngine(EngineDeps{}), InstanceID: "old", Inputs: map[string]any{"mode": tc.mode}, State: map[string]any{"mode_policy": stored}}
			frozen, err := freezeModePolicy(ec)
			if err != nil || frozen.Digest != tc.digest || frozen.Tracks != nil {
				t.Fatalf("stored snapshot %+v %v", frozen, err)
			}
			rep := trackFixtureReport()
			sel, err := resolveTrackSelection(frozen, rep)
			if err != nil || sel != nil {
				t.Fatalf("an old snapshot selected %+v %v", sel, err)
			}
			kept, bytes := keptView(rep, sel, levelSourceBytes)
			if len(kept.Audio) != 8 || len(kept.Subtitles) != 32 || bytes != levelSourceBytes {
				t.Fatalf("an old snapshot lost tracks: %d audio %d subtitles %d bytes", len(kept.Audio), len(kept.Subtitles), bytes)
			}
		})
	}
	// A plan with no selection hashes as it did, and a v6 plan differs.
	plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8, Mode: "size", AudioMode: transcode.AudioModeSizeAAC, ModePolicyVersion: transcode.ModePolicyV5, SubtitleActions: []transcode.SubtitleAction{{TypeIndex: 0, SourceStreamIndex: 3}}}
	digest, err := transcode.DigestPlan(plan)
	if err != nil || digest != "sha256:f468bb5a54186270fe8794b7513e0d9bb5fc75f0f2a14bee6c511294c0a1ca0d" {
		t.Fatalf("plan digest %s %v", digest, err)
	}
	if err := applyTrackSelection(plan, &transcode.TrackSelection{KeepAudio: []int{1}, KeepSubtitles: []int{3}}); err != nil || plan.PlanDigest == digest || plan.ModePolicyVersion != transcode.ModePolicyV6 {
		t.Fatalf("v6 plan %+v %v", plan, err)
	}
}

func fixtureSnapshot(t *testing.T, inputs map[string]any) *ModePolicySnapshot {
	t.Helper()
	in := map[string]any{"mode": "size"}
	for k, v := range inputs {
		in[k] = v
	}
	p, err := freezeModePolicy(&ExecutionContext{Engine: NewEngine(EngineDeps{}), InstanceID: "fixture", Inputs: in, State: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveTrackSelectionOfTheFixture(t *testing.T) {
	rep := trackFixtureReport()
	snapshot := fixtureSnapshot(t, nil)
	sel, err := resolveTrackSelection(snapshot, rep)
	if err != nil || sel == nil {
		t.Fatalf("%+v %v", sel, err)
	}
	// eng, the two Spanish tracks and jpn; the six subtitles in eng, spa, jpn.
	if !slices.Equal(sel.KeepAudio, []int{1, 2, 3, 8}) || !slices.Equal(sel.KeepSubtitles, []int{9, 10, 11, 12, 13, 14}) {
		t.Fatalf("selection %+v", sel)
	}
	report := trackSelectionReport(snapshot.Tracks, sel, rep, levelSourceBytes)
	if report == nil || len(report.Audio.Kept) != 4 || len(report.Audio.Dropped) != 4 || len(report.Subtitles.Kept) != 6 || len(report.Subtitles.Dropped) != 26 {
		t.Fatalf("report %+v", report)
	}
	wantSaved := int64(4*144_000_000 + 26*4_000_000)
	if report.BytesSaved != wantSaved {
		t.Fatalf("bytes saved %d, want %d", report.BytesSaved, wantSaved)
	}
	if !strings.HasPrefix(report.Message, "Kept 4 of 8 audio tracks (eng, spa, spa, jpn) and 6 of 32 subtitles; dropping the rest saves about ") {
		t.Fatalf("message %q", report.Message)
	}
	dropped := report.Audio.Dropped[0]
	if dropped.Index != 4 || dropped.Language != "fre" || dropped.Title != "French" || dropped.Codec != "eac3" || dropped.Bytes != 144_000_000 {
		t.Fatalf("dropped entry %+v", dropped)
	}
	// The report round-trips through the stored state.
	raw, _ := json.Marshal(map[string]any{trackReportKey: report, trackSelectionKey: sel})
	var state map[string]any
	_ = json.Unmarshal(raw, &state)
	if got := trackReportOf(state); got == nil || got.Message != report.Message || got.BytesSaved != report.BytesSaved {
		t.Fatalf("stored report %+v", got)
	}
	if got := trackSelectionOf(state); !got.Equal(sel) {
		t.Fatalf("stored selection %+v", got)
	}
	result := map[string]any{}
	addTrackReport(result, state)
	if result["tracks_message"] != report.Message || result["tracks"] == nil {
		t.Fatalf("result %+v", result)
	}

	// Manual picks beat the tags: a mislabelled track is kept by index.
	manual := fixtureSnapshot(t, map[string]any{"keep_audio_streams": []any{4.0, 8.0}, "keep_subtitle_streams": []any{}})
	msel, err := resolveTrackSelection(manual, rep)
	if err != nil || !slices.Equal(msel.KeepAudio, []int{4, 8}) || len(msel.KeepSubtitles) != 0 {
		t.Fatalf("manual selection %+v %v", msel, err)
	}
	// ...and a stream that is not an audio stream of this file fails.
	wrong := fixtureSnapshot(t, map[string]any{"keep_audio_streams": []any{9.0}})
	if _, err := resolveTrackSelection(wrong, rep); err == nil {
		t.Fatal("a subtitle index was accepted as audio")
	}
	// The default track is reassigned when it is dropped.
	ka, _ := keptStreams(rep.Audio, rep.Subtitles, msel)
	if len(ka) != 2 || ka[0].Disposition["default"] != 1 || ka[0].Index != 4 {
		t.Fatalf("kept audio %+v", ka)
	}
}

func TestTrackSelectionMatchesTheFrozenPolicy(t *testing.T) {
	rep := trackFixtureReport()
	snapshot := fixtureSnapshot(t, nil)
	sel, _ := resolveTrackSelection(snapshot, rep)
	newState := func() map[string]any {
		return map[string]any{"source_report": rep, trackSelectionKey: sel}
	}
	newPlan := func() *transcode.Plan {
		plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Mode: "size", ModePolicyVersion: transcode.ModePolicyV5}
		for i := range rep.Subtitles {
			plan.SubtitleActions = append(plan.SubtitleActions, transcode.SubtitleAction{TypeIndex: i, SourceStreamIndex: rep.Subtitles[i].Index})
		}
		if err := applyTrackSelection(plan, sel); err != nil {
			t.Fatal(err)
		}
		return plan
	}
	plan := newPlan()
	if plan.ModePolicyVersion != transcode.ModePolicyV6 || len(plan.SubtitleActions) != 6 || plan.SubtitleActions[0].TypeIndex != 0 || plan.SubtitleActions[5].TypeIndex != 5 {
		t.Fatalf("plan %+v", plan)
	}
	if !trackSelectionMatches(snapshot, newState(), plan) {
		t.Fatal("the plan built from the selection does not match it")
	}
	for name, mutate := range map[string]func(*transcode.Plan, map[string]any){
		"another stream kept":     func(p *transcode.Plan, _ map[string]any) { p.Tracks.KeepAudio = []int{1, 2, 3, 7} },
		"a stream added":          func(p *transcode.Plan, _ map[string]any) { p.Tracks.KeepAudio = []int{1, 2, 3, 4, 8} },
		"no selection":            func(p *transcode.Plan, _ map[string]any) { p.Tracks = nil },
		"old plan version":        func(p *transcode.Plan, _ map[string]any) { p.ModePolicyVersion = transcode.ModePolicyV5 },
		"a dropped subtitle kept": func(p *transcode.Plan, _ map[string]any) { p.SubtitleActions = p.SubtitleActions[:5] },
		"a foreign subtitle":      func(p *transcode.Plan, _ map[string]any) { p.SubtitleActions[0].SourceStreamIndex = 30 },
		"state disagrees": func(_ *transcode.Plan, s map[string]any) {
			s[trackSelectionKey] = &transcode.TrackSelection{KeepAudio: []int{1}, KeepSubtitles: []int{}}
		},
		"state lost": func(_ *transcode.Plan, s map[string]any) { delete(s, trackSelectionKey) },
	} {
		t.Run(name, func(t *testing.T) {
			p, s := newPlan(), newState()
			mutate(p, s)
			if trackSelectionMatches(snapshot, s, p) {
				t.Fatal("a plan that is not the frozen selection matched")
			}
		})
	}
	// A snapshot with no selection requires a plan with none.
	old := fixtureSnapshot(t, map[string]any{"keep_languages": []any{}})
	if old.Tracks != nil {
		t.Fatal("expected no policy")
	}
	if !trackSelectionMatches(old, map[string]any{"source_report": rep}, &transcode.Plan{ModePolicyVersion: transcode.ModePolicyV5}) {
		t.Fatal("a plan without a selection must match a snapshot without one")
	}
	if trackSelectionMatches(old, map[string]any{"source_report": rep, trackSelectionKey: sel}, plan) {
		t.Fatal("a v6 plan matched a snapshot that selects nothing")
	}
}

// The owner's example. Eight E-AC-3 640 kbps 5.1 tracks, a 1.0 GB budget and a
// 1080p23.976 episode of 1800 s, keeping spa, eng and jpn: the audio levels are
// chosen on the four tracks that stay, so the picture gets more of the budget
// than the same file with all eight tracks (which needs HE-AAC and still leaves
// less).
func TestSizeBudgetCountsOnlyTheKeptTracks(t *testing.T) {
	rep := trackFixtureReport()
	snapshot := fixtureSnapshot(t, nil)
	sel, err := resolveTrackSelection(snapshot, rep)
	if err != nil || sel == nil || len(sel.KeepAudio) != 4 {
		t.Fatalf("selection %+v %v", sel, err)
	}
	policy := levelPolicy(t, 1_000_000_000)
	good := policy.Audio.goodVideoKbps(rep, levelSourceBytes)

	all := planSizeBudgetEncode(policy, rep, levelSourceBytes, 1_000_000_000)
	keptRep, keptBytes := keptView(rep, sel, levelSourceBytes)
	if len(keptRep.Audio) != 4 || len(keptRep.Subtitles) != 6 || keptBytes != levelSourceBytes-int64(4*144_000_000+26*4_000_000) {
		t.Fatalf("kept view %d audio %d subtitles %d bytes", len(keptRep.Audio), len(keptRep.Subtitles), keptBytes)
	}
	kept := planSizeBudgetEncode(policy, keptRep, keptBytes, 1_000_000_000)
	t.Logf("all 8 tracks: %s at %d kbps video (good %d kbps); kept 4 tracks: %s at %d kbps video, audio %d tracks %d bytes non-video", all.AudioMode, all.X265.BitrateKbps, good, kept.AudioMode, kept.X265.BitrateKbps, len(kept.Audio.Tracks), kept.NonVideoBytes)
	if all.Reason != "" || all.AudioMode != transcode.AudioModeSizeHEAACBed {
		t.Fatalf("the eight tracks plan %+v", all)
	}
	if kept.Reason != "" || kept.AudioMode != transcode.AudioModeSizeAACBed || kept.Audio == nil || kept.Audio.Level != 1 || len(kept.Audio.Tracks) != 4 {
		t.Fatalf("the four tracks plan %+v", kept)
	}
	if kept.X265.BitrateKbps < good || kept.X265.BitrateKbps <= all.X265.BitrateKbps {
		t.Fatalf("keeping four tracks must leave a good picture: %d kbps (good %d, all tracks %d)", kept.X265.BitrateKbps, good, all.X265.BitrateKbps)
	}
	for i, tr := range kept.Audio.Tracks {
		if tr.Index != i || tr.Output != "aac" || tr.Kbps != 384 {
			t.Fatalf("kept track %d: %+v", i, tr)
		}
	}
	// 4 tracks at 384 kbps over 1800 s, 6 subtitles of 4 MB and the container.
	want := int64(4*384_000/8*1800) + 6*4_000_000
	if kept.NonVideoBytes < want || kept.NonVideoBytes > want+60_000_000 {
		t.Fatalf("non-video bytes %d, want about %d", kept.NonVideoBytes, want)
	}
}

// Through the plan step: the plan keeps policy v6 and the selection, the level
// is the one chosen on the kept tracks, and the plan rebuilds from the frozen
// policy and the stored selection.
func TestSizeBudgetPlanStepUsesTheSelection(t *testing.T) {
	c := newRecipeCase(t, map[string]any{"size_budget_bytes": 1_000_000_000.0, "allow_hardware": false}, nil)
	if c.snapshot.Tracks == nil {
		t.Fatal("the case froze no track policy")
	}
	rep := trackFixtureReport()
	c.ec.State["source_report"], c.ec.State["original_size"] = rep, levelSourceBytes
	sel, err := resolveTrackSelection(c.snapshot, rep)
	if err != nil || sel == nil {
		t.Fatal(sel, err)
	}
	c.ec.State[trackSelectionKey] = sel
	base := getPlan(c.ec.State["plan"])
	for i, s := range rep.Subtitles {
		base.SubtitleActions = append(base.SubtitleActions, transcode.SubtitleAction{TypeIndex: i, SourceStreamIndex: s.Index})
	}
	if err := applyTrackSelection(base, sel); err != nil {
		t.Fatal(err)
	}
	planRecipe(t, c)
	plan := getPlan(c.ec.State["plan"])
	rec := sizeBudgetOf(c.ec.State)
	if plan == nil || rec == nil || rec.Outcome == SizeBudgetExceeded || getBool(c.ec.State, "skip_transcode") {
		t.Fatalf("not planned: %+v skip=%v reason=%v note=%v", rec, c.ec.State["skip_transcode"], c.ec.State["reason_code"], c.ec.State["plan_note"])
	}
	if plan.ModePolicyVersion != transcode.ModePolicyV6 || !plan.Tracks.Equal(sel) || plan.AudioMode != transcode.AudioModeSizeAACBed || rec.Audio == nil || len(rec.Audio.Tracks) != 4 {
		t.Fatalf("plan %s %s tracks %+v record %+v", plan.ModePolicyVersion, plan.AudioMode, plan.Tracks, rec.Audio)
	}
	if !trackSelectionMatches(c.snapshot, c.ec.State, plan) || !sizeBudgetPlanMatches(c.snapshot, c.ec.State, plan, rec) {
		t.Fatal("the planned encode does not rebuild from the frozen policy")
	}
	// A record planned for the other eight tracks is not this plan's.
	delete(c.ec.State, trackSelectionKey)
	if sizeBudgetPlanMatches(c.snapshot, c.ec.State, plan, rec) {
		t.Fatal("the plan matched without the selection it was planned on")
	}
}

// The size-budget flow end to end at the action level: the frozen policy
// selects the tracks as preflight does and stores the report, the plan step
// plans the encode on them, and the validation step of a finished candidate
// puts the report among the outputs the operation shows. Readers of an
// operation look a key up in its outputs and state, never inside the result, so
// the report must be there under its own keys, and in the result too.
func TestSizeBudgetFlowReportsTheTrackSelection(t *testing.T) {
	c := newRecipeCase(t, map[string]any{"size_budget_bytes": 1_000_000_000.0, "allow_hardware": false}, nil)
	rep := trackFixtureReport()
	c.ec.State["source_report"], c.ec.State["original_size"] = rep, int64(levelSourceBytes)
	c.ec.State["original"] = map[string]any{"size_bytes": int64(levelSourceBytes)}
	sel, err := resolveTrackSelection(c.snapshot, rep)
	if err != nil || sel == nil {
		t.Fatal(sel, err)
	}
	// What preflight stores for the job.
	c.ec.State[trackSelectionKey] = sel
	c.ec.State[trackReportKey] = trackSelectionReport(c.snapshot.Tracks, sel, rep, int64(levelSourceBytes))
	base := getPlan(c.ec.State["plan"])
	for i, s := range rep.Subtitles {
		base.SubtitleActions = append(base.SubtitleActions, transcode.SubtitleAction{TypeIndex: i, SourceStreamIndex: s.Index})
	}
	if err := applyTrackSelection(base, sel); err != nil {
		t.Fatal(err)
	}
	planRecipe(t, c)
	plan := getPlan(c.ec.State["plan"])
	if plan == nil || getBool(c.ec.State, "skip_transcode") || plan.Tracks == nil {
		t.Fatalf("not planned on the selection: skip=%v reason=%v", c.ec.State["skip_transcode"], c.ec.State["reason_code"])
	}

	// The worker finished: a candidate inside the budget with passing evidence.
	candidate := filepath.Join(t.TempDir(), "candidate.mkv")
	if err := os.WriteFile(candidate, nil, 0o600); err != nil || os.Truncate(candidate, 900_000_000) != nil {
		t.Fatal(err)
	}
	c.ec.State["candidate_path"], c.ec.State["candidate_sha256"], c.ec.State["candidate_size_bytes"] = candidate, "candidate", int64(900_000_000)
	c.ec.State["quality_evidence"] = map[string]any{"verdict": "pass", "candidate_sha256": "candidate", "plan_digest": plan.PlanDigest, "benchmark_request_digest": plan.QualityValidation.BenchmarkRequestDigest,
		"vmaf": map[string]any{"mean": 91.0, "p5": 86.0, "worst_window_mean": 87.0, "min": 50.0}, "cambi": map[string]any{"mean": 2.0, "max": 9.0}}
	res, err := c.engine.stepTranscodeValidate(context.Background(), c.ec)
	if err != nil || res.Status != StepCompleted || getBool(c.ec.State, "skip_transcode") {
		t.Fatalf("validate %+v %v state reason %v", res, err, c.ec.State["reason_code"])
	}
	want := trackReportOf(c.ec.State)
	if want == nil || want.Message == "" {
		t.Fatalf("the job stored no report: %+v", c.ec.State[trackReportKey])
	}
	// The way the engine keeps a step's outputs, then the way an operation is read.
	mergeMap(c.ec.State, res.Outputs)
	var got TrackSelectionReport
	if !reencode(res.Outputs["tracks"], &got) || got.Message != want.Message || len(got.Audio.Kept) != 4 || len(got.Audio.Dropped) != 4 {
		t.Fatalf("outputs[tracks] = %+v, want the report %+v", res.Outputs["tracks"], want)
	}
	if res.Outputs["tracks_message"] != want.Message || c.ec.State["tracks_message"] != want.Message {
		t.Fatalf("tracks_message %v / %v, want %q", res.Outputs["tracks_message"], c.ec.State["tracks_message"], want.Message)
	}
	inResult, _ := res.Outputs["result"].(map[string]any)
	if inResult["tracks"] == nil || inResult["tracks_message"] != want.Message {
		t.Fatalf("the result lacks the report: %+v", inResult)
	}
	// A goal that keeps every track has no report to show.
	plain := map[string]any{}
	addTrackReport(plain, map[string]any{})
	if len(plain) != 0 {
		t.Fatalf("a report was made up: %+v", plain)
	}
}

// A source without audio can still lose subtitles. The selection keeps no
// audio, the budget is planned from the video and the kept subtitles, the plan
// is v6 and matches the frozen policy: the job goes on to the worker, which
// accepts such a plan only for a source that has no audio.
func TestSelectionOfASourceWithoutAudio(t *testing.T) {
	c := newRecipeCase(t, map[string]any{"size_budget_bytes": 1_500_000_000.0, "allow_hardware": false}, nil)
	rep := trackFixtureReport()
	// The fixture's eight audio tracks take 144 MB each; the silent file is
	// the same file without them.
	rep.Audio = nil
	silent := int64(levelSourceBytes - 8*144_000_000)
	c.ec.State["source_report"], c.ec.State["original_size"] = rep, silent
	sel, err := resolveTrackSelection(c.snapshot, rep)
	if err != nil || sel == nil || len(sel.KeepAudio) != 0 || len(sel.KeepSubtitles) == 0 || len(sel.KeepSubtitles) >= len(rep.Subtitles) {
		t.Fatalf("selection %+v: %v", sel, err)
	}
	c.ec.State[trackSelectionKey] = sel
	base := getPlan(c.ec.State["plan"])
	for i, s := range rep.Subtitles {
		base.SubtitleActions = append(base.SubtitleActions, transcode.SubtitleAction{TypeIndex: i, SourceStreamIndex: s.Index})
	}
	if err := applyTrackSelection(base, sel); err != nil {
		t.Fatal(err)
	}
	planRecipe(t, c)
	plan := getPlan(c.ec.State["plan"])
	rec := sizeBudgetOf(c.ec.State)
	if plan == nil || rec == nil || getBool(c.ec.State, "skip_transcode") {
		t.Fatalf("not planned: %+v skip=%v reason=%v note=%v", rec, c.ec.State["skip_transcode"], c.ec.State["reason_code"], c.ec.State["plan_note"])
	}
	if plan.ModePolicyVersion != transcode.ModePolicyV6 || plan.Tracks == nil || len(plan.Tracks.KeepAudio) != 0 || !trackSelectionMatches(c.snapshot, c.ec.State, plan) {
		t.Fatalf("plan %s tracks %+v", plan.ModePolicyVersion, plan.Tracks)
	}
	if report := trackSelectionReport(c.snapshot.Tracks, sel, rep, silent); report.Audio.Total != 0 || len(report.Audio.Kept) != 0 || report.Message == "" {
		t.Fatalf("report %+v", report)
	}
}

// A plan that keeps only some tracks needs a worker that follows the selection:
// the submit step, the size-budget ladder and the final-validation request all
// refuse an old one, and none reach it.
func TestTrackSelectionNeedsAWorkerThatFollowsIt(t *testing.T) {
	for _, capable := range []bool{false, true} {
		mock := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
			return transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, SizeAudioLevels: true, SizeAudioObjectBed: true, TrackSelection: capable}, nil
		}}
		e := NewEngine(EngineDeps{Transcode: mock})
		ec := testReconcileEC("job-tracks")
		plan := testReconcilePlan()
		plan.Mode, plan.ModePolicyVersion, plan.AudioMode = "size", transcode.ModePolicyV6, transcode.AudioModeSizeAAC
		plan.Tracks = &transcode.TrackSelection{KeepAudio: []int{1}, KeepSubtitles: []int{}}
		ec.State["plan"] = plan
		res, err := e.stepTranscodeSubmit(context.Background(), ec)
		if err != nil {
			t.Fatal(err)
		}
		refused := res.Status == StepFailed && strings.Contains(res.Error, "audio and subtitle tracks")
		if refused == capable {
			t.Fatalf("capable=%v: status %s error %q", capable, res.Status, res.Error)
		}
		if !capable && mock.submitCalls != 0 {
			t.Fatalf("an old worker was sent %d jobs", mock.submitCalls)
		}
	}

	// The ladder declines a budget it would have to plan with a selection.
	c := newRecipeCase(t, map[string]any{"size_budget_bytes": 1_000_000_000.0, "allow_hardware": false}, func(caps *transcode.WorkerCapabilities) { caps.TrackSelection = false })
	rep := trackFixtureReport()
	c.ec.State["source_report"], c.ec.State["original_size"] = rep, levelSourceBytes
	sel, _ := resolveTrackSelection(c.snapshot, rep)
	c.ec.State[trackSelectionKey] = sel
	res, err := c.engine.stepPlanEncode(context.Background(), c.ec)
	if err != nil || res.Status != StepCompleted || !getBool(c.ec.State, "skip_transcode") {
		t.Fatalf("an old worker was given the plan: %+v %v", res, err)
	}
	if reason := getString(c.ec.State, "reason_code"); reason != "size_budget_worker_unsupported" {
		t.Fatalf("reason %q", reason)
	}
	if c.executor.submitCalls != 0 {
		t.Fatalf("submitted %d jobs", c.executor.submitCalls)
	}

	// The final-validation request for a v6 plan needs the capability too.
	ec := &ExecutionContext{InstanceID: "tracks", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
	policy, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	ec.State["plan"] = &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8, Mode: "size", AudioMode: transcode.AudioModeSizeAAC, ModePolicyVersion: transcode.ModePolicyV6, Tracks: &transcode.TrackSelection{KeepAudio: []int{1}, KeepSubtitles: []int{}}}
	vrep := &mediainspect.DetailedReport{DurationSec: 600, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 24, BitRate: 5000000}}}
	caps := modeTestCapabilities()
	caps.TrackSelection = false
	if _, err := finalRequestForPlan(ec, "/media/source.mkv", vrep, policy.Profile.Optimization, caps); err == nil || !strings.Contains(err.Error(), "audio and subtitle tracks") {
		t.Fatalf("an old worker was given the final validation: %v", err)
	}
	caps.TrackSelection = true
	if _, err := finalRequestForPlan(ec, "/media/source.mkv", vrep, policy.Profile.Optimization, caps); err != nil && strings.Contains(err.Error(), "audio and subtitle tracks") {
		t.Fatalf("a worker with the capability was refused: %v", err)
	}
}

// A plan without a selection never needs the capability: old snapshots keep
// working against an old worker.
func TestPlansWithoutSelectionNeedNoCapability(t *testing.T) {
	mock := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		return transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, SizeAudioLevels: true, SizeAudioObjectBed: true}, nil
	}}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-old")
	plan := testReconcilePlan()
	plan.Mode, plan.ModePolicyVersion, plan.AudioMode = "size", transcode.ModePolicyV5, transcode.AudioModeSizeAAC
	ec.State["plan"] = plan
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Error, "audio and subtitle tracks") {
		t.Fatalf("an old plan was refused for a capability it does not need: %q", res.Error)
	}
}

func TestTrackReportMessages(t *testing.T) {
	for name, tc := range map[string]struct {
		report TrackSelectionReport
		want   string
	}{
		"subtitles only": {
			TrackSelectionReport{
				Audio:     TrackSplit{Total: 2, Kept: []TrackEntry{{Language: "eng"}, {Language: "spa"}}},
				Subtitles: TrackSplit{Total: 9, Kept: []TrackEntry{{}, {}}, Dropped: make([]TrackEntry, 7)},
			}, "Kept all 2 audio tracks and 2 of 9 subtitles"},
		"audio only": {
			TrackSelectionReport{
				Audio:     TrackSplit{Total: 3, Kept: []TrackEntry{{Language: "jpn"}}, Dropped: make([]TrackEntry, 2)},
				Subtitles: TrackSplit{Total: 0},
			}, "Kept 1 of 3 audio tracks (jpn)"},
		"saved": {
			TrackSelectionReport{
				Audio:      TrackSplit{Total: 2, Kept: []TrackEntry{{Language: "eng"}}, Dropped: make([]TrackEntry, 1)},
				Subtitles:  TrackSplit{Total: 1, Kept: []TrackEntry{{}}},
				BytesSaved: 144_000_000,
			}, "Kept 1 of 2 audio tracks (eng) and the only subtitle; dropping the rest saves about "},
	} {
		t.Run(name, func(t *testing.T) {
			if got := trackSelectionMessage(&tc.report); !strings.HasPrefix(got, tc.want) {
				t.Fatalf("%q, want prefix %q", got, tc.want)
			}
		})
	}
}
