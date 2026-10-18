package action

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

func TestCompactModePolicy(t *testing.T) {
	p, err := modePolicyFor("compact", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.MinSavingsPercent != 30 || p.Compact == nil || p.Compact.MaxHeight != 1080 || p.Compact.ChainVersion != transcode.CompactChainVersion || p.Profile.Audio.Mode != "compact" {
		t.Fatalf("compact policy %+v", p)
	}
	size, _ := modePolicy("size", false)
	if p.Profile.Optimization.Quality.VMAF.Minimum != size.Profile.Optimization.Quality.VMAF.Minimum || p.Profile.Optimization.Quality.VMAF.Target != size.Profile.Optimization.Quality.VMAF.Target {
		t.Fatal("compact must keep the size-mode quality floor")
	}
	if size.Compact != nil || size.Profile.Audio.Mode == "compact" {
		t.Fatal("compact settings leaked into size mode")
	}
	p2160, err := modePolicyFor("compact", false, 2160)
	if err != nil || p2160.Compact.MaxHeight != 2160 || p2160.Digest == p.Digest {
		t.Fatalf("2160 policy %+v %v", p2160, err)
	}
	for _, bad := range []int{480, 900, 4320, -1} {
		if _, err := modePolicyFor("compact", false, bad); err == nil {
			t.Errorf("max height %d accepted", bad)
		}
	}
	if _, err := modePolicyFor("size", false, 1080); err == nil {
		t.Fatal("max_height accepted for size mode")
	}
	if min, ok := optimization.ModeMinimumSavings("compact", optimization.ModePolicyV2); !ok || min != 30 {
		t.Fatalf("selector compact minimum %d %v", min, ok)
	}
	if _, ok := optimization.ModeMinimumSavings("compact", "transcode-modes-v1"); ok {
		t.Fatal("compact must not exist under policy v1")
	}
}

func TestCompactModeInputs(t *testing.T) {
	ok := []map[string]any{
		{"mode": "compact"},
		{"mode": "compact", "max_height": float64(720)},
		{"mode": "compact", "max_height": 2160},
	}
	for _, in := range ok {
		if err := validateModeInputs(in); err != nil {
			t.Errorf("%v rejected: %v", in, err)
		}
	}
	bad := []map[string]any{
		{"max_height": 1080},
		{"mode": "size", "max_height": 1080},
		{"mode": "compact", "max_height": 1080.5},
		{"mode": "compact", "max_height": "1080"},
		{"mode": "compact", "max_height": 1000},
		{"mode": "compact", "profile": "general-hevc"},
		{"mode": "compact", "min_savings_percent": 10},
	}
	for _, in := range bad {
		if err := validateModeInputs(in); err == nil {
			t.Errorf("%v accepted", in)
		}
	}
}

func TestCompactModePolicyFreezesMaxHeight(t *testing.T) {
	ec := &ExecutionContext{Inputs: map[string]any{"mode": "compact", "max_height": 720}, State: map[string]any{}}
	p, err := freezeModePolicy(ec)
	if err != nil || p.Compact == nil || p.Compact.MaxHeight != 720 {
		t.Fatalf("frozen %+v %v", p, err)
	}
	data, _ := json.Marshal(ec.State)
	_ = json.Unmarshal(data, &ec.State)
	if after, err := freezeModePolicy(ec); err != nil || after.Digest != p.Digest {
		t.Fatalf("restart %+v %v", after, err)
	}
	altered := *p
	altered.Compact = &ModeCompactPolicy{MaxHeight: 2160, ChainVersion: transcode.CompactChainVersion}
	ec.State["mode_policy"] = altered
	if _, err := freezeModePolicy(ec); err == nil {
		t.Fatal("max height tampering not rejected")
	}
}

func TestCompactInventoryReason(t *testing.T) {
	ec := func() *ExecutionContext {
		return &ExecutionContext{Inputs: map[string]any{"mode": "compact"}, State: map[string]any{}}
	}
	report := func(v mediainspect.DetailedStream) *mediainspect.DetailedReport {
		v.Kind = "video"
		return &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{v}}
	}
	sdr4k := mediainspect.DetailedStream{Codec: "hevc", Width: 3840, Height: 2160, ColorPrimaries: "bt709", ColorTransfer: "bt709", ColorSpace: "bt709"}
	if r := compactInventoryReason(ec(), report(sdr4k)); r != "" {
		t.Fatalf("4K SDR skipped: %s", r)
	}
	sdr1080 := sdr4k
	sdr1080.Width, sdr1080.Height = 1920, 1080
	if r := compactInventoryReason(ec(), report(sdr1080)); r != "compact_not_needed" {
		t.Fatalf("1080p SDR: %q", r)
	}
	// HDR needs frame evidence, which the child preflight collects.
	hdr := sdr4k
	hdr.ColorPrimaries, hdr.ColorTransfer, hdr.ColorSpace = "bt2020", "smpte2084", "bt2020nc"
	if r := compactInventoryReason(ec(), report(hdr)); r != "" {
		t.Fatalf("HDR deferred: %q", r)
	}
	if r := compactInventoryReason(&ExecutionContext{Inputs: map[string]any{}, State: map[string]any{}}, report(sdr4k)); r != "compact_policy_unavailable" {
		t.Fatalf("missing policy: %q", r)
	}
}

