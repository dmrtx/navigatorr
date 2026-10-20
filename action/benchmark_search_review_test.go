package action

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

func TestBenchmarkSearchStopsAcrossNewActionsAndSupportsExplicitReview(t *testing.T) {
	mock := &mockTranscodeExecutor{benchmarkStatusFunc: func(ctx context.Context, id string) (transcode.BenchmarkStatus, error) {
		return transcode.BenchmarkStatus{ID: id, Status: transcode.StatusCompleted, Decision: &transcode.BenchmarkDecision{DecisionReason: "all_candidates_invalid"}}, nil
	}}
	e, st, path, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
	ctx := context.Background()
	for _, name := range []string{"transcode_media", "benchmark_transcode"} {
		r, err := e.Run(ctx, name, map[string]any{"path": path, "profile": "opt-vt"})
		if err != nil || !getBool(r.State, "benchmark_done") {
			t.Fatalf("initial round: %v %+v", err, r)
		}
	}
	// A different recipe name and a fresh Engine cannot reset durable history.
	e.deps.Config.Transcode.Profiles["another-recipe"] = e.deps.Config.Transcode.Profiles["opt-vt"]
	e = NewEngine(e.deps)
	r, err := e.Run(ctx, "benchmark_transcode", map[string]any{"path": path, "profile": "another-recipe"})
	if err != nil || r.Status != StatusWaitingDecision || mock.benchmarkSubmitCalls != 2 {
		t.Fatalf("third round escaped: %v %+v calls=%d", err, r, mock.benchmarkSubmitCalls)
	}
	id := r.ID
	proposal, _ := json.Marshal(r.State["benchmark_request"])
	inst, _ := st.GetActionInstance(id)
	if !getBool(parseExecutionContext(inst, e).State, "benchmark_search_review_required") || len(r.WaitingOptions) != 2 {
		t.Fatalf("missing review choices: %+v", r)
	}
	for _, decision := range []string{"", "approve"} {
		r, err = e.Resume(ctx, id, decision, nil)
		if err != nil || r.Status != StatusWaitingDecision || mock.benchmarkSubmitCalls != 2 {
			t.Fatalf("unexpected authorization %q: %v %+v", decision, err, r)
		}
	}
	// A reviewed proposal is fixed even if coordinator auto-capability probing
	// would now report a different set; the worker still validates execution.
	mock.capabilitiesFunc = func(context.Context) (transcode.WorkerCapabilities, error) {
		t.Error("rebuilt the reviewed proposal")
		return transcode.WorkerCapabilities{}, nil
	}
	mock.benchmarkSubmitFunc = func(_ context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
		actual, _ := json.Marshal(req)
		if string(actual) != string(proposal) {
			t.Error("submitted different parameters from the displayed proposal")
		}
		return transcode.BenchmarkJob{ID: req.ID}, nil
	}
	r, err = e.Resume(ctx, id, "run_once_after_review", nil)
	mock.capabilitiesFunc, mock.benchmarkSubmitFunc = nil, nil
	if err != nil || r.Status != StatusCompleted || mock.benchmarkSubmitCalls != 3 {
		t.Fatalf("explicit round failed: %v %+v", err, r)
	}
	r, err = e.Run(ctx, "benchmark_transcode", map[string]any{"path": path, "profile": "opt-vt"})
	if err != nil || r.Status != StatusWaitingDecision {
		t.Fatalf("review leaked to future actions: %v %+v", err, r)
	}
	r, err = e.Resume(ctx, r.ID, "cancel", nil)
	if err != nil || r.Status != StatusCancelled || mock.benchmarkSubmitCalls != 3 {
		t.Fatalf("stop failed: %v %+v", err, r)
	}
	// Direct encoding never visits search admission.
	ec := &ExecutionContext{State: map[string]any{"optimization_enabled": false, "original_sha256": "same"}}
	if res, err := e.stepBenchmarkSubmit(ctx, ec); err != nil || res.Status != StepSkipped {
		t.Fatalf("direct route blocked: %v %+v", err, res)
	}
	// Changed source bytes are a different search; history is not path-based.
	if err := os.WriteFile(path, []byte("new media contents"), 0644); err != nil {
		t.Fatal(err)
	}
	r, err = e.Run(ctx, "benchmark_transcode", map[string]any{"path": path, "profile": "opt-vt"})
	if err != nil || r.Status != StatusCompleted || mock.benchmarkSubmitCalls != 4 {
		t.Fatalf("new source blocked: %v %+v", err, r)
	}
}

