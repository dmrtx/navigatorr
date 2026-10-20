package action

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// budgetFlow runs a size budget action through the engine: the real benchmark
// submit and wait steps (the search, then the encoder comparison round), a
// stubbed full encode, and the real validate_result. The engine merges every
// step's outputs into state, as it does in production.
type budgetFlow struct {
	engine  *Engine
	encoded []*transcode.Plan
}

func newBudgetFlow(t *testing.T, vt, x [2]float64) *budgetFlow {
	t.Helper()
	c := newComparisonCase(t, nil)
	f := &budgetFlow{}
	mock := c.executor
	mock.benchmarkStatusFunc = func(_ context.Context, id string) (transcode.BenchmarkStatus, error) {
		if !strings.HasSuffix(id, "-budget") {
			// The search found no passing setting.
			return transcode.BenchmarkStatus{ProtocolVersion: transcode.WorkerProtocolVersion, ID: id, Status: transcode.StatusCompleted, Decision: budgetDecision(nil)}, nil
		}
		st := roundStatus(id, vt, x)
		// The worker ranks the round like any benchmark and may name a
		// winner; the budget's own rule picks the encoder, not that.
		e := st.Decision.Evaluations[0]
		st.Decision.Winner = &transcode.BenchmarkWinner{CandidateID: e.CandidateID, VideoCodec: e.VideoCodec, AverageBitrateKbps: e.AverageBitrateKbps, EstimatedTotalBytes: e.EstimatedBytes, ExpectedBitDepth: 10, VideoProfile: "main10", PixelFormat: "p010le"}
		return st, nil
	}
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Transcode: mock})
	seed := map[string]any{"benchmark_submitted": true}
	for k, v := range c.ec.State {
		seed[k] = v
	}
	e.RegisterTemplate(ActionTemplate{Name: "transcode_media", Steps: []StepDefinition{
		{Name: "preflight", Run: func(context.Context, *ExecutionContext) (StepResult, error) {
			return StepResult{Status: StepCompleted, Outputs: seed}, nil
		}},
		{Name: "submit_benchmark", Run: e.stepBenchmarkSubmit},
		{Name: "wait_benchmark", Run: e.stepBenchmarkWait},
		{Name: "submit_transcode", Run: func(_ context.Context, ec *ExecutionContext) (StepResult, error) {
			f.encoded = append(f.encoded, getPlan(ec.State["plan"]))
			return StepResult{Status: StepCompleted, Outputs: map[string]any{"job_id": "job-" + ec.InstanceID, "transcode_submitted": true}}, nil
		}},
		{Name: "wait_transcode", Run: func(_ context.Context, ec *ExecutionContext) (StepResult, error) {
			plan := getPlan(ec.State["plan"])
			const size = 3_850_000_000
			candidate := t.TempDir() + "/candidate.mkv"
			// A sparse file has the candidate's size without its bytes.
			if err := os.WriteFile(candidate, nil, 0600); err != nil || os.Truncate(candidate, size) != nil {
				t.Fatal(err)
			}
			evidence := map[string]any{"verdict": "pass", "candidate_sha256": "candidate", "plan_digest": plan.PlanDigest, "benchmark_request_digest": plan.QualityValidation.BenchmarkRequestDigest,
				"vmaf": map[string]any{"mean": 91.0, "p5": 86.0, "worst_window_mean": 87.0, "min": 50.0}, "cambi": map[string]any{"mean": 2.0, "max": 9.0}}
			return StepResult{Status: StepCompleted, Outputs: map[string]any{"candidate_path": candidate, "candidate_sha256": "candidate", "candidate_size_bytes": int64(size), "quality_evidence": evidence,
				"original": map[string]any{"size_bytes": budgetSourceBytes, "video_codec": "h264"}}}, nil
		}},
		{Name: "validate_result", Run: e.stepTranscodeValidate},
	}})
	f.engine = e
	return f
}

func (f *budgetFlow) run(t *testing.T) *ActionResult {
	t.Helper()
	res, err := f.engine.Run(context.Background(), "transcode_media", map[string]any{"mode": "size", "size_budget_percent": 40.0})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The budget fallback, its encoder comparison, the encode and validate_result
// run end to end through the engine, and the chosen encode validates.
func TestSizeBudgetComparisonEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name   string
		vt, x  [2]float64
		codec  string
		reason string
	}{
		// VideoToolbox 1.4 VMAF mean and 3.2 p5 under x265: outside the
		// 1-point tolerance, so x265, whatever the worker ranked first.
		{"x265 better", [2]float64{92.18, 77.83}, [2]float64{93.59, 81.05}, transcode.VideoCodecLibX265, SizeBudgetChoiceX265Better},
		{"VideoToolbox within tolerance", [2]float64{93.0, 80.4}, [2]float64{93.59, 81.05}, transcode.VideoCodecHEVCVideoToolbox, SizeBudgetChoiceVTWithinTolerance},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBudgetFlow(t, tc.vt, tc.x)
			res := f.run(t)
			if res.Status != StatusCompleted || getBool(res.State, "skip_transcode") || res.State["reason_code"] != nil {
				t.Fatalf("status %s error %s state %+v", res.Status, res.Error, res.State)
			}
			rec := sizeBudgetOf(res.State)
			if res.State["plan_source"] != sizeBudgetPlanSource || res.State["benchmark_winner"] != nil || rec == nil || rec.Choice == nil || rec.Choice.Reason != tc.reason || rec.Choice.VideoCodec != tc.codec {
				t.Fatalf("plan source %v choice %+v", res.State["plan_source"], rec)
			}
			if len(f.encoded) != 1 || transcode.NormalizeVideoCodec(f.encoded[0].VideoCodec) != tc.codec {
				t.Fatalf("encoded %+v", f.encoded)
			}
			if rec.Outcome != SizeBudgetMeetsTarget || rec.CandidateBytes != 3_850_000_000 {
				t.Fatalf("validated record %+v", rec)
			}
			// The round's results were put back: the search's identity and
			// request are what the plan's final validation is pinned to.
			if res.State[sizeBudgetRoundKey] != nil || res.State["benchmark_job_id"] != "bench-1" || getPlan(res.State["plan"]).QualityValidation.BenchmarkRequestDigest != res.State["benchmark_request_digest"] {
				t.Fatalf("search state not restored: %+v", res.State)
			}
		})
	}
}

// A round whose record is gone is not read as an ordinary benchmark: its
// results would pick the encode by the worker's ranking.
func TestSizeBudgetRoundLostFailsClosed(t *testing.T) {
	c := newComparisonCase(t, nil)
	if r, _ := c.engine.sizeBudgetFallback(context.Background(), c.ec, map[string]any{}, budgetDecision(nil), nil); r.RewindTo != "submit_benchmark" {
		t.Fatal("round not started")
	}
	c.ec.State[sizeBudgetRoundKey] = map[string]any{"benchmark_job_id": sizeBudgetRoundID(c.ec.InstanceID)}
	for name, step := range map[string]func(context.Context, *ExecutionContext) (StepResult, error){"submit": c.engine.stepBenchmarkSubmit, "wait": c.engine.stepBenchmarkWait} {
		if r, err := step(context.Background(), c.ec); err != nil || r.Status != StepFailed || r.Error != sizeBudgetRoundLostError {
			t.Fatalf("%s: %+v %v", name, r, err)
		}
	}
}
