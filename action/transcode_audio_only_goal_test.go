package action

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

func audioOnlyGoalCaps() transcode.WorkerCapabilities {
	caps := audioOnlyCaps()
	caps.SizeAudioOnlyGoal = true
	return caps
}

// newAudioOnlyGoalCase is newAudioOnlyCase under the audio-only goal: the
// frozen policy and its base plan are v7.
func newAudioOnlyGoalCase(t *testing.T, caps transcode.WorkerCapabilities) *audioOnlyCase {
	t.Helper()
	ec := &ExecutionContext{ActionName: "transcode_media", InstanceID: "audio-only-goal", Inputs: map[string]any{"mode": "size", "audio_only": true}, State: map[string]any{"original_sha256": strings.Repeat("a", 64), "original_size": int64(10_700_000_000), "source_report": audioOnlyReport()}}
	snapshot, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	base := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8, AudioMode: snapshot.Profile.Audio.Mode}
	if err := applyModePlan(base, snapshot); err != nil {
		t.Fatal(err)
	}
	ec.State["plan"] = base
	executor := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) { return caps, nil }}
	return &audioOnlyCase{ec: ec, engine: NewEngine(EngineDeps{Transcode: executor}), outputs: map[string]any{}, base: base, snapshot: snapshot}
}

func TestAudioOnlyGoalFreezesPolicyV7(t *testing.T) {
	c := newAudioOnlyGoalCase(t, audioOnlyGoalCaps())
	p := c.snapshot
	if p.Version != ModePolicyV4 || p.workerVersion() != transcode.ModePolicyV7 || p.Recipe == nil || p.Recipe.Kind != RecipeAudioOnly || p.Recipe.VideoCodec != transcode.VideoCodecCopy {
		t.Fatalf("snapshot %s recipe %+v worker version %s", p.Version, p.Recipe, p.workerVersion())
	}
	if p.MinSavingsPercent != 0 || p.Size == nil || !p.Size.AudioOnly || p.Size.Hardware != nil || p.Size.EightBitMain10 || p.Profile.Audio.Mode != transcode.AudioModeSizeAACBed {
		t.Fatalf("snapshot content %+v size %+v", p, p.Size)
	}
	if c.base.ModePolicyVersion != transcode.ModePolicyV7 || c.base.SizePolicy.MinSavingsPercent != 0 {
		t.Fatalf("base plan %+v", c.base)
	}
	// A plain size goal is unchanged by the field.
	plain, err := modePolicyWithTracks("size", false, 0, nil, false, nil, nil)
	if err != nil || plain.Size.AudioOnly || plain.workerVersion() != transcode.ModePolicyV5 || plain.MinSavingsPercent != 15 {
		t.Fatalf("plain size goal %+v %v", plain, err)
	}

	for name, inputs := range map[string]map[string]any{
		"another goal":      {"mode": "compact", "audio_only": true},
		"no goal":           {"audio_only": true},
		"a size budget":     {"mode": "size", "audio_only": true, "size_budget_percent": 40.0},
		"the slow preset":   {"mode": "size", "audio_only": true, "allow_slow_preset": true},
		"not true or false": {"mode": "size", "audio_only": "yes"},
	} {
		if err := validateModeInputs(inputs); err == nil {
			t.Errorf("audio_only with %s accepted", name)
		}
	}
	// The media engine has nothing to encode, so asking for it is harmless.
	if err := validateModeInputs(map[string]any{"mode": "size", "audio_only": true, "allow_hardware": true}); err != nil {
		t.Fatalf("audio_only with allow_hardware: %v", err)
	}

	// A stored snapshot whose recipe disagrees with the flag is refused.
	for name, mutate := range map[string]func(*ModePolicySnapshot){
		"flag without its recipe": func(s *ModePolicySnapshot) {
			s.Recipe = modeRecipeFor(&ModePolicySnapshot{Mode: "size", Size: &ModeSizePolicy{}}, false)
		},
		"recipe without its flag": func(s *ModePolicySnapshot) { size := *s.Size; size.AudioOnly = false; s.Size = &size },
		"a minimum saving":        func(s *ModePolicySnapshot) { s.MinSavingsPercent = 15 },
	} {
		s := *p
		mutate(&s)
		s.Digest = digestModePolicySnapshot(t, s)
		ec := &ExecutionContext{Inputs: map[string]any{"mode": "size", "audio_only": true}, State: map[string]any{"mode_policy": s}}
		if _, err := freezeModePolicy(ec); err == nil {
			t.Errorf("snapshot with %s accepted", name)
		}
	}
}

