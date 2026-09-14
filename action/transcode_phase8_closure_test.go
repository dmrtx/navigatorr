package action

// Phase 8 closure tests for the transcode optimization milestone.
//
// These tests lock the final integration guarantees required before the
// single PR towards main: the 10-bit SDR full candidate path, the absence
// of worker-internal secrets/paths in persisted coordinator outputs, and
// the public benchmark API surface carrying no secret fields.
//
// All external boundaries are stubbed with the same clearly-separated test
// doubles used throughout this file's suite: a fake ffprobe shell script
// (createSmartFakeFFprobeScript) and mockTranscodeExecutor. Nothing here
// exercises real Apple Silicon hardware; live-hardware evidence is recorded
// separately in docs/TRANSCODE_OPTIMIZATION_IMPLEMENTATION_PLAN.md.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// TestPhase8_Optimized10BitSDRWinner_FullCandidatePath exercises the complete
// optimized orchestration for a 10-bit SDR source: preflight, capabilities,
// benchmark_transcode, winning-plan materialization with a concrete
// PlanDigest, full candidate submit, validate_result acceptance of the 10-bit
// candidate, and candidate-only accept_result. The source must remain
// untouched and replace_original must stay false throughout.
func TestPhase8_Optimized10BitSDRWinner_FullCandidatePath(t *testing.T) {
	var fullTranscodeReq transcode.Request
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusCompleted,
				Progress:        100,
				Decision: &transcode.BenchmarkDecision{
					Winner: &transcode.BenchmarkWinner{
						CandidateID:         "cand_q65_main10",
						CandidateIndex:      1,
						Quality:             65,
						VideoProfile:        "main10",
						PixelFormat:         "p010le",
						ExpectedBitDepth:    10,
						MetricType:          "ssim",
						Score:               0.992,
						TargetReached:       true,
						MinimumMet:          true,
						EstimatedVideoBytes: 400000000,
						EstimatedTotalBytes: 450000000,
						EstimatedTotalMB:    450.0,
						SavingsPercent:      50.0,
					},
					DecisionReason: "winner cand_q65_main10 optimal",
				},
			}, nil
		},
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			fullTranscodeReq = req
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
			_ = os.WriteFile(req.CandidatePath, []byte("transcoded-output-10bit"), 0644)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:       jobID,
				Status:   transcode.StatusCompleted,
				Progress: 100,
			}, nil
		},
	}

	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard10BitSDRProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
		"metric":  "ssim",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("expected StatusCompleted, got %s (error: %s)", res.Status, res.Error)
	}

	if atomic.LoadInt32(&mock.benchmarkSubmitCalls) != 1 {
		t.Errorf("expected 1 benchmark submit call, got %d", atomic.LoadInt32(&mock.benchmarkSubmitCalls))
	}
	if atomic.LoadInt32(&mock.submitCalls) != 1 {
		t.Errorf("expected 1 full transcode submit call, got %d", atomic.LoadInt32(&mock.submitCalls))
	}

	if fullTranscodeReq.Plan == nil {
		t.Fatalf("submitted transcode request has nil Plan")
	}
	if fullTranscodeReq.Plan.Quality != 65 {
		t.Errorf("submitted plan quality = %d, want winner's quality 65", fullTranscodeReq.Plan.Quality)
	}
	if fullTranscodeReq.Plan.VideoProfile != "main10" {
		t.Errorf("submitted plan video profile = %q, want main10", fullTranscodeReq.Plan.VideoProfile)
	}
	if fullTranscodeReq.Plan.PixelFormat != "p010le" {
		t.Errorf("submitted plan pixel format = %q, want p010le", fullTranscodeReq.Plan.PixelFormat)
	}
	if fullTranscodeReq.Plan.ExpectedBitDepth != 10 {
		t.Errorf("submitted plan expected bit depth = %d, want 10", fullTranscodeReq.Plan.ExpectedBitDepth)
	}

	expectedDigest, err := transcode.DigestPlan(fullTranscodeReq.Plan)
	if err != nil {
		t.Fatalf("DigestPlan error: %v", err)
	}
	if fullTranscodeReq.Plan.PlanDigest != expectedDigest {
		t.Errorf("PlanDigest %s does not match expected %s", fullTranscodeReq.Plan.PlanDigest, expectedDigest)
	}
	if getString(res.Outputs, "plan_digest") != expectedDigest {
		t.Errorf("action output plan_digest %q does not match concrete winning plan digest %q",
			getString(res.Outputs, "plan_digest"), expectedDigest)
	}

	// Candidate-only acceptance: the original must be byte-identical and no
	// replace_original behavior may have occurred.
	sourceBytes, _ := os.ReadFile(mediaFile)
	if string(sourceBytes) != "fake-source-media-bytes-for-benchmark-testing" {
		t.Errorf("source media was unexpectedly mutated")
	}
}

