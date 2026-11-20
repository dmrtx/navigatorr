package nodes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const lp = LegacyBasePath

func legacyCall(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type legacyNodesBody struct {
	Nodes []struct {
		Node            string `json:"node"`
		TranscodePaused bool   `json:"transcode_paused"`
		TranscodeMode   string `json:"transcode_mode"`
		BuildsPaused    bool   `json:"builds_paused"`
		RunningBuilds   []struct {
			ID, Source, Label string
			StartedAt         string `json:"started_at"`
		} `json:"running_builds"`
	} `json:"nodes"`
}

func legacyNodes(t *testing.T, h http.Handler, query string) legacyNodesBody {
	t.Helper()
	w := legacyCall(h, "GET", lp+"/nodes"+query, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var out legacyNodesBody
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err, w.Body.String())
	}
	return out
}

// With a view: the transcode fields come from it, the builds fields from the
// registry, and the wire shape is the one nbuild and the workers read.
func TestLegacyNodesJoinsRegistryAndView(t *testing.T) {
	s, _ := buildSvc(t)
	s.tv = fakeTranscode{"nodeA": {Paused: true, Mode: "graceful", HeldBy: "operator"}}
	h := s.LegacyHandler()
	b := startBuild(t, s, "nodeB", "graceful", 0)
	pauseBuilds(t, s, "nodeA", true)

	w := legacyCall(h, "GET", lp+"/nodes", "")
	want := `{"nodes":[` +
		`{"node":"nodeA","transcode_paused":true,"transcode_mode":"graceful","builds_paused":true,"running_builds":[]},` +
		`{"node":"nodeB","transcode_paused":false,"transcode_mode":"","builds_paused":false,"running_builds":[` +
		`{"id":"` + b.ID + `","source":"ci-tool","label":"integration run","started_at":"` + b.StartedAt + `"}]}]}`
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("code %d\n got %s\nwant %s", w.Code, w.Body.String(), want)
	}
	if one := legacyNodes(t, h, "?node=nodeB"); len(one.Nodes) != 1 || one.Nodes[0].Node != "nodeB" {
		t.Fatal(one)
	}
	w = legacyCall(h, "GET", lp+"/nodes?node=nope", "")
	if w.Code != 404 || strings.TrimSpace(w.Body.String()) != `{"error":"unknown node nope"}` {
		t.Fatal(w.Code, w.Body.String())
	}
}

// A missing view, an empty one, or one that does not know a node leave the
// transcode fields at their zero values; the builds fields stay the registry's.
func TestLegacyNodesWithoutAView(t *testing.T) {
	for name, view := range map[string]TranscodeView{"nil": nil, "empty": fakeTranscode{}, "other node": fakeTranscode{"nodeZ": {Paused: true, Mode: "suspend"}}} {
		t.Run(name, func(t *testing.T) {
			s, _ := buildSvc(t)
			s.tv = view
			h := s.LegacyHandler()
			b := startBuild(t, s, "nodeA", "graceful", 0)
			pauseBuilds(t, s, "nodeB", true)
			got := legacyNodes(t, h, "")
			if len(got.Nodes) != 2 {
				t.Fatal(got)
			}
			a, bn := got.Nodes[0], got.Nodes[1]
			if a.TranscodePaused || a.TranscodeMode != "" || bn.TranscodePaused || bn.TranscodeMode != "" {
				t.Fatalf("no transcode state without a view: %+v", got)
			}
			if a.BuildsPaused || len(a.RunningBuilds) != 1 || a.RunningBuilds[0].ID != b.ID || !bn.BuildsPaused || len(bn.RunningBuilds) != 0 {
				t.Fatalf("builds state still comes from the registry: %+v", got)
			}
		})
	}
}

// N3: a registry that cannot be read is a 503, never "builds paused".
func TestLegacyNodesUnreadableRegistryIs503(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.LegacyHandler()
	pauseBuilds(t, s, "nodeA", true)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"", "?node=nodeA"} {
		w := legacyCall(h, "GET", lp+"/nodes"+q, "")
		if w.Code != 503 || strings.Contains(w.Body.String(), "builds_paused") || strings.TrimSpace(w.Body.String()) != `{"error":"builds are not available"}` {
			t.Fatalf("%q: %d %s", q, w.Code, w.Body.String())
		}
	}
}

