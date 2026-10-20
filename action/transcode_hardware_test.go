package action

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

func hardwareTestCapabilities() transcode.WorkerCapabilities {
	caps := rulesTestCapabilities()
	caps.HardwareSearch = true
	caps.HardwareFallbackSearch = true
	caps.Encoders[transcode.VideoCodecHEVCVideoToolbox] = true
	return caps
}

// New size operations allow the media engine unless they turn it off, and that
// is its own frozen snapshot. Without it the size policy is byte-for-byte what
// it was, so a snapshot frozen earlier keeps its digest and its behaviour.
func TestSizePolicyHardwareDefaultsOnAndFreezes(t *testing.T) {
	base, err := modePolicy("size", false)
	if err != nil {
		t.Fatal(err)
	}
	same, err := modePolicyWithHardware("size", false, 0, nil)
	if err != nil || same.Digest != base.Digest || same.Size.Hardware != nil {
		t.Fatalf("size without hardware changed: %+v %v", same.Size, err)
	}
	raw, _ := json.Marshal(base)
	if strings.Contains(string(raw), "hardware") {
		t.Fatalf("default size snapshot mentions hardware: %s", raw)
	}
	rules, err := modeHardwareInput("size", map[string]any{})
	if err != nil || rules == nil {
		t.Fatalf("rules %+v %v", rules, err)
	}
	if rules.MarginPercent != 15 || rules.GOPSize != 240 || rules.BFrames != 1 || len(rules.QualityValues) != 4 || rules.QualityValues[0] != 45 || rules.QualityValues[3] != 60 {
		t.Fatalf("default rules %+v", rules)
	}
	if got := fmt.Sprint(rules.FallbackQualityValues); got != "[65 70 75]" {
		t.Fatalf("default fallback rungs %s", got)
	}
	hw, err := modePolicyWithHardware("size", false, 0, rules)
	if err != nil {
		t.Fatal(err)
	}
	if hw.Digest == base.Digest || hw.Version != SizeModePolicyVersion || hw.Size.Hardware == nil || hw.Size.Hardware == rules {
		t.Fatalf("hardware snapshot %+v", hw.Size)
	}
	// The default snapshot is a frozen artifact: changing its rules, their order
	// or their defaults changes the digest of every new size operation.
	// Adding the higher-quality fallback rungs gave new snapshots a new digest;
	// verifying on the final grid gave the one below (snapshots frozen before
	// it re-hash to sha256:1b73a704...ad6, see
	// TestGridEraSnapshotsKeepTheirDigestAndSearch). Snapshots frozen with the
	// media engine before the rungs carry no such field.
	if hw.Digest != "sha256:8613e1dd656b5181907007220b154265614a2418db98601de08bd591bc24e09a" {
		t.Fatalf("size snapshot with the media engine on changed digest: %s", hw.Digest)
	}
	// The frozen snapshot round-trips through storage and re-verifies.
	stored := map[string]any{}
	b, _ := json.Marshal(hw)
	_ = json.Unmarshal(b, &stored)
	ec := &ExecutionContext{InstanceID: "hw", Inputs: map[string]any{"mode": "size", "allow_hardware": true}, State: map[string]any{"mode_policy": stored}}
	frozen, err := freezeModePolicy(ec)
	if err != nil || frozen.Size.Hardware == nil || frozen.Size.Hardware.GOPSize != 240 {
		t.Fatalf("stored policy: %+v %v", frozen, err)
	}
	// A fresh operation freezes the same snapshot from its inputs, whether it
	// says nothing, says yes or gives the default margin.
	for name, inputs := range map[string]map[string]any{
		"no hardware inputs": {"mode": "size"},
		"allowed":            {"mode": "size", "allow_hardware": true},
		"default margin":     {"mode": "size", "allow_hardware": true, "hardware_margin_percent": float64(15)},
	} {
		ec = &ExecutionContext{InstanceID: "hw2", Inputs: inputs, State: map[string]any{}}
		fresh, err := freezeModePolicy(ec)
		if err != nil || fresh.Digest != hw.Digest {
			t.Fatalf("%s: fresh policy digest %v %v", name, fresh, err)
		}
	}
	// Turning it off is the earlier policy exactly.
	ec = &ExecutionContext{InstanceID: "hw-off", Inputs: map[string]any{"mode": "size", "allow_hardware": false}, State: map[string]any{}}
	off, err := freezeModePolicy(ec)
	if err != nil || off.Digest != base.Digest || off.Size.Hardware != nil {
		t.Fatalf("hardware off changed the policy: %+v %v", off, err)
	}
	// A snapshot frozen before the option existed is never re-derived from the
	// new default: it keeps its digest and stays x265-only.
	earlier := map[string]any{}
	b, _ = json.Marshal(base)
	_ = json.Unmarshal(b, &earlier)
	ec = &ExecutionContext{InstanceID: "hw-earlier", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"mode_policy": earlier}}
	kept, err := freezeModePolicy(ec)
	if err != nil || kept.Digest != base.Digest || kept.Size.Hardware != nil {
		t.Fatalf("an earlier snapshot picked up the default: %+v %v", kept, err)
	}
	// The margin is part of the frozen policy.
	ec = &ExecutionContext{InstanceID: "hw3", Inputs: map[string]any{"mode": "size", "allow_hardware": true, "hardware_margin_percent": float64(30)}, State: map[string]any{}}
	wider, err := freezeModePolicy(ec)
	if err != nil || wider.Digest == hw.Digest || wider.Size.Hardware.MarginPercent != 30 {
		t.Fatalf("wider margin %+v %v", wider, err)
	}
}

func TestHardwareInputValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		inputs map[string]any
		want   string
	}{
		"size, nothing said":       {map[string]any{"mode": "size"}, ""},
		"size with the option":     {map[string]any{"mode": "size", "allow_hardware": true}, ""},
		"margin alone":             {map[string]any{"mode": "size", "hardware_margin_percent": 20}, ""},
		"size with a margin":       {map[string]any{"mode": "size", "allow_hardware": true, "hardware_margin_percent": 0}, ""},
		"upper bound":              {map[string]any{"mode": "size", "allow_hardware": true, "hardware_margin_percent": 50}, ""},
		"explicitly off":           {map[string]any{"mode": "size", "allow_hardware": false}, ""},
		"null values":              {map[string]any{"mode": "size", "allow_hardware": nil, "hardware_margin_percent": nil}, ""},
		"margin too large":         {map[string]any{"mode": "size", "allow_hardware": true, "hardware_margin_percent": 51}, "margin_percent"},
		"negative margin":          {map[string]any{"mode": "size", "allow_hardware": true, "hardware_margin_percent": -1}, "margin_percent"},
		"margin is not a number":   {map[string]any{"mode": "size", "allow_hardware": true, "hardware_margin_percent": "15"}, "must be a number"},
		"margin in quality mode":   {map[string]any{"mode": "quality", "hardware_margin_percent": 15}, "only supported with mode=size"},
		"margin without a mode":    {map[string]any{"hardware_margin_percent": 15}, "only supported with mode=size"},
		"margin with the flag off": {map[string]any{"mode": "size", "allow_hardware": false, "hardware_margin_percent": 15}, "requires allow_hardware"},
		"flag is not a boolean":    {map[string]any{"mode": "size", "allow_hardware": "yes"}, "true or false"},
		"quality mode":             {map[string]any{"mode": "quality", "allow_hardware": true}, "only supported with mode=size"},
		"quality mode, flag off":   {map[string]any{"mode": "quality", "allow_hardware": false}, ""},
		"compact mode":             {map[string]any{"mode": "compact", "allow_hardware": true}, "only supported with mode=size"},
		"preserve mode":            {map[string]any{"mode": "x265_preserve", "allow_hardware": true}, "only supported with mode=size"},
		"no mode":                  {map[string]any{"allow_hardware": true}, "only supported with mode=size"},
		"no mode, flag off":        {map[string]any{"allow_hardware": false}, ""},
	} {
		err := validateModeInputs(tc.inputs)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func hardwareRequest(t *testing.T, bitDepth int, duration float64, inputs map[string]any, caps transcode.WorkerCapabilities) *transcode.BenchmarkRequest {
	t.Helper()
	ec := &ExecutionContext{InstanceID: "hw-request", Inputs: inputs, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
	return hardwareRequestWith(t, ec, bitDepth, duration, caps)
}

// hardwareRequestFromState builds the request of an 8-bit 45-minute source for
// an operation whose state may already hold a frozen policy.
func hardwareRequestFromState(t *testing.T, ec *ExecutionContext, caps transcode.WorkerCapabilities) *transcode.BenchmarkRequest {
	t.Helper()
	return hardwareRequestWith(t, ec, 8, 45*60, caps)
}

func hardwareRequestWith(t *testing.T, ec *ExecutionContext, bitDepth int, duration float64, caps transcode.WorkerCapabilities) *transcode.BenchmarkRequest {
	t.Helper()
	policy, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", ExpectedBitDepth: bitDepth, AudioMode: policy.Profile.Audio.Mode}
	if err := preserveSourceBitDepth(plan, bitDepth, true); err != nil {
		t.Fatal(err)
	}
	if bitDepth == 8 {
		if err := setMain10(plan); err != nil {
			t.Fatal(err)
		}
	}
	ec.State["plan"] = plan
	rep := &mediainspect.DetailedReport{DurationSec: duration, Video: []mediainspect.DetailedStream{{BitDepth: bitDepth, FPS: 23.976, BitRate: 10000000}}}
	req, err := buildBenchmarkRequest(ec, "/media/show/show S01E04.mkv", rep, policy.Profile.Optimization, caps)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestSizeHardwareSearchRequest(t *testing.T) {
	for _, depth := range []int{8, 10} {
		// Nothing said is the default: allowed, at the default margin.
		def := hardwareRequest(t, depth, 45*60, map[string]any{"mode": "size"}, hardwareTestCapabilities())
		if def.HardwareSearch == nil || def.HardwareSearch.MarginPercent != 15 || len(def.Candidates) != 21 {
			t.Fatalf("depth %d: default request %+v with %d candidates", depth, def.HardwareSearch, len(def.Candidates))
		}
		req := hardwareRequest(t, depth, 45*60, map[string]any{"mode": "size", "allow_hardware": true, "hardware_margin_percent": 20}, hardwareTestCapabilities())
		if req.HardwareSearch == nil || req.HardwareSearch.MarginPercent != 20 || req.AutomaticSearch == nil {
			t.Fatalf("depth %d: rules %+v", depth, req.HardwareSearch)
		}
		if err := transcode.ValidateBenchmarkRequest(req); err != nil {
			t.Fatalf("depth %d: %v", depth, err)
		}
		var hardware, fallback []transcode.BenchmarkCandidate
		x265 := 0
		for _, c := range req.Candidates {
			if strings.HasPrefix(c.ID, hardwareCandidatePrefix) {
				hardware = append(hardware, c)
			} else if strings.HasPrefix(c.ID, transcode.HardwareFallbackCandidatePrefix) {
				fallback = append(fallback, c)
			} else if transcode.BenchmarkCandidateVideoCodec(c) == transcode.VideoCodecLibX265 {
				x265++
			}
		}
		// The x265 ladder and its fallback families are untouched.
		if x265 != 14 || len(hardware) != 4 || len(fallback) != 3 {
			t.Fatalf("depth %d: %d x265, %d hardware and %d hardware fallback candidates", depth, x265, len(hardware), len(fallback))
		}
		if len(req.Candidates) > transcode.MaxAutomaticHardwareCandidates {
			t.Fatalf("depth %d: %d candidates exceed the cap", depth, len(req.Candidates))
		}
		for i, c := range append(append([]transcode.BenchmarkCandidate(nil), hardware...), fallback...) {
			wantQuality := []int{45, 50, 55, 60, 65, 70, 75}[i]
			wantID := fmt.Sprintf("vt-cand_q%d", wantQuality)
			if i >= 4 {
				wantID = fmt.Sprintf("%scand_q%d", transcode.HardwareFallbackCandidatePrefix, wantQuality)
			}
			if c.ID != wantID || c.Quality != wantQuality {
				t.Fatalf("hardware candidate %d: %+v", i, c)
			}
			if transcode.BenchmarkCandidateVideoCodec(c) != transcode.VideoCodecHEVCVideoToolbox || c.Preset != "" || c.Tune != "" {
				t.Fatalf("hardware candidate %+v", c)
			}
			// The keyframe interval is what makes the files comparable in size.
			if c.GOPSize == nil || *c.GOPSize != 240 || c.BFrames == nil || *c.BFrames != 1 {
				t.Fatalf("hardware candidate %s lacks the frozen GOP and B-frames: %+v", c.ID, c)
			}
			// An 8-bit source follows the size policy's Main10 output.
			if c.VideoProfile != "main10" || c.PixelFormat != "p010le" {
				t.Fatalf("depth %d: hardware candidate shape %s/%s", depth, c.VideoProfile, c.PixelFormat)
			}
		}
	}
}

// The option is a permission: a search that cannot use it is the earlier
// x265-only request, never a failure.
func TestSizeHardwareSearchFallsBackToSoftware(t *testing.T) {
	noEncoder := hardwareTestCapabilities()
	delete(noEncoder.Encoders, transcode.VideoCodecHEVCVideoToolbox)
	oldWorker := rulesTestCapabilities()
	oldWorker.Encoders[transcode.VideoCodecHEVCVideoToolbox] = true
	for name, tc := range map[string]struct {
		inputs   map[string]any
		duration float64
		caps     transcode.WorkerCapabilities
	}{
		"option off":                      {map[string]any{"mode": "size", "allow_hardware": false}, 45 * 60, hardwareTestCapabilities()},
		"worker without hardware_search":  {map[string]any{"mode": "size", "allow_hardware": true}, 45 * 60, oldWorker},
		"worker without the encoder":      {map[string]any{"mode": "size", "allow_hardware": true}, 45 * 60, noEncoder},
		"source too short for the search": {map[string]any{"mode": "size", "allow_hardware": true}, 40, hardwareTestCapabilities()},
	} {
		req := hardwareRequest(t, 8, tc.duration, tc.inputs, tc.caps)
		if req.HardwareSearch != nil {
			t.Fatalf("%s: rules sent: %+v", name, req.HardwareSearch)
		}
		for _, c := range req.Candidates {
			if strings.HasPrefix(c.ID, hardwareCandidatePrefix) {
				t.Fatalf("%s: hardware candidate %s", name, c.ID)
			}
		}
		if err := transcode.ValidateBenchmarkRequest(req); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// A worker that does not know the fallback rungs gets the ladder alone: the
// request is what #177 shipped, and validates on an older worker's cap.
func TestSizeHardwareSearchWithoutFallbackCapability(t *testing.T) {
	caps := hardwareTestCapabilities()
	caps.HardwareFallbackSearch = false
	req := hardwareRequest(t, 8, 45*60, map[string]any{"mode": "size"}, caps)
	if req.HardwareSearch == nil || len(req.HardwareSearch.FallbackQualityValues) != 0 || len(req.Candidates) != 18 {
		t.Fatalf("request %+v with %d candidates", req.HardwareSearch, len(req.Candidates))
	}
	for _, c := range req.Candidates {
		if strings.HasPrefix(c.ID, transcode.HardwareFallbackCandidatePrefix) {
			t.Fatalf("fallback candidate %s sent to a worker without the capability", c.ID)
		}
	}
	if err := transcode.ValidateBenchmarkRequest(req); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(req.HardwareSearch)
	if strings.Contains(string(b), "fallback") {
		t.Fatalf("an older worker would see a field it rejects: %s", b)
	}
}

// Rules frozen with the media engine before the fallback rungs existed have no
// such field: they re-hash to the digest they were frozen under and stay on the
// ladder alone.
func TestHardwarePolicyWithoutFallbackRungsKeepsItsDigest(t *testing.T) {
	rules := &transcode.HardwareSearchRules{MarginPercent: 15, QualityValues: []int{45, 50, 55, 60}, GOPSize: 240, BFrames: 1}
	p, err := modePolicyWithHardware("size", false, 0, rules)
	if err != nil {
		t.Fatal(err)
	}
	if p.Digest != "sha256:ab4a33d9aa290b79b3eeaef9a6b3c17afda00376e9defade7d71e9ce65cb6128" {
		t.Fatalf("the earlier media engine snapshot changed digest: %s", p.Digest)
	}
	stored := map[string]any{}
	b, _ := json.Marshal(p)
	if hb, _ := json.Marshal(p.Size.Hardware); strings.Contains(string(hb), "fallback") {
		t.Fatalf("frozen hardware rules mention the fallback rungs: %s", hb)
	}
	_ = json.Unmarshal(b, &stored)
	ec := &ExecutionContext{InstanceID: "hw-earlier-engine", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"mode_policy": stored}}
	kept, err := freezeModePolicy(ec)
	if err != nil || kept.Digest != p.Digest || len(kept.Size.Hardware.FallbackQualityValues) != 0 {
		t.Fatalf("an earlier snapshot picked up the fallback rungs: %+v %v", kept, err)
	}
	caps := hardwareTestCapabilities()
	ec.State["original_sha256"] = strings.Repeat("a", 64)
	req := hardwareRequestFromState(t, ec, caps)
	if len(req.Candidates) != 18 || len(req.HardwareSearch.FallbackQualityValues) != 0 {
		t.Fatalf("an earlier snapshot sent fallback rungs: %d candidates, %+v", len(req.Candidates), req.HardwareSearch)
	}
}

func TestHardwareFallbackRulesValidation(t *testing.T) {
	good := func() *transcode.HardwareSearchRules {
		return &transcode.HardwareSearchRules{MarginPercent: 15, QualityValues: []int{45, 50, 55, 60}, FallbackQualityValues: []int{65, 70, 75}, GOPSize: 240, BFrames: 1}
	}
	if err := good().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, fallback := range map[string][]int{
		"too many rungs":          {61, 65, 70, 75},
		"not above the ladder":    {60, 65},
		"unordered":               {70, 65},
		"duplicate":               {65, 65},
		"above the encoder's max": {65, 101},
	} {
		r := good()
		r.FallbackQualityValues = fallback
		if r.Validate() == nil {
			t.Fatalf("%s accepted: %v", name, fallback)
		}
	}
	// The fallback candidates belong to rules that name them, and to the
	// VideoToolbox encoder.
	req := hardwareRequest(t, 8, 45*60, map[string]any{"mode": "size"}, hardwareTestCapabilities())
	noValues := *req
	noValues.HardwareSearch = good()
	noValues.HardwareSearch.FallbackQualityValues = nil
	if transcode.ValidateBenchmarkRequest(&noValues) == nil {
		t.Fatal("fallback candidates accepted without fallback rules")
	}
	wrongEncoder := *req
	wrongEncoder.Candidates = append([]transcode.BenchmarkCandidate(nil), req.Candidates...)
	for i := range wrongEncoder.Candidates {
		if strings.HasPrefix(wrongEncoder.Candidates[i].ID, transcode.HardwareFallbackCandidatePrefix) {
			wrongEncoder.Candidates[i].VideoCodec = transcode.VideoCodecLibX265
		}
	}
	if transcode.ValidateBenchmarkRequest(&wrongEncoder) == nil {
		t.Fatal("fallback candidate on another encoder accepted")
	}
	// A fallback candidate is one of the frozen rungs, encoded as the rules say.
	for name, mutate := range map[string]func(*transcode.BenchmarkCandidate){
		"a quality outside the rungs": func(c *transcode.BenchmarkCandidate) { c.Quality = 10 },
		"another keyframe interval":   func(c *transcode.BenchmarkCandidate) { g := 12; c.GOPSize = &g },
		"no keyframe interval":        func(c *transcode.BenchmarkCandidate) { c.GOPSize = nil },
		"another B-frame setting":     func(c *transcode.BenchmarkCandidate) { b := 0; c.BFrames = &b },
		"an x265 preset":              func(c *transcode.BenchmarkCandidate) { c.Preset = "slow" },
	} {
		mutated := *req
		mutated.Candidates = append([]transcode.BenchmarkCandidate(nil), req.Candidates...)
		for i := range mutated.Candidates {
			if strings.HasPrefix(mutated.Candidates[i].ID, transcode.HardwareFallbackCandidatePrefix) {
				mutate(&mutated.Candidates[i])
				break
			}
		}
		if err := transcode.ValidateBenchmarkRequest(&mutated); err == nil || !strings.Contains(err.Error(), "frozen hardware search rules") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The cap is on distinct candidates: 22 pass, 23 do not, and a fourth
	// fallback candidate is refused by its own bound.
	room := *req
	room.Candidates = append([]transcode.BenchmarkCandidate(nil), req.Candidates...)
	extra := func(n int) transcode.BenchmarkCandidate {
		c := room.Candidates[0]
		c.ID, c.Quality = fmt.Sprintf("extra-%d", n), 40+n
		return c
	}
	room.Candidates = append(room.Candidates, extra(1))
	if len(room.Candidates) != transcode.MaxAutomaticHardwareCandidates {
		t.Fatalf("%d candidates, want the cap", len(room.Candidates))
	}
	if err := transcode.ValidateBenchmarkRequest(&room); err != nil {
		t.Fatalf("candidates at the cap refused: %v", err)
	}
	room.Candidates = append(room.Candidates, extra(2))
	if err := transcode.ValidateBenchmarkRequest(&room); err == nil || !strings.Contains(err.Error(), "candidates") {
		t.Fatalf("%d candidates accepted: %v", len(room.Candidates), err)
	}
	fourth := *req
	fourth.Candidates = append([]transcode.BenchmarkCandidate(nil), req.Candidates...)
	for _, c := range req.Candidates {
		if strings.HasPrefix(c.ID, transcode.HardwareFallbackCandidatePrefix) {
			c.ID += "-again"
			fourth.Candidates = append(fourth.Candidates, c)
			break
		}
	}
	if err := transcode.ValidateBenchmarkRequest(&fourth); err == nil || !strings.Contains(err.Error(), "at most 3 hardware fallback") {
		t.Fatalf("a fourth fallback candidate: %v", err)
	}
}

func TestHardwareSearchRulesValidation(t *testing.T) {
	good := func() *transcode.HardwareSearchRules {
		return &transcode.HardwareSearchRules{MarginPercent: 15, QualityValues: []int{45, 50, 55, 60}, GOPSize: 240, BFrames: 1}
	}
	if err := good().Validate(); err != nil {
		t.Fatal(err)
	}
	var none *transcode.HardwareSearchRules
	if err := none.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*transcode.HardwareSearchRules){
		"margin above bound":    func(r *transcode.HardwareSearchRules) { r.MarginPercent = 51 },
		"negative margin":       func(r *transcode.HardwareSearchRules) { r.MarginPercent = -0.5 },
		"no qualities":          func(r *transcode.HardwareSearchRules) { r.QualityValues = nil },
		"too many qualities":    func(r *transcode.HardwareSearchRules) { r.QualityValues = []int{40, 45, 50, 55, 60} },
		"unordered qualities":   func(r *transcode.HardwareSearchRules) { r.QualityValues = []int{50, 45} },
		"duplicate qualities":   func(r *transcode.HardwareSearchRules) { r.QualityValues = []int{50, 50} },
		"quality out of range":  func(r *transcode.HardwareSearchRules) { r.QualityValues = []int{50, 101} },
		"no keyframe interval":  func(r *transcode.HardwareSearchRules) { r.GOPSize = 0 },
		"too many B-frames":     func(r *transcode.HardwareSearchRules) { r.BFrames = 2 },
		"negative B-frames":     func(r *transcode.HardwareSearchRules) { r.BFrames = -1 },
		"keyframe interval cap": func(r *transcode.HardwareSearchRules) { r.GOPSize = transcode.MaxBenchmarkGOPSize + 1 },
	} {
		r := good()
		mutate(r)
		if r.Validate() == nil {
			t.Fatalf("%s accepted: %+v", name, r)
		}
	}
	// Rules ride only with an automatic size v3 search that has its own rules.
	req := hardwareRequest(t, 8, 45*60, map[string]any{"mode": "size", "allow_hardware": true}, hardwareTestCapabilities())
	withoutRules := *req
	withoutRules.AutomaticSearch = nil
	if transcode.ValidateBenchmarkRequest(&withoutRules) == nil {
		t.Fatal("hardware search without automatic search rules accepted")
	}
	notAutomatic := *req
	notAutomatic.Automatic = false
	if transcode.ValidateBenchmarkRequest(&notAutomatic) == nil {
		t.Fatal("hardware search without automatic search accepted")
	}
	bad := *req
	bad.HardwareSearch = good()
	bad.HardwareSearch.MarginPercent = 99
	if transcode.ValidateBenchmarkRequest(&bad) == nil {
		t.Fatal("out-of-range margin accepted")
	}
}

// childOf runs one batch item through processBatchItem, as the batch's step
// does, and returns the child action it created. The test engine has no
// filesystem resolver, so the child stops right after its preflight has
// validated its inputs, bound them to the parent's frozen policy and frozen
// that policy: anything it rejects earlier is a wrong input.
func childOf(t *testing.T, e *Engine, st *store.Store, parentID string) (inputs, state map[string]any, errorJSON string) {
	t.Helper()
	parent, err := st.GetActionInstance(parentID)
	if err != nil {
		t.Fatal(err)
	}
	items, err := st.ListTranscodeBatchItems(parentID)
	if err != nil || len(items) != 1 {
		t.Fatal("expected one batch item", items, err)
	}
	item := items[0]
	if _, err := e.processBatchItem(context.Background(), &item, parseExecutionContext(parent, e), "Series", false); err != nil {
		t.Fatal(err)
	}
	if item.ChildActionID == "" {
		t.Fatal("no child action was created", item)
	}
	child, err := st.GetActionInstance(item.ChildActionID)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal([]byte(child.InputsJSON), &inputs) != nil || json.Unmarshal([]byte(child.StateJSON), &state) != nil {
		t.Fatal("unreadable child", child.InputsJSON, child.StateJSON)
	}
	return inputs, state, child.ErrorJSON + item.Error
}

// A batch item's child carries the inputs of the policy it runs under. A goal
// change gives the item its own policy, so the parent's media engine choice
// and compact output height must not leak into the child: they would fail its
// preflight for every goal but the one they belong to, or contradict a size
// attempt that turned the engine off. This goes through processBatchItem, so
// dropping the adjustment there fails it.
func TestGoalChangeChildInputsAgreeWithTheFrozenPolicy(t *testing.T) {
	type want struct {
		mode      string
		allow     any
		margin    any
		maxHeight any
	}
	sizeParent := map[string]any{"mode": "size", "allow_hardware": true, "hardware_margin_percent": float64(25)}
	compactParent := map[string]any{"mode": "compact", "max_height": float64(720)}
	for _, tc := range []struct {
		name     string
		parent   map[string]any
		settings map[string]any
		want     want
	}{
		{"quality", sizeParent, map[string]any{"mode": "quality"}, want{mode: "quality"}},
		{"x265_preserve", sizeParent, map[string]any{"mode": "x265_preserve"}, want{mode: "x265_preserve"}},
		{"compact", sizeParent, map[string]any{"mode": "compact"}, want{mode: "compact", maxHeight: float64(1080)}},
		{"compact, 1440", sizeParent, map[string]any{"mode": "compact", "max_height": float64(1440)}, want{mode: "compact", maxHeight: float64(1440)}},
		{"size, engine off", sizeParent, map[string]any{"mode": "size", "allow_hardware": false}, want{mode: "size", allow: false}},
		{"size, new margin", sizeParent, map[string]any{"mode": "size", "allow_hardware": true, "hardware_margin_percent": float64(30)}, want{mode: "size", allow: true, margin: float64(30)}},
		{"size, default", sizeParent, map[string]any{"mode": "size"}, want{mode: "size", allow: true, margin: float64(15)}},
		// The parent's 720p height is its own goal's: another goal does not take it
		// and compact without a height takes the default, not the parent's.
		{"compact parent to quality", compactParent, map[string]any{"mode": "quality"}, want{mode: "quality"}},
		{"compact parent to size", compactParent, map[string]any{"mode": "size"}, want{mode: "size", allow: true, margin: float64(15)}},
		{"compact parent to compact", compactParent, map[string]any{"mode": "compact"}, want{mode: "compact", maxHeight: float64(1080)}},
		{"compact parent to compact, 2160", compactParent, map[string]any{"mode": "compact", "max_height": float64(2160)}, want{mode: "compact", maxHeight: float64(2160)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := setupTestStore(t)
			e := NewEngine(EngineDeps{Store: st})
			seedActionInstance(t, st, "goal-parent", "transcode_batch", StatusCompleted, 3, "", tc.parent, map[string]any{})
			parent, _ := st.GetActionInstance("goal-parent")
			item := store.TranscodeBatchItem{BatchID: parent.ID, ItemKey: "file", FilePath: "/media/source.mkv", Decision: "transcode", Status: "failed", Profile: "mode-" + getString(tc.parent, "mode")}
			if err := st.CreateTranscodeBatchItem(item); err != nil {
				t.Fatal(err)
			}
			items, _ := st.ListTranscodeBatchItems(parent.ID)
			if _, err := e.ReconfigureBatch(context.Background(), parent.ID, "all", "", BatchSelectionVersion(parent, items), "", "goal-change", tc.settings); err != nil {
				t.Fatal(err)
			}
			inputs, state, failure := childOf(t, e, st, parent.ID)
			// What is encoded never depended on the child's inputs: the policy is the
			// item's own, compact height included.
			boundPolicy, _ := state["mode_policy"].(map[string]any)
			if compact, _ := boundPolicy["compact"].(map[string]any); tc.want.maxHeight != nil && compact["max_height"] != tc.want.maxHeight {
				t.Fatalf("child policy %v does not carry the %v height", state["mode_policy"], tc.want.maxHeight)
			}
			if inputs["mode"] != tc.want.mode || inputs["allow_hardware"] != tc.want.allow || inputs["hardware_margin_percent"] != tc.want.margin || inputs["max_height"] != tc.want.maxHeight {
				t.Fatalf("child inputs %v do not match the frozen policy, want %+v", inputs, tc.want)
			}
			// The child runs under the item's policy, not the parent's.
			policy, _ := state["mode_policy"].(map[string]any)
			if policy["mode"] != tc.want.mode || state["mode"] != tc.want.mode {
				t.Fatalf("child bound to %v, want the %s policy", state["mode_policy"], tc.want.mode)
			}
			if !strings.Contains(failure, "filesystem resolver") {
				t.Fatalf("child would fail preflight: %q", failure)
			}
		})
	}
}

// Without a goal change a child takes the batch's inputs: a compact batch's
// output height reaches it, and the policy it is bound to is the batch's.
func TestBatchChildInputsCarryTheBatchGoalInputs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inputs    map[string]any
		maxHeight any
		hardware  any
	}{
		{"compact at 720", map[string]any{"mode": "compact", "max_height": float64(720)}, float64(720), nil},
		{"compact, default height", map[string]any{"mode": "compact"}, nil, nil},
		{"size, engine off", map[string]any{"mode": "size", "allow_hardware": false}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := setupTestStore(t)
			e := NewEngine(EngineDeps{Store: st})
			maxHeight, _ := modeMaxHeightInput(tc.inputs)
			hardware, err := modeHardwareInput(getString(tc.inputs, "mode"), tc.inputs)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := modePolicyWithHardware(getString(tc.inputs, "mode"), false, maxHeight, hardware)
			if err != nil {
				t.Fatal(err)
			}
			seedActionInstance(t, st, "plain-parent", "transcode_batch", StatusCompleted, 3, "", tc.inputs, map[string]any{"mode_policy": policy, "policy_digest": policy.Digest})
			if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "plain-parent", ItemKey: "file", FilePath: "/media/source.mkv", Decision: "transcode", Status: "queued", Profile: "mode-" + getString(tc.inputs, "mode")}); err != nil {
				t.Fatal(err)
			}
			inputs, state, failure := childOf(t, e, st, "plain-parent")
			if inputs["max_height"] != tc.maxHeight || inputs["allow_hardware"] != tc.hardware {
				t.Fatalf("child inputs %v, want max_height %v and allow_hardware %v", inputs, tc.maxHeight, tc.hardware)
			}
			if got, _ := state["mode_policy"].(map[string]any); got["digest"] != policy.Digest || !strings.Contains(failure, "filesystem resolver") {
				t.Fatalf("child not bound to the batch policy or stopped early: %v %q", state["mode_policy"], failure)
			}
		})
	}
}

