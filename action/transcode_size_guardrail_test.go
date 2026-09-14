package action

// Focused size-guardrail tests for transcode_media Phase 8 benchmark
// optimization.
//
// A direct transcode_media call may omit min_savings_percent and
// max_size_increase_percent. The benchmark winner's predicted savings must
// then be evaluated against effective production defaults (15% minimum
// savings, 0% allowed growth) BEFORE any full-file encode is submitted.
// Standalone benchmark_transcode stays report-only and must not block.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

func benchmarkWinnerWithSavings(savings float64) func(context.Context, string) (transcode.BenchmarkStatus, error) {
	return func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
		return transcode.BenchmarkStatus{
			ProtocolVersion: transcode.WorkerProtocolVersion,
			ID:              jobID,
			Status:          transcode.StatusCompleted,
			Progress:        100,
			Decision: &transcode.BenchmarkDecision{
				Winner: &transcode.BenchmarkWinner{
					CandidateID:         "cand_q70",
					CandidateIndex:      2,
					Quality:             70,
					VideoProfile:        "main",
					PixelFormat:         "yuv420p",
					ExpectedBitDepth:    8,
					MetricType:          "vmaf",
					Score:               96.8,
					TargetReached:       true,
					MinimumMet:          true,
					EstimatedVideoBytes: 200000000,
					EstimatedTotalBytes: 250000000,
					EstimatedTotalMB:    250.0,
					SavingsPercent:      savings,
				},
				DecisionReason: "test winner selected",
			},
		}, nil
	}
}

func completingTranscodeMockWithSavings(savings float64) *mockTranscodeExecutor {
	return &mockTranscodeExecutor{
		benchmarkStatusFunc: benchmarkWinnerWithSavings(savings),
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
			_ = os.WriteFile(req.CandidatePath, []byte("transcoded-output"), 0644)
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
}

// A. Direct transcode_media with omitted guardrails whose benchmark winner
// predicts growth must use effective defaults (15/0) and must NOT submit
// the full transcode.
func TestTranscodeMedia_BenchmarkGuard_BlocksGrowthWithOmittedGuardrails(t *testing.T) {
	mock := completingTranscodeMockWithSavings(-276.92)
	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("unexpected engine error: %v", err)
	}

	if atomic.LoadInt32(&mock.submitCalls) != 0 {
		t.Errorf("predicted growth must prevent full transcode submit, got %d submit calls", atomic.LoadInt32(&mock.submitCalls))
	}
	if res.Status != StatusFailed && res.Status != StatusWaitingDecision {
		t.Fatalf("expected fail-closed (failed) or waiting_decision, got %s (error: %s)", res.Status, res.Error)
	}
	if got := getFloat(res.Outputs, "effective_min_savings_percent"); got != 15 {
		t.Errorf("effective_min_savings_percent = %v, want 15", got)
	}
	if got := getFloat(res.Outputs, "effective_max_size_increase_percent"); got != 0 {
		t.Errorf("effective_max_size_increase_percent = %v, want 0", got)
	}
	if !strings.Contains(strings.ToLower(res.Error), "saving") && !strings.Contains(strings.ToLower(res.Error), "growth") {
		t.Errorf("expected error to explain predicted savings/growth, got %q", res.Error)
	}
}

// B. Explicit caller-provided guardrails that allow the benchmark result
// must be respected and the full transcode submit allowed.
func TestTranscodeMedia_BenchmarkGuard_ExplicitOverrideAllowsSubmit(t *testing.T) {
	mock := completingTranscodeMockWithSavings(-20.0)
	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":                      mediaFile,
		"profile":                   "opt-vt",
		"min_savings_percent":       0.0,
		"max_size_increase_percent": 25.0,
	})
	if err != nil {
		t.Fatalf("unexpected engine error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("expected StatusCompleted with explicit override, got %s (error: %s)", res.Status, res.Error)
	}
	if atomic.LoadInt32(&mock.submitCalls) != 1 {
		t.Errorf("expected exactly 1 full transcode submit call, got %d", atomic.LoadInt32(&mock.submitCalls))
	}
}

// C. Direct transcode_media with omitted guardrails and comfortable
// predicted savings must proceed to the full transcode submit.
func TestTranscodeMedia_BenchmarkGuard_NormalSavingsProceeds(t *testing.T) {
	mock := completingTranscodeMockWithSavings(40.0)
	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("unexpected engine error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("expected StatusCompleted, got %s (error: %s)", res.Status, res.Error)
	}
	if atomic.LoadInt32(&mock.submitCalls) != 1 {
		t.Errorf("expected exactly 1 full transcode submit call, got %d", atomic.LoadInt32(&mock.submitCalls))
	}
}

