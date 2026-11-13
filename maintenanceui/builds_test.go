package maintenanceui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

const testBuildsToken = "synthetic-builds-token-0123456789"

// buildsUI is the test server with a builds token configured.
func buildsUI(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	base, _ := testUI(t)
	path := filepath.Join(t.TempDir(), "builds-token")
	if err := os.WriteFile(path, []byte(testBuildsToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := *base.cfg
	cfg.Web.BuildsTokenFile = path
	s, err := New(&cfg, base.registry, base.engine, base.mcp)
	if err != nil {
		t.Fatal(err)
	}
	return s, s.Handler()
}

// withBuildsToken sends a request the way a plain curl script does: a bearer
// and no same-origin header.
func withBuildsToken(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testBuildsToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeBuild(t *testing.T, w *httptest.ResponseRecorder) store.Build {
	t.Helper()
	var b store.Build
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err, w.Body.String())
	}
	return b
}

func TestBuildsEndpointsLifecycle(t *testing.T) {
	s, h := buildsUI(t)
	node := transcode.DefaultWorkerName
	const path = "/api/maintenance/builds"

	for _, bad := range []string{``, `nope`, `{}`, `{"target":"` + node + `","source":"ci","title":"x","extra":1}`,
		`{"target":"bad name","source":"ci","title":"x"}`, `{"target":"` + node + `","source":"ci","title":"x","preempt_mode":"sideways"}`,
		`{"target":"` + node + `","source":"ci","title":"x","priority":"urgent"}`, `{"target":"` + node + `","source":"ci"}`, `{"target":"` + node + `","source":"ci","title":"x","ttl_seconds":-5}`} {
		if w := request(h, "POST", path, bad, true); w.Code != 400 {
			t.Fatalf("%q must be a 400, got %d %s", bad, w.Code, w.Body.String())
		}
	}
	if list, _ := s.engine.ListBuilds(store.BuildFilter{}); len(list) != 0 {
		t.Fatal("a refused request records nothing", list)
	}

	// A machine that is not a transcode node is tracked, not refused.
	w := withBuildsToken(h, "POST", path, `{"target":"laptop","source":"mdm-stack","holder":"runner1","kind":"gradle","title":"unit run"}`)
	other := decodeBuild(t, w)
	if w.Code != 200 || other.ID == "" || other.HoldsTranscodeNode || other.Status != store.BuildRunning || !strings.Contains(w.Body.String(), `"holds_transcode_node":false`) {
		t.Fatal("a non-node target", w.Code, w.Body.String())
	}
	if s.engine.NodePauseStatuses(t.Context())[node].Paused {
		t.Fatal("a build on another machine holds no node")
	}

	w = withBuildsToken(h, "POST", path, `{"target":"`+node+`","source":"mdm-stack","holder":"runner1","kind":"integration-test","title":"enrollment run","ttl_seconds":600,"progress":"starting"}`)
	b := decodeBuild(t, w)
	if w.Code != 200 || !b.HoldsTranscodeNode || b.Status != store.BuildRunning || b.PreemptMode != "suspend" || b.Priority != "normal" || b.Progress != "starting" || b.ExpiresAt == "" || b.StartedAt != b.CreatedAt {
		t.Fatal("create on a node", w.Code, w.Body.String())
	}
	if st := s.engine.NodePauseStatuses(t.Context())[node]; !st.Paused || st.HeldBy != "builds" || len(st.Builds) != 1 {
		t.Fatalf("the node is held: %+v", st)
	}

	one := decodeBuild(t, withBuildsToken(h, "GET", path+"/"+b.ID, ""))
	if one.ID != b.ID || one.Title != "enrollment run" {
		t.Fatal(one)
	}
	if w := withBuildsToken(h, "GET", path+"/bld_missing", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}

	w = withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", `{"progress":"step 3","ttl_seconds":900}`)
	hb := decodeBuild(t, w)
	if w.Code != 200 || hb.Progress != "step 3" || hb.Status != store.BuildRunning {
		t.Fatal("heartbeat", w.Code, w.Body.String())
	}
	if w := withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", ``); w.Code != 200 {
		t.Fatal("a heartbeat may carry no body", w.Code, w.Body.String())
	}
	if w := withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", `{"ttl_seconds":-1}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := withBuildsToken(h, "POST", path+"/bld_missing/heartbeat", ``); w.Code != 404 {
		t.Fatal(w.Code)
	}

	var list struct {
		Builds []store.Build `json:"builds"`
	}
	w = withBuildsToken(h, "GET", path+"?target="+node+"&status=running&source=mdm-stack&limit=5", "")
	if json.Unmarshal(w.Body.Bytes(), &list) != nil || w.Code != 200 || len(list.Builds) != 1 || list.Builds[0].ID != b.ID {
		t.Fatal("filtered list", w.Code, w.Body.String())
	}
	w = withBuildsToken(h, "GET", path, "")
	if json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Builds) != 2 {
		t.Fatal("list", w.Body.String())
	}
	if w := withBuildsToken(h, "GET", path+"?limit=0", ""); w.Code != 400 {
		t.Fatal(w.Code)
	}

	for _, bad := range []string{``, `{}`, `{"status":"running"}`, `{"status":"succeeded","extra":1}`} {
		if w := withBuildsToken(h, "POST", path+"/"+b.ID+"/finish", bad); w.Code != 400 {
			t.Fatalf("finish %q must be a 400, got %d", bad, w.Code)
		}
	}
	w = withBuildsToken(h, "POST", path+"/"+b.ID+"/finish", `{"status":"failed","exit_code":2}`)
	done := decodeBuild(t, w)
	if w.Code != 200 || done.Status != store.BuildFailed || done.EndedAt == "" || done.ExitCode == nil || *done.ExitCode != 2 {
		t.Fatal("finish", w.Code, w.Body.String())
	}
	if s.engine.NodePauseStatuses(t.Context())[node].Paused {
		t.Fatal("the node resumes when the build ends")
	}
	// Finishing again answers 200 with the build unchanged; a heartbeat is a 409.
	w = withBuildsToken(h, "POST", path+"/"+b.ID+"/finish", `{"status":"succeeded"}`)
	if again := decodeBuild(t, w); w.Code != 200 || again.Status != store.BuildFailed {
		t.Fatal("finish twice", w.Code, w.Body.String())
	}
	if w := withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", ``); w.Code != 409 || !strings.Contains(w.Body.String(), `"status":"failed"`) {
		t.Fatal("a heartbeat after the end", w.Code, w.Body.String())
	}
}