func TestAudioOnlyGoalPlansACopy(t *testing.T) {
	c := newAudioOnlyGoalCase(t, audioOnlyGoalCaps())
	result, handled := c.engine.startAudioOnlyGoal(context.Background(), c.ec)
	if !handled || result.Status != StepCompleted || getBool(c.ec.State, "skip_transcode") {
		t.Fatalf("goal %+v handled=%v state=%+v", result, handled, c.ec.State)
	}
	plan := getPlan(c.ec.State["plan"])
	if !transcode.IsSizeAudioOnlyPlan(plan) || transcode.ValidateSizeAudioOnlyShape(plan) != nil || plan.ModePolicyVersion != transcode.ModePolicyV7 || plan.SizePolicy.MinSavingsPercent != 0 || plan.AudioMode != transcode.AudioModeSizeAACBed || plan.PolicyDigest != c.snapshot.Digest {
		t.Fatalf("audio-only goal plan %+v", plan)
	}
	if c.ec.State["plan_source"] != "audio_only" || !getBool(c.ec.State, "size_audio_only") || !strings.Contains(getString(result.Outputs, "plan_note"), "copies the video") {
		t.Fatalf("state %+v outputs %+v", c.ec.State, result.Outputs)
	}
	// A plain size goal is not the audio-only goal's to plan.
	plain := newAudioOnlyCase(t, audioOnlyGoalCaps(), nil)
	if _, handled := plain.engine.startAudioOnlyGoal(context.Background(), plain.ec); handled {
		t.Fatal("plain size goal planned as audio only")
	}

	// A worker that predates v7 fails the step instead of copying under v5.
	old := newAudioOnlyGoalCase(t, audioOnlyCaps())
	if result, handled := old.engine.startAudioOnlyGoal(context.Background(), old.ec); !handled || result.Status != StepFailed || !strings.Contains(result.Error, "audio-only goal") {
		t.Fatalf("old worker %+v handled=%v", result, handled)
	}

	// Nothing to convert and nothing removed: skipped, not encoded.
	small := newAudioOnlyGoalCase(t, audioOnlyGoalCaps())
	rep := audioOnlyReport()
	rep.Audio = []mediainspect.DetailedStream{{Index: 1, Codec: "aac", Profile: "LC", Channels: 2, ChannelLayout: "stereo", Language: "eng", BitRate: 128000}}
	small.ec.State["source_report"] = rep
	if result, handled := small.engine.startAudioOnlyGoal(context.Background(), small.ec); !handled || result.Status != StepCompleted || getString(small.ec.State, "reason_code") != "audio_only_nothing_to_convert" {
		t.Fatalf("nothing to convert %+v state %+v", result, small.ec.State)
	}
}

func TestAudioOnlyGoalKeepsAnySmallerFile(t *testing.T) {
	c := newAudioOnlyGoalCase(t, audioOnlyGoalCaps())
	if result, _ := c.engine.startAudioOnlyGoal(context.Background(), c.ec); result.Status != StepCompleted {
		t.Fatalf("plan %+v", result)
	}
	plan := getPlan(c.ec.State["plan"])
	for name, tc := range map[string]struct {
		candidate int
		saved     bool
	}{
		"5% smaller": {candidate: 95, saved: true},
		"larger":     {candidate: 101},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := t.TempDir() + "/candidate.mkv"
			if err := os.WriteFile(candidate, make([]byte, tc.candidate), 0600); err != nil {
				t.Fatal(err)
			}
			state := map[string]any{}
			for k, v := range c.ec.State {
				state[k] = v
			}
			state["candidate_path"], state["candidate_sha256"] = candidate, "candidate"
			state["quality_evidence"] = map[string]any{"verdict": "pass", "candidate_sha256": "candidate", "plan_digest": plan.PlanDigest,
				"video_copy": map[string]any{"method": transcode.VideoCopyMethod, "verdict": "pass", "source_hash": "sha256:v", "candidate_hash": "sha256:v", "source_packets": 1200, "candidate_packets": 1200, "source_codec": "hevc"}}
			state["original_size"], state["original"] = int64(100), map[string]any{"size_bytes": int64(100), "video_codec": "hevc"}
			ec := &ExecutionContext{ActionName: "transcode_media", InstanceID: c.ec.InstanceID, Inputs: c.ec.Inputs, State: state}
			result, err := (&Engine{}).stepTranscodeValidate(context.Background(), ec)
			if err != nil || result.Status != StepCompleted || getBool(ec.State, "skip_transcode") == tc.saved {
				t.Fatalf("%+v %v state %+v", result, err, ec.State)
			}
		})
	}
	// An encode plan under the goal is refused, whatever planned it.
	encode := *c.base
	c.ec.State["plan"] = &encode
	if result, _ := (&Engine{}).stepTranscodeValidate(context.Background(), c.ec); result.Status != StepFailed {
		t.Fatalf("encode plan under the audio-only goal passed: %+v", result)
	}
}

func TestAudioOnlySourceReasonTakesHDR(t *testing.T) {
	rep := audioOnlyReport()
	rep.HDR = &mediainspect.HDRReport{Present: true}
	rep.Video[0].BitDepth, rep.Video[0].FPS = 10, 60
	if modeSourceReason("size", rep, false) == "" {
		t.Fatal("plain size goal took an HDR 60 fps source")
	}
	if got := audioOnlySourceReason(rep); got != "" {
		t.Fatalf("audio-only goal refused an HDR 60 fps source: %s", got)
	}
	rep.Video[0].BitDepth = 12
	if audioOnlySourceReason(rep) != "unsupported_source" {
		t.Fatal("12-bit video accepted")
	}
}
