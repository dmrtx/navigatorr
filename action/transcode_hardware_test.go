package action

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

func hardwareTestCapabilities() transcode.WorkerCapabilities {
	caps := rulesTestCapabilities()
	caps.HardwareSearch = true
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
	hw, err := modePolicyWithHardware("size", false, 0, rules)
	if err != nil {
		t.Fatal(err)
	}
	if hw.Digest == base.Digest || hw.Version != SizeModePolicyVersion || hw.Size.Hardware == nil || hw.Size.Hardware == rules {
		t.Fatalf("hardware snapshot %+v", hw.Size)
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
		if def.HardwareSearch == nil || def.HardwareSearch.MarginPercent != 15 || len(def.Candidates) != 18 {
			t.Fatalf("depth %d: default request %+v with %d candidates", depth, def.HardwareSearch, len(def.Candidates))
		}
		req := hardwareRequest(t, depth, 45*60, map[string]any{"mode": "size", "allow_hardware": true, "hardware_margin_percent": 20}, hardwareTestCapabilities())
		if req.HardwareSearch == nil || req.HardwareSearch.MarginPercent != 20 || req.AutomaticSearch == nil {
			t.Fatalf("depth %d: rules %+v", depth, req.HardwareSearch)
		}
		if err := transcode.ValidateBenchmarkRequest(req); err != nil {
			t.Fatalf("depth %d: %v", depth, err)
		}
		var hardware []transcode.BenchmarkCandidate
		x265 := 0
		for _, c := range req.Candidates {
			if strings.HasPrefix(c.ID, hardwareCandidatePrefix) {
				hardware = append(hardware, c)
			} else if transcode.BenchmarkCandidateVideoCodec(c) == transcode.VideoCodecLibX265 {
				x265++
			}
		}
		// The x265 ladder and its fallback families are untouched.
		if x265 != 14 || len(hardware) != 4 {
			t.Fatalf("depth %d: %d x265 and %d hardware candidates", depth, x265, len(hardware))
		}
		for i, c := range hardware {
			wantQuality := []int{45, 50, 55, 60}[i]
			if c.ID != fmt.Sprintf("vt-cand_q%d", wantQuality) || c.Quality != wantQuality {
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
