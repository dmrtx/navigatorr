package action

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
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
