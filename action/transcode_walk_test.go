package action

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// walkTestCapabilities is a worker that also takes the quality walk.
func walkTestCapabilities() transcode.WorkerCapabilities {
	caps := gridTestCapabilities()
	caps.AutomaticQualityWalk = true
	return caps
}

// rehashPolicy recomputes a test snapshot's digest the way freezing does.
func rehashPolicy(p ModePolicySnapshot) ModePolicySnapshot {
	p.Digest = ""
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	p.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return p
}

// preWalkPolicy is a size v3 snapshot as it was frozen before the quality walk:
// CRF 16/18/20 for quality at medium and then slow, no refinement, banding
// measured with VMAF, and the operation budget in the snapshot.
func preWalkPolicy(p ModePolicySnapshot) ModePolicySnapshot {
	size := *p.Size
	size.FallbackQualityValues = []int{16, 18, 20}
	if size.AutomaticSearch != nil {
		rules := *size.AutomaticSearch
		rules.QualityFallbackPresets = []string{"medium", "slow"}
		rules.QualityWalk = false
		rules.RefinementQualityValues = nil
		rules.DeferBanding = false
		size.AutomaticSearch = &rules
	}
	p.Size = &size
	p.OperationBudgetSeconds = modeOperationBudgetSeconds
	return rehashPolicy(p)
}

// walkRequest builds the size request for a 45 minute episode under the given
// operation inputs.
func walkRequest(t *testing.T, inputs map[string]any, caps transcode.WorkerCapabilities) *transcode.BenchmarkRequest {
	t.Helper()
	ec := &ExecutionContext{InstanceID: "walk", Inputs: inputs, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
	policy, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", ExpectedBitDepth: 8, AudioMode: policy.Profile.Audio.Mode}
	if err := preserveSourceBitDepth(plan, 8, true); err != nil {
		t.Fatal(err)
	}
	if err := setMain10(plan); err != nil {
		t.Fatal(err)
	}
	ec.State["plan"] = plan
	rep := &mediainspect.DetailedReport{DurationSec: 45 * 60, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 23.976, BitRate: 10000000}}}
	req, err := buildBenchmarkRequest(ec, "/media/show/show S01E04.mkv", rep, policy.Profile.Optimization, caps)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func candidateList(req *transcode.BenchmarkRequest) string {
	var out []string
	for _, c := range req.Candidates {
		if c.VideoCodec == transcode.VideoCodecHEVCVideoToolbox {
			continue
		}
		out = append(out, fmt.Sprintf("%s:%s", c.ID, c.Preset))
	}
	return strings.Join(out, ",")
}

