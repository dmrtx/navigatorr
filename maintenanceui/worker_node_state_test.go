package maintenanceui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

func nodeStateByName(t *testing.T, h http.Handler) map[string]map[string]any {
	t.Helper()
	out := getJSON(t, h, "/api/maintenance/worker-node-state")
	list, _ := out["nodes"].([]any)
	byName := map[string]map[string]any{}
	for _, raw := range list {
		node := raw.(map[string]any)
		byName[node["name"].(string)] = node
	}
	return byName
}

// The page polls worker-node-state every second or two, so a build starting,
// ending or being cancelled, and a pause, must show on the very next read, with
// no worker involved (m4 here points at a closed port).
func TestWorkerNodeStateFollowsTheStoreImmediately(t *testing.T) {
	s, h, _ := twoWorkerUI(t)
	idle := nodeStateByName(t, h)
	if len(idle) != 2 || idle["m1"]["builds"] != nil || idle["m1"]["held_by"] != nil || idle["m4"]["held_by"] != nil {
		t.Fatal("idle nodes carry no build, hold or pause", idle)
	}

	build, err := s.engine.CreateBuild(t.Context(), action.BuildInput{Target: "m1", Via: action.BuildViaNodeLease, Source: "ci", Title: "compile"})
	if err != nil {
		t.Fatal(err)
	}
	running := nodeStateByName(t, h)
	builds, _ := running["m1"]["builds"].([]any)
	if len(builds) != 1 || builds[0].(map[string]any)["id"] != build.ID || running["m1"]["held_by"] != nil {
		t.Fatal("a running record is listed on the next read but holds nothing until the worker reports a lease", running["m1"])
	}

	if running["m4"]["builds"] != nil || running["m4"]["held_by"] != nil {
		t.Fatal("the build belongs to m1 only", running["m4"])
	}

	if _, changed, err := s.engine.FinishBuild(t.Context(), build.ID, "succeeded", nil); err != nil || !changed {
		t.Fatal(err, changed)
	}
	done := nodeStateByName(t, h)
	if done["m1"]["builds"] != nil {
		t.Fatal("a finished build is gone on the next read", done["m1"])
	}

	if w := request(h, "POST", "/api/maintenance/worker-pause", `{"node":"m4","mode":"graceful"}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := nodeStateByName(t, h); got["m4"]["held_by"] != "operator" || got["m1"]["held_by"] != nil {
		t.Fatal("an operator pause shows at once, on its node only", got)
	}
	if request(h, "GET", "/api/maintenance/worker-node-state", "", false).Code != 401 {
		t.Fatal("the state needs the session, like the other maintenance reads")
	}
}

// A worker that is busy with a build can take long to answer. The activity
// view must not wait for it: the builds and the hold it shows are the
// server's own, so they come back with the node marked unavailable.
func TestWorkerActivityDoesNotWaitForASlowWorkerAndKeepsItsBuilds(t *testing.T) {
	old := activityProbeTimeout
	activityProbeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { activityProbeTimeout = old })

	s, h := testUI(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/activity", "/v1/ready":
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(slow.Close)
	m1, err := httpclient.NewHTTPExecutor(httpclient.HTTPConfig{BaseURL: slow.URL, Token: strings.Repeat("w", 16)})
	if err != nil {
		t.Fatal(err)
	}
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: "m1", Executor: m1, Address: "192.0.2.10", Podcast: true}})
	if err != nil {
		t.Fatal(err)
	}
	deps := s.engine.Deps()
	deps.Transcode, deps.Workers = nil, set
	s.engine = action.NewEngine(deps)
	if _, err := s.engine.CreateBuild(t.Context(), action.BuildInput{Target: "m1", Via: action.BuildViaNodeLease, Source: "ci", Title: "compile"}); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	activity := getJSON(t, h, "/api/maintenance/worker-activity")
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("the activity view waited %v for a slow worker", took)
	}
	workers, _ := activity["workers"].([]any)
	if len(workers) != 1 {
		t.Fatal(activity)
	}
	node := workers[0].(map[string]any)
	if node["available"] != false || node["held_by"] != nil {
		t.Fatal("the node is unavailable: it reports no lease, so nothing shows it as held", node)
	}
	if builds, _ := node["builds"].([]any); len(builds) != 1 {
		t.Fatal("the build the server knows stays in the answer", node)
	}
}

// One node loaded by a build answers slowly while the other is fine: the fast
// one is reported as usual and the slow one keeps its build, in one answer
// that does not wait for the slow worker.
func TestWorkerActivityWithOneSlowAndOneFastWorker(t *testing.T) {
	old := activityProbeTimeout
	activityProbeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { activityProbeTimeout = old })

	s, h := testUI(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ready":
			writeJSON(w, 200, map[string]bool{"ready": true})
		case "/v1/activity":
			writeJSON(w, 200, map[string]any{"worker_slots_total": 2, "worker_slots_used": 0, "jobs": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fast.Close)
	executor := func(url string) *httpclient.HTTPExecutor {
		e, err := httpclient.NewHTTPExecutor(httpclient.HTTPConfig{BaseURL: url, Token: strings.Repeat("w", 16)})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{
		{Name: "m1", Executor: executor(slow.URL), Address: "192.0.2.10", Podcast: true},
		{Name: "m4", Executor: executor(fast.URL), Address: "192.0.2.11"},
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := s.engine.Deps()
	deps.Transcode, deps.Workers = nil, set
	s.engine = action.NewEngine(deps)
	if _, err := s.engine.CreateBuild(t.Context(), action.BuildInput{Target: "m1", Via: action.BuildViaNodeLease, Source: "ci", Title: "compile"}); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	activity := getJSON(t, h, "/api/maintenance/worker-activity")
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("the activity view waited %v for the slow worker", took)
	}
	list, _ := activity["workers"].([]any)
	if len(list) != 2 {
		t.Fatal(activity)
	}
	byName := map[string]map[string]any{}
	for _, raw := range list {
		node := raw.(map[string]any)
		byName[node["name"].(string)] = node
	}
	if byName["m4"]["available"] != true || byName["m4"]["builds"] != nil {
		t.Fatal("the fast node is reported as usual", byName["m4"])
	}
	if builds, _ := byName["m1"]["builds"].([]any); byName["m1"]["available"] != false || len(builds) != 1 || byName["m1"]["held_by"] != nil {
		t.Fatal("the slow node is unavailable but keeps its build", byName["m1"])
	}
}

// An install without a worker set (the single local transcoder) has no nodes to
// report; the page polls this every couple of seconds, so it must answer, not fail.
func TestWorkerNodeStateWithoutWorkersAnswersEmpty(t *testing.T) {
	s, h := testUI(t)
	deps := s.engine.Deps()
	deps.Workers = nil
	s.engine = action.NewEngine(deps)
	out := getJSON(t, h, "/api/maintenance/worker-node-state")
	if nodes, ok := out["nodes"].([]any); !ok || len(nodes) != 0 {
		t.Fatal("no workers, no nodes", out)
	}
}

// The node state names "builds" as the hold only for a lease its worker reports.
func TestWorkerNodeStateHeldByBuildsComesFromTheReportedLease(t *testing.T) {
	s, h, _ := twoWorkerUI(t)
	reportLeases(t, s, runningLease("lease1"))
	got := nodeStateByName(t, h)
	if got["m1"]["held_by"] != "builds" || got["m4"]["held_by"] != nil {
		t.Fatal("a reported lease holds its node only", got)
	}
}
