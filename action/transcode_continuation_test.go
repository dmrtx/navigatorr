package action

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

func failedFinalQuality(p5, window float64) transcode.FinalQualityEvidence {
	threshold := 80.0
	return transcode.FinalQualityEvidence{
		Verdict:     "fail",
		ReasonCodes: []string{"quality_vmaf_p5_below_minimum", "quality_vmaf_worst_window_below_minimum"},
		VMAF:        &quality.VMAFStats{Mean: 93.4, P5: p5, Min: 61.2, WorstWindowSeconds: 10, WorstWindowMean: window, FrameThreshold: &threshold, FramesBelowThreshold: 30, FramesTotal: 1000},
	}
}

// continuationFlow runs the part of the size template a continuation changes:
// the search is stubbed, the final-validation wait is the real step, and the
// engine's rewind moves the action back to the search. The stubbed search
// finds a VideoToolbox winner first and an x265 winner when it is continued.
type continuationFlow struct {
	engine       *Engine
	benchmarkIDs []string
	jobIDs       []string
	accepted     int
	searches     int
	// firstSearchSeconds is the time the VideoToolbox search is reported to
	// have used.
	firstSearchSeconds float64
}

func newContinuationFlow(t *testing.T, firstSearchSeconds float64) *continuationFlow {
	t.Helper()
	f := &continuationFlow{firstSearchSeconds: firstSearchSeconds}
	mock := &mockTranscodeExecutor{}
	mock.capabilitiesFunc = func(context.Context) (transcode.WorkerCapabilities, error) {
		return hardwareTestCapabilities(), nil
	}
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Transcode: mock})
	policy, err := modePolicyWithHardware("size", false, 0, mustHardwareRules(t))
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]any{}
	b, _ := json.Marshal(policy)
	_ = json.Unmarshal(b, &stored)
	base := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", ExpectedBitDepth: 8, PlanDigest: "sha256:" + strings.Repeat("b", 64)}
	e.RegisterTemplate(ActionTemplate{Name: "transcode_media", Steps: []StepDefinition{
		{Name: "preflight", Run: func(context.Context, *ExecutionContext) (StepResult, error) {
			return StepResult{Status: StepCompleted, Outputs: map[string]any{
				"mode_policy": stored, "plan": base, "plan_source": "profile", "search_budget_seconds": 600, "optimization_enabled": true,
				"original_sha256": strings.Repeat("a", 64),
			}}, nil
		}},
		{Name: "submit_benchmark", Run: func(_ context.Context, ec *ExecutionContext) (StepResult, error) {
			id, err := getOrCreateBenchmarkJobID(ec)
			if err != nil {
				return StepResult{}, err
			}
			f.searches++
			f.benchmarkIDs = append(f.benchmarkIDs, id)
			return StepResult{Status: StepCompleted, Outputs: map[string]any{"benchmark_job_id": id, "benchmark_submitted": true, "benchmark_request_digest": "sha256:" + strings.Repeat("c", 64)}}, nil
		}},
		{Name: "wait_benchmark", Run: func(_ context.Context, ec *ExecutionContext) (StepResult, error) {
			if continuationActive(ec.State) {
				ec.State["benchmark_winner"] = &transcode.BenchmarkWinner{CandidateID: "cand_crf16", VideoCodec: transcode.VideoCodecLibX265}
				ec.State["benchmark_search_seconds"] = 420 + getFloat(ec.State, continuationPriorSecondsKey)
				return StepResult{Status: StepCompleted, Outputs: map[string]any{"benchmark_done": true}}, nil
			}
			winner := &transcode.BenchmarkWinner{CandidateID: "fallback-hardware-cand_q70", VideoCodec: transcode.VideoCodecHEVCVideoToolbox}
			rememberContinuationBase(ec, getPlan(ec.State["plan"]), winner)
			won := *base
			won.VideoCodec, won.PlanDigest = transcode.VideoCodecHEVCVideoToolbox, "sha256:"+strings.Repeat("d", 64)
			ec.State["plan"], ec.State["plan_source"] = &won, "benchmark_winner"
			ec.State["benchmark_winner"] = winner
			ec.State["benchmark_search_seconds"] = f.firstSearchSeconds
			return StepResult{Status: StepCompleted, Outputs: map[string]any{"benchmark_done": true}}, nil
		}},
		{Name: "submit_transcode", Run: func(_ context.Context, ec *ExecutionContext) (StepResult, error) {
			jobID := getString(ec.State, "job_id")
			if jobID == "" {
				jobID = "job-" + ec.InstanceID
			}
			f.jobIDs = append(f.jobIDs, jobID)
			return StepResult{Status: StepCompleted, Outputs: map[string]any{"job_id": jobID, "external_reference": jobID, "transcode_submitted": true}}, nil
		}},
		{Name: "wait_transcode", Run: e.stepTranscodeWait},
		{Name: "accept_result", Run: func(_ context.Context, ec *ExecutionContext) (StepResult, error) {
			if getBool(ec.State, "skip_transcode") {
				return StepResult{Status: StepSkipped}, nil
			}
			f.accepted++
			return StepResult{Status: StepCompleted}, nil
		}},
	}})
	f.engine = e
	return f
}