func TestBenchmarkHistoryIgnoresTechnicalFailuresAndAcceptedJobsStillReconcile(t *testing.T) {
	st := setupTestStore(t)
	for _, id := range []string{"technical", "unfinished", "winner", "other-source", "no-winner"} {
		state := map[string]any{"original_sha256": "source", "benchmark_done": true, "has_winner": false}
		switch id {
		case "technical", "unfinished":
			delete(state, "benchmark_done")
		case "winner":
			state["has_winner"] = true
		case "other-source":
			state["original_sha256"] = "other"
		}
		if err := st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: "benchmark_transcode", StateJSON: toJSON(state)}); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := st.UnsuccessfulBenchmarks("source", "current")
	if err != nil || len(ids) != 1 || ids[0] != "no-winner" {
		t.Fatalf("bad history: %v %v", ids, err)
	}
	ids, err = st.UnsuccessfulBenchmarks("source", "no-winner")
	if err != nil || len(ids) != 0 {
		t.Fatalf("counted current action: %v %v", ids, err)
	}
	e := NewEngine(EngineDeps{Store: st})
	ec := &ExecutionContext{State: map[string]any{"optimization_enabled": true, "benchmark_submitted": true, "benchmark_job_id": "already-accepted", "benchmark_search_review_required": true}}
	r, err := e.stepBenchmarkSubmit(context.Background(), ec)
	if err != nil || r.Status != StepCompleted {
		t.Fatalf("accepted job tracking gated: %v %+v", err, r)
	}
}

func seedSearchRound(t *testing.T, st *store.Store, id, mode string, floor float64, candidates []transcode.BenchmarkCandidate, adjust ...func(*transcode.BenchmarkRequest, *transcode.BenchmarkDecision)) {
	t.Helper()
	p5 := floor - 2
	req := transcode.BenchmarkRequest{Candidates: candidates, Quality: &transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Minimum: floor, P5Minimum: &p5, GuardrailEnforcement: "reject"}}}
	decision := transcode.BenchmarkDecision{DecisionReason: "no_candidate_met_minimum"}
	var evidence []transcode.BenchmarkCandidateQualityEvidence
	for _, c := range candidates {
		decision.Evaluations = append(decision.Evaluations, transcode.BenchmarkCandidateEvaluation{CandidateID: c.ID, VideoCodec: c.VideoCodec, Quality: c.Quality, Preset: c.Preset, MetricType: "vmaf", Score: floor - 1, EvaluationReason: "sample_below_minimum_quality"})
		evidence = append(evidence, transcode.BenchmarkCandidateQualityEvidence{CandidateID: c.ID, VMAF: &quality.VMAFStats{ModelID: "vmaf_v0.6.1", Mean: floor - 1, P5: floor - 4, Min: floor - 6, WorstWindowMean: floor - 3, FramesTotal: 240}})
	}
	for _, f := range adjust {
		f(&req, &decision)
	}
	state := map[string]any{"original_sha256": "source", "benchmark_done": true, "has_winner": false, "benchmark_request": req, "benchmark_decision": decision, "benchmark_quality": evidence}
	if err := st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: "transcode_media", InputsJSON: toJSON(map[string]any{"mode": mode}), StateJSON: toJSON(state)}); err != nil {
		t.Fatal(err)
	}
}

func x265(id string, crf int, preset string) transcode.BenchmarkCandidate {
	return transcode.BenchmarkCandidate{ID: id, VideoCodec: "libx265", Quality: crf, Preset: preset}
}

