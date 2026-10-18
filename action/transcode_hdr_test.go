package action

import (
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

func hdrTestDisplay() *mediainspect.MasteringDisplayMetadata {
	return &mediainspect.MasteringDisplayMetadata{RedX: "34000/50000", RedY: "16000/50000", GreenX: "13250/50000", GreenY: "34500/50000", BlueX: "7500/50000", BlueY: "3000/50000", WhitePointX: "15635/50000", WhitePointY: "16450/50000", MinLuminance: "50/10000", MaxLuminance: "10000000/10000"}
}

func hdrTestRange(class string) mediainspect.DynamicRange {
	d := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeHDR10, Label: "HDR10", MasteringDisplay: hdrTestDisplay(), ContentLightLevel: &mediainspect.ContentLightLevelMetadata{MaxCLL: 1000, MaxFALL: 400}}
	if class == mediainspect.DynamicRangeDolbyVision {
		d.Class, d.Label, d.BaseLayer = class, "Dolby Vision profile 8.1 (HDR10 base layer)", mediainspect.DynamicRangeHDR10
		d.DolbyVision = &mediainspect.DolbyVisionInfo{Profile: 8, BLCompatibilityID: 1, ConfigRecord: true}
	}
	return d
}

func TestModeHDRPolicySnapshot(t *testing.T) {
	for _, mode := range []string{"size", "quality", "x265_preserve"} {
		legacy, err := modePolicy(mode, false)
		if err != nil {
			t.Fatal(err)
		}
		rejecting, _ := modePolicyWithHDR(mode, false, "")
		if legacy.Digest != rejecting.Digest || legacy.HDRPolicy != "" || legacy.HDR10Quality != nil {
			t.Fatalf("%s: the default HDR rejection changed the frozen SDR policy", mode)
		}
		preserving, err := modePolicyWithHDR(mode, false, HDRPolicyPreserveHDR10)
		if err != nil || preserving.Digest == legacy.Digest || preserving.HDR10Quality == nil || preserving.HDR10Quality.ToneMapMethod != transcode.HDRToneMapMethod {
			t.Fatalf("%s: preserve_hdr10 must freeze its HDR quality bar: %+v %v", mode, preserving, err)
		}
		sdr := legacy.Profile.Optimization.Quality
		hdr := preserving.HDR10Quality
		if hdr.VMAFMinimum <= sdr.VMAF.Minimum || hdr.VMAFTarget <= sdr.VMAF.Target || hdr.VMAFP5Minimum <= *sdr.VMAF.P5Minimum || hdr.CAMBIMaxMean >= *sdr.Banding.MaxMean || hdr.CAMBIMaxPeak >= *sdr.Banding.MaxPeak {
			t.Fatalf("%s: HDR thresholds must be stricter than SDR: %+v", mode, hdr)
		}
		profile := legacy.Profile
		if err := applyHDR10ModeQuality(&profile, hdr); err != nil {
			t.Fatal(err)
		}
		q := profile.Optimization.Quality
		if q.VMAF.Minimum != hdr.VMAFMinimum || *q.VMAF.WorstWindowMinimum != hdr.VMAFWorstWindowMinimum || *q.Banding.MaxPeak != hdr.CAMBIMaxPeak || q.SSIM != nil || profile.Video.Codec != transcode.VideoCodecLibX265 {
			t.Fatalf("%s: HDR quality not applied: %+v", mode, q)
		}
		if legacy.Profile.Optimization.Quality.VMAF.Minimum != sdr.VMAF.Minimum {
			t.Fatal("applying HDR quality mutated the frozen snapshot")
		}
	}
}

func TestModeHDRPolicyInputs(t *testing.T) {
	if err := validateModeInputs(map[string]any{"hdr_policy": HDRPolicyPreserveHDR10}); err == nil {
		t.Fatal("hdr_policy without mode must be rejected")
	}
	if err := validateModeInputs(map[string]any{"mode": "quality", "hdr_policy": "tonemap_to_sdr"}); err == nil {
		t.Fatal("unknown hdr_policy must be rejected")
	}
	for _, policy := range []any{HDRPolicyReject, HDRPolicyPreserveHDR10} {
		if err := validateModeInputs(map[string]any{"mode": "quality", "hdr_policy": policy}); err != nil {
			t.Fatal(err)
		}
	}
	ec := &ExecutionContext{Inputs: map[string]any{"mode": "quality"}, State: map[string]any{}}
	if _, err := freezeModePolicy(ec); err != nil {
		t.Fatal(err)
	}
	ec.Inputs["hdr_policy"] = HDRPolicyPreserveHDR10
	if _, err := freezeModePolicy(ec); err == nil {
		t.Fatal("a frozen policy cannot be reused under a different hdr_policy")
	}
}

func TestModeSourceReasonHDR(t *testing.T) {
	rep := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{Codec: "h264", BitDepth: 10, FPS: 24, ColorTransfer: "smpte2084", ColorPrimaries: "bt2020"}}}
	if modeSourceReason("quality", rep) != "unsupported_source" {
		t.Fatal("HDR must stay unsupported without preserve_hdr10")
	}
	d := hdrTestRange(mediainspect.DynamicRangeHDR10)
	if got := modeSourceReasonHDR("quality", rep, &d); got != "" {
		t.Fatalf("convertible HDR10 rejected: %s", got)
	}
	d.Class, d.Reason = mediainspect.DynamicRangeHDR10Plus, "hdr10plus_dynamic_metadata_not_preserved"
	if modeSourceReasonHDR("quality", rep, &d) != "unsupported_source" {
		t.Fatal("HDR10+ must stay rejected")
	}
}

