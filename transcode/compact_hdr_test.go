package transcode

import (
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
)

const testMasterDisplay = "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,50)"

func TestNewCompactHDRPlan(t *testing.T) {
	hdr10 := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeHDR10, Primaries: "bt2020", Matrix: "bt2020nc", MasteringDisplay: compactMD("10000000/10000"), ContentLightLevel: &mediainspect.ContentLightLevelMetadata{MaxCLL: 1000, MaxFALL: 400}}
	p, err := NewCompactPlanForChain(CompactHDRChainVersion, hdr10, uhd(), 1080)
	if err != nil {
		t.Fatal(err)
	}
	if !p.KeepsHDR() || p.SourceSignal != mediainspect.SignalPQ || p.TargetHeight != 1080 || p.ToneMap == nil || p.ToneMap.SourcePeakNits != 1000 {
		t.Fatalf("hdr10 plan %+v", p)
	}
	if want := (CompactHDROutput{Transfer: CompactHDRTransferPQ, MasterDisplay: testMasterDisplay, MaxCLL: 1000, MaxFALL: 400}); p.HDR == nil || *p.HDR != want {
		t.Fatalf("hdr output %+v", p.HDR)
	}
	// Keeping HDR at the source size is the point of 4K: never "not needed".
	if p, err := NewCompactPlanForChain(CompactHDRChainVersion, hdr10, uhd(), 2160); err != nil || p.TargetWidth != 3840 || p.TargetHeight != 2160 {
		t.Fatalf("4K keep-HDR plan %+v %v", p, err)
	}
	// The SDR chain of the same source differs only in version and output.
	sdr, err := NewCompactPlan(hdr10, uhd(), 1080)
	if err != nil {
		t.Fatal(err)
	}
	if m := p.MeasurementPlan(); !m.Equal(sdr) || m == p || p.HDR == nil {
		t.Fatalf("measurement plan %+v, want %+v", m, sdr)
	}
	if sdr.MeasurementPlan() != sdr || sdr.KeepsHDR() || sdr.HDR != nil {
		t.Fatal("an SDR plan is its own measurement plan")
	}

	// PQ without static metadata keeps none; zero light levels are none.
	pq10 := mediainspect.DynamicRange{Class: mediainspect.DynamicRangePQ10, ContentLightLevel: &mediainspect.ContentLightLevelMetadata{}}
	if p, err := NewCompactPlanForChain(CompactHDRChainVersion, pq10, uhd(), 1080); err != nil || *p.HDR != (CompactHDROutput{Transfer: CompactHDRTransferPQ}) {
		t.Fatalf("pq10 plan %+v %v", p, err)
	}
	// HLG signals HLG and no static metadata, even if the source has some.
	hlg := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeHLG, MasteringDisplay: compactMD("10000000/10000")}
	if p, err := NewCompactPlanForChain(CompactHDRChainVersion, hlg, uhd(), 1080); err != nil || *p.HDR != (CompactHDROutput{Transfer: CompactHDRTransferHLG}) {
		t.Fatalf("hlg plan %+v %v", p, err)
	}
	// Dolby Vision 8.1 and HDR10+ come out as HDR10 with the base layer's metadata.
	dv81 := hdr10
	dv81.Class, dv81.BaseLayer, dv81.DolbyVision = mediainspect.DynamicRangeDolbyVision, mediainspect.DynamicRangeHDR10, &mediainspect.DolbyVisionInfo{Profile: 8, BLCompatibilityID: 1}
	if p, err := NewCompactPlanForChain(CompactHDRChainVersion, dv81, uhd(), 1080); err != nil || p.DolbyVisionProfile != 8 || p.HDR.MasterDisplay != testMasterDisplay {
		t.Fatalf("dv 8.1 plan %+v %v", p, err)
	}
	plus := hdr10
	plus.Class, plus.HDR10Plus = mediainspect.DynamicRangeHDR10Plus, true
	if p, err := NewCompactPlanForChain(CompactHDRChainVersion, plus, uhd(), 1080); err != nil || p.HDR.Transfer != CompactHDRTransferPQ {
		t.Fatalf("hdr10+ plan %+v %v", p, err)
	}

	refused := map[string]struct {
		d    mediainspect.DynamicRange
		want string
	}{
		"sdr":           {mediainspect.DynamicRange{Class: mediainspect.DynamicRangeSDR}, "compact_keep_hdr_requires_hdr"},
		"dv 8.2 sdr":    {mediainspect.DynamicRange{Class: mediainspect.DynamicRangeDolbyVision, BaseLayer: mediainspect.DynamicRangeSDR, DolbyVision: &mediainspect.DolbyVisionInfo{Profile: 8, BLCompatibilityID: 2}}, "compact_keep_hdr_requires_hdr"},
		"dv 5":          {mediainspect.DynamicRange{Class: mediainspect.DynamicRangeDolbyVision, BaseLayer: "none", Reason: "dolby_vision_profile_5_no_compatible_base_layer"}, "dolby_vision_profile_5_no_compatible_base_layer"},
		"bad mastering": {mediainspect.DynamicRange{Class: mediainspect.DynamicRangeHDR10, MasteringDisplay: compactMD("10/10000")}, "hdr_mastering_display_invalid"},
	}
	for name, c := range refused {
		if _, err := NewCompactPlanForChain(CompactHDRChainVersion, c.d, uhd(), 1080); err == nil || err.Error() != c.want {
			t.Errorf("%s: got %v, want %s", name, err, c.want)
		}
	}
	if _, err := NewCompactPlanForChain("compact-hdr-v0", hdr10, uhd(), 1080); err == nil {
		t.Fatal("unknown chain accepted")
	}
}

