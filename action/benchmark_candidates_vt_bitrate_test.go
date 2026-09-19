package action

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

func vtBitrateBasePlan() *transcode.Plan {
	return &transcode.Plan{
		Container:          "mkv",
		VideoCodec:         transcode.VideoCodecHEVCVideoToolbox,
		AverageBitrateKbps: 3500,
		VideoProfile:       "main",
		PixelFormat:        "yuv420p",
		PrioritizeSpeed:    boolPtr(false),
		SpatialAQ:          boolPtr(true),
		Realtime:           boolPtr(false),
	}
}

func boolPtr(v bool) *bool { return &v }

func intPtr(v int) *int { return &v }

func TestBuildBenchmarkCandidatesVTBitrate(t *testing.T) {
	srch := &recipe.SearchPolicy{MaxCandidates: 3, BitrateValues: []int{3200, 3500, 3800}}
	candidates, err := buildBenchmarkCandidates(vtBitrateBasePlan(), 8, srch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}
	for i, want := range []int{3200, 3500, 3800} {
		c := candidates[i]
		if c.VideoCodec != transcode.VideoCodecHEVCVideoToolbox {
			t.Fatalf("candidate %d codec = %q, want hevc_videotoolbox", i, c.VideoCodec)
		}
		if c.Quality != 0 || c.AverageBitrateKbps != want {
			t.Fatalf("candidate %d quality/bitrate = %d/%d, want 0/%d", i, c.Quality, c.AverageBitrateKbps, want)
		}
		if c.ID != fmt.Sprintf("cand_br%dk", want) {
			t.Fatalf("candidate %d id = %q, want cand_br%dk", i, c.ID, want)
		}
		// Base-plan knobs are inherited verbatim.
		if c.VideoProfile != "main" || c.PixelFormat != "yuv420p" {
			t.Fatalf("candidate %d profile/pix = %q/%q, want main/yuv420p", i, c.VideoProfile, c.PixelFormat)
		}
		if c.PrioritizeSpeed == nil || *c.PrioritizeSpeed || c.SpatialAQ == nil || !*c.SpatialAQ || c.Realtime == nil || *c.Realtime {
			t.Fatalf("candidate %d did not inherit prio/aq/realtime knobs: %+v", i, c)
		}
		if c.Preset != "" {
			t.Fatalf("candidate %d unexpectedly has preset %q", i, c.Preset)
		}
	}
}

func TestBuildBenchmarkCandidatesVTBitrateMaxCap(t *testing.T) {
	base := vtBitrateBasePlan()
	base.MaxBitrateKbps = 3400
	srch := &recipe.SearchPolicy{MaxCandidates: 3, BitrateValues: []int{3200, 3500, 3800}}
	if _, err := buildBenchmarkCandidates(base, 8, srch); err == nil || !strings.Contains(err.Error(), "exceeds inherited max_bitrate_kbps") {
		t.Fatalf("sweep exceeding inherited maxrate did not fail closed: %v", err)
	}
}