func reviewProposal(floor float64, candidates ...transcode.BenchmarkCandidate) *ExecutionContext {
	p5 := floor - 2
	return &ExecutionContext{InstanceID: "current", State: map[string]any{"original_sha256": "source", "benchmark_request": transcode.BenchmarkRequest{Candidates: candidates, Quality: &transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Minimum: floor, P5Minimum: &p5, GuardrailEnforcement: "reject"}}}}}
}

func TestSearchReviewIgnoresRoundsUnderAStricterGoal(t *testing.T) {
	st := setupTestStore(t)
	ladder := []transcode.BenchmarkCandidate{x265("a", 16, "slow"), x265("b", 20, "medium")}
	seedSearchRound(t, st, "quality-1", "quality", 95, ladder)
	seedSearchRound(t, st, "quality-2", "quality", 95, ladder)
	e := NewEngine(EngineDeps{Store: st})
	// The size goal's floor is lower: failing at 95 says nothing about 88.
	ec := reviewProposal(88, ladder...)
	if res := e.reviewBenchmarkSearch(ec); res != nil {
		t.Fatalf("stricter rounds blocked a lenient goal: %+v", res)
	}
	// A legacy review recorded before this check resumes without a decision.
	ec.State["benchmark_search_review_required"] = true
	if res := e.reviewBenchmarkSearch(ec); res != nil || getBool(ec.State, "benchmark_search_review_required") {
		t.Fatalf("legacy review stayed blocked: %+v", res)
	}
}

func TestSearchReviewOffersNoRepeatOfTheSameLadder(t *testing.T) {
	st := setupTestStore(t)
	ladder := []transcode.BenchmarkCandidate{x265("a", 16, "slow"), x265("b", 20, "medium")}
	seedSearchRound(t, st, "size-1", "size", 88, ladder)
	seedSearchRound(t, st, "size-2", "size", 88, []transcode.BenchmarkCandidate{x265("c", 18, "slow")})
	e := NewEngine(EngineDeps{Store: st})
	// Same settings under new IDs and an equally strict floor: nothing new.
	ec := reviewProposal(88, x265("next-1", 16, "slow"), x265("next-2", 18, "slow"))
	res := e.reviewBenchmarkSearch(ec)
	if res == nil || res.Status != StepWaitingDecision || len(res.WaitingOptions) != 1 || res.WaitingOptions[0].Decision != "cancel" || !strings.Contains(res.WaitingReason, "would repeat them") {
		t.Fatalf("identical ladder offered again: %+v", res)
	}
	review := ec.State["benchmark_search_review"].(*SearchReview)
	if len(review.Rounds) != 2 || len(review.NewCandidates) != 0 || review.Rounds[0].Candidates[0].VMAF == nil || review.Rounds[0].Candidates[0].VMAF.P5 != 84 {
		t.Fatalf("review lacks the measured evidence: %+v", review)
	}
	ec.Decision = "run_once_after_review"
	if res := e.reviewBenchmarkSearch(ec); res == nil || res.Status != StepWaitingDecision || getBool(ec.State, "benchmark_search_reviewed") {
		t.Fatalf("a repeat was authorized: %+v", res)
	}
	// A proposal with an unmeasured setting may run once, naming what is new.
	ec = reviewProposal(88, x265("next-1", 16, "slow"), x265("grain", 18, "slower"))
	res = e.reviewBenchmarkSearch(ec)
	if res == nil || len(res.WaitingOptions) != 2 || !strings.Contains(res.WaitingReason, "1 setting not measured before (grain)") {
		t.Fatalf("new setting not offered: %+v", res)
	}
	ec.Decision = "run_once_after_review"
	if res := e.reviewBenchmarkSearch(ec); res != nil || !getBool(ec.State, "benchmark_search_reviewed") {
		t.Fatalf("explicit run of a new setting refused: %+v", res)
	}
}