// A worker with the walk gets CRF 14-21 at medium, the refinement rungs between
// the ladder's steps, and slow only for the savings fallback; asking for the
// slow preset adds its quality rungs. Every shape stays inside the caps.
func TestQualityWalkRequest(t *testing.T) {
	const initial = "cand_crf22:medium,cand_crf24:medium,cand_crf26:medium,cand_crf28:medium,cand_crf30:medium,"
	const medium = "fallback-quality-medium-cand_crf14:medium,fallback-quality-medium-cand_crf15:medium,fallback-quality-medium-cand_crf16:medium,fallback-quality-medium-cand_crf17:medium," +
		"fallback-quality-medium-cand_crf18:medium,fallback-quality-medium-cand_crf19:medium,fallback-quality-medium-cand_crf20:medium,fallback-quality-medium-cand_crf21:medium,"
	const slowQuality = "fallback-quality-cand_crf14:slow,fallback-quality-cand_crf15:slow,fallback-quality-cand_crf16:slow,fallback-quality-cand_crf17:slow," +
		"fallback-quality-cand_crf18:slow,fallback-quality-cand_crf19:slow,fallback-quality-cand_crf20:slow,fallback-quality-cand_crf21:slow,"
	const savings = "fallback-savings-cand_crf24:slow,fallback-savings-cand_crf27:slow,fallback-savings-cand_crf30:slow,"
	const refine = "fallback-refine-cand_crf23:medium,fallback-refine-cand_crf25:medium,fallback-refine-cand_crf27:medium,fallback-refine-cand_crf29:medium"
	for name, tc := range map[string]struct {
		inputs map[string]any
		want   string
		count  int
	}{
		"default":     {map[string]any{"mode": "size", "allow_hardware": false}, initial + medium + savings + refine, 20},
		"slow opt-in": {map[string]any{"mode": "size", "allow_hardware": false, "allow_slow_preset": true}, initial + medium + slowQuality + savings + refine, 28},
	} {
		req := walkRequest(t, tc.inputs, walkTestCapabilities())
		if got := candidateList(req); got != tc.want || len(req.Candidates) != tc.count {
			t.Fatalf("%s: %d candidates\n got %s\nwant %s", name, len(req.Candidates), got, tc.want)
		}
		r := req.AutomaticSearch
		if r == nil || !r.QualityWalk || !r.DeferBanding || fmt.Sprint(r.RefinementQualityValues) != "[23 25 27 29]" {
			t.Fatalf("%s: rules %+v", name, r)
		}
		if err := transcode.ValidateBenchmarkRequest(req); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(req.Candidates) > transcode.MaxAutomaticWalkCandidates {
			t.Fatalf("%s: %d candidates exceed the cap", name, len(req.Candidates))
		}
	}
	// With the media engine in the search, the largest request stays under the
	// hardware cap.
	caps := walkTestCapabilities()
	caps.HardwareSearch, caps.HardwareFallbackSearch, caps.HardwareSelectionMargin, caps.SearchContinuation = true, true, true, true
	caps.Encoders[transcode.VideoCodecHEVCVideoToolbox] = true
	for _, slow := range []bool{false, true} {
		req := walkRequest(t, map[string]any{"mode": "size", "allow_slow_preset": slow}, caps)
		if req.HardwareSearch == nil || len(req.Candidates) > transcode.MaxAutomaticWalkHardwareCandidates {
			t.Fatalf("slow=%v: %d candidates, hardware %+v", slow, len(req.Candidates), req.HardwareSearch)
		}
		if err := transcode.ValidateBenchmarkRequest(req); err != nil {
			t.Fatalf("slow=%v: %v", slow, err)
		}
	}
}

// A worker that does not advertise the walk gets the earlier request, which its
// strict decoder and candidate cap accept: CRF 16/18/20, no refinement rungs and
// no walk fields. The slow preset it is asked for still applies.
func TestQualityWalkDegradesForOlderWorkers(t *testing.T) {
	old := gridTestCapabilities()
	for name, tc := range map[string]struct {
		inputs map[string]any
		want   string
	}{
		"default": {map[string]any{"mode": "size", "allow_hardware": false}, "cand_crf22:medium,cand_crf24:medium,cand_crf26:medium,cand_crf28:medium,cand_crf30:medium," +
			"fallback-quality-medium-cand_crf16:medium,fallback-quality-medium-cand_crf18:medium,fallback-quality-medium-cand_crf20:medium," +
			"fallback-savings-cand_crf24:slow,fallback-savings-cand_crf27:slow,fallback-savings-cand_crf30:slow"},
		"slow opt-in": {map[string]any{"mode": "size", "allow_hardware": false, "allow_slow_preset": true}, "cand_crf22:medium,cand_crf24:medium,cand_crf26:medium,cand_crf28:medium,cand_crf30:medium," +
			"fallback-quality-medium-cand_crf16:medium,fallback-quality-medium-cand_crf18:medium,fallback-quality-medium-cand_crf20:medium," +
			"fallback-quality-cand_crf16:slow,fallback-quality-cand_crf18:slow,fallback-quality-cand_crf20:slow," +
			"fallback-savings-cand_crf24:slow,fallback-savings-cand_crf27:slow,fallback-savings-cand_crf30:slow"},
	} {
		req := walkRequest(t, tc.inputs, old)
		if got := candidateList(req); got != tc.want {
			t.Fatalf("%s:\n got %s\nwant %s", name, got, tc.want)
		}
		r := req.AutomaticSearch
		if r == nil || r.QualityWalk || r.DeferBanding || len(r.RefinementQualityValues) != 0 {
			t.Fatalf("%s: walk fields sent to a worker without the capability: %+v", name, r)
		}
		raw, _ := json.Marshal(r)
		for _, field := range []string{"quality_walk", "refinement_quality_values", "defer_banding"} {
			if strings.Contains(string(raw), field) {
				t.Fatalf("%s: %s in %s", name, field, raw)
			}
		}
		if err := transcode.ValidateBenchmarkRequest(req); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(req.Candidates) > transcode.MaxAutomaticRulesCandidates {
			t.Fatalf("%s: %d candidates exceed the earlier cap", name, len(req.Candidates))
		}
	}
}

