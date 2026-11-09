package transcode

import (
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
)

func TestCompactTargetSize(t *testing.T) {
	cases := []struct {
		w, h, max, tw, th int
	}{
		{3840, 2160, 1080, 1920, 1080},
		{3840, 1600, 1080, 1920, 800},  // scope, width bound
		{3840, 1608, 1080, 1920, 804},  // rounds to even
		{2880, 2160, 1080, 1440, 1080}, // 4:3, height bound
		{3840, 2160, 2160, 3840, 2160}, // already fits
		{1920, 1080, 1080, 1920, 1080}, // never upscales
		{1280, 720, 1080, 1280, 720},
		{3840, 2160, 720, 1280, 720},
		{3840, 2160, 1440, 2560, 1440},
		// A source no taller than the limit keeps its size, however wide.
		{4096, 1716, 2160, 4096, 1716},
		{4096, 2160, 2160, 4096, 2160},
		{2560, 1080, 1080, 2560, 1080},
		{1998, 1080, 1080, 1998, 1080},
		{1440, 1080, 1080, 1440, 1080},
	}
	for _, c := range cases {
		tw, th, err := CompactTargetSize(c.w, c.h, c.max)
		if err != nil || tw != c.tw || th != c.th {
			t.Errorf("CompactTargetSize(%d,%d,%d) = %d,%d,%v want %d,%d", c.w, c.h, c.max, tw, th, err, c.tw, c.th)
		}
		if tw%2 != 0 || th%2 != 0 {
			t.Errorf("odd target %dx%d", tw, th)
		}
	}
	for _, bad := range [][3]int{{0, 1080, 1080}, {3840, 2160, 900}, {3840, 2160, 0}, {20000, 2160, 1080}} {
		if _, _, err := CompactTargetSize(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("CompactTargetSize%v accepted", bad)
		}
	}
}

func compactMD(maxLum string) *mediainspect.MasteringDisplayMetadata {
	return &mediainspect.MasteringDisplayMetadata{RedX: "34000/50000", RedY: "16000/50000", GreenX: "13250/50000", GreenY: "34500/50000", BlueX: "7500/50000", BlueY: "3000/50000", WhitePointX: "15635/50000", WhitePointY: "16450/50000", MinLuminance: "50/10000", MaxLuminance: maxLum}
}

func uhd() mediainspect.DetailedStream {
	return mediainspect.DetailedStream{Kind: "video", Width: 3840, Height: 2160, SampleAspect: "1:1"}
}