func TestDolbyVisionRemovalRequiresExplicitPerFileApproval(t *testing.T) {
	e := &Engine{}
	sha := strings.Repeat("a", 64)
	dv := hdrTestRange(mediainspect.DynamicRangeDolbyVision)
	ec := &ExecutionContext{Inputs: map[string]any{"mode": "quality"}, State: map[string]any{}}
	for _, decision := range []string{"", "approve", "accept_loss", "resume"} {
		ec.Decision = decision
		plan, res := e.resolveHDR10Plan(ec, dv, sha, "/media/Example.mkv", nil)
		if plan != nil || res == nil || res.Status != StepWaitingDecision || res.WaitingCondition != waitingDolbyVisionRemoval {
			t.Fatalf("decision %q must not approve Dolby Vision removal: %+v", decision, res)
		}
		if len(res.WaitingOptions) != 2 || res.WaitingOptions[0].Decision != DecisionApproveDolbyVisionRemoval || res.WaitingOptions[1].Decision != "cancel" {
			t.Fatalf("options %+v", res.WaitingOptions)
		}
		if !strings.Contains(res.WaitingReason, "Example.mkv") || !strings.Contains(res.WaitingReason, "removes Dolby Vision") || !strings.Contains(res.WaitingReason, "keeps HDR10") {
			t.Fatalf("reason must name the file and the loss: %s", res.WaitingReason)
		}
	}
	ec.Decision = DecisionApproveDolbyVisionRemoval
	plan, res := e.resolveHDR10Plan(ec, dv, sha, "/media/Example.mkv", nil)
	if res != nil || plan == nil || plan.SourceClass != transcode.HDRSourceDolbyVision || !plan.DolbyVisionRemoval || plan.DolbyVisionProfile != 8 {
		t.Fatalf("approved plan %+v %+v", plan, res)
	}
	ec.Decision = ""
	if plan, res := e.resolveHDR10Plan(ec, dv, sha, "/media/Example.mkv", nil); res != nil || plan == nil {
		t.Fatal("a persisted approval for the same bytes must be honoured on re-entry")
	}
	if _, res := e.resolveHDR10Plan(ec, dv, strings.Repeat("b", 64), "/media/Example.mkv", nil); res == nil || res.Status != StepWaitingDecision {
		t.Fatal("an approval is bound to the exact source content")
	}
	ec.Decision = "cancel"
	if _, res := e.resolveHDR10Plan(&ExecutionContext{State: map[string]any{}, Decision: "cancel"}, dv, sha, "/media/Example.mkv", nil); res == nil || res.Status != StepCancelled {
		t.Fatal("cancel keeps the original")
	}
	hdr, res := e.resolveHDR10Plan(&ExecutionContext{State: map[string]any{}}, hdrTestRange(mediainspect.DynamicRangeHDR10), sha, "/media/Example.mkv", nil)
	if res != nil || hdr == nil || hdr.DolbyVisionRemoval {
		t.Fatal("plain HDR10 needs no approval")
	}
}

func TestHDR10BenchmarkRequestIsLibX265Only(t *testing.T) {
	ec := &ExecutionContext{InstanceID: "hdr-bench", Inputs: map[string]any{"mode": "quality", "hdr_policy": HDRPolicyPreserveHDR10}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
	policy, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	profile := policy.Profile
	if err := applyHDR10ModeQuality(&profile, policy.HDR10Quality); err != nil {
		t.Fatal(err)
	}
	hdr, err := transcode.NewHDR10Plan(hdrTestRange(mediainspect.DynamicRangeHDR10), false)
	if err != nil {
		t.Fatal(err)
	}
	plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", HDR10: hdr}
	if err := preserveSourceBitDepth(plan, 10, true); err != nil {
		t.Fatal(err)
	}
	ec.State["plan"] = plan
	rep := &mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: 10, FPS: 24, BitRate: 20000000}}}
	caps := modeTestCapabilities()
	if _, err := buildBenchmarkRequest(ec, "/media/source.mkv", rep, profile.Optimization, caps); err == nil || !strings.HasPrefix(err.Error(), "hdr10_worker_capability_unavailable") {
		t.Fatalf("a worker without the HDR10 capability must never receive HDR: %v", err)
	}
	caps.HDR10Transcode = true
	caps.Filters["lut3d"] = true
	req, err := buildBenchmarkRequest(ec, "/media/source.mkv", rep, profile.Optimization, caps)
	if err != nil {
		t.Fatal(err)
	}
	if req.HDR10 == nil || *req.HDR10 != *hdr || req.Metric != "vmaf" || req.Quality.VMAF.Minimum != policy.HDR10Quality.VMAFMinimum || *req.Quality.Banding.MaxMean != policy.HDR10Quality.CAMBIMaxMean {
		t.Fatalf("HDR request %+v", req)
	}
	for _, c := range req.Candidates {
		if transcode.BenchmarkCandidateVideoCodec(c) != transcode.VideoCodecLibX265 || c.VideoProfile != "main10" {
			t.Fatalf("HDR candidate %+v", c)
		}
	}
}
