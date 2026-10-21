package action

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// gridEraPolicy is a size v3 snapshot as it was frozen while the holdouts sat
// on the grid edges: the same policy without the grid verification rules.
func gridEraPolicy(t *testing.T, hardware *transcode.HardwareSearchRules) ModePolicySnapshot {
	t.Helper()
	p := marginEraPolicy(t, hardware)
	size := *p.Size
	rules := *size.AutomaticSearch
	rules.HoldoutWindows = transcode.HoldoutWindowsFinalGridEdges
	rules.VerifyUntilExhausted = false
	rules.FinalFreshWindows = 0
	size.AutomaticSearch = &rules
	p.Size = &size
	p.Digest = ""
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	p.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return p
}

// marginEraPolicy is a size v3 snapshot as #182 froze it: verification on the
// final grid, with the 1.5 selection margin, and from before the quality walk
// (see preWalkPolicy). hardware is frozen as given, so a
// caller that wants the extra hardware margin passes it.
func marginEraPolicy(t *testing.T, hardware *transcode.HardwareSearchRules) ModePolicySnapshot {
	t.Helper()
	p, err := legacyModePolicyWithHardware("size", false, 0, hardware)
	if err != nil {
		t.Fatal(err)
	}
	p = preWalkPolicy(p)
	size := *p.Size
	rules := *size.AutomaticSearch
	rules.SelectionMarginVMAF = sizeEdgeSelectionMarginVMAF
	size.AutomaticSearch = &rules
	p.Size = &size
	p.Digest = ""
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	p.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return p
}

// withExtraMargin is the hardware rules as #181 froze them: the extra VMAF
// selection margin of a VideoToolbox result.
func withExtraMargin(rules *transcode.HardwareSearchRules) *transcode.HardwareSearchRules {
	c := rules.Clone()
	c.ExtraSelectionMarginVMAF = sizeEdgeHardwareExtraMarginVMAF
	return c
}

func TestSizePolicyFreezesGridVerification(t *testing.T) {
	p, err := legacyModePolicy("size", false)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Size.AutomaticSearch
	if r.HoldoutWindows != transcode.HoldoutWindowsFinalGrid || !r.VerifyUntilExhausted || r.FinalFreshWindows != 2 || r.SelectionMarginVMAF != 0 || r.Validate() != nil {
		t.Fatalf("rules %+v", r)
	}
	// The quality limits and the ladder are untouched.
	v := p.Profile.Optimization.Quality.VMAF
	if v.Minimum != 88 || *v.P5Minimum != 84 || *v.WorstWindowMinimum != 85 || p.MinSavingsPercent != 15 {
		t.Fatalf("limits changed: %+v", v)
	}
}

// Snapshots frozen while the holdouts sat on the grid edges re-hash to the
// digests they were frozen under.
func TestGridEraSnapshotsKeepTheirDigest(t *testing.T) {
	earlier := &transcode.HardwareSearchRules{MarginPercent: 15, QualityValues: []int{45, 50, 55, 60}, GOPSize: 240, BFrames: 1}
	beforeMargin := &transcode.HardwareSearchRules{MarginPercent: 15, QualityValues: []int{45, 50, 55, 60}, GOPSize: 240, BFrames: 1, FallbackQualityValues: []int{65, 70, 75}}
	current, err := modeHardwareInput("size", map[string]any{"mode": "size"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		hardware *transcode.HardwareSearchRules
		digest   string
	}{
		"no media engine":           {nil, "sha256:33812f4b39f3aff6852beb34dc5ceffdaeb3504f71d00cd17555c89fbf4c80ee"},
		"media engine":              {withExtraMargin(current), "sha256:e5ec773de13a1eab8d1b3e9bc087f0583316221625cc310d463fca137ecd4264"},
		"media engine, no margin":   {beforeMargin, "sha256:1b73a704427a9c6a89a9ae36079422c842de9cbd799fa48f634d54d1a3b7ead6"},
		"media engine, earlier set": {earlier, "sha256:bc2e370bf9f26965f7faa121693c299d1fe2e6c2d02b37aee033dc369cb12e03"},
	} {
		legacy := gridEraPolicy(t, tc.hardware)
		if legacy.Digest != tc.digest {
			t.Fatalf("%s: grid-era digest %s, want %s", name, legacy.Digest, tc.digest)
		}
		fresh, _ := legacyModePolicyWithHardware("size", false, 0, tc.hardware)
		if fresh.Digest == legacy.Digest {
			t.Fatalf("%s: new snapshots share the grid-era digest", name)
		}
		stored := map[string]any{}
		raw, _ := json.Marshal(legacy)
		_ = json.Unmarshal(raw, &stored)
		ec := &ExecutionContext{InstanceID: "grid-era", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"mode_policy": stored, "original_sha256": strings.Repeat("a", 64)}}
		frozen, err := freezeModePolicy(ec)
		if err != nil || frozen.Digest != tc.digest {
			t.Fatalf("%s: stored snapshot %+v %v", name, frozen, err)
		}
	}
}