func TestNewCompactPlan(t *testing.T) {
	hdr10 := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeHDR10, Primaries: "bt2020", Matrix: "bt2020nc", MasteringDisplay: compactMD("10000000/10000"), ContentLightLevel: &mediainspect.ContentLightLevelMetadata{MaxCLL: 1000, MaxFALL: 400}}
	p, err := NewCompactPlan(hdr10, uhd(), 1080)
	if err != nil {
		t.Fatal(err)
	}
	if p.SourceSignal != mediainspect.SignalPQ || p.TargetWidth != 1920 || p.TargetHeight != 1080 || p.ToneMap == nil || p.ToneMap.Method != CompactToneMapPQ || p.ToneMap.SourcePeakNits != 1000 || p.ToneMap.TargetPeakNits != 100 {
		t.Fatalf("hdr10 plan %+v %+v", p, p.ToneMap)
	}

	// The mastering peak is used when MaxCLL is absent, clamped to 10000.
	noCLL := hdr10
	noCLL.ContentLightLevel = nil
	noCLL.MasteringDisplay = compactMD("40000000/10000")
	if p, err := NewCompactPlan(noCLL, uhd(), 1080); err != nil || p.ToneMap.SourcePeakNits != 4000 {
		t.Fatalf("mastering peak: %+v %v", p, err)
	}
	// Neither signalled: the BT.2390 default.
	pq10 := mediainspect.DynamicRange{Class: mediainspect.DynamicRangePQ10}
	if p, err := NewCompactPlan(pq10, uhd(), 2160); err != nil || p.ToneMap.SourcePeakNits != 1000 || p.TargetWidth != 3840 {
		t.Fatalf("pq10 plan: %+v %v", p, err)
	}

	hlg := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeHLG}
	if p, err := NewCompactPlan(hlg, uhd(), 1080); err != nil || p.SourceSignal != mediainspect.SignalHLG || p.ToneMap.Method != CompactToneMapHLG || p.ToneMap.SourcePeakNits != CompactHLGDisplayPeakNits {
		t.Fatalf("hlg plan: %+v %v", p, err)
	}

	dv81 := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeDolbyVision, BaseLayer: mediainspect.DynamicRangeHDR10, DolbyVision: &mediainspect.DolbyVisionInfo{Profile: 8, BLCompatibilityID: 1}}
	if p, err := NewCompactPlan(dv81, uhd(), 1080); err != nil || p.SourceSignal != mediainspect.SignalPQ || p.DolbyVisionProfile != 8 {
		t.Fatalf("dv 8.1 plan: %+v %v", p, err)
	}

	dv5 := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeDolbyVision, BaseLayer: "none", DolbyVision: &mediainspect.DolbyVisionInfo{Profile: 5}, Reason: "dolby_vision_profile_5_no_compatible_base_layer"}
	if _, err := NewCompactPlan(dv5, uhd(), 1080); err == nil || err.Error() != "dolby_vision_profile_5_no_compatible_base_layer" {
		t.Fatalf("dv5 must be rejected with its reason, got %v", err)
	}

	sdr := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeSDR, Primaries: "bt709", Matrix: "bt709"}
	if p, err := NewCompactPlan(sdr, uhd(), 1080); err != nil || p.ToneMap != nil || p.SourceSignal != mediainspect.SignalSDR || p.TargetHeight != 1080 {
		t.Fatalf("4k sdr plan: %+v %v", p, err)
	}
	hd := mediainspect.DetailedStream{Width: 1920, Height: 1080}
	if _, err := NewCompactPlan(sdr, hd, 1080); err == nil || err.Error() != "compact_not_needed" {
		t.Fatalf("1080p sdr at 1080 must be compact_not_needed, got %v", err)
	}
	wide := sdr
	wide.Primaries, wide.Matrix = "bt2020", "bt2020nc"
	if _, err := NewCompactPlan(wide, uhd(), 1080); err == nil || err.Error() != "compact_sdr_requires_bt709" {
		t.Fatalf("wide-gamut SDR must be rejected, got %v", err)
	}
	full := sdr
	full.Range = "pc"
	if _, err := NewCompactPlan(full, uhd(), 1080); err == nil || err.Error() != "compact_sdr_requires_limited_range" {
		t.Fatalf("full-range SDR must be rejected, got %v", err)
	}
	jpeg := uhd()
	jpeg.PixelFormat = "yuvj420p"
	if _, err := NewCompactPlan(sdr, jpeg, 1080); err == nil || err.Error() != "compact_sdr_requires_limited_range" {
		t.Fatalf("yuvj SDR must be rejected, got %v", err)
	}
	anamorphic := uhd()
	anamorphic.SampleAspect = "4:3"
	if _, err := NewCompactPlan(hdr10, anamorphic, 1080); err == nil || err.Error() != "compact_requires_square_pixels" {
		t.Fatalf("anamorphic must be rejected, got %v", err)
	}
	if _, err := NewCompactPlan(hdr10, uhd(), 900); err == nil {
		t.Fatal("unsupported max height accepted")
	}
}

func TestCompactPlanValidateEqualMatch(t *testing.T) {
	d := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeHDR10, MasteringDisplay: compactMD("10000000/10000"), ContentLightLevel: &mediainspect.ContentLightLevelMetadata{MaxCLL: 1000}}
	p, err := NewCompactPlan(d, uhd(), 1080)
	if err != nil {
		t.Fatal(err)
	}
	var nilPlan *CompactPlan
	if nilPlan.Validate() != nil || nilPlan.Clone() != nil || !nilPlan.Equal(nil) || nilPlan.Equal(p) {
		t.Fatal("nil plan semantics")
	}
	c := p.Clone()
	if !c.Equal(p) || c.ToneMap == p.ToneMap {
		t.Fatal("clone must be equal and deep")
	}
	if err := p.MatchSource(d, uhd()); err != nil {
		t.Fatal(err)
	}

	mutations := map[string]func(*CompactPlan){
		"version":        func(p *CompactPlan) { p.ChainVersion = "compact-sdr-v0" },
		"target":         func(p *CompactPlan) { p.TargetWidth = 1280 },
		"max height":     func(p *CompactPlan) { p.MaxHeight = 720 },
		"method":         func(p *CompactPlan) { p.ToneMap.Method = CompactToneMapHLG },
		"peak":           func(p *CompactPlan) { p.ToneMap.SourcePeakNits = 100 },
		"target peak":    func(p *CompactPlan) { p.ToneMap.TargetPeakNits = 203 },
		"no tone map":    func(p *CompactPlan) { p.ToneMap = nil },
		"signal":         func(p *CompactPlan) { p.SourceSignal = "linear" },
		"dv on non-dv":   func(p *CompactPlan) { p.DolbyVisionProfile = 8 },
		"sdr tone map":   func(p *CompactPlan) { p.SourceSignal = mediainspect.SignalSDR },
		"missing source": func(p *CompactPlan) { p.SourceClass = "" },
	}
	for name, mutate := range mutations {
		m := p.Clone()
		mutate(m)
		if m.Validate() == nil {
			t.Errorf("%s: invalid plan validated", name)
		}
		if m.Equal(p) {
			t.Errorf("%s: mutated plan equal", name)
		}
	}

	// A source that changed since the plan was frozen fails closed.
	brighter := d
	brighter.ContentLightLevel = &mediainspect.ContentLightLevelMetadata{MaxCLL: 4000}
	brighter.MasteringDisplay = compactMD("40000000/10000")
	if p.MatchSource(brighter, uhd()) == nil {
		t.Fatal("changed peak accepted")
	}
	smaller := uhd()
	smaller.Width, smaller.Height = 2560, 1440
	if p.MatchSource(d, smaller) == nil {
		t.Fatal("changed size accepted")
	}
	if nilPlan.MatchSource(d, uhd()) == nil {
		t.Fatal("missing plan accepted")
	}
}