func TestBuildsLowPriorityWaitsOnTheNodeThenRuns(t *testing.T) {
	s, h := buildsUI(t)
	node := transcode.DefaultWorkerName
	const path = "/api/maintenance/builds"
	normal := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"`+node+`","source":"ci","title":"build"}`))
	w := withBuildsToken(h, "POST", path, `{"target":"`+node+`","source":"warm","title":"warm","priority":"low"}`)
	low := decodeBuild(t, w)
	if low.Status != store.BuildWaiting || low.StartedAt != "" || low.Priority != "low" || !low.HoldsTranscodeNode {
		t.Fatal("a low priority build waits behind a running one", w.Body.String())
	}
	if hb := decodeBuild(t, withBuildsToken(h, "POST", path+"/"+low.ID+"/heartbeat", ``)); hb.Status != store.BuildWaiting {
		t.Fatal("the poll shows waiting", hb)
	}
	withBuildsToken(h, "POST", path+"/"+normal.ID+"/finish", `{"status":"succeeded"}`)
	if hb := decodeBuild(t, withBuildsToken(h, "POST", path+"/"+low.ID+"/heartbeat", ``)); hb.Status != store.BuildRunning || hb.StartedAt == "" {
		t.Fatal("the poll shows running once the node is idle", hb)
	}
	// Elsewhere a low priority build starts at once.
	other := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"laptop","source":"warm","title":"warm","priority":"low"}`))
	if other.Status != store.BuildRunning || other.StartedAt != other.CreatedAt || other.HoldsTranscodeNode {
		t.Fatal(other)
	}
	_ = s
}

