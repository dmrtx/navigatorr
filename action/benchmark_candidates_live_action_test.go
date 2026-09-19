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
	candidates, err := buildBenchmarkCandidates(8, transcode.VideoCodecLibX265, "slow", []int{20, 22, 24, 26}, 4)
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

func TestBuildBenchmarkCandidatesVideoToolboxUnchanged(t *testing.T) {
	candidates, err := buildBenchmarkCandidates(8, "", "", []int{60, 65, 70}, 5)
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
	candidates, err := buildBenchmarkCandidates(8, transcode.VideoCodecLibX265, "", []int{24}, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Preset != "medium" {
		t.Fatalf("expected default medium preset, got %+v", candidates)
	}
}

func TestBuildBenchmarkCandidatesRejectsInvalidInputs(t *testing.T) {
	if _, err := buildBenchmarkCandidates(8, transcode.VideoCodecLibX265, "slow", []int{52}, 4); err == nil || !strings.Contains(err.Error(), "invalid libx265 crf") {
		t.Fatalf("out-of-range CRF did not fail closed: %v", err)
	}
	if _, err := buildBenchmarkCandidates(8, transcode.VideoCodecLibX265, "turbo", []int{24}, 4); err == nil || !strings.Contains(err.Error(), "unsupported libx265 preset") {
		t.Fatalf("invalid preset did not fail closed: %v", err)
	}
	if _, err := buildBenchmarkCandidates(8, "libx264", "", []int{24}, 4); err == nil || !strings.Contains(err.Error(), "unsupported video codec") {
		t.Fatalf("unsupported codec did not fail closed: %v", err)
	}
	if _, err := buildBenchmarkCandidates(8, transcode.VideoCodecHEVCVideoToolbox, "slow", []int{65}, 4); err == nil || !strings.Contains(err.Error(), "only supported for libx265") {
		t.Fatalf("preset for VideoToolbox did not fail closed: %v", err)
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
