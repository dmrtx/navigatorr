package action

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

func TestBuildBenchmarkCandidatesLibX265(t *testing.T) {
	base := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 24, Preset: "slow"}
	srch := &recipe.SearchPolicy{MaxCandidates: 4, QualityValues: []int{20, 22, 24, 26}}
	candidates, err := buildBenchmarkCandidates(base, 8, srch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 4 {
		t.Fatalf("expected 4 candidates, got %d", len(candidates))
	}
	for i, want := range []int{20, 22, 24, 26} {
		c := candidates[i]
		if c.VideoCodec != transcode.VideoCodecLibX265 {
			t.Fatalf("candidate %d codec = %q, want libx265", i, c.VideoCodec)
		}
		if c.Quality != want {
			t.Fatalf("candidate %d crf = %d, want %d", i, c.Quality, want)
		}
		if c.Preset != "slow" {
			t.Fatalf("candidate %d preset = %q, want slow", i, c.Preset)
		}
		if c.VideoProfile != "main" || c.PixelFormat != "yuv420p" {
			t.Fatalf("candidate %d profile/pix = %q/%q, want main/yuv420p", i, c.VideoProfile, c.PixelFormat)
		}
		if c.ID != fmt.Sprintf("cand_crf%d", want) {
			t.Fatalf("candidate %d id = %q, want cand_crf%d", i, c.ID, want)
		}
	}
}

