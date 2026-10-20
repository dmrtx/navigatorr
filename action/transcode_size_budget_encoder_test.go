package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

// newComparisonCase is a budget case whose search request is complete enough
// for the encoder comparison round to be built from it.
func newComparisonCase(t *testing.T, inputs map[string]any) *budgetCase {
	t.Helper()
	in := map[string]any{"size_budget_percent": 40.0}
	for k, v := range inputs {
		in[k] = v
	}
	c := newBudgetCase(t, in, budgetCaps())
	r := c.request
	r.ProtocolVersion, r.ModePolicyVersion, r.SearchBudgetSeconds, r.AudioMode, r.SourcePath = transcode.WorkerProtocolVersion, optimization.ModePolicyV3, 7200, transcode.AudioModeCompactLossless, "/media/source.mkv"
	digest, err := transcode.DigestBenchmarkRequest(&r)
	if err != nil {
		t.Fatal(err)
	}
	c.request = r
	c.ec.State["benchmark_request"], c.ec.State["benchmark_request_digest"] = r, digest
	c.ec.State["benchmark_quality"] = map[string]any{"search": true}
	return c
}

// roundStatus is the comparison round's report with the two encodes'
// sampled numbers.
func roundStatus(id string, vt, x [2]float64) transcode.BenchmarkStatus {
	return transcode.BenchmarkStatus{ProtocolVersion: transcode.WorkerProtocolVersion, ID: id, Status: transcode.StatusCompleted, Decision: &transcode.BenchmarkDecision{Evaluations: []transcode.BenchmarkCandidateEvaluation{
		{CandidateID: sizeBudgetVTCandidateID, VideoCodec: transcode.VideoCodecHEVCVideoToolbox, AverageBitrateKbps: 4773, EstimatedBytes: 3_910_000_000, VMAF: &transcode.BenchmarkVMAFSummary{Mean: vt[0], P5: vt[1], WorstWindowMean: vt[1] + 1}},
		{CandidateID: sizeBudgetX265CandidateID, VideoCodec: transcode.VideoCodecLibX265, Preset: "medium", EstimatedBytes: 3_870_000_000, VMAF: &transcode.BenchmarkVMAFSummary{Mean: x[0], P5: x[1], WorstWindowMean: x[1] + 1}},
	}}}
}

func TestSizeBudgetComparisonFreeze(t *testing.T) {
	freeze := func(inputs map[string]any) *ModePolicySnapshot {
		t.Helper()
		in := map[string]any{"mode": "size"}
		for k, v := range inputs {
			in[k] = v
		}
		snapshot, err := freezeModePolicy(&ExecutionContext{InstanceID: "freeze", Inputs: in, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}})
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	if got := freeze(map[string]any{"size_budget_percent": 40.0}).Size.SizeBudget.Comparison; got == nil || *got != sizeBudgetComparisonRules {
		t.Fatalf("comparison with the media engine allowed: %+v", got)
	}
	if got := freeze(map[string]any{"size_budget_percent": 40.0, "allow_hardware": false}).Size.SizeBudget.Comparison; got != nil {
		t.Fatalf("comparison frozen with allow_hardware=false: %+v", got)
	}
	if freeze(nil).Size.SizeBudget != nil {
		t.Fatal("a size goal without a budget gained one")
	}
	for _, bad := range []SizeBudgetComparison{
		{VMAFMeanTolerance: 7, VMAFP5Tolerance: 1, SearchSeconds: 1200},
		{VMAFMeanTolerance: 1, VMAFP5Tolerance: -1, SearchSeconds: 1200},
		{VMAFMeanTolerance: 1, VMAFP5Tolerance: 1, SearchSeconds: 10},
		{VMAFMeanTolerance: 1, VMAFP5Tolerance: 1, SearchSeconds: 1200, MaxBitrateCorrections: 2},
	} {
		if bad.validate() == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	// The preview compares budgets the user chose, not the comparison rules.
	withRules := map[string]any{"mode_policy": map[string]any{"size": map[string]any{"size_budget": map[string]any{"percent": 40.0, "encoder_comparison": map[string]any{"vmaf_mean_tolerance": 1.0}}}}}
	without := map[string]any{"mode_policy": map[string]any{"size": map[string]any{"size_budget": map[string]any{"percent": 40.0}}}}
	if a, b := BatchItemFrozenSizeBudget(withRules, "x"), BatchItemFrozenSizeBudget(without, "x"); fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("frozen budgets differ only by the comparison: %v %v", a, b)
	}
	if sizeBudgetComparisonRules.validate() != nil {
		t.Fatal("default rules rejected")
	}
}

