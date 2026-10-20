package action

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// warmStartEnv is the outcome library stub with a worker that runs size v3's
// search rules with quality pruning and accepts a warm start.
func warmStartEnv(t *testing.T) *outcomeLibraryEnv {
	t.Helper()
	env := newOutcomeLibraryEnv(t)
	env.caps.AutomaticSearchRules = true
	env.caps.AutomaticQualityPruning = true
	env.caps.AutomaticWarmStart = true
	return env
}

// sibling writes another episode next to the source: other bytes, so exact
// reuse does not apply.
func (env *outcomeLibraryEnv) sibling(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(filepath.Dir(env.source), name)
	b := make([]byte, 1000)
	copy(b, name)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (env *outcomeLibraryEnv) lastRequest(t *testing.T, res *ActionResult) transcode.BenchmarkRequest {
	t.Helper()
	req, ok := env.requests[getString(res.State, "benchmark_job_id")]
	if !ok {
		t.Fatal("no search was sent")
	}
	return req
}

func TestWarmStartFromSameSeasonSibling(t *testing.T) {
	env := warmStartEnv(t)
	season := map[string]any{"library_context": map[string]any{"service": "sonarr", "id": 7, "season": 1, "episode": 1}}
	first := env.run(t, season)
	if req := env.lastRequest(t, first); req.WarmStart != nil {
		t.Fatalf("the first episode has no siblings yet: %+v", req.WarmStart)
	}
	env.source = env.sibling(t, "episode2.mkv")
	season["library_context"].(map[string]any)["episode"] = 2
	second := env.run(t, season)
	if atomic.LoadInt32(&env.mock.benchmarkSubmitCalls) != 2 {
		t.Fatalf("a sibling is searched, not reused: %d searches", env.mock.benchmarkSubmitCalls)
	}
	req := env.lastRequest(t, second)
	if req.WarmStart == nil || *req.WarmStart != (transcode.WarmStart{QualityValue: 26, Basis: transcode.WarmStartSeriesSeason, Siblings: 1}) {
		t.Fatalf("warm start %+v", req.WarmStart)
	}
	if err := transcode.ValidateBenchmarkRequest(&req); err != nil {
		t.Fatalf("the request with its prior is valid: %v", err)
	}
	report, ok := second.State[warmStartKey].(*warmStartReport)
	if !ok || report.QualityValue != 26 || len(report.FromActions) != 1 || report.FromActions[0] != first.ID || report.Note == "" {
		t.Fatalf("status: %#v", second.State[warmStartKey])
	}
	if req.AutomaticSearch == nil || !req.QualityPruning {
		t.Fatal("the prior rides on a pruned search with rules")
	}
	// The prior changes no limit: the frozen policy and its quality are those
	// of the first episode's search.
	firstReq := env.lastRequestByID(t, getString(first.State, "benchmark_job_id"))
	if firstReq.Quality == nil || req.Quality == nil || *firstReq.Quality.VMAF.P5Minimum != *req.Quality.VMAF.P5Minimum || firstReq.Quality.VMAF.Minimum != req.Quality.VMAF.Minimum {
		t.Fatal("quality limits differ between a search with a prior and one without")
	}
}

func (env *outcomeLibraryEnv) lastRequestByID(t *testing.T, id string) transcode.BenchmarkRequest {
	t.Helper()
	req, ok := env.requests[id]
	if !ok {
		t.Fatalf("no search %s", id)
	}
	return req
}

func TestWarmStartFallsBackToFolderAndRespectsSwitches(t *testing.T) {
	env := warmStartEnv(t)
	env.run(t, nil)
	env.source = env.sibling(t, "episode2.mkv")
	if req := env.lastRequest(t, env.run(t, nil)); req.WarmStart == nil || req.WarmStart.Basis != transcode.WarmStartFolder {
		t.Fatalf("a source without library identity uses its folder: %+v", req.WarmStart)
	}
	env.source = env.sibling(t, "episode3.mkv")
	if res := env.run(t, map[string]any{reuseLibraryOutcomeInput: false}); env.lastRequest(t, res).WarmStart != nil || res.State[warmStartKey] != nil {
		t.Fatal("reuse_library_outcome=false also turns the prior off")
	}
	env.caps.AutomaticWarmStart = false
	env.source = env.sibling(t, "episode4.mkv")
	if res := env.run(t, nil); env.lastRequest(t, res).WarmStart != nil {
		t.Fatal("a worker without automatic_warm_start rejects the field and must not get it")
	}
}

// A sibling whose winner failed final validation gives no prior.
func TestWarmStartIgnoresUnvalidatedWinners(t *testing.T) {
	env := warmStartEnv(t)
	env.verdict = "fail"
	env.run(t, nil)
	env.verdict = "pass"
	env.source = env.sibling(t, "episode2.mkv")
	if req := env.lastRequest(t, env.run(t, nil)); req.WarmStart != nil {
		t.Fatalf("prior from a winner that failed final validation: %+v", req.WarmStart)
	}
}