func TestSearchReviewDoesNotCountTechnicalFailuresOrOtherOutputsAsTried(t *testing.T) {
	ladder := []transcode.BenchmarkCandidate{x265("a", 16, "slow"), x265("b", 20, "medium")}
	for _, reason := range []string{"candidate_encode_failed", "no_valid_metric", "metric_missing", "incomplete_sample_scores", "invalid_metric", "score_out_of_bounds", "unusable_estimate", "verification_not_passed", "not_verified", "quality_vmaf_evidence_incomplete"} {
		st := setupTestStore(t)
		technical := func(_ *transcode.BenchmarkRequest, d *transcode.BenchmarkDecision) {
			for i := range d.Evaluations {
				d.Evaluations[i].EvaluationReason = reason
			}
		}
		seedSearchRound(t, st, "r1", "size", 88, ladder, technical)
		seedSearchRound(t, st, "r2", "size", 88, ladder, technical)
		e := NewEngine(EngineDeps{Store: st})
		res := e.reviewBenchmarkSearch(reviewProposal(88, ladder...))
		if res == nil || len(res.WaitingOptions) != 2 || res.WaitingOptions[1].Decision != "run_once_after_review" {
			t.Fatalf("%s removed the explicit retry: %+v", reason, res)
		}
	}
	// An earlier goal policy (different audio plan) measured a different
	// output: its savings verdict does not block the current one.
	st := setupTestStore(t)
	v2 := func(r *transcode.BenchmarkRequest, d *transcode.BenchmarkDecision) {
		r.ModePolicyVersion, r.AudioMode = "size-v2", "compact"
		for i := range d.Evaluations {
			d.Evaluations[i].EvaluationReason = "insufficient_savings"
		}
	}
	seedSearchRound(t, st, "v2-1", "size", 88, ladder, v2)
	seedSearchRound(t, st, "v2-2", "size", 88, ladder, v2)
	e := NewEngine(EngineDeps{Store: st})
	ec := reviewProposal(88, ladder...)
	req := ec.State["benchmark_request"].(transcode.BenchmarkRequest)
	req.ModePolicyVersion, req.AudioMode = "size-v3", "compact_lossless"
	ec.State["benchmark_request"] = req
	if res := e.reviewBenchmarkSearch(ec); res != nil {
		t.Fatalf("a v2 round blocked a v3 proposal: %+v", res)
	}
	// The same policy's savings verdicts do count.
	req.ModePolicyVersion, req.AudioMode = "size-v2", "compact"
	ec.State["benchmark_request"] = req
	if res := e.reviewBenchmarkSearch(ec); res == nil || len(res.WaitingOptions) != 1 {
		t.Fatalf("same-policy savings verdicts ignored: %+v", res)
	}
}

// A round verified on the edge windows with a two-attempt cap is not evidence
// about a search that verifies on the final grid until exhausted, and the
// other way round.
func TestRoundComparableSeparatesVerificationRegimes(t *testing.T) {
	ladder := []transcode.BenchmarkCandidate{x265("a", 16, "slow")}
	req := func(rules *transcode.AutomaticSearchRules) *transcode.BenchmarkRequest {
		return &transcode.BenchmarkRequest{Candidates: ladder, AutomaticSearch: rules}
	}
	edges := req(&transcode.AutomaticSearchRules{HoldoutWindows: transcode.HoldoutWindowsFinalGridEdges})
	grid := req(&transcode.AutomaticSearchRules{HoldoutWindows: transcode.HoldoutWindowsFinalGrid, VerifyUntilExhausted: true, FinalFreshWindows: 2})
	if !roundComparable(edges, req(&transcode.AutomaticSearchRules{HoldoutWindows: transcode.HoldoutWindowsFinalGridEdges})) || !roundComparable(grid, req(&transcode.AutomaticSearchRules{HoldoutWindows: transcode.HoldoutWindowsFinalGrid, VerifyUntilExhausted: true, FinalFreshWindows: 2})) {
		t.Fatal("rounds under the same regime stay comparable")
	}
	if roundComparable(edges, grid) || roundComparable(grid, edges) || roundComparable(req(nil), grid) {
		t.Fatal("rounds under different verification regimes must not count against each other")
	}
	if !roundComparable(req(nil), req(&transcode.AutomaticSearchRules{})) {
		t.Fatal("no rules and empty rules are the same regime")
	}
}