// No topology at all: no node, so an empty list (and 404 for a name).
func TestLegacyNodesWithoutTopology(t *testing.T) {
	s, err := Open(Options{Path: t.TempDir() + "/nodes.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h := s.LegacyHandler()
	if w := legacyCall(h, "GET", lp+"/nodes", ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"nodes":[]}` {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := legacyCall(h, "GET", lp+"/nodes?node=x", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestLegacyErrorCodesAndBodies(t *testing.T) {
	s, clock := buildSvc(t)
	h := s.LegacyHandler()
	create := func(body string) *httptest.ResponseRecorder { return legacyCall(h, "POST", lp, body) }

	// 400: unreadable body, unknown field, invalid value.
	for _, body := range []string{`{`, `{"nope":1}`, `{"target":"nodeA"} {}`} {
		if w := create(body); w.Code != 400 || !strings.Contains(w.Body.String(), `"error":"send {\"target\"`) {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	if w := create(`{"target":"nodeA","source":"ci","title":"t","priority":"urgent"}`); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := legacyCall(h, "GET", lp+"?limit=0", ""); w.Code != 400 || strings.TrimSpace(w.Body.String()) != `{"error":"limit must be between 1 and 1000"}` {
		t.Fatal(w.Code, w.Body.String())
	}
	// Body size limit: 256 KiB on create and finish, HeartbeatBodyMax on heartbeat.
	huge := `{"target":"nodeA","source":"ci","title":"` + strings.Repeat("x", 300<<10) + `"}`
	if w := create(huge); w.Code != 400 {
		t.Fatal("oversized create", w.Code)
	}

	// 200, then 409 for the id taken and for a pause, 404 for an unknown build.
	ok := create(`{"id":"bld_job00001","target":"nodeA","source":"ci","title":"t","via":"node_lease"}`)
	if ok.Code != 200 {
		t.Fatal(ok.Code, ok.Body.String())
	}
	if w := create(`{"id":"bld_job00001","target":"nodeB","source":"ci","title":"t","via":"node_lease"}`); w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"build_id_taken"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	pauseBuilds(t, s, "nodeB", true)
	if w := create(`{"target":"nodeB","source":"ci","title":"t","via":"node_lease"}`); w.Code != 409 || strings.TrimSpace(w.Body.String()) != `{"code":"builds_paused","error":"builds paused on nodeB"}` {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := legacyCall(h, "GET", lp+"/nope", ""); w.Code != 404 || strings.TrimSpace(w.Body.String()) != `{"error":"build not found"}` {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := legacyCall(h, "POST", lp+"/bld_job00001/finish", strings.Repeat(" ", 300<<10)+"{}"); w.Code != 400 {
		t.Fatal("oversized finish", w.Code)
	}
	if w := legacyCall(h, "POST", lp+"/bld_job00001/heartbeat", `{"log":[1]}`); w.Code != 400 || !strings.Contains(w.Body.String(), "log must be an array of strings") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := legacyCall(h, "POST", lp+"/bld_job00001/heartbeat", ""); w.Code != 200 {
		t.Fatal("an empty heartbeat is fine", w.Code, w.Body.String())
	}

	// 409 on a heartbeat to an ended build carries the record; finishing again is 200.
	if w := legacyCall(h, "POST", lp+"/bld_job00001/finish", `{"status":"cancelled"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	clock.advance(time.Second)
	w := legacyCall(h, "POST", lp+"/bld_job00001/heartbeat", `{}`)
	var ended struct {
		Error string `json:"error"`
		Build Build  `json:"build"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ended); err != nil || w.Code != 409 || ended.Error != "build already ended" || ended.Build.Status != "cancelled" {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := legacyCall(h, "POST", lp+"/bld_job00001/finish", `{"status":"succeeded"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"cancelled"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := legacyCall(h, "GET", lp+"/bld_job00001/log", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"expired":false`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
