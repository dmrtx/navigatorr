package action

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// New size operations allow the media engine unless they turn it off, and that
// is its own frozen snapshot. Without it the size policy is byte-for-byte what
// it was, so a snapshot frozen earlier keeps its digest and its behaviour.
func TestSizePolicyHardwareDefaultsOnAndFreezes(t *testing.T) {
	base, err := legacyModePolicy("size", false)
	if err != nil {
		t.Fatal(err)
	}
	same, err := legacyModePolicyWithHardware("size", false, 0, nil)
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
	// New snapshots verify on the final grid, so a hardware result needs no
	// more than the plain limits; the extra 2 is kept only for the edge
	// holdouts (sizeEdgeHardwareExtraMarginVMAF).
	if rules.ExtraSelectionMarginVMAF != 0 || sizeEdgeHardwareExtraMarginVMAF != 2 {
		t.Fatalf("default hardware selection margin %+v", rules)
	}
	if rules.MarginPercent != 15 || rules.GOPSize != 240 || rules.BFrames != 1 || len(rules.QualityValues) != 4 || rules.QualityValues[0] != 45 || rules.QualityValues[3] != 60 {
		t.Fatalf("default rules %+v", rules)
	}
	if got := fmt.Sprint(rules.FallbackQualityValues); got != "[65 70 75]" {
		t.Fatalf("default fallback rungs %s", got)
	}
	hw, err := legacyModePolicyWithHardware("size", false, 0, rules)
	if err != nil {
		t.Fatal(err)
	}
	if hw.Digest == base.Digest || hw.Version != SizeModePolicyVersion || hw.Size.Hardware == nil || hw.Size.Hardware == rules {
		t.Fatalf("hardware snapshot %+v", hw.Size)
	}
	// The default snapshot is a frozen artifact: changing its rules, their order
	// or their defaults changes the digest of every new size operation.
	// Adding the higher-quality fallback rungs gave new snapshots a digest;
	// snapshots frozen with the media engine before it carry no such field and
	// still re-hash to sha256:bc2e370b...e03 (see
	// TestHardwarePolicyWithoutFallbackRungsKeepsItsDigest). The stricter
	// hardware selection margin gave new snapshots the digest below; those
	// frozen with the rungs but without it still re-hash to sha256:1b73a704...ad6
	// and keep selecting with the margin they were frozen with (see
	// TestHardwarePolicyWithoutExtraMarginKeepsItsDigest). Verifying on the
	// final grid gave a new digest; snapshots frozen with the margin but
	// before it re-hash to sha256:e5ec773d...4264 (see
	// TestGridEraSnapshotsKeepTheirDigestAndSearch). Removing the selection
	// margins for grid verification gave the digest sha256:1f903e42...1903; the
	// snapshots that carried them still re-hash to sha256:206ca220...03e7 (see
	// TestMarginEraSnapshotsKeepTheirDigestAndMargin). The x265 quality walk
	// gave the digest below; snapshots frozen without the margins but before
	// it re-hash to sha256:1f903e42...1903 (see
	// TestPreWalkSnapshotsKeepTheirDigestAndSearch).
	if hw.Digest != "sha256:ac41a174311afba39ec13ba20d9df6cfab95555097805812a4e4e571159180b4" {
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

// Rules frozen with the media engine before the fallback rungs existed have no
// such field: they re-hash to the digest they were frozen under and stay on the
// ladder alone.
// A snapshot frozen with the fallback rungs but before the stricter hardware
// selection margin keeps its digest and its margin: only new operations select
// hardware results with the extra margin.
func TestHardwarePolicyWithoutExtraMarginKeepsItsDigest(t *testing.T) {
	rules := &transcode.HardwareSearchRules{MarginPercent: 15, QualityValues: []int{45, 50, 55, 60}, GOPSize: 240, BFrames: 1, FallbackQualityValues: []int{65, 70, 75}}
	// Frozen before the grid rules too, so it is the grid-era snapshot.
	p := gridEraPolicy(t, rules)
	if p.Digest != "sha256:1b73a704427a9c6a89a9ae36079422c842de9cbd799fa48f634d54d1a3b7ead6" {
		t.Fatalf("the earlier media engine snapshot changed digest: %s", p.Digest)
	}
	if hb, _ := json.Marshal(p.Size.Hardware); strings.Contains(string(hb), "extra_selection_margin") {
		t.Fatalf("frozen hardware rules mention the extra margin: %s", hb)
	}
	stored := map[string]any{}
	b, _ := json.Marshal(p)
	_ = json.Unmarshal(b, &stored)
	ec := &ExecutionContext{InstanceID: "hw-earlier-margin", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"mode_policy": stored}}
	kept, err := freezeModePolicy(ec)
	if err != nil || kept.Digest != p.Digest || kept.Size.Hardware.ExtraSelectionMarginVMAF != 0 {
		t.Fatalf("an earlier snapshot picked up the extra margin: %+v %v", kept, err)
	}
}

func mustHardwareRules(t *testing.T) *transcode.HardwareSearchRules {
	t.Helper()
	rules, err := modeHardwareInput("size", map[string]any{})
	if err != nil || rules == nil {
		t.Fatalf("rules %+v %v", rules, err)
	}
	return rules
}

func TestHardwarePolicyWithoutFallbackRungsKeepsItsDigest(t *testing.T) {
	rules := &transcode.HardwareSearchRules{MarginPercent: 15, QualityValues: []int{45, 50, 55, 60}, GOPSize: 240, BFrames: 1}
	p := marginEraPolicy(t, rules)
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
			policy, err := legacyModePolicyWithHardware(getString(tc.inputs, "mode"), false, maxHeight, hardware)
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
