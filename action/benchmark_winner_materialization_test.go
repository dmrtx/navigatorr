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

func TestBenchmarkWaitBindsFinalQualityToImmutableRequest(t *testing.T) {
	req := transcode.BenchmarkRequest{
		ID: "bench-quality", Metric: "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 10, DurationSeconds: 5}},
		Quality: &transcode.BenchmarkQualityConfig{
			VMAF:            &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Target: 96, Minimum: 95},
			FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"},
		},
	}
	digest, err := transcode.DigestBenchmarkRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	mock := &mockTranscodeExecutor{benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
		return transcode.BenchmarkStatus{Status: transcode.StatusCompleted, Decision: &transcode.BenchmarkDecision{Winner: &transcode.BenchmarkWinner{CandidateID: "winner", VideoCodec: transcode.VideoCodecHEVCVideoToolbox, Quality: 65, VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8, MetricType: "vmaf", Score: 97}}}, nil
	}}
	e := NewEngine(EngineDeps{Transcode: mock})
	contextFor := func(request transcode.BenchmarkRequest) *ExecutionContext {
		return &ExecutionContext{InstanceID: "bench-quality", ActionName: "benchmark_transcode", Inputs: map[string]any{}, Outputs: map[string]any{}, State: map[string]any{
			"optimization_enabled": true, "benchmark_job_id": req.ID, "benchmark_request": request, "benchmark_request_digest": digest,
			"plan": &transcode.Plan{Container: "mkv", VideoCodec: transcode.VideoCodecHEVCVideoToolbox, Quality: 70},
		}}
	}
	ec := contextFor(req)
	res, err := e.stepBenchmarkWait(context.Background(), ec)
	if err != nil || res.Status != StepCompleted {
		t.Fatalf("valid quality handoff failed: %+v %v", res, err)
	}
	plan := getPlan(ec.State["plan"])
	if plan == nil || plan.QualityValidation == nil || plan.QualityValidation.BenchmarkRequestDigest != digest || len(plan.QualityValidation.Samples) != 1 {
		t.Fatalf("missing immutable final quality plan: %+v", plan)
	}
	changed := req
	changed.Quality = &transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Target: 96, Minimum: 50}, FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}
	res, err = e.stepBenchmarkWait(context.Background(), contextFor(changed))
	if err != nil || res.Status != StepFailed {
		t.Fatalf("tampered quality request accepted: %+v %v", res, err)
	}
}