func TestSizeBudgetPick(t *testing.T) {
	point := func(mean, p5 float64) *SizeBudgetPoint { return &SizeBudgetPoint{VMAFMean: &mean, VMAFP5: &p5} }
	rules := &sizeBudgetComparisonRules
	for _, tc := range []struct {
		name         string
		vt, x        *SizeBudgetPoint
		codec, cause string
	}{
		{"equal", point(80, 70), point(80, 70), transcode.VideoCodecHEVCVideoToolbox, SizeBudgetChoiceVTWithinTolerance},
		{"just within", point(79, 69), point(80, 70), transcode.VideoCodecHEVCVideoToolbox, SizeBudgetChoiceVTWithinTolerance},
		{"better", point(82, 72), point(80, 70), transcode.VideoCodecHEVCVideoToolbox, SizeBudgetChoiceVTWithinTolerance},
		{"mean behind", point(78.9, 70), point(80, 70), transcode.VideoCodecLibX265, SizeBudgetChoiceX265Better},
		{"p5 behind", point(80, 68.5), point(80, 70), transcode.VideoCodecLibX265, SizeBudgetChoiceX265Better},
		{"vt missing", nil, point(80, 70), transcode.VideoCodecLibX265, SizeBudgetChoiceIncomplete},
		{"x265 missing", point(80, 70), &SizeBudgetPoint{}, transcode.VideoCodecLibX265, SizeBudgetChoiceIncomplete},
	} {
		if codec, cause := sizeBudgetPick(rules, tc.vt, tc.x); codec != tc.codec || cause != tc.cause {
			t.Errorf("%s: %s %s", tc.name, codec, cause)
		}
	}
	if codec, _ := sizeBudgetPick(nil, point(80, 70), point(80, 70)); codec != transcode.VideoCodecLibX265 {
		t.Fatal("picked VideoToolbox without frozen rules")
	}
}