func TestCompactHDRPlanValidateEqualMatch(t *testing.T) {
	d := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeHDR10, MasteringDisplay: compactMD("10000000/10000"), ContentLightLevel: &mediainspect.ContentLightLevelMetadata{MaxCLL: 1000, MaxFALL: 400}}
	p, err := NewCompactPlanForChain(CompactHDRChainVersion, d, uhd(), 1080)
	if err != nil {
		t.Fatal(err)
	}
	c := p.Clone()
	if !c.Equal(p) || c.HDR == p.HDR {
		t.Fatal("clone must be equal and deep")
	}
	if err := p.MatchSource(d, uhd()); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*CompactPlan){
		"no hdr output":    func(p *CompactPlan) { p.HDR = nil },
		"hlg transfer":     func(p *CompactPlan) { p.HDR.Transfer = CompactHDRTransferHLG },
		"sdr transfer":     func(p *CompactPlan) { p.HDR.Transfer = "bt709" },
		"injected params":  func(p *CompactPlan) { p.HDR.MasterDisplay += ":dhdr10-info=/tmp/x" },
		"light level":      func(p *CompactPlan) { p.HDR.MaxCLL = 20000 },
		"negative fall":    func(p *CompactPlan) { p.HDR.MaxFALL = -1 },
		"sdr source":       func(p *CompactPlan) { p.SourceSignal, p.ToneMap = mediainspect.SignalSDR, nil },
		"sdr chain w/ hdr": func(p *CompactPlan) { p.ChainVersion = CompactChainVersion },
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
	hlg := &CompactPlan{ChainVersion: CompactHDRChainVersion, SourceClass: mediainspect.DynamicRangeHLG, SourceSignal: mediainspect.SignalHLG, SourceWidth: 3840, SourceHeight: 2160, MaxHeight: 1080, TargetWidth: 1920, TargetHeight: 1080,
		ToneMap: &CompactToneMap{Method: CompactToneMapHLG, SourcePeakNits: CompactHLGDisplayPeakNits, TargetPeakNits: CompactTargetPeakNits},
		HDR:     &CompactHDROutput{Transfer: CompactHDRTransferHLG, MaxCLL: 1000}}
	if hlg.Validate() == nil {
		t.Fatal("HLG plan with static metadata validated")
	}
	// The source's static metadata is part of the frozen plan.
	other := d
	other.ContentLightLevel = &mediainspect.ContentLightLevelMetadata{MaxCLL: 1000, MaxFALL: 300}
	if p.MatchSource(other, uhd()) == nil {
		t.Fatal("changed light level accepted")
	}
	// The same source frozen for the SDR chain is a different plan.
	sdr, _ := NewCompactPlan(d, uhd(), 1080)
	if sdr.Equal(p) || sdr.MatchSource(d, uhd()) != nil {
		t.Fatal("SDR and keep-HDR plans must differ and each match its source")
	}
}