// The builds token reaches the build routes and nothing else.
func TestBuildsTokenCallsOnlyTheBuildEndpoints(t *testing.T) {
	_, h := buildsUI(t)
	denied := []struct{ method, path, body string }{
		{"GET", "/api/maintenance/bootstrap", ""},
		{"GET", "/api/maintenance/operations", ""},
		{"GET", "/api/maintenance/workers", ""},
		{"GET", "/api/maintenance/worker-activity", ""},
		{"GET", "/api/maintenance/worker-pause", ""},
		{"POST", "/api/maintenance/worker-pause", `{"node":"default","mode":"suspend"}`},
		{"POST", "/api/maintenance/worker-drain", `{"enabled":true}`},
		{"POST", "/api/maintenance/tool", `{"name":"action_list","arguments":{}}`},
		{"POST", "/api/maintenance/tool", `{"name":"build_list","arguments":{}}`},
		{"GET", "/api/maintenance/preferences", ""},
		{"GET", "/api/maintenance/logs", ""},
		{"GET", "/api/maintenance/builds/", ""},
		{"GET", "/api/maintenance/builds/x/y/z", ""},
		{"POST", "/api/maintenance/builds/x/other", `{}`},
		{"GET", "/api/maintenance/builds/../worker-pause", ""},
		{"GET", "/api/maintenance/buildsx", ""},
		{"GET", "/api/maintenance/build", ""},
	}
	for _, c := range denied {
		// Without the same-origin header a write stops there (403); with it the
		// token itself is what is refused (401). Either way nothing runs.
		if w := withBuildsToken(h, c.method, c.path, c.body); w.Code != 401 && w.Code != 403 {
			t.Errorf("%s %s with the builds token must be rejected, got %d", c.method, c.path, w.Code)
		}
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		r.Header.Set("Authorization", "Bearer "+testBuildsToken)
		r.Header.Set("X-Navigatorr-Request", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Errorf("%s %s with the builds token and the header must be 401, got %d", c.method, c.path, w.Code)
		}
	}
	// A wrong token, or none, is no better on the build routes.
	r := httptest.NewRequest("GET", "/api/maintenance/builds", nil)
	r.Header.Set("Authorization", "Bearer not-the-builds-token-0000000000")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("a wrong token", w.Code)
	}
	if w := request(h, "GET", "/api/maintenance/builds", "", false); w.Code != 401 {
		t.Fatal("no token", w.Code)
	}
	// The builds token is no web session: it cannot log in with it either.
	if w := request(h, "POST", "/api/maintenance/login", `{"token":"`+testBuildsToken+`"}`, false); w.Code != 401 {
		t.Fatal("login with the builds token", w.Code)
	}
	// Other credentials keep working: the web token reaches the build routes
	// (with the same-origin header the browser sends) and everything else.
	if w := request(h, "GET", "/api/maintenance/builds", "", true); w.Code != 200 {
		t.Fatal("the web token reads builds", w.Code)
	}
	if w := request(h, "POST", "/api/maintenance/builds", `{"target":"laptop","source":"ci","title":"t"}`, true); w.Code != 200 {
		t.Fatal("the web token creates builds", w.Code, w.Body.String())
	}
	if w := request(h, "GET", "/api/maintenance/worker-pause", "", true); w.Code != 200 {
		t.Fatal("the web token still works", w.Code)
	}
	// The web token's writes still need the same-origin header; only the
	// builds bearer is exempt from it.
	r = httptest.NewRequest("POST", "/api/maintenance/builds", strings.NewReader(`{"target":"laptop","source":"ci","title":"t"}`))
	r.Header.Set("Authorization", "Bearer "+testToken)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("the web token without the header", w.Code)
	}
}

func TestBuildsTokenIsOffWithoutAConfiguredFile(t *testing.T) {
	_, h := testUI(t)
	if w := withBuildsToken(h, "GET", "/api/maintenance/builds", ""); w.Code != 401 {
		t.Fatal("no builds token is configured", w.Code)
	}
}

func TestBuildsTokenFileMustHoldALongToken(t *testing.T) {
	base, _ := testUI(t)
	path := filepath.Join(t.TempDir(), "short")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := *base.cfg
	cfg.Web.BuildsTokenFile = path
	if _, err := New(&cfg, base.registry, base.engine, base.mcp); err == nil {
		t.Fatal("a short builds token must be refused")
	}
	cfg.Web.BuildsTokenFile = filepath.Join(t.TempDir(), "missing")
	if _, err := New(&cfg, base.registry, base.engine, base.mcp); err == nil {
		t.Fatal("a missing builds token file must be refused")
	}
}