// wideHDR10 is a 2.39:1 DCI 4K HDR10 source, wider than the 16:9 box of 2160.
func wideHDR10() (mediainspect.DynamicRange, mediainspect.DetailedStream) {
	d := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeHDR10, Primaries: "bt2020", Matrix: "bt2020nc", MasteringDisplay: compactMD("10000000/10000"), ContentLightLevel: &mediainspect.ContentLightLevelMetadata{MaxCLL: 1000, MaxFALL: 400}}
	return d, mediainspect.DetailedStream{Kind: "video", Width: 4096, Height: 1716, SampleAspect: "1:1"}
}

func TestCompactKeepsTheSizeOfASourceNoTallerThanTheLimit(t *testing.T) {
	d, v := wideHDR10()
	p, err := NewCompactPlan(d, v, 2160)
	if err != nil {
		t.Fatal(err)
	}
	// Same width and height, tone mapped to SDR BT.709 with no HDR output.
	if p.TargetWidth != 4096 || p.TargetHeight != 1716 || !p.KeepSourceSize || p.KeepsHDR() || p.HDR != nil || p.ToneMap == nil || p.ToneMap.Method != CompactToneMapPQ {
		t.Fatalf("plan = %+v", p)
	}
	if err := p.MatchSource(d, v); err != nil {
		t.Fatalf("a new plan does not match its source: %v", err)
	}

	// A 16:9 4K HDR source at 2160 is the same under both rules: kept, not
	// marked, still tone mapped.
	uhdPlan, err := NewCompactPlan(d, uhd(), 2160)
	if err != nil || uhdPlan.TargetWidth != 3840 || uhdPlan.TargetHeight != 2160 || uhdPlan.KeepSourceSize || uhdPlan.ToneMap == nil {
		t.Fatalf("uhd plan = %+v %v", uhdPlan, err)
	}

	// A taller source is still fitted in the 16:9 box, unmarked.
	if p, err := NewCompactPlan(d, v, 1080); err != nil || p.TargetWidth != 1920 || p.TargetHeight != 804 || p.KeepSourceSize {
		t.Fatalf("taller source plan = %+v %v", p, err)
	}

	// An SDR source that keeps its size has nothing left to do.
	sdr := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeSDR, Primaries: "bt709", Matrix: "bt709"}
	for _, s := range []mediainspect.DetailedStream{{Kind: "video", Width: 2560, Height: 1080}, {Kind: "video", Width: 3840, Height: 2160}} {
		if _, err := NewCompactPlan(sdr, s, 1080); s.Height <= 1080 && (err == nil || err.Error() != "compact_not_needed") {
			t.Errorf("%dx%d SDR at 1080: %v, want compact_not_needed", s.Width, s.Height, err)
		}
	}
}

func TestCompactPlansFrozenUnderTheBoxRuleStillMatch(t *testing.T) {
	d, v := wideHDR10()
	// A queued plan frozen before the rule: shrunk to the 16:9 box, unmarked.
	queued := &CompactPlan{ChainVersion: CompactChainVersion, SourceClass: d.Class, SourceSignal: mediainspect.SignalPQ, SourceWidth: 4096, SourceHeight: 1716, MaxHeight: 2160, TargetWidth: 3840, TargetHeight: 1608,
		ToneMap: &CompactToneMap{Method: CompactToneMapPQ, SourcePeakNits: 1000, TargetPeakNits: CompactTargetPeakNits}}
	if err := queued.Validate(); err != nil {
		t.Fatalf("queued plan invalid: %v", err)
	}
	if err := queued.MatchSource(d, v); err != nil {
		t.Fatalf("queued plan no longer matches its source: %v", err)
	}
	// The new plan differs from it, so it cannot be mistaken for it.
	p, _ := NewCompactPlan(d, v, 2160)
	if p.Equal(queued) {
		t.Fatal("new and queued plans compare equal")
	}

	// The mark is only valid where the rules differ, and with the source size.
	marked := queued.Clone()
	marked.KeepSourceSize = true
	if err := marked.Validate(); err == nil {
		t.Error("a marked plan shrunk to the box was accepted")
	}
	uhdPlan, _ := NewCompactPlan(d, uhd(), 2160)
	uhdPlan.KeepSourceSize = true
	if err := uhdPlan.Validate(); err == nil {
		t.Error("a mark on a 16:9 source was accepted")
	}
	unmarked := p.Clone()
	unmarked.KeepSourceSize = false
	if err := unmarked.Validate(); err == nil {
		t.Error("a kept wide size without the mark was accepted")
	}
}