func TestLibX265BenchmarkUsesResolvedMain10OnEightBitSource(t *testing.T) {
	base := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 20, Preset: "medium", Tune: "animation", VideoProfile: "main10", PixelFormat: "yuv420p10le", ExpectedBitDepth: 10}
	candidates, err := buildBenchmarkCandidates(base, 8, &recipe.SearchPolicy{MaxCandidates: 3, QualityValues: []int{18, 20, 22}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range candidates {
		if c.VideoProfile != "main10" || c.PixelFormat != "yuv420p10le" || c.Preset != "medium" || c.Tune != "animation" {
			t.Fatalf("x265 experiment was changed to source shape: %+v", c)
		}
	}
}

func TestBuildBenchmarkCandidatesVideoToolboxUnchanged(t *testing.T) {
	base := &transcode.Plan{VideoCodec: transcode.VideoCodecHEVCVideoToolbox, Quality: 65}
	srch := &recipe.SearchPolicy{MaxCandidates: 5, QualityValues: []int{60, 65, 70}}
	candidates, err := buildBenchmarkCandidates(base, 8, srch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}
	for i, c := range candidates {
		if transcode.BenchmarkCandidateVideoCodec(c) != transcode.VideoCodecHEVCVideoToolbox {
			t.Fatalf("candidate %d codec = %q, want hevc_videotoolbox", i, c.VideoCodec)
		}
		if c.Preset != "" {
			t.Fatalf("candidate %d unexpectedly has preset %q", i, c.Preset)
		}
		if c.ID != fmt.Sprintf("cand_q%d", c.Quality) {
			t.Fatalf("candidate %d id = %q", i, c.ID)
		}
	}
}

func TestBuildBenchmarkCandidatesLibX265DefaultsPreset(t *testing.T) {
	base := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 24}
	srch := &recipe.SearchPolicy{MaxCandidates: 4, QualityValues: []int{24}}
	candidates, err := buildBenchmarkCandidates(base, 8, srch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Preset != "medium" {
		t.Fatalf("expected default medium preset, got %+v", candidates)
	}
}

func TestBuildBenchmarkCandidatesRejectsInvalidInputs(t *testing.T) {
	cases := []struct {
		name    string
		codec   string
		preset  string
		qVals   []int
		brVals  []int
		wantErr string
	}{
		{name: "out-of-range CRF", codec: transcode.VideoCodecLibX265, preset: "slow", qVals: []int{52}, wantErr: "invalid libx265 crf"},
		{name: "invalid preset", codec: transcode.VideoCodecLibX265, preset: "turbo", qVals: []int{24}, wantErr: "unsupported libx265 preset"},
		{name: "unsupported codec", codec: "libx264", qVals: []int{24}, wantErr: "unsupported video codec"},
		{name: "preset for VideoToolbox", codec: transcode.VideoCodecHEVCVideoToolbox, preset: "slow", qVals: []int{65}, wantErr: "only supported for libx265"},
		{name: "bitrate sweep for libx265", codec: transcode.VideoCodecLibX265, preset: "slow", brVals: []int{3500}, wantErr: "only supported for hevc_videotoolbox"},
		{name: "mixed sweep", codec: transcode.VideoCodecHEVCVideoToolbox, qVals: []int{65}, brVals: []int{3500}, wantErr: "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := &transcode.Plan{VideoCodec: tc.codec, Preset: tc.preset, Quality: 24}
			srch := &recipe.SearchPolicy{MaxCandidates: 4, QualityValues: tc.qVals, BitrateValues: tc.brVals}
			if _, err := buildBenchmarkCandidates(base, 8, srch); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestBuildBenchmarkAdaptiveDisabledForLibX265(t *testing.T) {
	// Adaptive ordering assumes ascending value => higher quality, which is false
	// for libx265 CRF (lower = higher quality), so libx265 must use exhaustive.
	srch := &recipe.SearchPolicy{AdaptiveMode: "adaptive", AdaptiveInitialQuality: 24, QualityValues: []int{20, 22, 24, 26}}
	if cfg := buildBenchmarkAdaptiveConfig(srch, transcode.VideoCodecLibX265); cfg != nil {
		t.Fatalf("expected exhaustive (nil adaptive config) for libx265, got %+v", cfg)
	}
	// VideoToolbox keeps adaptive behavior unchanged.
	if cfg := buildBenchmarkAdaptiveConfig(srch, transcode.VideoCodecHEVCVideoToolbox); cfg == nil || cfg.Mode != "adaptive" {
		t.Fatalf("expected adaptive config preserved for videotoolbox, got %+v", cfg)
	}
}

// TestBuildBenchmarkRequestFailsClosedWithoutResolvedPlan proves that a missing
// or unreadable resolved plan cannot silently degrade the benchmark to
// VideoToolbox candidates. The encoder must come from the plan (fail closed).
func TestBuildBenchmarkRequestFailsClosedWithoutResolvedPlan(t *testing.T) {
	rep := &mediainspect.DetailedReport{
		DurationSec: 100,
		Video:       []mediainspect.DetailedStream{{BitDepth: 8, BitRate: 5000000}},
	}
	opt := &recipe.OptimizationPolicy{
		Enabled:  true,
		Sampling: &recipe.SamplingPolicy{SampleSeconds: 5, SampleCount: 1, Positions: []float64{0.5}},
		Search:   &recipe.SearchPolicy{MaxCandidates: 4, QualityValues: []int{24}},
		Quality:  &recipe.QualityPolicy{PreferredMetric: "vmaf"},
	}

	cases := []struct {
		name  string
		state map[string]any
	}{
		{name: "missing plan", state: map[string]any{}},
		{name: "unreadable plan", state: map[string]any{"plan": make(chan int)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ec := &ExecutionContext{InstanceID: "bench-test", State: tc.state}
			req, err := buildBenchmarkRequest(ec, "/media/source.mkv", rep, opt, transcode.WorkerCapabilities{})
			if err == nil {
				t.Fatalf("expected fail-closed error, got request %+v", req)
			}
			if !strings.Contains(err.Error(), "resolved plan is missing or unreadable") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestBuildBenchmarkRequestRejectsNativeMain10CAMBI(t *testing.T) {
	rep := &mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: 10, BitRate: 5000000, FPS: 24}}}
	opt := &recipe.OptimizationPolicy{
		Enabled:  true,
		Sampling: &recipe.SamplingPolicy{SampleSeconds: 5, SampleCount: 1, Positions: []float64{0.5}},
		Search:   &recipe.SearchPolicy{MaxCandidates: 4, QualityValues: []int{65}},
		Quality:  &recipe.QualityPolicy{PreferredMetric: "ssim", Banding: &recipe.BandingPolicy{Enabled: true, Metric: "cambi", Mode: "full_ref", Enforcement: "observe"}},
	}
	ec := &ExecutionContext{InstanceID: "bench-main10-cambi", Inputs: map[string]any{}, State: map[string]any{"plan": &transcode.Plan{VideoCodec: transcode.VideoCodecHEVCVideoToolbox, Quality: 65}}}
	_, err := buildBenchmarkRequest(ec, "/media/source.mkv", rep, opt, transcode.WorkerCapabilities{})
	if err == nil || !strings.Contains(err.Error(), "quality_cambi_native_main10_unverified") {
		t.Fatalf("native Main10 CAMBI was not rejected: %v", err)
	}
}

func TestBuildBenchmarkRequestMain10CompactAudio(t *testing.T) {
	rep := &mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: 10, BitRate: 5000000, FPS: 24}}}
	opt := &recipe.OptimizationPolicy{
		Enabled:  true,
		Sampling: &recipe.SamplingPolicy{SampleSeconds: 5, SampleCount: 1, Positions: []float64{0.5}},
		Search:   &recipe.SearchPolicy{MaxCandidates: 3, QualityValues: []int{20, 23, 26}},
		Quality:  &recipe.QualityPolicy{PreferredMetric: "vmaf", VMAF: &recipe.MetricTarget{Model: "v1_1080p_3h", Target: 96, Minimum: 95}, Banding: &recipe.BandingPolicy{Enabled: true, Metric: "cambi", Mode: "full_ref", Enforcement: "observe"}},
	}
	caps := transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, CompactAudio: true, Encoders: map[string]bool{"libx265": true}, Filters: map[string]bool{"libvmaf": true, "ssim": true}, Quality: &transcode.QualityCapabilities{Native10Bit: true, CAMBIFullRef: true, Models: map[string]transcode.QualityModelCapability{"v1_1080p_3h": {Available: true}}}}
	ec := &ExecutionContext{InstanceID: "bench-main10-compact", Inputs: map[string]any{}, State: map[string]any{"plan": &transcode.Plan{VideoCodec: "libx265", Quality: 23, AudioMode: "compact"}}}
	req, err := buildBenchmarkRequest(ec, "/media/source.mkv", rep, opt, caps)
	if err != nil {
		t.Fatal(err)
	}
	if req.AudioMode != "compact" || req.Metric != "vmaf" || len(req.Candidates) != 3 {
		t.Fatalf("unexpected request: %+v", req)
	}
	for _, c := range req.Candidates {
		if c.VideoProfile != "main10" || c.PixelFormat != "p010le" {
			t.Fatalf("lost source precision: %+v", c)
		}
	}
	if metric, err := resolveAutoBenchmarkMetric("auto", caps, 10); err != nil || metric != "vmaf" {
		t.Fatalf("verified auto: %s %v", metric, err)
	}
	caps.CompactAudio = false
	if _, err := buildBenchmarkRequest(ec, "/media/source.mkv", rep, opt, caps); err == nil || !strings.Contains(err.Error(), "compact audio") {
		t.Fatalf("old worker accepted audio: %v", err)
	}
	caps.Quality.Native10Bit = false
	if err := validateWorkerCapabilitiesForBenchmark(caps, "vmaf", 10, "libx265"); err == nil {
		t.Fatal("unverified native VMAF accepted")
	}
	if metric, err := resolveAutoBenchmarkMetric("auto", caps, 10); err != nil || metric != "ssim" {
		t.Fatalf("legacy auto: %s %v", metric, err)
	}
}
