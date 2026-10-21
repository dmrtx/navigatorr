package action

import (
	"context"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// ladderCase is a size budget case at the point the benchmark submit step runs
// for it: no search has started, the worker can encode with VideoToolbox.
func newLadderCase(t *testing.T, inputs map[string]any, mutate func(*transcode.WorkerCapabilities)) *budgetCase {
	t.Helper()
	caps := budgetCaps()
	if mutate != nil {
		mutate(&caps)
	}
	in := map[string]any{"size_budget_percent": 40.0}
	for k, v := range inputs {
		in[k] = v
	}
	c := newBudgetCase(t, in, caps)
	delete(c.ec.State, "benchmark_job_id")
	delete(c.ec.State, "benchmark_request")
	delete(c.ec.State, "benchmark_request_digest")
	c.ec.State["optimization_policy"] = c.snapshot.Profile.Optimization
	c.ec.State["resolved_path"] = "/media/Show S01E01.mkv"
	return c
}

func TestSizeBudgetLadderIsFrozenForHardwareBudgets(t *testing.T) {
	c := newLadderCase(t, nil, nil)
	policy := c.snapshot.Size.SizeBudget
	if got := policy.VTLadder; len(got) != 3 || got[0] != 65 || got[1] != 58 || got[2] != 50 || policy.Comparison != nil {
		t.Fatalf("frozen ladder %v comparison %+v", got, policy.Comparison)
	}
	clone := policy.Clone()
	clone.VTLadder[0] = 1
	if policy.VTLadder[0] != 65 {
		t.Fatal("Clone shares the ladder")
	}
	for name, mutate := range map[string]func(*SizeBudgetPolicy){
		"with a comparison": func(p *SizeBudgetPolicy) { r := sizeBudgetComparisonRules; p.Comparison = &r },
		"too long":          func(p *SizeBudgetPolicy) { p.VTLadder = []int{90, 80, 70, 60, 50} },
		"out of range":      func(p *SizeBudgetPolicy) { p.VTLadder = []int{101, 50} },
		"not descending":    func(p *SizeBudgetPolicy) { p.VTLadder = []int{50, 58} },
		"repeated":          func(p *SizeBudgetPolicy) { p.VTLadder = []int{58, 58} },
	} {
		p := policy.Clone()
		mutate(p)
		if p.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The search is skipped: the submit step plans the q65 encode itself and never
// asks the worker for a benchmark, and the wait step has nothing to wait for.
func TestSizeBudgetLadderStartsWithoutSearch(t *testing.T) {
	c := newLadderCase(t, nil, nil)
	res, err := c.engine.stepBenchmarkSubmit(context.Background(), c.ec)
	if err != nil || res.Status != StepCompleted {
		t.Fatalf("submit %+v %v", res, err)
	}
	if c.executor.benchmarkSubmitCalls != 0 {
		t.Fatalf("a benchmark was submitted %d times", c.executor.benchmarkSubmitCalls)
	}
	mergeMap(c.ec.State, res.Outputs)
	rec := sizeBudgetLadderOf(c.ec.State)
	plan := getPlan(c.ec.State["plan"])
	if rec == nil || plan == nil || rec.Rung != 0 || rec.Trigger != sizeBudgetTriggerBudgetFirst || rec.Choice.VideoCodec != transcode.VideoCodecHEVCVideoToolbox {
		t.Fatalf("record %+v plan %+v", rec, plan)
	}
	if plan.VideoCodec != transcode.VideoCodecHEVCVideoToolbox || plan.Quality != 65 || plan.AverageBitrateKbps != 0 || plan.SizePolicy == nil || plan.SizePolicy.MaxCandidateBytes != rec.BudgetBytes || rec.BudgetBytes != 4_000_000_000 {
		t.Fatalf("first rung plan %+v", plan)
	}
	if getString(c.ec.State, "benchmark_job_id") != "" || !getBool(c.ec.State, "benchmark_done") || getBool(c.ec.State, "has_winner") || c.ec.State["plan_source"] != sizeBudgetPlanSource {
		t.Fatalf("search state %+v", c.ec.State)
	}
	if plan.PolicyDigest != c.snapshot.Digest || plan.QualityValidation == nil || plan.QualityValidation.BenchmarkRequestDigest != c.ec.State["benchmark_request_digest"] {
		t.Fatalf("plan is not pinned to the snapshot and request: %+v", plan)
	}
	if again, _ := c.engine.stepBenchmarkSubmit(context.Background(), c.ec); again.Status != StepCompleted || getPlan(c.ec.State["plan"]).PlanDigest != plan.PlanDigest {
		t.Fatalf("a second pass replanned: %+v", again)
	}
	wait, err := c.engine.stepBenchmarkWait(context.Background(), c.ec)
	if err != nil || wait.Status != StepCompleted || c.executor.benchmarkSubmitCalls != 0 {
		t.Fatalf("wait %+v %v", wait, err)
	}
	if !sizeBudgetPlanMatches(c.snapshot, c.ec.InstanceID, c.ec.State, plan, rec) {
		t.Fatal("the planned rung does not rebuild from the frozen policy")
	}
}

// Without the pieces of the ladder the search runs as it always has.
func TestSizeBudgetLadderDoesNotApply(t *testing.T) {
	for name, tc := range map[string]struct {
		inputs map[string]any
		caps   func(*transcode.WorkerCapabilities)
		legacy bool
	}{
		"no VideoToolbox encoder":        {caps: func(c *transcode.WorkerCapabilities) { c.Encoders[transcode.VideoCodecHEVCVideoToolbox] = false }},
		"hardware not allowed":           {inputs: map[string]any{"allow_hardware": false}},
		"snapshot frozen with a compare": {legacy: true},
	} {
		t.Run(name, func(t *testing.T) {
			in := tc.inputs
			if tc.legacy {
				in = map[string]any{"legacy_comparison": true}
			}
			c := newLadderCase(t, in, tc.caps)
			if res, handled := c.engine.startSizeBudgetLadder(context.Background(), c.ec, "/media/Show S01E01.mkv"); handled || res.Status != "" {
				t.Fatalf("handled %+v", res)
			}
			if sizeBudgetLadderOf(c.ec.State) != nil || sizeBudgetOf(c.ec.State) != nil {
				t.Fatal("a ladder record was written")
			}
		})
	}
}

// ladderRung fakes the worker refusing the current rung's file as over the
// budget and returns the engine's reply.
func refuseRung(t *testing.T, c *budgetCase, bytes int64) StepResult {
	t.Helper()
	res, handled := c.engine.advanceSizeBudgetRung(c.ec, bytes)
	if !handled {
		t.Fatal("rung not advanced")
	}
	mergeMap(c.ec.State, res.Outputs)
	return res
}

func TestSizeBudgetLadderAdvancesRungByRung(t *testing.T) {
	c := newLadderCase(t, nil, nil)
	res, err := c.engine.stepBenchmarkSubmit(context.Background(), c.ec)
	if err != nil || res.Status != StepCompleted {
		t.Fatal(res, err)
	}
	mergeMap(c.ec.State, res.Outputs)
	budget := sizeBudgetLadderOf(c.ec.State).BudgetBytes
	enc := planSizeBudgetEncode(c.snapshot.Size.SizeBudget, budgetReport(), budgetSourceBytes, budget)
	type want struct{ quality, bitrate int }
	for i, w := range []want{{58, 0}, {50, 0}, {0, enc.X265.BitrateKbps}} {
		res := refuseRung(t, c, budget+int64(i)+1)
		plan, rec := getPlan(c.ec.State["plan"]), sizeBudgetLadderOf(c.ec.State)
		if res.RewindTo != "submit_transcode" || rec.Rung != i+1 || plan.Quality != w.quality || plan.AverageBitrateKbps != w.bitrate || plan.VideoCodec != transcode.VideoCodecHEVCVideoToolbox || plan.SizePolicy.MaxCandidateBytes != budget {
			t.Fatalf("step %d: rewind %q rung %d plan %+v", i+1, res.RewindTo, rec.Rung, plan)
		}
		if w.bitrate == 0 && plan.AverageBitrateKbps != 0 || w.quality == 0 && w.bitrate == 0 {
			t.Fatalf("step %d: rate control %+v", i+1, plan)
		}
		if !sizeBudgetPlanMatches(c.snapshot, c.ec.InstanceID, c.ec.State, plan, rec) {
			t.Fatalf("step %d: plan does not rebuild from the frozen policy", i+1)
		}
		if plan.PlanDigest == "" || c.ec.State["plan_digest"] != plan.PlanDigest || !strings.Contains(rec.Message, "over the") {
			t.Fatalf("step %d: %+v", i+1, rec)
		}
	}
	// A plan for another rung is not the rung the record names.
	rec := sizeBudgetLadderOf(c.ec.State)
	stale := *getPlan(c.ec.State["plan"])
	rec.Rung = 1
	if sizeBudgetPlanMatches(c.snapshot, c.ec.InstanceID, c.ec.State, &stale, rec) {
		t.Fatal("a plan matched the wrong rung")
	}
	rec.Rung = 3
	// Past the bitrate rung there is nowhere left to go.
	if _, handled := c.engine.advanceSizeBudgetRung(c.ec, budget+1); handled {
		t.Fatal("the ladder advanced past its last rung")
	}
}

func TestSizeBudgetLadderPlanMatchesRejectsTampering(t *testing.T) {
	c := newLadderCase(t, nil, nil)
	if res, err := c.engine.stepBenchmarkSubmit(context.Background(), c.ec); err != nil || res.Status != StepCompleted {
		t.Fatal(res, err)
	}
	rec := sizeBudgetLadderOf(c.ec.State)
	budget := rec.BudgetBytes
	for name, mutate := range map[string]func(*transcode.Plan){
		"another quality":  func(p *transcode.Plan) { p.Quality = 66 },
		"x265":             func(p *transcode.Plan) { p.VideoCodec = transcode.VideoCodecLibX265 },
		"no ceiling":       func(p *transcode.Plan) { p.SizePolicy.MaxCandidateBytes = 0 },
		"a higher ceiling": func(p *transcode.Plan) { p.SizePolicy.MaxCandidateBytes = budget + 1 },
		"a bitrate":        func(p *transcode.Plan) { p.AverageBitrateKbps = 5000 },
	} {
		plan := *getPlan(c.ec.State["plan"])
		sp := *plan.SizePolicy
		plan.SizePolicy = &sp
		mutate(&plan)
		if sizeBudgetPlanMatches(c.snapshot, c.ec.InstanceID, c.ec.State, &plan, rec) {
			t.Errorf("%s accepted", name)
		}
	}
}

// The worker refuses each over-budget rung; the wait step rewinds to encode the
// next, and the last refusal ends the action as exceeded with the original kept.
func TestSizeBudgetLadderWorkerRefusals(t *testing.T) {
	c := newLadderCase(t, nil, nil)
	res, _ := c.engine.stepBenchmarkSubmit(context.Background(), c.ec)
	mergeMap(c.ec.State, res.Outputs)
	budget := sizeBudgetLadderOf(c.ec.State).BudgetBytes
	c.ec.State["job_id"], c.ec.State["transcode_submitted"] = "job-first", true
	var asked []string
	c.executor.statusFunc = func(_ context.Context, id string) (transcode.JobStatus, error) {
		asked = append(asked, id)
		return transcode.JobStatus{ID: id, Status: transcode.StatusFailed, ReasonCode: sizeBudgetExceededReason, FailureClassification: "policy_rejected", CandidateSizeBytes: budget + 123}, nil
	}
	for rung := 1; rung <= 3; rung++ {
		step, err := c.engine.stepTranscodeWait(context.Background(), c.ec)
		if err != nil || step.Status != StepCompleted || step.RewindTo != "submit_transcode" {
			t.Fatalf("refusal %d: %+v %v", rung, step, err)
		}
		mergeMap(c.ec.State, step.Outputs)
		if sizeBudgetLadderOf(c.ec.State).Rung != rung || getBool(c.ec.State, "skip_transcode") || getString(c.ec.State, "job_id") == asked[len(asked)-1] {
			t.Fatalf("after refusal %d: %+v", rung, c.ec.State)
		}
	}
	step, err := c.engine.stepTranscodeWait(context.Background(), c.ec)
	if err != nil || step.Status != StepCompleted || step.RewindTo != "" {
		t.Fatalf("last refusal: %+v %v", step, err)
	}
	rec := sizeBudgetOf(c.ec.State)
	if rec.Outcome != SizeBudgetExceeded || rec.CandidateBytes != budget+123 || !getBool(c.ec.State, "skip_transcode") || c.ec.State["reason_code"] != SizeBudgetExceeded || rec.RetryWithoutHardware {
		t.Fatalf("record %+v state %+v", rec, c.ec.State)
	}
	if !strings.Contains(rec.Message, "All 4 VideoToolbox steps") || !strings.HasSuffix(rec.Message, "Original preserved.") {
		t.Fatalf("message %q", rec.Message)
	}
	if len(asked) != 4 || c.executor.benchmarkSubmitCalls != 0 {
		t.Fatalf("worker asked %v, %d benchmarks", asked, c.executor.benchmarkSubmitCalls)
	}
}

// A rung that fits but misses the floor offers no candidate and points at x265.
func TestSizeBudgetLadderBelowFloorOffersX265(t *testing.T) {
	c := newLadderCase(t, nil, nil)
	res, _ := c.engine.stepBenchmarkSubmit(context.Background(), c.ec)
	mergeMap(c.ec.State, res.Outputs)
	refuseRung(t, c, 4_100_000_000) // rung 0 over the budget; rung 1 is the first to fit
	c.ec.State["job_id"] = "job-second"
	ev := &transcode.FinalQualityEvidence{Verdict: "fail", ReasonCodes: []string{"quality_vmaf_p5_below_minimum"}, CandidateSizeBytes: 3_900_000_000}
	c.executor.statusFunc = func(_ context.Context, id string) (transcode.JobStatus, error) {
		return transcode.JobStatus{ID: id, Status: transcode.StatusFailed, ReasonCode: reasonQualityNotMet, FailureClassification: "policy_rejected", CandidateSizeBytes: ev.CandidateSizeBytes, QualityEvidence: ev}, nil
	}
	step, err := c.engine.stepTranscodeWait(context.Background(), c.ec)
	if err != nil || step.Status != StepCompleted || step.RewindTo != "" {
		t.Fatalf("%+v %v", step, err)
	}
	rec := sizeBudgetOf(c.ec.State)
	if rec.Outcome != SizeBudgetBelowFloor || !rec.RetryWithoutHardware || !getBool(c.ec.State, "skip_transcode") || c.ec.State["reason_code"] != reasonQualityNotMet {
		t.Fatalf("record %+v state %+v", rec, c.ec.State)
	}
	if !strings.Contains(rec.Message, "quality 58") || !strings.HasSuffix(rec.Message, "Try again with x265 (allow_hardware off): slower, but it usually keeps more quality at the same size.") {
		t.Fatalf("message %q", rec.Message)
	}
}

// A rung that fits and measures well finishes through the existing validation.
func TestSizeBudgetLadderFittingRungIsValidated(t *testing.T) {
	for _, tc := range []struct {
		name    string
		vmaf    float64
		outcome string
	}{
		{"meets the target", 95.0, SizeBudgetMeetsTarget},
		{"below the target, above the floor", 80.0, SizeBudgetBelowTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newLadderCase(t, nil, nil)
			res, _ := c.engine.stepBenchmarkSubmit(context.Background(), c.ec)
			mergeMap(c.ec.State, res.Outputs)
			rec := sizeBudgetLadderOf(c.ec.State)
			c.ec.State["quality_evidence"] = map[string]any{"verdict": "pass", "candidate_size_bytes": 3_800_000_000,
				"vmaf":  map[string]any{"mean": tc.vmaf, "p5": tc.vmaf - 5, "worst_window_mean": tc.vmaf - 4, "min": 50.0},
				"cambi": map[string]any{"mean": 2.0, "max": 9.0}}
			got, fits := finishSizeBudget(c.ec, 3_800_000_000)
			if !fits || got.Outcome != tc.outcome || got.Rung != rec.Rung || got.RetryWithoutHardware {
				t.Fatalf("outcome %q fits %v record %+v", got.Outcome, fits, got)
			}
		})
	}
}