func (f *continuationFlow) run(t *testing.T) *ActionResult {
	t.Helper()
	res, err := f.engine.Run(context.Background(), "transcode_media", map[string]any{"mode": "size"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A VideoToolbox result that fails the final quality check continues the same
// action with an x265 search, which wins and validates.
func TestHardwareWinnerFailingFinalValidationContinuesWithX265(t *testing.T) {
	f := newContinuationFlow(t, 700)
	// The action id is only known once it runs, so the worker answers by suffix.
	mock := f.engine.deps.Transcode.(*mockTranscodeExecutor)
	mock.statusFunc = func(_ context.Context, id string) (transcode.JobStatus, error) {
		if strings.HasSuffix(id, "-x265") {
			return transcode.JobStatus{ID: id, Status: transcode.StatusCompleted, Progress: 100}, nil
		}
		ev := failedFinalQuality(82.4, 83.9)
		return transcode.JobStatus{ID: id, Status: transcode.StatusFailed, ReasonCode: "quality_not_met", QualityEvidence: &ev}, nil
	}
	res := f.run(t)
	if res.Status != StatusCompleted || f.accepted != 1 || f.searches != 2 {
		t.Fatalf("status %s accepted %d searches %d: %+v", res.Status, f.accepted, f.searches, res.Outputs)
	}
	if getBool(res.State, "skip_transcode") || res.State["reason_code"] != nil {
		t.Fatalf("the x265 result was not taken: %+v", res.State)
	}
	if len(f.benchmarkIDs) != 2 || f.benchmarkIDs[0] != "bench-"+res.ID || f.benchmarkIDs[1] != "bench-"+res.ID+"-x265" {
		t.Fatalf("benchmark identities %v", f.benchmarkIDs)
	}
	if len(f.jobIDs) != 2 || f.jobIDs[0] != "job-"+res.ID || f.jobIDs[1] != "job-"+res.ID+"-x265" {
		t.Fatalf("job identities %v", f.jobIDs)
	}
	rec := continuationOf(res.State)
	if rec == nil || rec.Outcome != continuationContinued || rec.FailedCandidateID != "fallback-hardware-cand_q70" || rec.FailedJobID != "job-"+res.ID || rec.FirstSearchSeconds != 700 || rec.RemainingSearchSeconds != 2300 {
		t.Fatalf("continuation record %+v", rec)
	}
	if !strings.Contains(rec.Message, "VideoToolbox result failed the final quality check; continued with x265") {
		t.Fatalf("message %q", rec.Message)
	}
	// The failed attempt's validation numbers stay visible.
	if rec.Final == nil || rec.Final.Verdict != "fail" || len(rec.Final.FailedChecks) != 2 || *rec.Final.Measured.VMAFP5 != 82.4 || *rec.Final.Measured.VMAFWorstWindow != 83.9 {
		t.Fatalf("failed final validation not kept: %+v", rec.Final)
	}
	// The search ran from the plan it started with, and its total time is both searches.
	plan := getPlan(res.State["plan"])
	if plan == nil || plan.VideoCodec != transcode.VideoCodecLibX265 || plan.PlanDigest != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("continuation did not restart from the base plan: %+v", plan)
	}
	if got := getFloat(res.State, "benchmark_search_seconds"); got != 1120 {
		t.Fatalf("search seconds %v, want both searches (700 + 420)", got)
	}
	if res.State["previous_job_id"] != "job-"+res.ID {
		t.Fatalf("failed job not recorded: %v", res.State["previous_job_id"])
	}
	explanation := ExplainTranscode(res)
	if explanation == nil || explanation.HardwareContinuation == nil || explanation.HardwareContinuation.Outcome != continuationContinued {
		t.Fatalf("the result does not say it continued: %+v", explanation)
	}
}

// An x265 continuation that also fails ends the action as a quality rejection:
// there is one continuation per action and no loop.
func TestFailedX265ContinuationEndsTheActionOnce(t *testing.T) {
	f := newContinuationFlow(t, 700)
	mock := f.engine.deps.Transcode.(*mockTranscodeExecutor)
	polls := 0
	mock.statusFunc = func(_ context.Context, id string) (transcode.JobStatus, error) {
		polls++
		ev := failedFinalQuality(81, 83)
		if strings.HasSuffix(id, "-x265") {
			ev = failedFinalQuality(83.1, 84.6)
		}
		return transcode.JobStatus{ID: id, Status: transcode.StatusFailed, ReasonCode: "quality_not_met", QualityEvidence: &ev}, nil
	}
	res := f.run(t)
	if res.Status != StatusCompleted || f.accepted != 0 || f.searches != 2 || polls != 2 || len(f.jobIDs) != 2 {
		t.Fatalf("status %s accepted %d searches %d polls %d: %+v", res.Status, f.accepted, f.searches, polls, res.Outputs)
	}
	if !getBool(res.State, "skip_transcode") || res.State["reason_code"] != "quality_not_met" {
		t.Fatalf("the action did not end as a quality rejection: %+v", res.State)
	}
	rec := continuationOf(res.State)
	if rec == nil || rec.Outcome != continuationContinued {
		t.Fatalf("the continuation record was lost: %+v", rec)
	}
	// The final failure is the x265 result's; the hardware attempt's is in the record.
	final := summarizeFinalQuality(statusEvidence(t, res), nil)
	if final == nil || *final.Measured.VMAFP5 != 83.1 || *rec.Final.Measured.VMAFP5 != 81 {
		t.Fatalf("validation numbers mixed up: final %+v record %+v", final, rec.Final)
	}
	if explanation := ExplainTranscode(res); explanation.Outcome != "quality_rejected" || explanation.HardwareContinuation == nil {
		t.Fatalf("explanation %+v", explanation)
	}
}

func statusEvidence(t *testing.T, res *ActionResult) *transcode.FinalQualityEvidence {
	t.Helper()
	var ev transcode.FinalQualityEvidence
	if res.State["quality_evidence"] == nil || !reencode(res.State["quality_evidence"], &ev) {
		t.Fatalf("no quality evidence in state: %+v", res.State)
	}
	return &ev
}

// When the first search used the ceiling there is no time for another, so the
// rejection stands and says why.
func TestContinuationNeedsSearchTimeLeft(t *testing.T) {
	for name, seconds := range map[string]float64{"ceiling used": 3000, "less than a base budget left": 2500} {
		t.Run(name, func(t *testing.T) {
			f := newContinuationFlow(t, seconds)
			mock := f.engine.deps.Transcode.(*mockTranscodeExecutor)
			mock.statusFunc = func(_ context.Context, id string) (transcode.JobStatus, error) {
				ev := failedFinalQuality(82.4, 83.9)
				return transcode.JobStatus{ID: id, Status: transcode.StatusFailed, ReasonCode: "quality_not_met", QualityEvidence: &ev}, nil
			}
			res := f.run(t)
			if res.Status != StatusCompleted || f.searches != 1 || len(f.jobIDs) != 1 || f.accepted != 0 {
				t.Fatalf("status %s searches %d jobs %v accepted %d", res.Status, f.searches, f.jobIDs, f.accepted)
			}
			if !getBool(res.State, "skip_transcode") || res.State["reason_code"] != "quality_not_met" {
				t.Fatalf("the rejection did not stand: %+v", res.State)
			}
			rec := continuationOf(res.State)
			if rec == nil || rec.Outcome != continuationNotStarted || rec.Reason != "search_budget_exhausted" || rec.Final == nil || *rec.Final.Measured.VMAFP5 != 82.4 {
				t.Fatalf("record %+v", rec)
			}
			if continuationActive(res.State) {
				t.Fatal("a continuation that did not start is active")
			}
		})
	}
}

// Only a VideoToolbox winner failing the final quality check continues; an x265
// winner failing it, and other rejections, end as they always did.
func TestContinuationOnlyFollowsAHardwareQualityFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		codec, reason string
		mode          string
	}{
		"x265 winner":              {transcode.VideoCodecLibX265, "quality_not_met", "size"},
		"hardware size rejection":  {transcode.VideoCodecHEVCVideoToolbox, "insufficient_savings", "size"},
		"hardware other mode":      {transcode.VideoCodecHEVCVideoToolbox, "quality_not_met", "quality"},
		"hardware preservation":    {transcode.VideoCodecHEVCVideoToolbox, "preservation_not_met", "size"},
		"hardware already encoded": {transcode.VideoCodecHEVCVideoToolbox, "already_target_codec", "size"},
	} {
		t.Run(name, func(t *testing.T) {
			e := NewEngine(EngineDeps{Transcode: &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
				return hardwareTestCapabilities(), nil
			}}})
			ec := &ExecutionContext{ActionName: "transcode_media", InstanceID: "act", Inputs: map[string]any{"mode": tc.mode}, Outputs: map[string]any{},
				State: map[string]any{
					"benchmark_winner": &transcode.BenchmarkWinner{CandidateID: "c", VideoCodec: tc.codec}, continuationBasePlanKey: &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265},
					"benchmark_search_seconds": float64(700), "search_budget_seconds": 600, "job_id": "job-act", "benchmark_job_id": "bench-act",
				}}
			ev := failedFinalQuality(82, 83)
			res, handled, err := e.continueWithoutHardware(context.Background(), ec, transcode.JobStatus{Status: transcode.StatusFailed, ReasonCode: tc.reason, QualityEvidence: &ev})
			if err != nil || handled || res.RewindTo != "" || ec.State[hardwareContinuationKey] != nil {
				t.Fatalf("handled=%v res=%+v state=%v err=%v", handled, res, ec.State[hardwareContinuationKey], err)
			}
		})
	}
}

