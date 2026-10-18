package transcode

import (
	"errors"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
)

const testMasterDisplay = "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,50)"

func testDisplay() *mediainspect.MasteringDisplayMetadata {
	return &mediainspect.MasteringDisplayMetadata{RedX: "34000/50000", RedY: "16000/50000", GreenX: "13250/50000", GreenY: "34500/50000", BlueX: "7500/50000", BlueY: "3000/50000", WhitePointX: "15635/50000", WhitePointY: "16450/50000", MinLuminance: "50/10000", MaxLuminance: "10000000/10000"}
}

func testHDR10Range() mediainspect.DynamicRange {
	return mediainspect.DynamicRange{Class: mediainspect.DynamicRangeHDR10, MasteringDisplay: testDisplay(), ContentLightLevel: &mediainspect.ContentLightLevelMetadata{MaxCLL: 1000, MaxFALL: 400}}
}

func testDVRange(profile int) mediainspect.DynamicRange {
	d := testHDR10Range()
	d.Class = mediainspect.DynamicRangeDolbyVision
	d.BaseLayer = mediainspect.DynamicRangeHDR10
	d.DolbyVision = &mediainspect.DolbyVisionInfo{Profile: profile, BLCompatibilityID: 1, ConfigRecord: true}
	return d
}

func validHDR10Plan() *HDR10Plan {
	return &HDR10Plan{SourceClass: HDRSourceHDR10, MasterDisplay: testMasterDisplay, MaxCLL: 1000, MaxFALL: 400, ToneMap: HDRToneMap{Method: HDRToneMapMethod, SourcePeakNits: 1000, TargetPeakNits: HDRToneMapTargetNits}}
}

func TestHDR10PlanValidate(t *testing.T) {
	var nilPlan *HDR10Plan
	if err := nilPlan.Validate(); err != nil {
		t.Fatalf("nil plan is SDR: %v", err)
	}
	if err := validHDR10Plan().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*HDR10Plan){
		"unknown class":          func(p *HDR10Plan) { p.SourceClass = "hlg" },
		"hdr10 with dv approval": func(p *HDR10Plan) { p.DolbyVisionRemoval = true },
		"dv without approval":    func(p *HDR10Plan) { p.SourceClass, p.DolbyVisionProfile = HDRSourceDolbyVision, 8 },
		"dv profile 5": func(p *HDR10Plan) {
			p.SourceClass, p.DolbyVisionRemoval, p.DolbyVisionProfile = HDRSourceDolbyVision, true, 5
		},
		"malformed master display":    func(p *HDR10Plan) { p.MasterDisplay = "G(1,1)" },
		"injection in master display": func(p *HDR10Plan) { p.MasterDisplay = testMasterDisplay + ":pass=1" },
		"chromaticity out of range":   func(p *HDR10Plan) { p.MasterDisplay = strings.Replace(testMasterDisplay, "13250", "60000", 1) },
		"min luminance not below max": func(p *HDR10Plan) {
			p.MasterDisplay = strings.Replace(testMasterDisplay, "L(10000000,50)", "L(10000000,10000000)", 1)
		},
		"fall above cll":                func(p *HDR10Plan) { p.MaxFALL = 1200 },
		"fall without cll":              func(p *HDR10Plan) { p.MaxCLL = 0 },
		"tone map method":               func(p *HDR10Plan) { p.ToneMap.Method = "hable" },
		"tone map target":               func(p *HDR10Plan) { p.ToneMap.TargetPeakNits = 203 },
		"tone map peak above mastering": func(p *HDR10Plan) { p.ToneMap.SourcePeakNits = 4000 },
	}
	for name, mutate := range cases {
		p := validHDR10Plan()
		mutate(p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestHDR10X265Params(t *testing.T) {
	want := "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:range=limited:hdr10=1:hdr10-opt=1:repeat-headers=1:master-display=" + testMasterDisplay + ":max-cll=1000,400"
	if got := validHDR10Plan().X265Params(); got != want {
		t.Fatalf("x265 params\nwant %s\n got %s", want, got)
	}
}

func TestNewHDR10PlanAndMatchSource(t *testing.T) {
	plan, err := NewHDR10Plan(testHDR10Range(), false)
	if err != nil {
		t.Fatal(err)
	}
	if *plan != *validHDR10Plan() {
		t.Fatalf("derived plan %+v", plan)
	}
	if err := plan.MatchSource(testHDR10Range()); err != nil {
		t.Fatal(err)
	}
	changed := testHDR10Range()
	changed.ContentLightLevel.MaxCLL = 900
	if err := plan.MatchSource(changed); err == nil {
		t.Fatal("changed CLL must not match the frozen plan")
	}
	var none *HDR10Plan
	if err := none.MatchSource(testHDR10Range()); err == nil {
		t.Fatal("an HDR source without a plan must fail closed")
	}
	if err := none.MatchSource(mediainspect.DynamicRange{Class: mediainspect.DynamicRangeSDR}); err != nil {
		t.Fatal(err)
	}

	if _, err := NewHDR10Plan(testDVRange(8), false); !errors.Is(err, ErrDolbyVisionRemovalApprovalRequired) {
		t.Fatalf("dolby vision without approval: %v", err)
	}
	dv, err := NewHDR10Plan(testDVRange(8), true)
	if err != nil || dv.SourceClass != HDRSourceDolbyVision || !dv.DolbyVisionRemoval || dv.DolbyVisionProfile != 8 {
		t.Fatalf("approved dolby vision plan %+v %v", dv, err)
	}
	if err := dv.MatchSource(testHDR10Range()); err == nil {
		t.Fatal("a Dolby Vision approval must not be reused for a plain HDR10 source")
	}

	notConvertible := testDVRange(5)
	notConvertible.BaseLayer, notConvertible.Reason = "none", "dolby_vision_profile_5_no_compatible_base_layer"
	if _, err := NewHDR10Plan(notConvertible, true); err == nil || err.Error() != notConvertible.Reason {
		t.Fatalf("profile 5 must stay rejected with its reason: %v", err)
	}
	dim := testHDR10Range()
	dim.MasteringDisplay.MaxLuminance = "100"
	if _, err := NewHDR10Plan(dim, false); err == nil {
		t.Fatal("a mastering peak below the tone-map range must be rejected")
	}
}

func TestBenchmarkRequestHDR10Gate(t *testing.T) {
	base := func() *BenchmarkRequest {
		return &BenchmarkRequest{Mode: "quality", Metric: "vmaf", Quality: &BenchmarkQualityConfig{VMAF: &BenchmarkQualityThresholds{Model: "v1_1080p_3h"}}, Candidates: []BenchmarkCandidate{{ID: "a", VideoCodec: VideoCodecLibX265, Quality: 20}}, HDR10: validHDR10Plan()}
	}
	check := func(name string, req *BenchmarkRequest) {
		t.Helper()
		err := ValidateBenchmarkRequest(req)
		if err == nil || !strings.Contains(err.Error(), "HDR10") {
			t.Errorf("%s: expected HDR10 rejection, got %v", name, err)
		}
	}
	vt := base()
	vt.Candidates = append(vt.Candidates, BenchmarkCandidate{ID: "vt", VideoCodec: VideoCodecHEVCVideoToolbox, Quality: 70})
	check("videotoolbox candidate", vt)
	ssim := base()
	ssim.Metric = "ssim"
	check("ssim metric", ssim)
	noMode := base()
	noMode.Mode = ""
	check("no mode", noMode)
}
