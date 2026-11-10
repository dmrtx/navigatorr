package action

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// preFreshGatePolicy is a snapshot as it was frozen before the fresh final
// windows gated the verdict: the same rules without the gate.
func preFreshGatePolicy(p ModePolicySnapshot) ModePolicySnapshot {
	if p.Size != nil && p.Size.AutomaticSearch != nil {
		size, rules := *p.Size, *p.Size.AutomaticSearch
		rules.GateFinalFreshWindows = false
		size.AutomaticSearch, p.Size = &rules, &size
	}
	return rehashPolicy(p)
}

// freshGateEC is an action whose stored mode policy is p.
func freshGateEC(t *testing.T, p ModePolicySnapshot) *ExecutionContext {
	t.Helper()
	stored := map[string]any{}
	raw, _ := json.Marshal(p)
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	return &ExecutionContext{InstanceID: "fresh-gate", Inputs: map[string]any{"mode": p.Mode}, State: map[string]any{"mode_policy": stored, "original_sha256": strings.Repeat("a", 64)}}
}

// A new size goal gates its fresh windows; a snapshot stored before the gate
// re-hashes to the digest it was frozen under and stays ungated.
func TestSnapshotsBeforeFreshGateKeepTheirDigest(t *testing.T) {
	rules, err := modeHardwareInput("size", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		hardware *transcode.HardwareSearchRules
		digest   string
	}{
		"no media engine": {nil, "sha256:cb53779c2cbdfe1d2f3ef41b16a2c986ddac436f8b19f4801cb4cd4e830b825c"},
		"media engine":    {rules, "sha256:ac41a174311afba39ec13ba20d9df6cfab95555097805812a4e4e571159180b4"},
	} {
		current, err := legacyModePolicyWithHardware("size", false, 0, tc.hardware)
		if err != nil {
			t.Fatal(err)
		}
		if r := current.Size.AutomaticSearch; !r.GateFinalFreshWindows || r.FinalFreshWindows != 2 || r.Validate() != nil {
			t.Fatalf("%s: new snapshot rules %+v", name, r)
		}
		if !frozenGateFreshWindows(freshGateEC(t, current)) {
			t.Fatalf("%s: a new size goal does not gate its fresh windows", name)
		}
		old := preFreshGatePolicy(current)
		if old.Digest != tc.digest || old.Digest == current.Digest {
			t.Fatalf("%s: pre-gate digest %s, want %s", name, old.Digest, tc.digest)
		}
		raw, _ := json.Marshal(old)
		if strings.Contains(string(raw), "gate_final_fresh_windows") {
			t.Fatalf("%s: a pre-gate snapshot serialises the gate", name)
		}
		ec := freshGateEC(t, old)
		frozen, err := freezeModePolicy(ec)
		if err != nil || frozen.Digest != tc.digest {
			t.Fatalf("%s: stored pre-gate snapshot re-hashed to %v, %v", name, frozen, err)
		}
		if frozenGateFreshWindows(ec) || frozenFinalFreshWindows(ec) != 2 {
			t.Fatalf("%s: a pre-gate snapshot is gated, or lost its fresh windows", name)
		}
		fresh := []transcode.BenchmarkSampleWindow{{Index: 9, StartSeconds: 300, DurationSeconds: 10}}
		if freshWindowsGated(ec, fresh) || !freshWindowsGated(freshGateEC(t, current), fresh) || freshWindowsGated(freshGateEC(t, current), nil) {
			t.Fatalf("%s: plan gate does not follow the frozen rule and the windows", name)
		}
	}
}

// Only size goals have fresh windows, so only they are gated.
func TestOnlySizeGoalsGateFreshWindows(t *testing.T) {
	for _, mode := range []string{"compact", "quality", "x265_preserve"} {
		p, err := legacyModePolicy(mode, false)
		if err != nil {
			t.Fatal(err)
		}
		ec := freshGateEC(t, p)
		if frozenGateFreshWindows(ec) || frozenFinalFreshWindows(ec) != 0 {
			t.Fatalf("%s: has gated or fresh windows", mode)
		}
	}
}

// A gated plan never reaches a worker without the fresh_sample_gate
// capability: the goal stops before encoding, as for a policy version the
// worker does not run, and an ungated plan still goes to an old worker.
func TestGatedPlanNeedsFreshSampleGateWorker(t *testing.T) {
	for _, tc := range []struct {
		gated, capable, refused bool
	}{
		{gated: true, capable: false, refused: true},
		{gated: true, capable: true},
		{gated: false, capable: false},
	} {
		mock := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
			return transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, FreshSampleGate: tc.capable}, nil
		}}
		e := NewEngine(EngineDeps{Transcode: mock})
		ec := testReconcileEC("job-gate")
		plan := testReconcilePlan()
		plan.QualityValidation = &transcode.QualityValidationPlan{Metric: "vmaf", FreshSamples: []transcode.BenchmarkSampleWindow{{Index: 5, StartSeconds: 300, DurationSeconds: 10}}, GateFreshSamples: tc.gated}
		ec.State["plan"] = plan
		res, err := e.stepTranscodeSubmit(context.Background(), ec)
		if err != nil {
			t.Fatal(err)
		}
		refused := res.Status == StepFailed && strings.Contains(res.Error, errFreshSampleGateWorker)
		if refused != tc.refused {
			t.Fatalf("%+v: status %s error %q", tc, res.Status, res.Error)
		}
		if tc.refused && mock.submitCalls != 0 {
			t.Fatalf("%+v: an old worker was sent %d jobs", tc, mock.submitCalls)
		}
	}

	// Planning the goal's final validation refuses the worker too, before any
	// encode, while a snapshot frozen before the gate still plans on it.
	current, err := legacyModePolicy("size", false)
	if err != nil {
		t.Fatal(err)
	}
	vrep := &mediainspect.DetailedReport{DurationSec: 600, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 24, BitRate: 5000000}}}
	for _, tc := range []struct {
		policy  ModePolicySnapshot
		capable bool
		refused bool
	}{
		{current, false, true},
		{current, true, false},
		{preFreshGatePolicy(current), false, false},
	} {
		ec := freshGateEC(t, tc.policy)
		ec.State["plan"] = &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8, Mode: "size", AudioMode: transcode.AudioModeCompactLossless, ModePolicyVersion: transcode.ModePolicyV3}
		caps := modeTestCapabilities()
		caps.FreshSampleGate = tc.capable
		_, err := finalRequestForPlan(ec, "/media/source.mkv", vrep, tc.policy.Profile.Optimization, caps)
		refused := err != nil && strings.Contains(err.Error(), errFreshSampleGateWorker)
		if refused != tc.refused {
			t.Fatalf("capable %v, gated policy %v: %v", tc.capable, tc.policy.Digest != preFreshGatePolicy(current).Digest, err)
		}
	}
}
