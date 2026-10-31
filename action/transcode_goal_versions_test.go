package action

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func goalPlan(t *testing.T, p *ModePolicySnapshot) *transcode.Plan {
	t.Helper()
	plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8, AudioMode: p.Profile.Audio.Mode}
	if err := applyModePlan(plan, p); err != nil {
		t.Fatal(err)
	}
	return plan
}

// A new Shrink goal converts its audio at the size goal's level 1, so its
// plans are policy v8, with or without a track selection; one frozen before
// keeps compact audio and v2.
func TestShrinkFreezesAudioLevelsAsPolicyV8(t *testing.T) {
	p, err := modePolicyWithTracks("compact", false, 0, nil, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Profile.Audio.Mode != transcode.AudioModeSizeAACBed || p.workerVersion() != transcode.ModePolicyV8 || p.MinSavingsPercent != transcode.CompactMinimumSavingsPercent {
		t.Fatalf("compact snapshot audio %q version %s minimum %v", p.Profile.Audio.Mode, p.workerVersion(), p.MinSavingsPercent)
	}
	plan := goalPlan(t, &p)
	if plan.ModePolicyVersion != transcode.ModePolicyV8 || plan.SizePolicy.MinSavingsPercent != transcode.CompactMinimumSavingsPercent {
		t.Fatalf("compact plan %+v", plan)
	}
	if err := applyTrackSelection(plan, &transcode.TrackSelection{KeepAudio: []int{1}}); err != nil || plan.ModePolicyVersion != transcode.ModePolicyV8 {
		t.Fatalf("compact plan with tracks %s %v", plan.ModePolicyVersion, err)
	}

	old := p
	old.Profile.Audio.Mode = "compact"
	if old.workerVersion() != ModePolicyVersion {
		t.Fatalf("compact audio snapshot is %s", old.workerVersion())
	}
	oldPlan := goalPlan(t, &old)
	if err := applyTrackSelection(oldPlan, &transcode.TrackSelection{KeepAudio: []int{1}}); err != nil || oldPlan.ModePolicyVersion != transcode.ModePolicyV6 {
		t.Fatalf("compact audio plan with tracks %s %v", oldPlan.ModePolicyVersion, err)
	}
}

// accept_smaller_saving freezes a size goal at a 5% minimum, policy v9; a
// plain size goal's snapshot has no trace of it.
func TestSizeGoalCanAcceptASmallerSaving(t *testing.T) {
	ec := &ExecutionContext{Inputs: map[string]any{"mode": "size", "accept_smaller_saving": true}, State: map[string]any{}}
	p, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	if p.MinSavingsPercent != transcode.SizeSmallerSavingPercent || p.Size == nil || !p.Size.SmallerSaving || p.workerVersion() != transcode.ModePolicyV9 || !transcode.IsAudioLevelMode(p.Profile.Audio.Mode) {
		t.Fatalf("snapshot minimum %v size %+v version %s", p.MinSavingsPercent, p.Size, p.workerVersion())
	}
	plan := goalPlan(t, p)
	if plan.ModePolicyVersion != transcode.ModePolicyV9 || plan.SizePolicy.MinSavingsPercent != transcode.SizeSmallerSavingPercent {
		t.Fatalf("plan %+v", plan)
	}
	if err := applyTrackSelection(plan, &transcode.TrackSelection{KeepAudio: []int{1}}); err != nil || plan.ModePolicyVersion != transcode.ModePolicyV9 {
		t.Fatalf("plan with tracks %s %v", plan.ModePolicyVersion, err)
	}
	// 6% smaller is kept under v9, not under v5.
	if !transcode.ModeSizeAllowedVersion("size", 1000, 940, transcode.ModePolicyV9) || transcode.ModeSizeAllowedVersion("size", 1000, 960, transcode.ModePolicyV9) || transcode.ModeSizeAllowedVersion("size", 1000, 940, transcode.ModePolicyV5) {
		t.Fatal("v9 size gate")
	}

	plain, err := modePolicyWithTracks("size", false, 0, nil, false, nil, nil)
	if err != nil || plain.Size.SmallerSaving || plain.MinSavingsPercent != 15 || plain.workerVersion() != transcode.ModePolicyV5 {
		t.Fatalf("plain size goal %+v %v", plain, err)
	}
	if raw, _ := json.Marshal(plain); strings.Contains(string(raw), "smaller_saving") {
		t.Fatalf("plain snapshot names the field: %s", raw)
	}

	for name, inputs := range map[string]map[string]any{
		"another goal":      {"mode": "compact", "accept_smaller_saving": true},
		"no goal":           {"accept_smaller_saving": true},
		"audio only":        {"mode": "size", "accept_smaller_saving": true, "audio_only": true},
		"a size budget":     {"mode": "size", "accept_smaller_saving": true, "size_budget_percent": 80.0},
		"not true or false": {"mode": "size", "accept_smaller_saving": "yes"},
	} {
		if err := validateModeInputs(inputs); err == nil {
			t.Errorf("accept_smaller_saving with %s accepted", name)
		}
	}
	if err := validateModeInputs(map[string]any{"mode": "size", "accept_smaller_saving": false, "size_budget_percent": 80.0}); err != nil {
		t.Fatalf("accept_smaller_saving false with a budget: %v", err)
	}

	// A batch file whose goal changed to it starts with the input that froze it.
	inputs := map[string]any{"mode": "size", "accept_smaller_saving": true}
	applyGoalPolicyInputs(inputs, plain)
	if _, has := inputs["accept_smaller_saving"]; has {
		t.Fatalf("plain goal inputs %v", inputs)
	}
	applyGoalPolicyInputs(inputs, *p)
	if inputs["accept_smaller_saving"] != true {
		t.Fatalf("smaller saving goal inputs %v", inputs)
	}

	// A stored snapshot whose flag and minimum disagree is refused.
	for name, mutate := range map[string]func(*ModePolicySnapshot){
		"the plain minimum": func(s *ModePolicySnapshot) { s.MinSavingsPercent = 15 },
		"a flag on a minimum": func(s *ModePolicySnapshot) {
			size := *s.Size
			size.SmallerSaving = false
			s.Size = &size
			s.MinSavingsPercent = 5
		},
	} {
		s := *p
		mutate(&s)
		s.Digest = digestModePolicySnapshot(t, s)
		ec := &ExecutionContext{Inputs: map[string]any{"mode": "size", "accept_smaller_saving": true}, State: map[string]any{"mode_policy": s}}
		if _, err := freezeModePolicy(ec); err == nil {
			t.Errorf("snapshot with %s accepted", name)
		}
	}
}

// A worker runs v8 and v9 plans only when it says so.
func TestWorkerRunsPolicyVersion(t *testing.T) {
	var caps transcode.WorkerCapabilities
	for version, want := range map[string]string{transcode.ModePolicyV7: "audio-only", transcode.ModePolicyV8: "Shrink", transcode.ModePolicyV9: "smaller saving", transcode.ModePolicyV5: ""} {
		err := workerRunsPolicyVersion(caps, version)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v", version, err)
		}
	}
	caps.SizeAudioOnlyGoal, caps.CompactAudioLevels, caps.SizeSmallerSaving = true, true, true
	for _, version := range []string{transcode.ModePolicyV7, transcode.ModePolicyV8, transcode.ModePolicyV9} {
		if err := workerRunsPolicyVersion(caps, version); err != nil {
			t.Errorf("%s: %v", version, err)
		}
	}
}