// The workers list and the worker health read carry the node's builds.
func TestWorkerEndpointsListTheNodesBuilds(t *testing.T) {
	_, h := buildsUI(t)
	node := transcode.DefaultWorkerName
	withBuildsToken(h, "POST", "/api/maintenance/builds", `{"target":"`+node+`","source":"ci","title":"run","holder":"runner1"}`)
	var list struct {
		Nodes []struct {
			Pause *transcode.NodePauseStatus `json:"pause"`
		} `json:"nodes"`
		Observation transcode.WorkerObservation `json:"observation"`
	}
	w := request(h, "GET", "/api/maintenance/workers", "", true)
	if json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Nodes) == 0 || list.Nodes[0].Pause == nil || len(list.Nodes[0].Pause.Builds) != 1 || list.Nodes[0].Pause.HeldBy != "builds" {
		t.Fatal("the node's pause lists its builds", w.Body.String())
	}
	if len(list.Observation.Workers) == 0 || len(list.Observation.Workers[0].Builds) != 1 {
		t.Fatal("the observation lists the node's builds", w.Body.String())
	}
	w = request(h, "GET", "/api/maintenance/worker-activity", "", true)
	if !strings.Contains(w.Body.String(), `"builds"`) {
		t.Fatal("worker activity carries the node's builds", w.Body.String())
	}
}

func TestBuildsLateHeartbeatIs409AndMarksTheBuildLost(t *testing.T) {
	_, h := buildsUI(t)
	node := transcode.DefaultWorkerName
	const path = "/api/maintenance/builds"
	running := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"`+node+`","source":"ci","title":"build","ttl_seconds":1}`))
	waiting := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"`+node+`","source":"warm","title":"warm","priority":"low","ttl_seconds":1}`))
	if running.Status != store.BuildRunning || waiting.Status != store.BuildWaiting {
		t.Fatalf("%+v %+v", running, waiting)
	}
	time.Sleep(2100 * time.Millisecond) // past both windows, before any reconcile tick
	for _, b := range []store.Build{running, waiting} {
		w := withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", `{"ttl_seconds":600}`)
		if w.Code != 409 || !strings.Contains(w.Body.String(), `"status":"lost"`) {
			t.Fatal("a late heartbeat is a 409 with the lost build", w.Code, w.Body.String())
		}
		if got := decodeBuild(t, withBuildsToken(h, "GET", path+"/"+b.ID, "")); got.Status != store.BuildLost || got.ExpiresAt != b.ExpiresAt {
			t.Fatalf("not revived: %+v", got)
		}
	}
}

// The slot view leaves a held node out of the free slots: worker activity says
// what holds each node, and the operator's pause wins over a build.
func TestWorkerActivityNamesWhatHoldsTheNode(t *testing.T) {
	_, h := buildsUI(t)
	node := transcode.DefaultWorkerName
	heldBy := func() (any, bool) {
		w := request(h, "GET", "/api/maintenance/worker-activity", "", true)
		var out struct {
			Workers []map[string]any `json:"workers"`
		}
		if json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Workers) == 0 {
			t.Fatal("worker activity", w.Body.String())
		}
		held, ok := out.Workers[0]["held_by"]
		return held, ok
	}
	if held, ok := heldBy(); ok {
		t.Fatal("a free node has no hold", held)
	}
	b := decodeBuild(t, withBuildsToken(h, "POST", "/api/maintenance/builds", `{"target":"`+node+`","source":"ci","title":"run","holder":"runner1"}`))
	if held, _ := heldBy(); held != "builds" {
		t.Fatal("a running build holds the node", held)
	}
	if w := request(h, "POST", "/api/maintenance/worker-pause", `{"node":"`+node+`","mode":"graceful"}`, true); w.Code != 200 {
		t.Fatal("pause", w.Code, w.Body.String())
	}
	if held, _ := heldBy(); held != "operator" {
		t.Fatal("the operator's pause wins over the build", held)
	}
	withBuildsToken(h, "POST", "/api/maintenance/builds/"+b.ID+"/finish", `{"status":"succeeded"}`)
	if held, _ := heldBy(); held != "operator" {
		t.Fatal("the pause stays after the build ends", held)
	}
	if w := request(h, "POST", "/api/maintenance/worker-pause", `{"node":"`+node+`","mode":"off"}`, true); w.Code != 200 {
		t.Fatal("resume", w.Code, w.Body.String())
	}
	if held, ok := heldBy(); ok {
		t.Fatal("a resumed node with no build is free", held)
	}
}