// A worker that cannot encode with x265 cannot continue; the rejection stands.
func TestContinuationNeedsAnX265Worker(t *testing.T) {
	f := newContinuationFlow(t, 700)
	mock := f.engine.deps.Transcode.(*mockTranscodeExecutor)
	mock.capabilitiesFunc = func(context.Context) (transcode.WorkerCapabilities, error) {
		caps := hardwareTestCapabilities()
		caps.Encoders = map[string]bool{transcode.VideoCodecHEVCVideoToolbox: true}
		return caps, nil
	}
	mock.statusFunc = func(_ context.Context, id string) (transcode.JobStatus, error) {
		ev := failedFinalQuality(82.4, 83.9)
		return transcode.JobStatus{ID: id, Status: transcode.StatusFailed, ReasonCode: "quality_not_met", QualityEvidence: &ev}, nil
	}
	res := f.run(t)
	rec := continuationOf(res.State)
	if f.searches != 1 || rec == nil || rec.Outcome != continuationNotStarted || rec.Reason != "x265_unavailable" || res.State["reason_code"] != "quality_not_met" {
		t.Fatalf("searches %d record %+v state %+v", f.searches, rec, res.State)
	}
}

// The continued search asks the worker for x265 only, reuses nothing it cannot
// name, and fits its extension inside what the ceiling left.
func TestContinuedSearchRequestIsX265OnlyWithinTheCeiling(t *testing.T) {
	policy, err := modePolicyWithHardware("size", false, 0, mustHardwareRules(t))
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]any{}
	b, _ := json.Marshal(policy)
	_ = json.Unmarshal(b, &stored)
	newEC := func() *ExecutionContext {
		return &ExecutionContext{InstanceID: "act-cont", ActionName: "transcode_media", Inputs: map[string]any{"mode": "size"}, Outputs: map[string]any{},
			State: map[string]any{"mode_policy": stored, "original_sha256": strings.Repeat("a", 64), "search_budget_seconds": 600}}
	}
	// The first search: hardware candidates and rules.
	first := hardwareRequestFromState(t, newEC(), hardwareTestCapabilities())
	if first.HardwareSearch == nil || first.ContinuesBenchmark != nil {
		t.Fatalf("first search %+v", first.HardwareSearch)
	}
	digest := "sha256:" + strings.Repeat("e", 64)
	for name, tc := range map[string]struct {
		remaining   float64
		caps        func(*transcode.WorkerCapabilities)
		wantMax     int
		wantNoExt   bool
		wantContRef bool
	}{
		"room for the extension":        {remaining: 2300, wantMax: 1700, wantContRef: true},
		"room for the base budget only": {remaining: 900, wantNoExt: true, wantContRef: true},
		"worker cannot reuse":           {remaining: 2300, caps: func(c *transcode.WorkerCapabilities) { c.SearchContinuation = false }, wantMax: 1700},
	} {
		t.Run(name, func(t *testing.T) {
			ec := newEC()
			ec.State[hardwareContinuationKey] = &HardwareContinuation{Outcome: continuationContinued, FailedBenchmarkJobID: "bench-act-cont", FailedBenchmarkDigest: digest, RemainingSearchSeconds: tc.remaining}
			ec.State["benchmark_job_id"] = "bench-act-cont-x265"
			caps := hardwareTestCapabilities()
			caps.SearchBudgetExtension = true
			if tc.caps != nil {
				tc.caps(&caps)
			}
			req := hardwareRequestFromState(t, ec, caps)
			if req.HardwareSearch != nil {
				t.Fatalf("a continued search kept hardware rules: %+v", req.HardwareSearch)
			}
			for _, c := range req.Candidates {
				if c.VideoCodec == transcode.VideoCodecHEVCVideoToolbox {
					t.Fatalf("a continued search kept %s", c.ID)
				}
			}
			if req.ID != "bench-act-cont-x265" || len(req.Candidates) >= len(first.Candidates) {
				t.Fatalf("request %s with %d candidates (first search %d)", req.ID, len(req.Candidates), len(first.Candidates))
			}
			if tc.wantContRef != (req.ContinuesBenchmark != nil) || tc.wantContRef && (req.ContinuesBenchmark.ID != "bench-act-cont" || req.ContinuesBenchmark.RequestDigest != digest) {
				t.Fatalf("continuation reference %+v", req.ContinuesBenchmark)
			}
			if tc.wantNoExt {
				if req.SearchExtension != nil {
					t.Fatalf("an extension did not fit but was sent: %+v", req.SearchExtension)
				}
			} else if req.SearchExtension == nil || req.SearchExtension.MaxSeconds != tc.wantMax {
				t.Fatalf("extension %+v, want max %d", req.SearchExtension, tc.wantMax)
			}
			// Whatever it was sent, the search cannot pass what the ceiling left.
			longest := req.SearchBudgetSeconds
			if req.SearchExtension != nil {
				longest = req.SearchExtension.TotalSeconds()
			}
			if float64(longest) > tc.remaining {
				t.Fatalf("the search may run %ds with %.0fs left", longest, tc.remaining)
			}
			if err := transcode.ValidateBenchmarkRequest(req); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The base plan is kept only when a VideoToolbox winner replaces it.
func TestRememberContinuationBaseOnlyForHardwareWinners(t *testing.T) {
	base := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265}
	for name, tc := range map[string]struct {
		codec, mode string
		kept        bool
	}{
		"hardware size":       {transcode.VideoCodecHEVCVideoToolbox, "size", true},
		"x265 size":           {transcode.VideoCodecLibX265, "size", false},
		"hardware other mode": {transcode.VideoCodecHEVCVideoToolbox, "quality", false},
	} {
		ec := &ExecutionContext{ActionName: "transcode_media", Inputs: map[string]any{"mode": tc.mode}, State: map[string]any{}}
		rememberContinuationBase(ec, base, &transcode.BenchmarkWinner{VideoCodec: tc.codec})
		if (ec.State[continuationBasePlanKey] != nil) != tc.kept {
			t.Fatalf("%s: kept=%v", name, ec.State[continuationBasePlanKey] != nil)
		}
	}
}

// The failing metric and its numbers come out of the explanation both the web
// API and action_status read.
func TestExplanationNamesTheFailedFinalQualityMetric(t *testing.T) {
	ev := failedFinalQuality(82.4, 83.9)
	limits := 84.0
	window := 85.0
	res := &ActionResult{ActionName: "transcode_media", Status: StatusCompleted, Inputs: map[string]any{"mode": "size"},
		State: map[string]any{"reason_code": "quality_not_met", "skip_transcode": true, "original_intact": true, "quality_evidence": &ev,
			"benchmark_request": &transcode.BenchmarkRequest{Quality: &transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Minimum: 90, P5Minimum: &limits, WorstWindowMinimum: &window, WorstWindowSeconds: 10}}}}}
	explanation := ExplainTranscode(res)
	if explanation == nil || explanation.FinalQuality == nil {
		t.Fatalf("no final quality in %+v", explanation)
	}
	got := strings.Join(explanation.FinalQuality.Failures, "; ")
	if !strings.Contains(got, "VMAF p5 82.4 is below the minimum 84.0") || !strings.Contains(got, "worst 10-second window 83.9 is below the minimum 85.0") {
		t.Fatalf("failures %q", got)
	}
	if explanation.Outcome != "quality_rejected" || !strings.Contains(explanation.Message, "p5") {
		t.Fatalf("explanation %+v", explanation)
	}
}