// TestPhase8_PersistedOutputsLeakNoWorkerInternals runs both the
// benchmark-only action and the full optimized transcode_media action, then
// serializes their persisted outputs and asserts that no worker-internal
// secrets or scratch paths leak into coordinator-visible state: no run
// tokens, no file-lock names, no samples/scratch directories, and no worker
// persistence filenames. Legitimate identifiers (benchmark job IDs,
// plan/recipe digests, media and candidate paths) must remain present.
func TestPhase8_PersistedOutputsLeakNoWorkerInternals(t *testing.T) {
	winnerStatus := func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
		return transcode.BenchmarkStatus{
			ProtocolVersion: transcode.WorkerProtocolVersion,
			ID:              jobID,
			Status:          transcode.StatusCompleted,
			Progress:        100,
			Decision: &transcode.BenchmarkDecision{
				Winner: &transcode.BenchmarkWinner{
					CandidateID:      "cand_q70",
					CandidateIndex:   2,
					Quality:          70,
					VideoProfile:     "main",
					PixelFormat:      "yuv420p",
					ExpectedBitDepth: 8,
					MetricType:       "vmaf",
					Score:            96.8,
					TargetReached:    true,
					MinimumMet:       true,
				},
				DecisionReason: "candidate cand_q70 reached target",
			},
		}, nil
	}

	mockBench := &mockTranscodeExecutor{benchmarkStatusFunc: winnerStatus}
	engineBench, _, mediaBench, _ := setupBenchmarkTestEnv(t, mockBench, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
	resBench, err := engineBench.Run(context.Background(), "benchmark_transcode", map[string]any{
		"path":    mediaBench,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("benchmark run error: %v", err)
	}
	if resBench.Status != StatusCompleted {
		t.Fatalf("expected benchmark StatusCompleted, got %s (error: %s)", resBench.Status, resBench.Error)
	}

	mockFull := &mockTranscodeExecutor{
		benchmarkStatusFunc: winnerStatus,
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
			_ = os.WriteFile(req.CandidatePath, []byte("transcoded-output"), 0644)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted, Progress: 100}, nil
		},
	}
	engineFull, _, mediaFull, _ := setupBenchmarkTestEnv(t, mockFull, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
	resFull, err := engineFull.Run(context.Background(), "transcode_media", map[string]any{
		"path":    mediaFull,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("transcode run error: %v", err)
	}
	if resFull.Status != StatusCompleted {
		t.Fatalf("expected transcode StatusCompleted, got %s (error: %s)", resFull.Status, resFull.Error)
	}

	forbidden := []string{
		"run_token", "runToken", "RunToken", "RUN_TOKEN",
		".lock", "/samples/", "samples\\",
		"benchmark.json", "job.json", ".tmp",
		"process_start_time", "ProcessStartTime",
	}
	checkOutputs := func(name string, outputs map[string]any) {
		t.Helper()
		raw, err := json.Marshal(outputs)
		if err != nil {
			t.Fatalf("%s: marshaling outputs: %v", name, err)
		}
		lowered := strings.ToLower(string(raw))
		for _, needle := range forbidden {
			if strings.Contains(lowered, strings.ToLower(needle)) {
				t.Errorf("%s: persisted outputs leak worker-internal %q: %s", name, needle, string(raw))
			}
		}
	}
	checkOutputs("benchmark_transcode", resBench.Outputs)
	checkOutputs("transcode_media", resFull.Outputs)

	// Required explainable identifiers must still be present.
	for _, key := range []string{"benchmark_job_id", "has_winner", "winner_candidate_id", "plan_digest"} {
		if _, ok := resBench.Outputs[key]; !ok {
			t.Errorf("benchmark_transcode outputs missing required key %q", key)
		}
	}
	if getString(resFull.Outputs, "plan_digest") == "" {
		t.Errorf("transcode_media outputs missing plan_digest")
	}
}

// TestPhase8_BenchmarkAPITypesExposeNoSecrets guards the public coordinator/
// worker contract against secret leakage at the type level: none of the
// benchmark status, decision, winner, or per-candidate evaluation structs
// may carry a token/secret field (name or JSON tag). Worker run tokens must
// stay inside benchmark.json on the worker and never cross the protocol.
func TestPhase8_BenchmarkAPITypesExposeNoSecrets(t *testing.T) {
	types := []any{
		transcode.BenchmarkStatus{},
		transcode.BenchmarkDecision{},
		transcode.BenchmarkWinner{},
		transcode.BenchmarkCandidateEvaluation{},
		transcode.BenchmarkJob{},
		transcode.BenchmarkSubmitResponse{},
		transcode.BenchmarkCancelResponse{},
	}
	for _, v := range types {
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			name := strings.ToLower(f.Name + " " + f.Tag.Get("json"))
			if strings.Contains(name, "token") || strings.Contains(name, "secret") {
				t.Errorf("%s field %s looks like a leaked secret carrier (json tag %q)",
					rt.Name(), f.Name, f.Tag.Get("json"))
			}
		}
	}
}

