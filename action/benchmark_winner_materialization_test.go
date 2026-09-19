package action

import (
	"context"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// TestBenchmarkWaitMaterializesWinningEncoderAndPreset proves that a libx265
// winner's encoder and preset are carried into the materialized full-encode
// plan (and exposed as outputs), rather than silently inheriting the base
// plan's VideoToolbox encoder.
func TestBenchmarkWaitMaterializesWinningEncoderAndPreset(t *testing.T) {
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusCompleted,
				Progress:        100,
				Decision: &transcode.BenchmarkDecision{
					Winner: &transcode.BenchmarkWinner{
						CandidateID:      "cand_crf24",
						VideoCodec:       transcode.VideoCodecLibX265,
						Quality:          24,
						Preset:           "slow",
						VideoProfile:     "main",
						PixelFormat:      "yuv420p",
						ExpectedBitDepth: 8,
						MetricType:       "vmaf",
						Score:            93.5,
						SavingsPercent:   40,
					},
					DecisionReason: "cand_crf24 optimal",
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
				Container:  "mkv",
				VideoCodec: transcode.VideoCodecHEVCVideoToolbox,
				Quality:    65,
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
	if plan.VideoCodec != transcode.VideoCodecLibX265 || plan.Preset != "slow" {
		t.Fatalf("materialized plan codec/preset = %q/%q, want libx265/slow", plan.VideoCodec, plan.Preset)
	}
	if plan.Quality != 24 || plan.PlanDigest == "" {
		t.Fatalf("materialized plan quality/digest = %d/%q, want 24 and a recomputed digest", plan.Quality, plan.PlanDigest)
	}
	if res.Outputs["winner_video_codec"] != transcode.VideoCodecLibX265 {
		t.Fatalf("winner_video_codec output = %v, want libx265", res.Outputs["winner_video_codec"])
	}
	if res.Outputs["winner_preset"] != "slow" {
		t.Fatalf("winner_preset output = %v, want slow", res.Outputs["winner_preset"])
	}
}