// A batch child whose VideoToolbox result still awaits its final quality check
// may start a second search, so the first search's time never releases the
// reservation early: it is held until the child is terminal, and then settles
// on both searches' time.
func TestBatchBudgetHoldsTheReservationForAPossibleContinuation(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st})
	ec := &ExecutionContext{InstanceID: "parent", Inputs: map[string]any{"mode": "size"}, State: map[string]any{}}
	if _, err := freezeModePolicy(ec); err != nil {
		t.Fatal(err)
	}
	item := store.TranscodeBatchItem{ItemKey: "file-a", ChildActionID: "hw-child"}
	key := batchChildKey(ec, item)
	if !reserveModeSearch(ec, key) {
		t.Fatal("initial reservation failed")
	}
	child := map[string]any{"benchmark_submitted": true, "benchmark_search_seconds": 700.0, continuationBasePlanKey: map[string]any{"video_codec": "libx265"}}
	seedActionInstance(t, st, item.ChildActionID, "transcode_media", StatusWaitingExternal, 4, key, ec.Inputs, child)
	settle := func() SearchBudgetEntry {
		e.settleModeSearches(ec, []store.TranscodeBatchItem{item})
		return modeBudgetEntries(ec)[key]
	}
	update := func(status string, state map[string]any) {
		inst, _ := st.GetActionInstance(item.ChildActionID)
		inst.Status, inst.StateJSON = status, toJSON(state)
		if err := st.UpdateActionInstance(*inst); err != nil {
			t.Fatal(err)
		}
	}
	// Encoding the hardware result: the whole reservation stays held.
	if entry := settle(); entry.Consumed != nil || entry.Reserved != 3000 {
		t.Fatalf("the first search released its reservation early: %+v", entry)
	}
	// Continued: the second search is running, its time is not known yet.
	continued := map[string]any{"benchmark_submitted": true, continuationBasePlanKey: map[string]any{}, hardwareContinuationKey: map[string]any{"outcome": continuationContinued}}
	update(StatusWaitingExternal, continued)
	if entry := settle(); entry.Consumed != nil || entry.Reserved != 3000 {
		t.Fatalf("the continued search lost its reservation: %+v", entry)
	}
	// Finished: both searches' time is what the action spent.
	continued["benchmark_search_seconds"] = 1120.0
	update(StatusCompleted, continued)
	if entry := settle(); entry.Consumed == nil || *entry.Consumed != 1120 || entry.Provisional {
		t.Fatalf("the continued action did not settle on both searches: %+v", entry)
	}
}