func TestFinalValidationFreshWindows(t *testing.T) {
	ec := &ExecutionContext{InstanceID: "fresh", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
	grid, fresh := sizeFinalWindowsWithFresh(ec, 3700, 2)
	if len(grid) != 7 || len(fresh) != 2 {
		t.Fatalf("grid %d fresh %d", len(grid), len(fresh))
	}
	for _, f := range fresh {
		for _, g := range grid {
			if f.StartSeconds < g.StartSeconds+g.DurationSeconds && g.StartSeconds < f.StartSeconds+f.DurationSeconds {
				t.Fatalf("fresh window %+v overlaps grid window %+v", f, g)
			}
		}
		if f.Index < len(grid) {
			t.Fatalf("fresh window reuses a grid index: %+v", f)
		}
	}
	if _, none := sizeFinalWindowsWithFresh(ec, 3700, 0); none != nil {
		t.Fatalf("fresh windows without the rule: %+v", none)
	}
}

// Snapshots frozen with verification on the final grid and the 1.5 selection
// margin (and, for the media engine, the extra 2) re-hash to the digests they
// were frozen under.
func TestMarginEraSnapshotsKeepTheirDigest(t *testing.T) {
	earlier := &transcode.HardwareSearchRules{MarginPercent: 15, QualityValues: []int{45, 50, 55, 60}, GOPSize: 240, BFrames: 1}
	current, err := modeHardwareInput("size", map[string]any{"mode": "size"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		hardware *transcode.HardwareSearchRules
		digest   string
	}{
		"no media engine":           {nil, "sha256:6e519947ac9039f9dbfe36a8beee2ebdd891f0a02d28b5e5b776ddc7f8928ba8"},
		"media engine":              {withExtraMargin(current), "sha256:206ca220dda73866a0f7b9721a8a2bb93769b447cf7cb6f53347b2d3208d03e7"},
		"media engine, earlier set": {earlier, "sha256:ab4a33d9aa290b79b3eeaef9a6b3c17afda00376e9defade7d71e9ce65cb6128"},
	} {
		legacy := marginEraPolicy(t, tc.hardware)
		if legacy.Digest != tc.digest {
			t.Fatalf("%s: margin-era digest %s, want %s", name, legacy.Digest, tc.digest)
		}
		fresh, _ := legacyModePolicyWithHardware("size", false, 0, current)
		if tc.hardware == nil {
			fresh, _ = legacyModePolicyWithHardware("size", false, 0, nil)
		}
		if fresh.Digest == legacy.Digest || fresh.Size.AutomaticSearch.SelectionMarginVMAF != 0 {
			t.Fatalf("%s: new snapshots share the margin-era digest or carry a margin: %+v", name, fresh.Size.AutomaticSearch)
		}
		stored := map[string]any{}
		raw, _ := json.Marshal(legacy)
		_ = json.Unmarshal(raw, &stored)
		ec := &ExecutionContext{InstanceID: "margin-era", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"mode_policy": stored, "original_sha256": strings.Repeat("a", 64)}}
		frozen, err := freezeModePolicy(ec)
		if err != nil || frozen.Digest != tc.digest {
			t.Fatalf("%s: stored snapshot %+v %v", name, frozen, err)
		}
	}
}
