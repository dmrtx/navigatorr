package recipe

import (
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestLiveActionHEVCProfileResolvesLibX265(t *testing.T) {
	s, err := Parse(EmbeddedBytes())
	if err != nil {
		t.Fatalf("embedded bundle invalid: %v", err)
	}

	plan, err := Resolve(s, "live-action-hevc", nil, nil)
	if err != nil {
		t.Fatalf("resolving live-action-hevc: %v", err)
	}
	if plan.VideoCodec != transcode.VideoCodecLibX265 {
		t.Fatalf("video codec = %q, want %q", plan.VideoCodec, transcode.VideoCodecLibX265)
	}
	if plan.Preset != "slow" {
		t.Fatalf("preset = %q, want slow", plan.Preset)
	}
	if plan.Quality != 24 {
		t.Fatalf("crf (quality) = %d, want 24", plan.Quality)
	}
	if plan.VideoProfile != "main" || plan.PixelFormat != "yuv420p" {
		t.Fatalf("expected 8-bit main/yuv420p, got profile=%q pix=%q", plan.VideoProfile, plan.PixelFormat)
	}
	if plan.ExpectedBitDepth != 8 {
		t.Fatalf("expected bit depth = %d, want 8 (no 8->10 conversion)", plan.ExpectedBitDepth)
	}
	if plan.AudioMode != "copy" {
		t.Fatalf("audio mode = %q, want copy", plan.AudioMode)
	}
	if plan.SubtitleMode != "preserve" || !plan.PreserveMetadata || !plan.PreserveChapters || !plan.PreserveAttachments {
		t.Fatalf("preservation policy not intact: %+v", plan)
	}
}

func TestLiveActionHEVCOptimizationPolicy(t *testing.T) {
	s, err := Parse(EmbeddedBytes())
	if err != nil {
		t.Fatalf("embedded bundle invalid: %v", err)
	}
	p, ok := s.Bundle.Profiles["live-action-hevc"]
	if !ok {
		t.Fatal("live-action-hevc profile missing from embedded bundle")
	}
	opt := p.Optimization
	if opt == nil || !opt.Enabled {
		t.Fatalf("optimization must be enabled: %+v", opt)
	}
	if opt.Sampling == nil || opt.Sampling.SampleCount != 3 || opt.Sampling.SampleSeconds != 20 {
		t.Fatalf("expected 3 samples of 20s, got %+v", opt.Sampling)
	}
	if len(opt.Sampling.Positions) != 3 || opt.Sampling.Positions[0] != 0.2 || opt.Sampling.Positions[1] != 0.5 || opt.Sampling.Positions[2] != 0.8 {
		t.Fatalf("expected distributed positions [0.2,0.5,0.8], got %v", opt.Sampling.Positions)
	}
	if opt.Quality == nil || opt.Quality.VMAF == nil {
		t.Fatalf("expected vmaf quality policy, got %+v", opt.Quality)
	}
	if opt.Quality.PreferredMetric != "vmaf" || opt.Quality.VMAF.Target != 92 || opt.Quality.VMAF.Minimum != 88 {
		t.Fatalf("unexpected quality policy: metric=%q target=%v minimum=%v", opt.Quality.PreferredMetric, opt.Quality.VMAF.Target, opt.Quality.VMAF.Minimum)
	}
	if opt.Search == nil {
		t.Fatal("expected search policy")
	}
	wantCRF := []int{20, 22, 24, 26}
	if len(opt.Search.QualityValues) != len(wantCRF) {
		t.Fatalf("quality_values = %v, want %v", opt.Search.QualityValues, wantCRF)
	}
	for i, want := range wantCRF {
		if opt.Search.QualityValues[i] != want {
			t.Fatalf("quality_values = %v, want %v", opt.Search.QualityValues, wantCRF)
		}
	}
}

func TestExistingVideoToolboxProfilesUnchanged(t *testing.T) {
	s, err := Parse(EmbeddedBytes())
	if err != nil {
		t.Fatalf("embedded bundle invalid: %v", err)
	}
	for _, name := range []string{"hevc-vt", "hevc-vt-balanced", "hevc-vt-quality", "hevc-vt-space", "general-hevc", "anime-hevc", "anime-hevc-quality"} {
		p, ok := s.Bundle.Profiles[name]
		if !ok {
			t.Fatalf("existing profile %q missing", name)
		}
		if strings.ToLower(strings.TrimSpace(p.Video.Codec)) != transcode.VideoCodecHEVCVideoToolbox {
			t.Fatalf("profile %q codec changed to %q", name, p.Video.Codec)
		}
		if p.Video.Preset != "" {
			t.Fatalf("profile %q unexpectedly gained a preset %q", name, p.Video.Preset)
		}
	}
}

func TestLiveActionHEVTVTProfileResolvesBitrate(t *testing.T) {
	s, err := Parse(EmbeddedBytes())
	if err != nil {
		t.Fatalf("embedded bundle invalid: %v", err)
	}

	plan, err := Resolve(s, "live-action-hevc-vt", nil, nil)
	if err != nil {
		t.Fatalf("resolving live-action-hevc-vt: %v", err)
	}
	if plan.VideoCodec != transcode.VideoCodecHEVCVideoToolbox {
		t.Fatalf("video codec = %q, want %q", plan.VideoCodec, transcode.VideoCodecHEVCVideoToolbox)
	}
	if plan.Quality != 0 || plan.AverageBitrateKbps != 3500 {
		t.Fatalf("expected bitrate mode (quality 0, 3500k), got quality=%d bitrate=%d", plan.Quality, plan.AverageBitrateKbps)
	}
	if plan.Preset != "" {
		t.Fatalf("VT profile unexpectedly carries preset %q", plan.Preset)
	}
	if plan.VideoProfile != "main" || plan.PixelFormat != "yuv420p" {
		t.Fatalf("expected 8-bit main/yuv420p, got profile=%q pix=%q", plan.VideoProfile, plan.PixelFormat)
	}
	if plan.ExpectedBitDepth != 8 {
		t.Fatalf("expected bit depth = %d, want 8 (no 8->10 conversion)", plan.ExpectedBitDepth)
	}
	if plan.PrioritizeSpeed == nil || *plan.PrioritizeSpeed || plan.SpatialAQ == nil || !*plan.SpatialAQ || plan.Realtime == nil || *plan.Realtime {
		t.Fatalf("expected prio_speed=false/spatial_aq=true/realtime=false, got %+v", plan)
	}
	if plan.AudioMode != "copy" {
		t.Fatalf("audio mode = %q, want copy", plan.AudioMode)
	}
	if plan.SubtitleMode != "preserve" || !plan.PreserveMetadata || !plan.PreserveChapters || !plan.PreserveAttachments {
		t.Fatalf("preservation policy not intact: %+v", plan)
	}
}

func TestLiveActionHEVTVTOptimizationPolicy(t *testing.T) {
	s, err := Parse(EmbeddedBytes())
	if err != nil {
		t.Fatalf("embedded bundle invalid: %v", err)
	}
	p, ok := s.Bundle.Profiles["live-action-hevc-vt"]
	if !ok {
		t.Fatal("live-action-hevc-vt profile missing from embedded bundle")
	}
	opt := p.Optimization
	if opt == nil || !opt.Enabled {
		t.Fatalf("optimization must be enabled: %+v", opt)
	}
	if opt.Sampling == nil || opt.Sampling.SampleCount != 3 || opt.Sampling.SampleSeconds != 20 {
		t.Fatalf("expected 3 samples of 20s, got %+v", opt.Sampling)
	}
	if len(opt.Sampling.Positions) != 3 || opt.Sampling.Positions[0] != 0.2 || opt.Sampling.Positions[1] != 0.5 || opt.Sampling.Positions[2] != 0.8 {
		t.Fatalf("expected distributed positions [0.2,0.5,0.8], got %v", opt.Sampling.Positions)
	}
	if opt.Quality == nil || opt.Quality.VMAF == nil {
		t.Fatalf("expected vmaf quality policy, got %+v", opt.Quality)
	}
	// The fast VT alternative is judged by the same bar as live-action-hevc.
	if opt.Quality.PreferredMetric != "vmaf" || opt.Quality.VMAF.Target != 92 || opt.Quality.VMAF.Minimum != 88 {
		t.Fatalf("unexpected quality policy: metric=%q target=%v minimum=%v", opt.Quality.PreferredMetric, opt.Quality.VMAF.Target, opt.Quality.VMAF.Minimum)
	}
	if opt.Search == nil {
		t.Fatal("expected search policy")
	}
	wantBR := []int{3200, 3500, 3800}
	if len(opt.Search.BitrateValues) != len(wantBR) {
		t.Fatalf("bitrate_values = %v, want %v", opt.Search.BitrateValues, wantBR)
	}
	for i, want := range wantBR {
		if opt.Search.BitrateValues[i] != want {
			t.Fatalf("bitrate_values = %v, want %v", opt.Search.BitrateValues, wantBR)
		}
	}
	if len(opt.Search.QualityValues) != 0 {
		t.Fatalf("bitrate sweep must not carry quality_values: %v", opt.Search.QualityValues)
	}
}

func TestLiveActionHEVCX265ProfileUntouched(t *testing.T) {
	// The x265 live-action profile must survive the VideoToolbox capability
	// work byte-identical: same codec, CRF sweep, quality bar, no VT knobs.
	s, err := Parse(EmbeddedBytes())
	if err != nil {
		t.Fatalf("embedded bundle invalid: %v", err)
	}
	p, ok := s.Bundle.Profiles["live-action-hevc"]
	if !ok {
		t.Fatal("live-action-hevc profile missing from embedded bundle")
	}
	if strings.ToLower(strings.TrimSpace(p.Video.Codec)) != transcode.VideoCodecLibX265 {
		t.Fatalf("live-action-hevc codec changed to %q", p.Video.Codec)
	}
	if p.Video.Preset != "slow" || p.Video.Quality != 24 {
		t.Fatalf("live-action-hevc CRF/preset changed: quality=%d preset=%q", p.Video.Quality, p.Video.Preset)
	}
	if p.Video.AverageBitrateKbps != 0 || p.Video.MaxBitrateKbps != 0 || p.Video.ConstantBitrate != nil ||
		p.Video.QMin != nil || p.Video.QMax != nil || p.Video.GOPSize != nil || p.Video.BFrames != nil ||
		p.Video.ClosedGOP != nil || p.Video.PowerEfficient != nil || p.Video.MaxRefFrames != nil ||
		p.Video.PrioritizeSpeed != nil || p.Video.SpatialAQ != nil || p.Video.Realtime != nil {
		t.Fatalf("live-action-hevc gained VideoToolbox knobs: %+v", p.Video)
	}
	if p.Optimization == nil || len(p.Optimization.Search.QualityValues) != 4 || len(p.Optimization.Search.BitrateValues) != 0 {
		t.Fatalf("live-action-hevc search changed: %+v", p.Optimization.Search)
	}
}

func TestLibX265RecipeRejectsOutOfRangeCRFAndPreset(t *testing.T) {
	base := string(EmbeddedBytes())

	badCRF := strings.Replace(base, "codec: libx265\n      quality: 24", "codec: libx265\n      quality: 60", 1)
	if _, err := Parse([]byte(badCRF)); err == nil || !strings.Contains(err.Error(), "libx265 crf") {
		t.Fatalf("out-of-range CRF did not fail closed: %v", err)
	}

	badPreset := strings.Replace(base, "preset: slow", "preset: turbo", 1)
	if _, err := Parse([]byte(badPreset)); err == nil || !strings.Contains(err.Error(), "libx265 preset") {
		t.Fatalf("invalid preset did not fail closed: %v", err)
	}

	badSearchCRF := strings.Replace(base, "quality_values: [20, 22, 24, 26]", "quality_values: [20, 22, 24, 70]", 1)
	if _, err := Parse([]byte(badSearchCRF)); err == nil || !strings.Contains(err.Error(), "search crf") {
		t.Fatalf("out-of-range search CRF did not fail closed: %v", err)
	}
}