func TestBuildBenchmarkCandidatesVTQualityInheritsKnobs(t *testing.T) {
	base := &transcode.Plan{
		VideoCodec: transcode.VideoCodecHEVCVideoToolbox, Quality: 65,
		GOPSize: intPtr(300), BFrames: intPtr(0), ClosedGOP: boolPtr(true),
	}
	srch := &recipe.SearchPolicy{MaxCandidates: 2, QualityValues: []int{60, 65}}
	candidates, err := buildBenchmarkCandidates(base, 8, srch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 2 || candidates[0].ID != "cand_q60" {
		t.Fatalf("unexpected candidates: %+v", candidates)
	}
	if candidates[0].GOPSize == nil || *candidates[0].GOPSize != 300 || candidates[0].BFrames == nil || *candidates[0].BFrames != 0 {
		t.Fatalf("quality candidate did not inherit offline knobs: %+v", candidates[0])
	}
	if candidates[0].AverageBitrateKbps != 0 {
		t.Fatalf("quality candidate unexpectedly carries bitrate: %+v", candidates[0])
	}
}

func TestBuildBenchmarkAdaptiveExhaustiveForBitrate(t *testing.T) {
	// Bitrate sweeps cannot use adaptive probing (quality-ordered planner), so
	// they fall back to exhaustive exactly like libx265 CRF sweeps.
	srch := &recipe.SearchPolicy{AdaptiveMode: "adaptive", AdaptiveInitialQuality: 65, BitrateValues: []int{3200, 3500, 3800}}
	if cfg := buildBenchmarkAdaptiveConfig(srch, transcode.VideoCodecHEVCVideoToolbox); cfg != nil {
		t.Fatalf("expected exhaustive (nil adaptive config) for bitrate sweep, got %+v", cfg)
	}
}

// TestBenchmarkWaitMaterializesBitrateWinner proves a VideoToolbox bitrate
// winner's average bitrate (and not a quality value) lands in the
// materialized full-encode plan with a recomputed digest.
func TestBenchmarkWaitMaterializesBitrateWinner(t *testing.T) {
	audioAQ := true
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusCompleted,
				Progress:        100,
				Decision: &transcode.BenchmarkDecision{
					Winner: &transcode.BenchmarkWinner{
						CandidateID:        "cand_br3500k",
						VideoCodec:         transcode.VideoCodecHEVCVideoToolbox,
						AverageBitrateKbps: 3500,
						PrioritizeSpeed:    boolPtr(false),
						SpatialAQ:          &audioAQ,
						VideoProfile:       "main",
						PixelFormat:        "yuv420p",
						ExpectedBitDepth:   8,
						MetricType:         "vmaf",
						Score:              92.4,
						SavingsPercent:     35,
					},
					DecisionReason: "cand_br3500k optimal",
				},
			}, nil
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := &ExecutionContext{
		InstanceID: "bench-test",
		ActionName: "benchmark_transcode",
		Inputs:     map[string]any{},
		Outputs:    map[string]any{},
		State: map[string]any{
			"optimization_enabled": true,
			"benchmark_job_id":     "bench-test",
			"plan": &transcode.Plan{
				Container:          "mkv",
				VideoCodec:         transcode.VideoCodecHEVCVideoToolbox,
				AverageBitrateKbps: 3500,
			},
		},
	}

	res, err := e.stepBenchmarkWait(context.Background(), ec)
	if err != nil {
		t.Fatalf("stepBenchmarkWait error: %v", err)
	}
	if res.Status != StepCompleted {
		t.Fatalf("status = %s, want completed (error: %s)", res.Status, res.Error)
	}
	plan := getPlan(ec.State["plan"])
	if plan == nil {
		t.Fatal("winning plan was not materialized")
	}
	if plan.AverageBitrateKbps != 3500 || plan.Quality != 0 || plan.PlanDigest == "" {
		t.Fatalf("materialized plan bitrate/quality/digest = %d/%d/%q, want 3500/0 and a recomputed digest",
			plan.AverageBitrateKbps, plan.Quality, plan.PlanDigest)
	}
	if plan.SpatialAQ == nil || !*plan.SpatialAQ {
		t.Fatalf("materialized plan lost winner spatial_aq: %+v", plan)
	}
	if res.Outputs["winner_average_bitrate_kbps"] != 3500 {
		t.Fatalf("winner_average_bitrate_kbps output = %v, want 3500", res.Outputs["winner_average_bitrate_kbps"])
	}
}

// TestBenchmarkWaitRetainsBaseKnobsForSparseWinner proves a winner that omits
// optional knobs (foreign or pre-upgrade in-flight job) does not silently
// clear the base plan's encoder configuration.
func TestBenchmarkWaitRetainsBaseKnobsForSparseWinner(t *testing.T) {
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusCompleted,
				Progress:        100,
				Decision: &transcode.BenchmarkDecision{
					Winner: &transcode.BenchmarkWinner{
						CandidateID:      "cand_q55",
						Quality:          55,
						VideoProfile:     "main",
						PixelFormat:      "yuv420p",
						ExpectedBitDepth: 8,
						MetricType:       "ssim",
						Score:            0.99,
					},
					DecisionReason: "sparse winner",
				},
			}, nil
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := &ExecutionContext{
		InstanceID: "bench-test",
		ActionName: "benchmark_transcode",
		Inputs:     map[string]any{},
		Outputs:    map[string]any{},
		State: map[string]any{
			"optimization_enabled": true,
			"benchmark_job_id":     "bench-test",
			"plan": &transcode.Plan{
				Container:       "mkv",
				VideoCodec:      transcode.VideoCodecHEVCVideoToolbox,
				Quality:         75,
				PrioritizeSpeed: boolPtr(false),
				SpatialAQ:       boolPtr(true),
			},
		},
	}
	res, err := e.stepBenchmarkWait(context.Background(), ec)
	if err != nil {
		t.Fatalf("stepBenchmarkWait error: %v", err)
	}
	if res.Status != StepCompleted {
		t.Fatalf("status = %s, want completed (error: %s)", res.Status, res.Error)
	}
	plan := getPlan(ec.State["plan"])
	if plan == nil || plan.Quality != 55 {
		t.Fatalf("winning quality not materialized: %+v", plan)
	}
	if plan.PrioritizeSpeed == nil || *plan.PrioritizeSpeed || plan.SpatialAQ == nil || !*plan.SpatialAQ {
		t.Fatalf("sparse winner cleared base knobs: %+v", plan)
	}
}