func TestSizeBudgetComparisonRound(t *testing.T) {
	start := func(t *testing.T, inputs map[string]any) *budgetCase {
		t.Helper()
		c := newComparisonCase(t, inputs)
		result, handled := c.engine.sizeBudgetFallback(context.Background(), c.ec, map[string]any{}, budgetDecision(nil), nil)
		if !handled || result.Status != StepCompleted || result.RewindTo != "submit_benchmark" {
			t.Fatalf("round not started: %+v", result)
		}
		return c
	}
	t.Run("request", func(t *testing.T) {
		c := start(t, nil)
		var req transcode.BenchmarkRequest
		if !reencode(c.ec.State["benchmark_request"], &req) || req.ID != "bench-size-budget-budget" || c.ec.State["benchmark_job_id"] != req.ID || req.Automatic || req.SearchBudgetSeconds != 1200 || len(req.Candidates) != 2 {
			t.Fatalf("round request %+v", req)
		}
		if err := transcode.ValidateBenchmarkRequest(&req); err != nil {
			t.Fatal(err)
		}
		vt, x := req.Candidates[0], req.Candidates[1]
		if vt.ID != sizeBudgetVTCandidateID || vt.VideoCodec != transcode.VideoCodecHEVCVideoToolbox || vt.AverageBitrateKbps != 4773 || vt.Quality != 0 || vt.VideoProfile != "main10" || vt.PixelFormat != "p010le" || *vt.GOPSize != 240 || *vt.BFrames != 1 {
			t.Fatalf("VideoToolbox candidate %+v", vt)
		}
		want := sizeBudgetX265
		want.BitrateKbps = 4773
		if x.ID != sizeBudgetX265CandidateID || x.VideoCodec != transcode.VideoCodecLibX265 || x.X265 == nil || *x.X265 != want || x.Quality != 0 || x.Preset != "medium" || x.VideoProfile != "main10" {
			t.Fatalf("x265 candidate %+v %+v", x, x.X265)
		}
		if len(req.Samples) != len(c.request.Samples) || req.Samples[1] != c.request.Samples[1] || req.Quality.VMAF.Minimum != 70 {
			t.Fatalf("windows %+v quality %+v", req.Samples, req.Quality.VMAF)
		}
		if c.ec.State[sizeBudgetKey] != nil || getString(c.ec.State, "plan_source") == sizeBudgetPlanSource || sizeBudgetRoundOf(c.ec.State) == nil || getBool(c.ec.State, "benchmark_submitted") {
			t.Fatalf("round state %+v", c.ec.State)
		}
	})
	finish := func(t *testing.T, c *budgetCase, st transcode.BenchmarkStatus, statusErr error) StepResult {
		t.Helper()
		c.executor.benchmarkStatusFunc = func(_ context.Context, id string) (transcode.BenchmarkStatus, error) {
			st.ID = id
			return st, statusErr
		}
		c.ec.State["benchmark_submitted"] = true
		result, err := c.engine.stepBenchmarkWait(context.Background(), c.ec)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	restored := func(t *testing.T, c *budgetCase) {
		t.Helper()
		var req transcode.BenchmarkRequest
		if !reencode(c.ec.State["benchmark_request"], &req) || req.ID != "bench-1" || c.ec.State["benchmark_job_id"] != "bench-1" || sizeBudgetRoundActive(c.ec.State) || c.ec.State["benchmark_quality"] == nil {
			t.Fatalf("search state not restored: %+v", c.ec.State)
		}
		if _, err := c.engine.sizeBudgetPlan(c.ec, c.snapshot, sizeBudgetVideoOf(getPlan(c.ec.State["plan"]))); err != nil {
			t.Fatalf("the search's request no longer matches its digest: %v", err)
		}
	}
	t.Run("VideoToolbox within tolerance", func(t *testing.T) {
		c := newComparisonCase(t, nil)
		c.ec.State["benchmark_search_seconds"] = 900.0
		if r, _ := c.engine.sizeBudgetFallback(context.Background(), c.ec, map[string]any{}, budgetDecision(nil), nil); r.RewindTo != "submit_benchmark" {
			t.Fatal("round not started")
		}
		st := roundStatus("", [2]float64{83.6, 74.4}, [2]float64{84.1, 75.0})
		seconds := 240.0
		st.SearchSeconds = &seconds
		result := finish(t, c, st, nil)
		restored(t, c)
		if got := getFloat(c.ec.State, "benchmark_search_seconds"); got != 1140 {
			t.Fatalf("the round's time is not charged to the search: %v", got)
		}
		plan, rec := getPlan(c.ec.State["plan"]), sizeBudgetOf(c.ec.State)
		if result.Status != StepCompleted || plan.VideoCodec != transcode.VideoCodecHEVCVideoToolbox || plan.AverageBitrateKbps != 4773 || plan.X265 != nil || plan.Preset != "" || plan.Quality != 0 || plan.VideoProfile != "main10" || *plan.GOPSize != 240 {
			t.Fatalf("plan %+v", plan)
		}
		if rec == nil || rec.Choice == nil || rec.Choice.VideoCodec != transcode.VideoCodecHEVCVideoToolbox || rec.Choice.Reason != SizeBudgetChoiceVTWithinTolerance || rec.Choice.BenchmarkJobID != "bench-size-budget-budget" || !strings.Contains(rec.Message, "VideoToolbox at 4773 kbps") {
			t.Fatalf("record %+v choice %+v", rec, rec.Choice)
		}
		vt, x := sizeBudgetMeasured(rec)
		last := rec.Curve[len(rec.Curve)-1]
		if vt == nil || x == nil || !vt.Chosen || x.Chosen || *vt.VMAFMean != 83.6 || x.BitrateKbps != 4773 || last.Kind != "budget_fallback" || last.VideoCodec != transcode.VideoCodecHEVCVideoToolbox || last.BitrateKbps != 4773 {
			t.Fatalf("curve %+v", rec.Curve)
		}
		if !sizeBudgetPlanMatches(c.snapshot, c.ec.InstanceID, c.ec.State, plan, rec) {
			t.Fatal("validation does not rebuild the VideoToolbox plan")
		}
	})
	t.Run("x265 better", func(t *testing.T) {
		c := start(t, nil)
		finish(t, c, roundStatus("", [2]float64{81.0, 70.2}, [2]float64{84.1, 75.0}), nil)
		restored(t, c)
		plan, rec := getPlan(c.ec.State["plan"]), sizeBudgetOf(c.ec.State)
		if plan.VideoCodec != transcode.VideoCodecLibX265 || plan.X265 == nil || rec.Choice.Reason != SizeBudgetChoiceX265Better {
			t.Fatalf("plan %+v choice %+v", plan, rec.Choice)
		}
		if vt, x := sizeBudgetMeasured(rec); vt.Chosen || !x.Chosen {
			t.Fatalf("chosen flags %+v %+v", vt, x)
		}
		if !sizeBudgetPlanMatches(c.snapshot, c.ec.InstanceID, c.ec.State, plan, rec) {
			t.Fatal("validation does not rebuild the x265 plan")
		}
	})
	t.Run("round fails open", func(t *testing.T) {
		for name, st := range map[string]transcode.BenchmarkStatus{
			"failed":    {ProtocolVersion: transcode.WorkerProtocolVersion, Status: transcode.StatusFailed, Error: "encoder crashed"},
			"exhausted": {ProtocolVersion: transcode.WorkerProtocolVersion, Status: transcode.StatusCompleted, ReasonCode: "search_budget_exhausted"},
			"cancelled": {ProtocolVersion: transcode.WorkerProtocolVersion, Status: transcode.StatusCancelled},
		} {
			c := start(t, nil)
			result := finish(t, c, st, nil)
			restored(t, c)
			plan, rec := getPlan(c.ec.State["plan"]), sizeBudgetOf(c.ec.State)
			if result.Status != StepCompleted || plan.VideoCodec != transcode.VideoCodecLibX265 || rec.Choice.Reason != SizeBudgetChoiceRoundFailed || getBool(c.ec.State, "skip_transcode") {
				t.Fatalf("%s: %+v choice %+v", name, result, rec.Choice)
			}
		}
	})
	t.Run("refused submit fails open", func(t *testing.T) {
		c := start(t, nil)
		c.executor.benchmarkSubmitFunc = func(context.Context, transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
			return transcode.BenchmarkJob{}, errors.New("benchmark request rejected: unknown candidate")
		}
		result, err := c.engine.stepBenchmarkSubmit(context.Background(), c.ec)
		if err != nil || result.Status != StepCompleted || sizeBudgetRoundOf(c.ec.State).Failure == "" {
			t.Fatalf("submit %+v %v", result, err)
		}
		result, err = c.engine.stepBenchmarkWait(context.Background(), c.ec)
		if err != nil || result.Status != StepCompleted {
			t.Fatalf("wait %+v %v", result, err)
		}
		restored(t, c)
		if rec := sizeBudgetOf(c.ec.State); getPlan(c.ec.State["plan"]).VideoCodec != transcode.VideoCodecLibX265 || rec.Choice.Reason != SizeBudgetChoiceRoundFailed {
			t.Fatalf("choice %+v", rec.Choice)
		}
	})
	t.Run("status errors fail open", func(t *testing.T) {
		// Non-retryable reads; a retryable one waits and reconciles.
		for _, code := range []int{404, 403} {
			c := start(t, nil)
			result := finish(t, c, transcode.BenchmarkStatus{}, fmt.Errorf("worker read: %w", &httpclient.HTTPError{StatusCode: code}))
			restored(t, c)
			if rec := sizeBudgetOf(c.ec.State); result.Status != StepCompleted || getPlan(c.ec.State["plan"]).VideoCodec != transcode.VideoCodecLibX265 || rec.Choice.Reason != SizeBudgetChoiceRoundFailed {
				t.Fatalf("%d: %+v", code, result)
			}
		}
	})
	t.Run("a manual retry of a cancelled round continues with x265", func(t *testing.T) {
		c := start(t, nil)
		c.ec.State["benchmark_submitted"] = true
		c.executor.benchmarkStatusFunc = func(_ context.Context, id string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{ProtocolVersion: transcode.WorkerProtocolVersion, ID: id, Status: transcode.StatusCancelled}, nil
		}
		tmpl := ActionTemplate{Steps: []StepDefinition{{Name: "preflight"}, {Name: "submit_benchmark"}, {Name: "wait_benchmark"}}}
		resume, err := c.engine.prepareRemoteRetry(context.Background(), &store.ActionInstance{ActionName: "transcode_media"}, c.ec, tmpl, 2)
		if err != nil || resume != 1 || sizeBudgetRoundOf(c.ec.State).Failure == "" {
			t.Fatalf("retry %d %v", resume, err)
		}
		if result, err := c.engine.stepBenchmarkSubmit(context.Background(), c.ec); err != nil || result.Status != StepCompleted || c.executor.benchmarkSubmitCalls != 0 {
			t.Fatalf("submit %+v %v", result, err)
		}
		if result, err := c.engine.stepBenchmarkWait(context.Background(), c.ec); err != nil || result.Status != StepCompleted {
			t.Fatalf("wait %+v %v", result, err)
		}
		restored(t, c)
		if getPlan(c.ec.State["plan"]).VideoCodec != transcode.VideoCodecLibX265 {
			t.Fatal("not x265")
		}
	})
	t.Run("no round after VideoToolbox failed the full file", func(t *testing.T) {
		c := newComparisonCase(t, nil)
		c.ec.State[hardwareContinuationKey] = &HardwareContinuation{Outcome: continuationContinued, Message: "continued with x265"}
		if !continuationActive(c.ec.State) {
			t.Fatal("continuation not active")
		}
		result, _ := c.engine.sizeBudgetFallback(context.Background(), c.ec, map[string]any{}, budgetDecision(nil), nil)
		if rec := sizeBudgetOf(c.ec.State); result.RewindTo != "" || rec.Choice.Reason != SizeBudgetChoiceAfterContinuation || getPlan(c.ec.State["plan"]).VideoCodec != transcode.VideoCodecLibX265 {
			t.Fatalf("result %+v choice %+v", result, rec.Choice)
		}
	})
	t.Run("no round without the media engine", func(t *testing.T) {
		c := newComparisonCase(t, map[string]any{"allow_hardware": false})
		result, _ := c.engine.sizeBudgetFallback(context.Background(), c.ec, map[string]any{}, budgetDecision(nil), nil)
		rec := sizeBudgetOf(c.ec.State)
		if result.RewindTo != "" || rec.Choice.Reason != SizeBudgetChoiceHardwareOff || getPlan(c.ec.State["plan"]).VideoCodec != transcode.VideoCodecLibX265 {
			t.Fatalf("result %+v choice %+v", result, rec.Choice)
		}
	})
	t.Run("no round on a worker without VideoToolbox", func(t *testing.T) {
		c := newComparisonCase(t, nil)
		caps := budgetCaps()
		caps.Encoders = map[string]bool{transcode.VideoCodecLibX265: true}
		c.executor.capabilitiesFunc = func(context.Context) (transcode.WorkerCapabilities, error) { return caps, nil }
		result, _ := c.engine.sizeBudgetFallback(context.Background(), c.ec, map[string]any{}, budgetDecision(nil), nil)
		if rec := sizeBudgetOf(c.ec.State); result.RewindTo != "" || rec.Choice.Reason != SizeBudgetChoiceWorkerUnsupported {
			t.Fatalf("result %+v choice %+v", result, rec.Choice)
		}
	})
}

// vtBudgetCase is a budget case whose comparison picked VideoToolbox.
func vtBudgetCase(t *testing.T) *budgetCase {
	t.Helper()
	c := newComparisonCase(t, nil)
	if _, handled := c.engine.sizeBudgetFallback(context.Background(), c.ec, map[string]any{}, budgetDecision(nil), nil); !handled {
		t.Fatal("fallback not taken")
	}
	st := roundStatus("bench-size-budget-budget", [2]float64{84.0, 75.0}, [2]float64{84.1, 75.0})
	if _, err := c.engine.finishSizeBudgetComparison(c.ec, sizeBudgetRoundOf(c.ec.State), &st); err != nil {
		t.Fatal(err)
	}
	if getPlan(c.ec.State["plan"]).VideoCodec != transcode.VideoCodecHEVCVideoToolbox {
		t.Fatal("VideoToolbox not picked")
	}
	c.ec.State["job_id"] = "job-size-budget"
	return c
}

func TestSizeBudgetVideoToolboxValidation(t *testing.T) {
	c := vtBudgetCase(t)
	plan, rec := getPlan(c.ec.State["plan"]), sizeBudgetOf(c.ec.State)
	if !sizeBudgetPlanMatches(c.snapshot, c.ec.InstanceID, c.ec.State, plan, rec) {
		t.Fatal("VideoToolbox plan rejected")
	}
	tamper := map[string]func(p *transcode.Plan, r *SizeBudgetRecord){
		"bitrate":     func(p *transcode.Plan, _ *SizeBudgetRecord) { p.AverageBitrateKbps++ },
		"gop":         func(p *transcode.Plan, _ *SizeBudgetRecord) { g := 120; p.GOPSize = &g },
		"constant":    func(p *transcode.Plan, _ *SizeBudgetRecord) { b := true; p.ConstantBitrate = &b },
		"x265 choice": func(_ *transcode.Plan, r *SizeBudgetRecord) { r.Choice.VideoCodec = transcode.VideoCodecLibX265 },
		"numbers": func(_ *transcode.Plan, r *SizeBudgetRecord) {
			vt, _ := sizeBudgetMeasured(r)
			m := 80.0
			vt.VMAFMean = &m
		},
		"no measurements": func(_ *transcode.Plan, r *SizeBudgetRecord) { r.Curve = nil },
		"other round":     func(_ *transcode.Plan, r *SizeBudgetRecord) { r.Choice.BenchmarkJobID = "bench-other-budget" },
		"other bitrate":   func(_ *transcode.Plan, r *SizeBudgetRecord) { vt, _ := sizeBudgetMeasured(r); vt.BitrateKbps = 6000 },
		"8-bit output": func(p *transcode.Plan, _ *SizeBudgetRecord) {
			p.ExpectedBitDepth, p.VideoProfile, p.PixelFormat = 8, "main", "yuv420p"
		},
		"correction": func(_ *transcode.Plan, r *SizeBudgetRecord) {
			r.Correction = &SizeBudgetCorrection{FirstCandidateBytes: 4_100_000_000, FirstBitrateKbps: 4773, BitrateKbps: 4773}
		},
	}
	for name, mutate := range tamper {
		p, r := *plan, sizeBudgetOf(c.ec.State)
		mutate(&p, r)
		if sizeBudgetPlanMatches(c.snapshot, c.ec.InstanceID, c.ec.State, &p, r) {
			t.Errorf("%s: tampered plan accepted", name)
		}
	}
	// Without the comparison in the frozen policy, no VideoToolbox plan is
	// a budget plan.
	frozen := *c.snapshot
	size := *frozen.Size
	budget := *size.SizeBudget
	budget.Comparison = nil
	size.SizeBudget, frozen.Size = &budget, &size
	if sizeBudgetPlanMatches(&frozen, c.ec.InstanceID, c.ec.State, plan, rec) {
		t.Fatal("VideoToolbox plan accepted without the frozen comparison")
	}
	size.Hardware = nil
	budget.Comparison = c.snapshot.Size.SizeBudget.Comparison
	if sizeBudgetPlanMatches(&frozen, c.ec.InstanceID, c.ec.State, plan, rec) {
		t.Fatal("VideoToolbox plan accepted with allow_hardware=false")
	}
}

func TestSizeBudgetVideoToolboxCorrection(t *testing.T) {
	c := vtBudgetCase(t)
	plan := getPlan(c.ec.State["plan"])
	result, handled := c.engine.retrySizeBudgetBitrate(c.ec, plan, 4_200_000_000, &transcode.FinalQualityEvidence{})
	rec := sizeBudgetOf(c.ec.State)
	// (3.88 GB - 0.3 GB) / (4.2 GB - 0.3 GB) of 4773 kbps.
	const corrected = 4381
	next := getPlan(c.ec.State["plan"])
	if !handled || result.RewindTo != "submit_transcode" || rec.Correction == nil || rec.Correction.BitrateKbps != corrected || next.AverageBitrateKbps != corrected || c.ec.State["job_id"] != "job-size-budget-budget-rate" || c.ec.State["previous_job_id"] != "job-size-budget" {
		t.Fatalf("result %+v correction %+v plan kbps %d job %v", result, rec.Correction, next.AverageBitrateKbps, c.ec.State["job_id"])
	}
	if d, err := transcode.DigestPlan(withoutDigest(next)); err != nil || d != next.PlanDigest || c.ec.State["plan_digest"] != d {
		t.Fatal("corrected plan digest")
	}
	if !sizeBudgetPlanMatches(c.snapshot, c.ec.InstanceID, c.ec.State, next, rec) || sizeBudgetPlanMatches(c.snapshot, c.ec.InstanceID, c.ec.State, plan, rec) {
		t.Fatal("validation does not rebuild exactly the corrected plan")
	}
	kinds := []string{}
	for _, p := range rec.Curve {
		kinds = append(kinds, p.Kind)
	}
	if got := strings.Join(kinds, ","); !strings.HasSuffix(got, "budget_measured,budget_measured,budget_overshoot,budget_fallback") {
		t.Fatalf("curve kinds %s", got)
	}
	// The correction must name the encode it replaced.
	c.ec.State[sizeBudgetCorrectedFromKey] = "job-other"
	if sizeBudgetPlanMatches(c.snapshot, c.ec.InstanceID, c.ec.State, next, rec) {
		t.Fatal("correction accepted for another encode")
	}
	c.ec.State[sizeBudgetCorrectedFromKey] = "job-size-budget"
	// A manual retry of the corrected encode moves previous_job_id on; the
	// retried encode still validates.
	c.ec.State["previous_job_id"], c.ec.State["job_id"] = "job-size-budget-budget-rate", "job-size-budget-retry-1"
	if !sizeBudgetPlanMatches(c.snapshot, c.ec.InstanceID, c.ec.State, next, rec) {
		t.Fatal("corrected plan rejected after a manual retry")
	}
	// Only once.
	if _, handled := c.engine.retrySizeBudgetBitrate(c.ec, next, 4_100_000_000, nil); handled {
		t.Fatal("corrected twice")
	}
	// Not for x265, and not under the budget.
	x := newComparisonCase(t, map[string]any{"allow_hardware": false})
	x.engine.sizeBudgetFallback(context.Background(), x.ec, map[string]any{}, budgetDecision(nil), nil)
	if _, handled := x.engine.retrySizeBudgetBitrate(x.ec, getPlan(x.ec.State["plan"]), 4_200_000_000, nil); handled {
		t.Fatal("x265 corrected")
	}
	v := vtBudgetCase(t)
	if _, handled := v.engine.retrySizeBudgetBitrate(v.ec, getPlan(v.ec.State["plan"]), 3_990_000_000, nil); handled {
		t.Fatal("corrected under the budget")
	}
}

func TestSizeBudgetVideoToolboxOvershootOnValidation(t *testing.T) {
	c := vtBudgetCase(t)
	plan := getPlan(c.ec.State["plan"])
	candidate := t.TempDir() + "/candidate.mkv"
	if err := os.WriteFile(candidate, nil, 0600); err != nil || os.Truncate(candidate, 4_200_000_000) != nil {
		t.Fatal(err)
	}
	state := c.ec.State
	state["candidate_path"], state["candidate_sha256"], state["candidate_size_bytes"] = candidate, "candidate", int64(4_200_000_000)
	state["quality_evidence"] = map[string]any{"verdict": "pass", "candidate_sha256": "candidate", "plan_digest": plan.PlanDigest, "benchmark_request_digest": plan.QualityValidation.BenchmarkRequestDigest,
		"vmaf": map[string]any{"mean": 84.0, "p5": 75.0, "worst_window_mean": 76.0, "min": 50.0}, "cambi": map[string]any{"mean": 2.0, "max": 9.0}}
	state["original"] = map[string]any{"size_bytes": budgetSourceBytes, "video_codec": "h264"}
	ec := &ExecutionContext{ActionName: "transcode_media", InstanceID: c.ec.InstanceID, Inputs: c.ec.Inputs, State: state, Outputs: map[string]any{}}
	result, err := (&Engine{}).stepTranscodeValidate(context.Background(), ec)
	if err != nil || result.Status != StepCompleted || result.RewindTo != "submit_transcode" || getBool(state, "skip_transcode") {
		t.Fatalf("validation %+v %v", result, err)
	}
	rec := sizeBudgetOf(state)
	if rec.Correction == nil || rec.Outcome != "" || state["quality_evidence"] != nil || state["candidate_path"] != nil {
		t.Fatalf("correction %+v outcome %q", rec.Correction, rec.Outcome)
	}
	if over := rec.Curve[len(rec.Curve)-2]; over.Kind != "budget_overshoot" || over.Bytes != 4_200_000_000 || over.VMAFMean == nil || *over.VMAFMean != 84.0 {
		t.Fatalf("overshoot point %+v", over)
	}
}