func TestBuildBenchmarkRequestCompactNeedsWorkerCapability(t *testing.T) {
	rep := &mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: 10, BitRate: 40000000, FPS: 24, Width: 3840, Height: 2160}}}
	opt := &recipe.OptimizationPolicy{
		Enabled:  true,
		Sampling: &recipe.SamplingPolicy{SampleSeconds: 5, SampleCount: 3, Positions: []float64{0.2, 0.5, 0.8}},
		Search:   &recipe.SearchPolicy{MaxCandidates: 3, QualityValues: []int{20, 23, 26}},
		Quality:  &recipe.QualityPolicy{PreferredMetric: "vmaf", VMAF: &recipe.MetricTarget{Model: "v1_1080p_3h", Target: 92, Minimum: 88}, Banding: &recipe.BandingPolicy{Enabled: true, Metric: "cambi", Mode: "full_ref", Enforcement: "observe"}},
	}
	compact := &transcode.CompactPlan{ChainVersion: transcode.CompactChainVersion, SourceClass: mediainspect.DynamicRangeHDR10, SourceSignal: mediainspect.SignalPQ, SourceWidth: 3840, SourceHeight: 2160, MaxHeight: 1080, TargetWidth: 1920, TargetHeight: 1080, ToneMap: &transcode.CompactToneMap{Method: transcode.CompactToneMapPQ, SourcePeakNits: 1000, TargetPeakNits: 100}}
	caps := transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, CompactAudio: true, CompactTranscode: true, AutomaticModesV2: true, Encoders: map[string]bool{"libx265": true}, Filters: map[string]bool{"libvmaf": true, "ssim": true}, Quality: &transcode.QualityCapabilities{Native10Bit: true, CAMBIFullRef: true, Models: map[string]transcode.QualityModelCapability{"v1_1080p_3h": {Available: true}}}}
	ec := &ExecutionContext{InstanceID: "bench-compact", Inputs: map[string]any{"mode": "compact"}, State: map[string]any{"search_budget_seconds": 600, "plan": &transcode.Plan{VideoCodec: "libx265", Quality: 23, AudioMode: "compact", ExpectedBitDepth: 10, Compact: compact}}}
	req, err := buildBenchmarkRequest(ec, "/media/source.mkv", rep, opt, caps)
	if err != nil {
		t.Fatal(err)
	}
	if !req.Compact.Equal(compact) || req.Compact == compact {
		t.Fatalf("request compact %+v", req.Compact)
	}
	caps.CompactTranscode = false
	if _, err := buildBenchmarkRequest(ec, "/media/source.mkv", rep, opt, caps); err == nil || !strings.Contains(err.Error(), "compact SDR") {
		t.Fatalf("worker without compact_transcode accepted: %v", err)
	}
}
