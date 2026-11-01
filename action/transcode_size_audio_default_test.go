package action

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// preLevelSizePolicy is a size snapshot as it was frozen before level 1 became
// the default of a size goal without a budget: compact_lossless audio.
func preLevelSizePolicy(t *testing.T, p ModePolicySnapshot) ModePolicySnapshot {
	t.Helper()
	p.Profile.Audio.Mode = transcode.AudioModeCompactLossless
	if err := digestModePolicy(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

// objectCopySizePolicy is a size snapshot as it was frozen before the bed audio
// levels: level 1 of the pair that copies lossy Atmos, and that pair for a
// budget.
func objectCopySizePolicy(t *testing.T, p ModePolicySnapshot) ModePolicySnapshot {
	t.Helper()
	if p.Profile.Audio.Mode == transcode.AudioModeSizeAACBed {
		p.Profile.Audio.Mode = transcode.AudioModeSizeAAC
	}
	if p.Size != nil && p.Size.SizeBudget != nil && p.Size.SizeBudget.Audio != nil {
		size, budget, audio := *p.Size, *p.Size.SizeBudget, *p.Size.SizeBudget.Audio
		audio.Modes = slices.Clone(transcode.ObjectCopyAudioLevelModes)
		budget.Audio, size.SizeBudget, p.Size = &audio, &budget, &size
	}
	if err := digestModePolicy(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

// sizeLevelTestReport has the surround tracks a size goal without a budget used
// to copy: DTS 5.1 at 1536 kbps and AC-3 5.1 at 640 kbps, both well above the
// AAC-LC 5.1 target of 384, and AC-3 5.1 at 448, under 1.25 times it.
func sizeLevelTestReport() *mediainspect.DetailedReport {
	return &mediainspect.DetailedReport{DurationSec: 6000,
		Video: []mediainspect.DetailedStream{{Index: 0, Codec: "h264", Width: 1920, Height: 1080, FPS: 23.976, BitDepth: 8}},
		Audio: []mediainspect.DetailedStream{
			{Index: 1, Codec: "dts", Profile: "DTS", Channels: 6, ChannelLayout: "5.1(side)", BitRate: 1_536_000, Language: "eng"},
			{Index: 2, Codec: "ac3", Channels: 6, ChannelLayout: "5.1(side)", BitRate: 640_000, Language: "spa"},
			{Index: 3, Codec: "ac3", Channels: 6, ChannelLayout: "5.1(side)", BitRate: 448_000, Language: "jpn"},
		}}
}

// A new size goal without a budget freezes level 1 and plans under v5; a budget
// still freezes its own levels and starts from the first.
func TestNewSizeGoalFreezesAudioLevelOne(t *testing.T) {
	for name, inputs := range map[string]map[string]any{
		"media engine": {"mode": "size"},
		"x265 only":    {"mode": "size", "allow_hardware": false},
		"slow preset":  {"mode": "size", "allow_hardware": false, "allow_slow_preset": true},
		"track choice": {"mode": "size", "keep_languages": []any{"eng"}},
		"size budget":  {"mode": "size", "size_budget_percent": 40.0},
		"budget, x265": {"mode": "size", "size_budget_percent": 40.0, "allow_hardware": false},
		"byte budget":  {"mode": "size", "size_budget_bytes": 2_000_000_000.0},
	} {
		t.Run(name, func(t *testing.T) {
			p := freezeFresh(t, inputs)
			if p.Profile.Audio.Mode != transcode.AudioModeSizeAACBed || p.workerVersion() != transcode.ModePolicyV5 {
				t.Fatalf("audio %q worker version %s", p.Profile.Audio.Mode, p.workerVersion())
			}
			if b := p.Size.SizeBudget; b != nil && (b.Audio == nil || b.Audio.Modes[0] != p.Profile.Audio.Mode) {
				t.Fatalf("budget audio %+v", b.Audio)
			}
			plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 22, Preset: "medium", AudioMode: p.Profile.Audio.Mode}
			if err := applyModePlan(plan, p); err != nil || plan.ModePolicyVersion != transcode.ModePolicyV5 {
				t.Fatalf("plan version %s %v", plan.ModePolicyVersion, err)
			}
		})
	}
	// The other goals keep their audio; Shrink converts at the size goal's level 1.
	if p := freezeFresh(t, map[string]any{"mode": "compact"}); p.Profile.Audio.Mode != transcode.AudioModeSizeAACBed || p.workerVersion() != transcode.ModePolicyV8 {
		t.Errorf("compact: audio %q version %s", p.Profile.Audio.Mode, p.workerVersion())
	}
	for mode, audio := range map[string]string{"quality": "copy", "x265_preserve": "copy"} {
		if p := freezeFresh(t, map[string]any{"mode": mode}); p.Profile.Audio.Mode != audio || p.workerVersion() != ModePolicyVersion {
			t.Errorf("%s: audio %q version %s", mode, p.Profile.Audio.Mode, p.workerVersion())
		}
	}
}

// Pinned: a new size goal without a budget differs from one frozen before
// level 1 became its default only in the audio mode, and each keeps its digest.
func TestSizeGoalAudioLevelDigests(t *testing.T) {
	p, err := modePolicyWithBudget("size", false, 0, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Digest != "sha256:c12b3cef75a61457581478f248e774e906163c9a2a27140920d07d56a52c6999" {
		t.Fatalf("level 1 size snapshot digest changed: %s", p.Digest)
	}
	// Goals frozen at level 1 before the bed pair carry, and re-hash to, this.
	if old := objectCopySizePolicy(t, p); old.Digest != "sha256:805faf3a437219494d0ad06caacf5ed8e0a7e7ce5c128aec1a6d5ee774b41b89" {
		t.Fatalf("object-copy level 1 size snapshot digest changed: %s", old.Digest)
	}
	// The digest snapshots frozen before carry (and still re-hash to): see
	// TestSnapshotsBeforeTrackSelectionKeepTheirDigestAndEveryTrack.
	if old := preLevelSizePolicy(t, p); old.Digest != "sha256:4fd41a1c54c6224707e0639f01c01acc0d14673f8ff2bc260b6e3f2ad9ec0192" {
		t.Fatalf("pre-level size snapshot digest changed: %s", old.Digest)
	}
}

// Level 1 converts the lossy surround tracks well above the AAC-LC target and
// copies the one under 1.25 times it; a snapshot frozen with compact_lossless
// still copies every lossy track, plans under v3 and reads back unchanged.
func TestSizeGoalAudioLevelOnePlansLossySurround(t *testing.T) {
	fresh, err := modePolicyWithBudget("size", false, 0, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := preLevelSizePolicy(t, fresh)
	stored := map[string]any{}
	raw, _ := json.Marshal(old)
	_ = json.Unmarshal(raw, &stored)
	ec := &ExecutionContext{InstanceID: "pre-level", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"mode_policy": stored}}
	frozen, err := freezeModePolicy(ec)
	if err != nil || frozen.Digest != old.Digest || frozen.Profile.Audio.Mode != transcode.AudioModeCompactLossless || frozen.workerVersion() != transcode.ModePolicyV3 {
		t.Fatalf("frozen pre-level snapshot %+v %v", frozen, err)
	}
	type want struct {
		output string
		kbps   int
	}
	rep := sizeLevelTestReport()
	for name, tc := range map[string]struct {
		snapshot *ModePolicySnapshot
		version  string
		tracks   []want
	}{
		"new goal":         {&fresh, transcode.ModePolicyV5, []want{{"aac", 384}, {"aac", 384}, {"copy", 0}}},
		"frozen before it": {frozen, transcode.ModePolicyV3, []want{{"copy", 0}, {"copy", 0}, {"copy", 0}}},
	} {
		t.Run(name, func(t *testing.T) {
			plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 22, Preset: "medium", AudioMode: tc.snapshot.Profile.Audio.Mode}
			if err := applyModePlan(plan, tc.snapshot); err != nil || plan.ModePolicyVersion != tc.version {
				t.Fatalf("plan version %s %v", plan.ModePolicyVersion, err)
			}
			// The same record a budget freezes, read for the plan's mode: what
			// the worker builds and the planned audio show.
			rec := sizeBudgetAudioRecord(rep, plan.AudioMode, 1, 0, 0)
			for i, w := range tc.tracks {
				if got := rec.Tracks[i]; got.Output != w.output || got.Kbps != w.kbps {
					t.Errorf("track %d: %+v, want %+v", i, got, w)
				}
			}
			// An audio-only plan must carry the snapshot's own audio mode.
			if !sizeAudioOnlyLevelMatches(tc.snapshot, ec.State, plan) {
				t.Error("the snapshot's own audio mode was refused")
			}
			other := *plan
			other.AudioMode = transcode.AudioModeCompactLossless
			if plan.AudioMode == other.AudioMode {
				other.AudioMode = transcode.AudioModeSizeAAC
			}
			if sizeAudioOnlyLevelMatches(tc.snapshot, ec.State, &other) {
				t.Errorf("an audio-only plan at %s was accepted for a snapshot that froze %s", other.AudioMode, plan.AudioMode)
			}
		})
	}
}

// The quality ladder plans every rung with the frozen level and holds each
// rung to it.
func TestQualityLadderCarriesTheFrozenAudioLevel(t *testing.T) {
	c := newRecipeCase(t, map[string]any{"allow_hardware": false}, nil)
	planRecipe(t, c)
	plan := getPlan(c.ec.State["plan"])
	rec := QualityLadderOf(c.ec.State)
	if plan.AudioMode != transcode.AudioModeSizeAACBed || plan.ModePolicyVersion != transcode.ModePolicyV5 || !qualityLadderPlanMatches(c.snapshot, c.ec.State, plan, rec) {
		t.Fatalf("first rung audio %q version %s", plan.AudioMode, plan.ModePolicyVersion)
	}
	for _, mode := range []string{transcode.AudioModeCompactLossless, transcode.AudioModeSizeAAC, transcode.AudioModeSizeHEAACBed, "copy"} {
		other := *plan
		other.AudioMode = mode
		if qualityLadderPlanMatches(c.snapshot, c.ec.State, &other, rec) {
			t.Errorf("a rung at %s matched a goal that froze %s", mode, plan.AudioMode)
		}
	}
}