// The request type itself refuses a continuation that could bring hardware
// back or would reuse an unrelated search.
func TestContinuedSearchRequestValidation(t *testing.T) {
	policy, err := modePolicyWithHardware("size", false, 0, mustHardwareRules(t))
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]any{}
	b, _ := json.Marshal(policy)
	_ = json.Unmarshal(b, &stored)
	build := func() *transcode.BenchmarkRequest {
		ec := &ExecutionContext{InstanceID: "act-valid", ActionName: "transcode_media", Inputs: map[string]any{"mode": "size"}, Outputs: map[string]any{},
			State: map[string]any{"mode_policy": stored, "original_sha256": strings.Repeat("a", 64), "search_budget_seconds": 600,
				hardwareContinuationKey: &HardwareContinuation{Outcome: continuationContinued, FailedBenchmarkJobID: "bench-act-valid", FailedBenchmarkDigest: "sha256:" + strings.Repeat("e", 64), RemainingSearchSeconds: 2300},
				"benchmark_job_id":      "bench-act-valid-x265"}}
		return hardwareRequestFromState(t, ec, hardwareTestCapabilities())
	}
	if err := transcode.ValidateBenchmarkRequest(build()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*transcode.BenchmarkRequest){
		"continues itself":     func(r *transcode.BenchmarkRequest) { r.ContinuesBenchmark.ID = r.ID },
		"bad digest":           func(r *transcode.BenchmarkRequest) { r.ContinuesBenchmark.RequestDigest = "sha256:abc" },
		"bad identity":         func(r *transcode.BenchmarkRequest) { r.ContinuesBenchmark.ID = "../x" },
		"keeps hardware rules": func(r *transcode.BenchmarkRequest) { r.HardwareSearch = mustHardwareRules(t) },
		"has a media engine candidate": func(r *transcode.BenchmarkRequest) {
			r.Candidates = append(r.Candidates, transcode.BenchmarkCandidate{ID: "vt-cand_q50", VideoCodec: transcode.VideoCodecHEVCVideoToolbox, Quality: 50})
		},
		"not automatic":        func(r *transcode.BenchmarkRequest) { r.Automatic = false },
		"without search rules": func(r *transcode.BenchmarkRequest) { r.AutomaticSearch = nil },
	} {
		r := build()
		mutate(r)
		if err := transcode.ValidateBenchmarkRequest(r); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// The production wait step adds the time of the search before a continued
// search, however that search ends.
func TestBenchmarkWaitSumsContinuedSearchTime(t *testing.T) {
	seconds := 420.0
	mock := &mockTranscodeExecutor{benchmarkStatusFunc: func(_ context.Context, id string) (transcode.BenchmarkStatus, error) {
		return transcode.BenchmarkStatus{ProtocolVersion: transcode.WorkerProtocolVersion, ID: id, Status: transcode.StatusFailed, ReasonCode: "no_suitable_candidate", Error: "no candidate qualified", SearchSeconds: &seconds}, nil
	}}
	e := NewEngine(EngineDeps{Transcode: mock})
	state := func() map[string]any {
		return map[string]any{"optimization_enabled": true, "benchmark_job_id": "bench-wait-test", "search_budget_seconds": 600}
	}
	for name, tc := range map[string]struct {
		prior any
		want  float64
	}{"first search": {nil, 420}, "continued search": {700.0, 1120}} {
		ec := &ExecutionContext{InstanceID: "wait-test", ActionName: "transcode_media", Inputs: map[string]any{"mode": "size"}, Outputs: map[string]any{}, State: state()}
		if tc.prior != nil {
			ec.State[hardwareContinuationKey] = &HardwareContinuation{Outcome: continuationContinued}
			ec.State[continuationPriorSecondsKey] = tc.prior
		}
		if _, err := e.stepBenchmarkWait(context.Background(), ec); err != nil {
			t.Fatal(err)
		}
		if got := getFloat(ec.State, "benchmark_search_seconds"); got != tc.want {
			t.Fatalf("%s: recorded search time %v, want %v", name, got, tc.want)
		}
	}
}
