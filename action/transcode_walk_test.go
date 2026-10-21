package action

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

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

func TestAllowSlowPresetInput(t *testing.T) {
	ec := &ExecutionContext{InstanceID: "default", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
	def, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	ec = &ExecutionContext{InstanceID: "slow", Inputs: map[string]any{"mode": "size", "allow_slow_preset": true}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
	slow, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	// The opt-in is its own frozen snapshot.
	if got := strings.Join(slow.Size.AutomaticSearch.QualityFallbackPresets, ","); got != "medium,slow" || slow.Digest == def.Digest {
		t.Fatalf("slow opt-in presets %q digest %s (default %s)", got, slow.Digest, def.Digest)
	}
	if got := strings.Join(def.Size.AutomaticSearch.QualityFallbackPresets, ","); got != "medium" {
		t.Fatalf("default presets %q", got)
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
func TestPreWalkSnapshotsKeepTheirDigest(t *testing.T) {
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
		if frozen.OperationBudgetSeconds != modeOperationBudgetSeconds {
			t.Fatalf("%s: budget %d", name, frozen.OperationBudgetSeconds)
		}
	}
}