// The coordinator accepts a winner only if it repeats the measured candidate,
// including the GOP and B-frame settings the media engine candidates add.
func TestWinnerMustRepeatTheCandidateGOPAndBFrames(t *testing.T) {
	inputs := map[string]any{"mode": "size"}
	req := hardwareRequest(t, 8, 1800, inputs, hardwareTestCapabilities())
	var candidate transcode.BenchmarkCandidate
	for _, c := range req.Candidates {
		if c.ID == "vt-cand_q50" {
			candidate = c
		}
	}
	if candidate.ID == "" || candidate.GOPSize == nil || candidate.BFrames == nil {
		t.Fatalf("media engine candidate missing or incomplete: %+v", candidate)
	}
	digest, err := transcode.DigestBenchmarkRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	newContext := func() *ExecutionContext {
		ec := &ExecutionContext{InstanceID: "hw-winner", Inputs: inputs, State: map[string]any{"original_sha256": strings.Repeat("a", 64), "benchmark_request": *req, "benchmark_request_digest": digest, "plan": &transcode.Plan{}}}
		return ec
	}
	winner := func() *transcode.BenchmarkWinner {
		return &transcode.BenchmarkWinner{CandidateID: candidate.ID, VideoCodec: transcode.VideoCodecHEVCVideoToolbox, Quality: candidate.Quality, VideoProfile: candidate.VideoProfile, PixelFormat: candidate.PixelFormat, GOPSize: candidate.GOPSize, BFrames: candidate.BFrames, MinimumMet: true, MetricType: "vmaf", Score: 99}
	}
	if err := verifyModeBenchmarkWinner(newContext(), winner()); err != nil {
		t.Fatal("a faithful winner was refused:", err)
	}
	for name, change := range map[string]func(*transcode.BenchmarkWinner){
		"different GOP":    func(w *transcode.BenchmarkWinner) { v := 12; w.GOPSize = &v },
		"GOP dropped":      func(w *transcode.BenchmarkWinner) { w.GOPSize = nil },
		"B-frames dropped": func(w *transcode.BenchmarkWinner) { w.BFrames = nil },
		"different B":      func(w *transcode.BenchmarkWinner) { v := 0; w.BFrames = &v },
	} {
		w := winner()
		change(w)
		if err := verifyModeBenchmarkWinner(newContext(), w); err == nil {
			t.Fatalf("%s: accepted a winner that differs from the measured candidate", name)
		}
	}
}
