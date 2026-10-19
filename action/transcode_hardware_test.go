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
	// Adding the higher-quality fallback rungs gave new snapshots the digest
	// below. Snapshots frozen with the media engine before it carry no such
	// field and still re-hash to sha256:bc2e370b...e03 (see
	// TestHardwarePolicyWithoutFallbackRungsKeepsItsDigest).
	if hw.Digest != "sha256:1b73a704427a9c6a89a9ae36079422c842de9cbd799fa48f634d54d1a3b7ead6" {
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
	if p.Digest != "sha256:bc2e370bf9f26965f7faa121693c299d1fe2e6c2d02b37aee033dc369cb12e03" {
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
	tooMany := *req
	tooMany.Candidates = append(append([]transcode.BenchmarkCandidate(nil), req.Candidates...), req.Candidates[0])
	if transcode.ValidateBenchmarkRequest(&tooMany) == nil {
		t.Fatalf("%d candidates accepted", len(tooMany.Candidates))
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

// A goal change replaces the policy of a batch item, so the parent's media
// engine inputs must not leak into that item's child: they would fail preflight
// for every goal but smaller files, or contradict a size attempt that turned
// the engine off.
func TestGoalChangeChildInputsAgreeWithTheFrozenPolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings map[string]any
		allow    any
		margin   any
	}{
		{"quality", map[string]any{"mode": "quality"}, nil, nil},
		{"x265_preserve", map[string]any{"mode": "x265_preserve"}, nil, nil},
		{"compact", map[string]any{"mode": "compact"}, nil, nil},
		{"size, engine off", map[string]any{"mode": "size", "allow_hardware": false}, false, nil},
		{"size, new margin", map[string]any{"mode": "size", "allow_hardware": true, "hardware_margin_percent": float64(30)}, true, float64(30)},
		{"size, default", map[string]any{"mode": "size"}, true, float64(15)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := setupTestStore(t)
			defer st.Close()
			e := NewEngine(EngineDeps{Store: st})
			parentInputs := map[string]any{"mode": "size", "allow_hardware": true, "hardware_margin_percent": float64(25)}
			seedActionInstance(t, st, "goal-parent", "transcode_batch", StatusCompleted, 3, "", parentInputs, map[string]any{})
			parent, _ := st.GetActionInstance("goal-parent")
			item := store.TranscodeBatchItem{BatchID: parent.ID, ItemKey: "file", FilePath: "/media/source.mkv", Decision: "transcode", Status: "failed", Profile: "mode-size"}
			if err := st.CreateTranscodeBatchItem(item); err != nil {
				t.Fatal(err)
			}
			items, _ := st.ListTranscodeBatchItems(parent.ID)
			if _, err := e.ReconfigureBatch(context.Background(), parent.ID, "all", "", BatchSelectionVersion(parent, items), "", "goal-change", tc.settings); err != nil {
				t.Fatal(err)
			}
			updated, _ := st.GetActionInstance(parent.ID)
			ec := parseExecutionContext(updated, e)
			row, _ := stateObject(ec.State, "batch_item_settings")["file"].(map[string]any)
			if row == nil {
				t.Fatal("goal change was not recorded", ec.State)
			}
			// What processBatchItem hands to the child: the parent's inputs, the row's
			// goal and the hardware inputs that agree with the row's frozen policy.
			child := map[string]any{"path": item.FilePath, "mode": row["mode"]}
			for _, key := range []string{"allow_hardware", "hardware_margin_percent"} {
				child[key] = parentInputs[key]
			}
			if tc.name != "size, default" && tc.name != "size, new margin" && tc.name != "size, engine off" {
				if err := validateModeInputs(child); err == nil {
					t.Fatal("expected the unadjusted parent inputs to be refused for", tc.name)
				}
			}
			applyGoalHardwareInputs(child, row["mode_policy"])
			if err := validateModeInputs(child); err != nil {
				t.Fatal("child would fail preflight:", err)
			}
			if child["allow_hardware"] != tc.allow || child["hardware_margin_percent"] != tc.margin {
				t.Fatalf("child inputs %v do not match the frozen policy", child)
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
