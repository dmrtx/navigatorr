package transcodeworker

import (
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func resign(p *transcode.Plan) *transcode.Plan {
	p.PlanDigest = ""
	p.PlanDigest, _ = transcode.DigestPlan(p)
	return p
}

// Plan version 8 is a compact plan at the size goal's audio levels, with or
// without a track selection; compact's own audio stays v2's (or v6's).
func TestCompactV8PlanCarriesTheAudioLevels(t *testing.T) {
	v8 := func(t *testing.T, audio string) *transcode.Plan {
		p := compactModePlan(t, compactSDRPlan())
		p.ModePolicyVersion, p.AudioMode = transcode.ModePolicyV8, audio
		return resign(p)
	}
	for _, mode := range transcode.AudioLevelModes {
		if err := ValidatePlan(v8(t, mode)); err != nil {
			t.Fatalf("v8 compact plan at %s rejected: %v", mode, err)
		}
	}
	withTracks := v8(t, transcode.AudioModeSizeAACBed)
	withTracks.Tracks = &transcode.TrackSelection{KeepAudio: []int{1}}
	if err := ValidatePlan(resign(withTracks)); err != nil {
		t.Fatalf("v8 compact plan with tracks rejected: %v", err)
	}
	for name, mutate := range map[string]func(*transcode.Plan){
		"v8 with compact audio":   func(p *transcode.Plan) { p.AudioMode = "compact" },
		"v8 copying audio":        func(p *transcode.Plan) { p.AudioMode = "copy" },
		"v8 on a size plan":       func(p *transcode.Plan) { p.Mode = "size" },
		"v8 with a size minimum":  func(p *transcode.Plan) { p.SizePolicy.MinSavingsPercent = 15 },
		"v2 compact with a level": func(p *transcode.Plan) { p.ModePolicyVersion = transcode.ModePolicyV2 },
		"v6 compact with a level": func(p *transcode.Plan) {
			p.ModePolicyVersion = transcode.ModePolicyV6
			p.Tracks = &transcode.TrackSelection{KeepAudio: []int{1}}
		},
		"v9 on a compact plan": func(p *transcode.Plan) { p.ModePolicyVersion = transcode.ModePolicyV9 },
		"quality goal with v8": func(p *transcode.Plan) { p.Mode = "quality" },
	} {
		p := v8(t, transcode.AudioModeSizeAACBed)
		mutate(p)
		if err := ValidatePlan(resign(p)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Plan version 9 is a size plan at the audio levels whose minimum saving is
// SizeSmallerSavingPercent, with or without a track selection.
func TestSizeV9PlanKeepsASmallerSaving(t *testing.T) {
	v9 := func() *transcode.Plan {
		p := sizeV5TestPlan(transcode.AudioModeSizeAACBed)
		p.ModePolicyVersion = transcode.ModePolicyV9
		p.SizePolicy.MinSavingsPercent = transcode.SizeSmallerSavingPercent
		return resign(p)
	}
	if err := ValidatePlan(v9()); err != nil {
		t.Fatalf("v9 size plan rejected: %v", err)
	}
	withTracks := v9()
	withTracks.Tracks = &transcode.TrackSelection{KeepAudio: []int{1}}
	if err := ValidatePlan(resign(withTracks)); err != nil {
		t.Fatalf("v9 size plan with tracks rejected: %v", err)
	}
	for name, mutate := range map[string]func(*transcode.Plan){
		"v9 at the plain minimum": func(p *transcode.Plan) { p.SizePolicy.MinSavingsPercent = 15 },
		"v5 at the smaller one":   func(p *transcode.Plan) { p.ModePolicyVersion = transcode.ModePolicyV5 },
		"v9 compacting lossless":  func(p *transcode.Plan) { p.AudioMode = transcode.AudioModeCompactLossless },
		"v9 on a compact plan":    func(p *transcode.Plan) { p.Mode = "compact" },
	} {
		p := v9()
		mutate(p)
		if err := ValidatePlan(resign(p)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// A v8 compact plan converts each audio track exactly as a size plan at the
// same level does.
func TestCompactV8PlansAudioLikeTheSizeGoal(t *testing.T) {
	audioOf := func(p *transcode.Plan) []StreamAction {
		ep, err := BuildExecutionPlan(p, levelSource(), 1800)
		if err != nil {
			t.Fatal(err)
		}
		var audio []StreamAction
		for _, s := range ep.Streams {
			if s.Kind == "audio" {
				audio = append(audio, s)
			}
		}
		return audio
	}
	compact := compactModePlanFor(t, compactSDRPlan(), 8)
	compact.ModePolicyVersion, compact.AudioMode = transcode.ModePolicyV8, transcode.AudioModeSizeAACBed
	got, want := audioOf(resign(compact)), audioOf(sizeV5TestPlan(transcode.AudioModeSizeAACBed))
	if len(got) != len(want) || len(got) == 0 {
		t.Fatalf("compact audio %+v, size audio %+v", got, want)
	}
	for i := range want {
		if got[i].TargetCodec != want[i].TargetCodec || got[i].BitrateKbps != want[i].BitrateKbps || got[i].HEAAC != want[i].HEAAC {
			t.Fatalf("track %d: compact %+v, size %+v", i, got[i], want[i])
		}
	}
}