func TestAllowSlowPresetInput(t *testing.T) {
	def, err := legacyModePolicy("size", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(def.Size.AutomaticSearch.QualityFallbackPresets, ","); got != "medium" {
		t.Fatalf("default presets %q", got)
	}
	ec := &ExecutionContext{Engine: legacySearch(&Engine{}), InstanceID: "slow", Inputs: map[string]any{"mode": "size", "allow_slow_preset": true}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
	slow, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(slow.Size.AutomaticSearch.QualityFallbackPresets, ","); got != "medium,slow" || slow.Digest == def.Digest {
		t.Fatalf("slow opt-in presets %q digest %s (default %s)", got, slow.Digest, def.Digest)
	}
	// Pinned: the opt-in is its own frozen snapshot.
	if slow.Digest != "sha256:f681e347c8db8a8d8a7823f8d0260bdf322c0b6b60461ee3f8fa43f046b12d70" {
		t.Fatalf("slow opt-in digest %s", slow.Digest)
	}
	// A stored snapshot keeps the choice it was frozen with, whatever the inputs say now.
	stored := map[string]any{}
	raw, _ := json.Marshal(slow)
	_ = json.Unmarshal(raw, &stored)
	ec = &ExecutionContext{InstanceID: "slow", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"mode_policy": stored}}
	if frozen, err := freezeModePolicy(ec); err != nil || frozen.Digest != slow.Digest {
		t.Fatalf("stored slow snapshot %+v %v", frozen, err)
	}
	for name, inputs := range map[string]map[string]any{
		"not a bool":         {"mode": "size", "allow_slow_preset": "yes"},
		"quality mode":       {"mode": "quality", "allow_slow_preset": true},
		"compact mode":       {"mode": "compact", "allow_slow_preset": true},
		"x265_preserve mode": {"mode": "x265_preserve", "allow_slow_preset": true},
	} {
		if err := ValidateHardwareInputs(getString(inputs, "mode"), inputs); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		ec := &ExecutionContext{InstanceID: "slow", Inputs: inputs, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
		if _, err := freezeModePolicy(ec); err == nil {
			t.Fatalf("%s: froze", name)
		}
	}
	// false is the same as leaving it out; non-size modes accept false.
	for _, inputs := range []map[string]any{{"mode": "size", "allow_slow_preset": false}, {"mode": "quality", "allow_slow_preset": false}} {
		if err := ValidateHardwareInputs(getString(inputs, "mode"), inputs); err != nil {
			t.Fatalf("%v: %v", inputs, err)
		}
	}
	// A goal change carries the choice into the child inputs and drops it elsewhere.
	inputs := map[string]any{"allow_slow_preset": true}
	applyGoalPolicyInputs(inputs, slow)
	if inputs["allow_slow_preset"] != true {
		t.Fatalf("goal inputs lost the slow opt-in: %v", inputs)
	}
	inputs = map[string]any{"allow_slow_preset": true}
	applyGoalPolicyInputs(inputs, def)
	if _, kept := inputs["allow_slow_preset"]; kept {
		t.Fatalf("goal inputs kept a stale slow opt-in: %v", inputs)
	}
	quality, _ := legacyModePolicy("quality", false)
	inputs = map[string]any{"allow_slow_preset": true}
	applyGoalPolicyInputs(inputs, quality)
	if _, kept := inputs["allow_slow_preset"]; kept {
		t.Fatalf("a quality goal kept the slow opt-in: %v", inputs)
	}
}

// Snapshots frozen before the walk keep their digests and the search they were
// frozen with: CRF 16/18/20 at medium and then slow, no refinement, banding with
// VMAF, and the operation budget in the snapshot. A worker that has the walk does
// not run it for them.
func TestPreWalkSnapshotsKeepTheirDigestAndSearch(t *testing.T) {
	media, err := modeHardwareInput("size", map[string]any{"mode": "size"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		hardware *transcode.HardwareSearchRules
		margin   bool
		digest   string
	}{
		"margin, no media engine": {nil, true, "sha256:6e519947ac9039f9dbfe36a8beee2ebdd891f0a02d28b5e5b776ddc7f8928ba8"},
		"margin, media engine":    {withExtraMargin(media), true, "sha256:206ca220dda73866a0f7b9721a8a2bb93769b447cf7cb6f53347b2d3208d03e7"},
		"no media engine":         {nil, false, "sha256:5ca0b1d812f8e814a839209ad6cc4ab55169bba5af9e58d4fb15aaff96c30976"},
		"media engine":            {media, false, "sha256:1f903e4291fdce6d0257446dcc809a87ad819a9d36bd3f16ff6d74c28f881903"},
	} {
		p, err := legacyModePolicyWithHardware("size", false, 0, tc.hardware)
		if err != nil {
			t.Fatal(err)
		}
		legacy := preWalkPolicy(p)
		if tc.margin {
			legacy = marginEraPolicy(t, tc.hardware)
		}
		if legacy.Digest != tc.digest {
			t.Fatalf("%s: pre-walk digest %s, want %s", name, legacy.Digest, tc.digest)
		}
		if p.Digest == legacy.Digest {
			t.Fatalf("%s: new snapshots share the pre-walk digest", name)
		}
		stored := map[string]any{}
		raw, _ := json.Marshal(legacy)
		_ = json.Unmarshal(raw, &stored)
		ec := &ExecutionContext{InstanceID: "pre-walk", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"mode_policy": stored, "original_sha256": strings.Repeat("a", 64)}}
		frozen, err := freezeModePolicy(ec)
		if err != nil || frozen.Digest != tc.digest {
			t.Fatalf("%s: stored snapshot %+v %v", name, frozen, err)
		}
		families := policyAutomaticFallbacks(frozen, walkTestCapabilities())
		if r := families.Rules; r == nil || r.QualityWalk || r.DeferBanding || len(r.RefinementQualityValues) != 0 || strings.Join(r.QualityFallbackPresets, ",") != "medium,slow" {
			t.Fatalf("%s: a pre-walk snapshot runs the walk: %+v", name, families.Rules)
		}
		if fmt.Sprint(families.Quality) != "[16 18 20]" {
			t.Fatalf("%s: quality rungs %v", name, families.Quality)
		}
		if frozen.OperationBudgetSeconds != modeOperationBudgetSeconds {
			t.Fatalf("%s: budget %d", name, frozen.OperationBudgetSeconds)
		}
		req := hardwareRequestWith(t, ec, 8, 45*60, walkTestCapabilities())
		if len(req.Candidates) > transcode.MaxAutomaticHardwareCandidates {
			t.Fatalf("%s: %d candidates", name, len(req.Candidates))
		}
		for _, c := range req.Candidates {
			if strings.HasPrefix(c.ID, transcode.RefineCandidatePrefix) || c.Quality == 14 || c.Quality == 15 {
				t.Fatalf("%s: pre-walk snapshot got %s", name, c.ID)
			}
		}
		if err := transcode.ValidateBenchmarkRequest(req); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// New size snapshots have no operation budget: it never ended a search.
	def, _ := legacyModePolicy("size", false)
	if def.OperationBudgetSeconds != 0 {
		t.Fatalf("new size snapshot carries a %d s operation budget", def.OperationBudgetSeconds)
	}
	quality, _ := legacyModePolicy("quality", false)
	if quality.OperationBudgetSeconds != modeOperationBudgetSeconds {
		t.Fatalf("quality snapshot budget %d", quality.OperationBudgetSeconds)
	}
}