const liveValidation8BitProbeJSON = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "h264", "profile": "High", "pix_fmt": "yuv420p", "width": 1920, "height": 1080, "bit_rate": "5000000"},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2, "tags": {"language": "jpn"}}
  ],
  "format": {"format_name": "matroska", "duration": "1200.0", "size": "750000000", "bit_rate": "5000000"},
  "chapters": []
}`

// TestPhase8_LiveValidationReproduction_AnimeHevcQualitySSIMWinner tests the exact
// live reproduction from deployed 5b1be607: running transcode_media with profile
// anime-hevc-quality and metric ssim. Preflight must wire optimization_enabled=true,
// BenchmarkSubmit must be called exactly once, the benchmark steps must not be skipped,
// the winner must rewrite quality (55), profile (main), pixfmt (yuv420p), and bit depth (8),
// the winning PlanDigest must differ from the static base digest, and full transcode Submit
// must receive the winning plan with the new PlanDigest.
func TestPhase8_LiveValidationReproduction_AnimeHevcQualitySSIMWinner(t *testing.T) {
	var fullTranscodeReq transcode.Request
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusCompleted,
				Progress:        100,
				Decision: &transcode.BenchmarkDecision{
					Winner: &transcode.BenchmarkWinner{
						CandidateID:         "cand_q55",
						CandidateIndex:      0,
						Quality:             55,
						VideoProfile:        "main",
						PixelFormat:         "yuv420p",
						ExpectedBitDepth:    8,
						MetricType:          "ssim",
						Score:               0.9961,
						TargetReached:       true,
						MinimumMet:          true,
						EstimatedVideoBytes: 280000000,
						EstimatedTotalBytes: 320000000,
						EstimatedTotalMB:    320.0,
						SavingsPercent:      42.0,
					},
					DecisionReason: "winner cand_q55 optimal ssim 0.9961",
				},
			}, nil
		},
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			fullTranscodeReq = req
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
			_ = os.WriteFile(req.CandidatePath, []byte("transcoded-output-anime-hevc-quality"), 0644)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:       jobID,
				Status:   transcode.StatusCompleted,
				Progress: 100,
			}, nil
		},
	}

	// Setup with 8-bit probe without subtitles matching the live M1 fixture, so
	// anime-hevc-quality yields the exact live base digest sha256:5f379070...
	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, liveValidation8BitProbeJSON, nil)

	// Verify the static base plan digest for anime-hevc-quality before running.
	basePlan, err := engine.deps.Config.Transcode.ResolvePlan("anime-hevc-quality")
	if err != nil {
		t.Fatalf("resolving base plan for anime-hevc-quality: %v", err)
	}
	const expectedStaticBaseDigest = "sha256:5f3790702c4416380cb31fe56b128aa646aa5bd3fd577ca4b3435165e1a34bf6"
	if basePlan.PlanDigest != expectedStaticBaseDigest {
		t.Fatalf("static base plan digest = %q, want %q", basePlan.PlanDigest, expectedStaticBaseDigest)
	}

	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":             mediaFile,
		"profile":          "anime-hevc-quality",
		"metric":           "ssim",
		"replace_original": false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("expected StatusCompleted, got %s (error: %s)", res.Status, res.Error)
	}

	// 1. Preflight outputs must include optimization_enabled=true
	if !getBool(res.Outputs, "optimization_enabled") {
		t.Errorf("expected outputs to include optimization_enabled=true")
	}

	// 2. Assert transcode_media does not skip benchmark; BenchmarkSubmit called exactly once
	if atomic.LoadInt32(&mock.benchmarkSubmitCalls) != 1 {
		t.Errorf("expected 1 benchmark submit call, got %d", atomic.LoadInt32(&mock.benchmarkSubmitCalls))
	}
	if atomic.LoadInt32(&mock.submitCalls) != 1 {
		t.Errorf("expected 1 full transcode submit call, got %d", atomic.LoadInt32(&mock.submitCalls))
	}

	// 3. Winner rewrites quality, profile, pixfmt, and bit depth
	if fullTranscodeReq.Plan == nil {
		t.Fatalf("submitted transcode request has nil Plan")
	}
	if fullTranscodeReq.Plan.Quality != 55 {
		t.Errorf("submitted plan quality = %d, want winner's quality 55 (was base 75)", fullTranscodeReq.Plan.Quality)
	}
	if fullTranscodeReq.Plan.VideoProfile != "main" {
		t.Errorf("submitted plan video profile = %q, want %q", fullTranscodeReq.Plan.VideoProfile, "main")
	}
	if fullTranscodeReq.Plan.PixelFormat != "yuv420p" {
		t.Errorf("submitted plan pixel format = %q, want %q", fullTranscodeReq.Plan.PixelFormat, "yuv420p")
	}
	if fullTranscodeReq.Plan.ExpectedBitDepth != 8 {
		t.Errorf("submitted plan expected bit depth = %d, want 8", fullTranscodeReq.Plan.ExpectedBitDepth)
	}

	// 4. Concrete PlanDigest differs from static base and full Submit receives winner plan
	const expectedWinnerDigest = "sha256:bc5d40e661f50e1398d41674c96afee7e3f42027cf867dba21f35c7398803adf"
	if fullTranscodeReq.Plan.PlanDigest == expectedStaticBaseDigest {
		t.Errorf("submitted PlanDigest unexpectedly matches static base digest %s; static fallback occurred!", expectedStaticBaseDigest)
	}
	if fullTranscodeReq.Plan.PlanDigest != expectedWinnerDigest {
		t.Errorf("submitted PlanDigest = %s, want winner digest %s", fullTranscodeReq.Plan.PlanDigest, expectedWinnerDigest)
	}
	if getString(res.Outputs, "plan_digest") != expectedWinnerDigest {
		t.Errorf("action output plan_digest %q does not match concrete winning plan digest %q",
			getString(res.Outputs, "plan_digest"), expectedWinnerDigest)
	}

	// 5. Source media untouched and candidate isolation verified
	sourceBytes, _ := os.ReadFile(mediaFile)
	if string(sourceBytes) != "fake-source-media-bytes-for-benchmark-testing" {
		t.Errorf("source media was unexpectedly mutated")
	}
}

// TestPhase8_OptimizationDisabled_ExplicitPolicy_SkipsBenchmark tests that when
// a profile has optimization explicitly disabled (enabled: false), transcode_media
// remains unchanged, skips benchmark (BenchmarkSubmit called 0 times), and full
// Submit receives the base static plan, even if metric is specified.
func TestPhase8_OptimizationDisabled_ExplicitPolicy_SkipsBenchmark(t *testing.T) {
	var fullTranscodeReq transcode.Request
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			fullTranscodeReq = req
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
			_ = os.WriteFile(req.CandidatePath, []byte("transcoded-output-disabled"), 0644)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:       jobID,
				Status:   transcode.StatusCompleted,
				Progress: 100,
			}, nil
		},
	}

	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, nil)
	engine.deps.Config.Transcode.Profiles = map[string]config.TranscodeProfileConfig{
		"opt-disabled": {
			Container: "mkv",
			Video: config.VideoProfileConfig{
				Codec:   "hevc_videotoolbox",
				Quality: 70,
			},
			Audio:        config.AudioProfileConfig{Mode: "copy"},
			Subtitles:    config.SubtitleProfileConfig{Mode: "preserve", ConvertIncompatible: true},
			Preserve:     config.PreserveProfileConfig{Metadata: true, Chapters: true, Attachments: true},
			Optimization: &recipe.OptimizationPolicy{Enabled: false},
		},
	}

	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":             mediaFile,
		"profile":          "opt-disabled",
		"metric":           "ssim",
		"replace_original": false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("expected StatusCompleted, got %s (error: %s)", res.Status, res.Error)
	}

	if getBool(res.Outputs, "optimization_enabled") {
		t.Errorf("outputs unexpectedly reported optimization_enabled=true for disabled profile")
	}
	if atomic.LoadInt32(&mock.benchmarkSubmitCalls) != 0 {
		t.Errorf("expected 0 benchmark submit calls for optimization-disabled profile, got %d",
			atomic.LoadInt32(&mock.benchmarkSubmitCalls))
	}
	if atomic.LoadInt32(&mock.submitCalls) != 1 {
		t.Errorf("expected 1 full transcode submit call, got %d", atomic.LoadInt32(&mock.submitCalls))
	}
	if fullTranscodeReq.Plan == nil || fullTranscodeReq.Plan.Quality != 70 {
		t.Errorf("expected full submit to use static base quality 70, got %v", fullTranscodeReq.Plan)
	}
}

// TestPhase8_OptimizationDisabled_BuiltinWithoutMetric_SkipsBenchmark tests that a
// builtin profile without an optimization block and without metric input skips benchmark
// and proceeds with legacy static transcode unchanged.
func TestPhase8_OptimizationDisabled_BuiltinWithoutMetric_SkipsBenchmark(t *testing.T) {
	var fullTranscodeReq transcode.Request
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			fullTranscodeReq = req
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
			_ = os.WriteFile(req.CandidatePath, []byte("transcoded-output-static"), 0644)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:       jobID,
				Status:   transcode.StatusCompleted,
				Progress: 100,
			}, nil
		},
	}

	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, liveValidation8BitProbeJSON, nil)

	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":    mediaFile,
		"profile": "anime-hevc-quality",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("expected StatusCompleted, got %s (error: %s)", res.Status, res.Error)
	}

	if getBool(res.Outputs, "optimization_enabled") {
		t.Errorf("outputs unexpectedly reported optimization_enabled=true when no metric requested")
	}
	if atomic.LoadInt32(&mock.benchmarkSubmitCalls) != 0 {
		t.Errorf("expected 0 benchmark submit calls, got %d", atomic.LoadInt32(&mock.benchmarkSubmitCalls))
	}
	if atomic.LoadInt32(&mock.submitCalls) != 1 {
		t.Errorf("expected 1 full transcode submit call, got %d", atomic.LoadInt32(&mock.submitCalls))
	}
	const expectedStaticBaseDigest = "sha256:5f3790702c4416380cb31fe56b128aa646aa5bd3fd577ca4b3435165e1a34bf6"
	if fullTranscodeReq.Plan == nil || fullTranscodeReq.Plan.PlanDigest != expectedStaticBaseDigest {
		t.Errorf("expected full submit to use static base digest %s, got %v", expectedStaticBaseDigest, fullTranscodeReq.Plan)
	}
}
