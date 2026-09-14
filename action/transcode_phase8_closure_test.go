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