// D. Standalone benchmark_transcode reporting a winner with negative
// savings must complete normally and report the result.
func TestBenchmarkTranscode_NegativeSavingsStillReportsWinner(t *testing.T) {
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: benchmarkWinnerWithSavings(-30.0),
	}
	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("unexpected engine error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("expected StatusCompleted for report-only benchmark, got %s (error: %s)", res.Status, res.Error)
	}
	if !getBool(res.Outputs, "has_winner") {
		t.Errorf("expected has_winner=true in outputs")
	}
	if getString(res.Outputs, "winner_candidate_id") != "cand_q70" {
		t.Errorf("expected winner_candidate_id=cand_q70, got %s", getString(res.Outputs, "winner_candidate_id"))
	}
	if got := getFloat(res.Outputs, "estimated_savings_percent"); got != -30 {
		t.Errorf("estimated_savings_percent = %v, want -30", got)
	}
	if atomic.LoadInt32(&mock.submitCalls) != 0 {
		t.Errorf("benchmark_transcode must never submit full transcode, got %d submit calls", atomic.LoadInt32(&mock.submitCalls))
	}
}

// Second line of defense. The benchmark predicts +40% savings so the
// pre-transcode guard passes, but the real encode ends up larger than the
// original. With max_size_increase_percent omitted, validate_result must
// still stop in waiting_decision using the effective default (0%).
func TestTranscodeMedia_PostTranscodeGuardrailAppliesDefaultMaxIncrease(t *testing.T) {
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: benchmarkWinnerWithSavings(40.0),
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
			// Source fixture is tens of bytes; write a much larger candidate
			// so the real result exceeds the original.
			if err := os.WriteFile(req.CandidatePath, make([]byte, 64*1024), 0644); err != nil {
				t.Errorf("writing oversized candidate: %v", err)
			}
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
	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("unexpected engine error: %v", err)
	}
	if atomic.LoadInt32(&mock.submitCalls) != 1 {
		t.Fatalf("benchmark predicted +40%% savings, expected 1 full transcode submit call, got %d", atomic.LoadInt32(&mock.submitCalls))
	}
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected StatusWaitingDecision for oversized candidate under default 0%% guardrail, got %s (error: %s)", res.Status, res.Error)
	}
	if !strings.Contains(res.WaitingReason, "max_size_increase_percent") {
		t.Errorf("expected waiting reason to mention max_size_increase_percent, got %q", res.WaitingReason)
	}
}

// E (unit). Effective guardrail resolution: omitted inputs fall back to the
// established production behavior (15% minimum savings, 0% allowed growth);
// explicit caller values override; configured policy wins over the fallback.
func TestResolveTranscodeSizeGuardrails_DefaultsAndOverrides(t *testing.T) {
	tests := []struct {
		name      string
		inputs    map[string]any
		configMin float64
		wantMin   float64
		wantMax   float64
	}{
		{"omitted guardrails default to 15/0", map[string]any{}, 0, 15, 0},
		{"nil inputs default to 15/0", nil, 0, 15, 0},
		{"configured minimum wins over fallback", map[string]any{}, 20, 20, 0},
		{"explicit minimum overrides config", map[string]any{"min_savings_percent": 5.0}, 20, 5, 0},
		{"explicit zeros mean disabled minimum and zero allowed growth", map[string]any{"min_savings_percent": 0.0, "max_size_increase_percent": 0.0}, 15, 0, 0},
		{"explicit growth allowance respected", map[string]any{"max_size_increase_percent": 10.0}, 0, 15, 10},
		{"explicit negative minimum respected", map[string]any{"min_savings_percent": -30.0}, 0, -30, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotMin, gotMax := resolveTranscodeSizeGuardrails(tc.inputs, tc.configMin)
			if gotMin != tc.wantMin || gotMax != tc.wantMax {
				t.Errorf("resolveTranscodeSizeGuardrails = (%v, %v), want (%v, %v)", gotMin, gotMax, tc.wantMin, tc.wantMax)
			}
		})
	}
}

// E (unit). Boundary semantics for the production defaults: +40 and +15
// proceed; +5, 0, -1, and -20 do not encode.
func TestCheckBenchmarkSavingsGuardrail_ProductionBoundaries(t *testing.T) {
	tests := []struct {
		savings float64
		wantOK  bool
	}{
		{40, true},
		{15, true},
		{5, false},
		{0, false},
		{-1, false},
		{-20, false},
		{-276.92, false},
	}
	for _, tc := range tests {
		if err := checkBenchmarkSavingsGuardrail(tc.savings, 15, 0); (err == nil) != tc.wantOK {
			t.Errorf("checkBenchmarkSavingsGuardrail(%v, 15, 0) err = %v, wantOK = %v", tc.savings, err, tc.wantOK)
		}
	}

	// min=0 disables the minimum-savings requirement (selector semantics),
	// so max_size_increase_percent alone decides: 20% growth within 25% passes.
	if err := checkBenchmarkSavingsGuardrail(-20, 0, 25); err != nil {
		t.Errorf("min=0 with 25%% allowed growth should permit predicted -20%% savings, got %v", err)
	}
	// min=0 with max=0 allows exactly zero growth: any growth blocks.
	if err := checkBenchmarkSavingsGuardrail(-1, 0, 0); err == nil {
		t.Errorf("min=0 with 0%% allowed growth must block predicted -1%% savings")
	}
	// Explicit growth allowance that is exceeded must still block.
	if err := checkBenchmarkSavingsGuardrail(-20, 0, 10); err == nil {
		t.Errorf("predicted 20%% growth exceeding allowed 10%% must block")
	}
}
