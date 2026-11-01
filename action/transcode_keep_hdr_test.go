package action

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// keep_hdr is a Shrink input only; it freezes in the compact snapshot, and a
// goal without it keeps the snapshot (and digest) it had before.
func TestShrinkKeepHDRInput(t *testing.T) {
	for name, inputs := range map[string]map[string]any{
		"another goal":      {"mode": "size", "keep_hdr": true},
		"no goal":           {"keep_hdr": true},
		"not true or false": {"mode": "compact", "keep_hdr": "yes"},
	} {
		if err := validateModeInputs(inputs); err == nil {
			t.Errorf("keep_hdr with %s accepted", name)
		}
	}
	for _, inputs := range []map[string]any{{"mode": "compact", "keep_hdr": true}, {"mode": "compact", "keep_hdr": false, "max_height": 2160}} {
		if err := validateModeInputs(inputs); err != nil {
			t.Errorf("%v rejected: %v", inputs, err)
		}
	}

	ec := &ExecutionContext{Inputs: map[string]any{"mode": "compact", "keep_hdr": true}, State: map[string]any{}}
	p, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := modePolicyFor("compact", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Compact == nil || !p.Compact.KeepHDR || p.Compact.ChainVersion != transcode.CompactChainVersion || p.Digest == plain.Digest || p.workerVersion() != plain.workerVersion() {
		t.Fatalf("keep-HDR snapshot %+v (plain digest %s)", p.Compact, plain.Digest)
	}
	// keep_hdr false freezes the same snapshot as a plain Shrink.
	frozenPlain, err := freezeModePolicy(&ExecutionContext{Inputs: map[string]any{"mode": "compact"}, State: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	off := &ExecutionContext{Inputs: map[string]any{"mode": "compact", "keep_hdr": false}, State: map[string]any{}}
	if q, err := freezeModePolicy(off); err != nil || q.Digest != frozenPlain.Digest || q.Compact.KeepHDR || q.Digest == p.Digest {
		t.Fatalf("keep_hdr false snapshot %+v %v", q, err)
	}
	if raw, _ := json.Marshal(plain); strings.Contains(string(raw), "keep_hdr") {
		t.Fatalf("plain snapshot names the field: %s", raw)
	}
	// A restart reads the same snapshot back.
	data, _ := json.Marshal(ec.State)
	_ = json.Unmarshal(data, &ec.State)
	if after, err := freezeModePolicy(ec); err != nil || after.Digest != p.Digest {
		t.Fatalf("restart %+v %v", after, err)
	}

	// A batch file whose goal changed to it starts with the input that froze it.
	inputs := map[string]any{"mode": "compact", "keep_hdr": true}
	applyGoalPolicyInputs(inputs, plain)
	if _, has := inputs["keep_hdr"]; has {
		t.Fatalf("plain goal inputs %v", inputs)
	}
	applyGoalPolicyInputs(inputs, *p)
	if inputs["keep_hdr"] != true {
		t.Fatalf("keep-HDR goal inputs %v", inputs)
	}
}

// Keep HDR plans an HDR source on the keep-HDR chain and an SDR one exactly as
// without it.
func TestShrinkKeepHDRPlansHDRSourcesOnly(t *testing.T) {
	keep, err := freezeModePolicy(&ExecutionContext{Inputs: map[string]any{"mode": "compact", "keep_hdr": true}, State: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := modePolicyFor("compact", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	rep := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{Kind: "video", Codec: "hevc", Width: 3840, Height: 2160, BitDepth: 10}}}
	classify := func(d mediainspect.DynamicRange) func(context.Context, *mediainspect.DetailedReport) (mediainspect.DynamicRange, error) {
		return func(context.Context, *mediainspect.DetailedReport) (mediainspect.DynamicRange, error) { return d, nil }
	}
	hdr10 := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeHDR10, Primaries: "bt2020", Matrix: "bt2020nc", ContentLightLevel: &mediainspect.ContentLightLevelMetadata{MaxCLL: 1000, MaxFALL: 400}}
	sdr := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeSDR, Primaries: "bt709", Matrix: "bt709"}

	plan, reason := compactSourcePlanWith(context.Background(), rep, keep, classify(hdr10))
	if reason != "" || plan.ChainVersion != transcode.CompactHDRChainVersion || !plan.KeepsHDR() || plan.HDR.MaxCLL != 1000 {
		t.Fatalf("keep-HDR plan %+v %q", plan, reason)
	}
	plan, reason = compactSourcePlanWith(context.Background(), rep, &plain, classify(hdr10))
	if reason != "" || plan.ChainVersion != transcode.CompactChainVersion || plan.KeepsHDR() {
		t.Fatalf("SDR plan %+v %q", plan, reason)
	}
	keptSDR, reason := compactSourcePlanWith(context.Background(), rep, keep, classify(sdr))
	plainSDR, plainReason := compactSourcePlanWith(context.Background(), rep, &plain, classify(sdr))
	if reason != plainReason || (keptSDR == nil) != (plainSDR == nil) || keptSDR != nil && (!keptSDR.Equal(plainSDR) || keptSDR.KeepsHDR()) {
		t.Fatalf("SDR source with keep_hdr %+v %q, without %+v %q", keptSDR, reason, plainSDR, plainReason)
	}
}

// A keep-HDR plan runs only on a worker that reports the keep-HDR chain.
func TestShrinkKeepHDRNeedsWorkerCapability(t *testing.T) {
	plan := &transcode.CompactPlan{ChainVersion: transcode.CompactHDRChainVersion, HDR: &transcode.CompactHDROutput{Transfer: transcode.CompactHDRTransferPQ}}
	caps := transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, CompactAudio: true, CompactTranscode: true, AutomaticModesV2: true, Encoders: map[string]bool{"libx265": true}}
	if err := compactPlanWorkerError(caps, plan); err == nil || err.Error() != errCompactKeepHDRWorker {
		t.Fatalf("worker without keep-HDR: %v", err)
	}
	if err := compactPlanWorkerError(caps, &transcode.CompactPlan{ChainVersion: transcode.CompactChainVersion}); err != nil {
		t.Fatalf("SDR plan on a compact worker: %v", err)
	}
	caps.CompactKeepHDR = true
	if err := compactPlanWorkerError(caps, plan); err != nil {
		t.Fatalf("keep-HDR worker: %v", err)
	}
	caps.CompactTranscode = false
	if err := compactPlanWorkerError(caps, plan); err == nil || !strings.Contains(err.Error(), "compact SDR") {
		t.Fatalf("worker without compact: %v", err)
	}
}

// The final gate holds a keep-HDR result to its measurement chain and to the
// frozen static metadata; an SDR Shrink plan needs only the chain and the
// passing output check, as before.
func TestCompactEvidenceGate(t *testing.T) {
	md := "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,50)"
	keep := &transcode.CompactPlan{ChainVersion: transcode.CompactHDRChainVersion, TargetWidth: 1920, TargetHeight: 1080, HDR: &transcode.CompactHDROutput{Transfer: transcode.CompactHDRTransferPQ, MasterDisplay: md, MaxCLL: 1000, MaxFALL: 400}}
	sdr := &transcode.CompactPlan{ChainVersion: transcode.CompactChainVersion, TargetWidth: 1920, TargetHeight: 1080}
	evidence := func(chain string, mutate func(c, o map[string]any)) []byte {
		c := map[string]any{"chain_version": chain, "target_width": 1920, "target_height": 1080, "candidate_chain": "scale,lut", "applied_to": "reference_and_candidate"}
		o := map[string]any{"verdict": "pass", "master_display": md, "max_cll": 1000, "max_fall": 400}
		if mutate != nil {
			mutate(c, o)
		}
		raw, _ := json.Marshal(map[string]any{"compact_chain": c, "compact_output": o})
		return raw
	}
	if why := compactEvidenceError(keep, evidence(keep.ChainVersion, nil)); why != "" {
		t.Fatalf("matching keep-HDR evidence refused: %s", why)
	}
	if why := compactEvidenceError(nil, nil); why != "" {
		t.Fatalf("plan without compact: %s", why)
	}
	// An SDR plan's evidence never names a candidate chain or metadata.
	if why := compactEvidenceError(sdr, evidence(sdr.ChainVersion, func(c, o map[string]any) {
		c["applied_to"] = "reference"
		delete(c, "candidate_chain")
		delete(o, "master_display")
		delete(o, "max_cll")
		delete(o, "max_fall")
	})); why != "" {
		t.Fatalf("SDR evidence refused: %s", why)
	}
	for name, mutate := range map[string]func(c, o map[string]any){
		"no candidate chain":    func(c, o map[string]any) { delete(c, "candidate_chain") },
		"reference side only":   func(c, o map[string]any) { c["applied_to"] = "reference" },
		"other mastering":       func(c, o map[string]any) { o["master_display"] = "G(0,0)B(0,0)R(0,0)WP(0,0)L(0,0)" },
		"no light level":        func(c, o map[string]any) { delete(o, "max_cll") },
		"other frame average":   func(c, o map[string]any) { o["max_fall"] = 401 },
		"failed output check":   func(c, o map[string]any) { o["verdict"] = "fail" },
		"another target height": func(c, o map[string]any) { c["target_height"] = 2160 },
	} {
		if why := compactEvidenceError(keep, evidence(keep.ChainVersion, mutate)); !strings.Contains(why, "original preserved") {
			t.Errorf("keep-HDR evidence with %s accepted", name)
		}
	}
	// Evidence from the SDR chain never stands in for a keep-HDR plan.
	if why := compactEvidenceError(keep, evidence(transcode.CompactChainVersion, nil)); why == "" {
		t.Fatal("SDR chain evidence accepted for a keep-HDR plan")
	}
}
