package maintenanceui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/nodes"
	"github.com/jakenesler/navigatorr/transcode"
)

func v1(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func apiKey() map[string]string { return map[string]string{"X-Api-Key": testBuildsToken} }

func TestNodesV1Auth(t *testing.T) {
	_, h := buildsUI(t)
	// Public spec.
	if w := v1(h, "GET", "/nodes/v1/openapi.json", "", nil); w.Code != 200 || !json.Valid(w.Body.Bytes()) {
		t.Fatal("the spec is public", w.Code)
	}
	// No credential, wrong credential: 401 JSON with a hint.
	for _, hd := range []map[string]string{nil, {"X-Api-Key": "not.the.builds.key"}, {"Authorization": "Bearer not.the.builds.key"}} {
		w := v1(h, "GET", "/nodes/v1/builds", "", hd)
		var e map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != 401 || e["code"] != "unauthorized" || e["hint"] == "" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	// The builds token, as X-Api-Key (call_api) and as Bearer (legacy scripts).
	if w := v1(h, "GET", "/nodes/v1/builds", "", apiKey()); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := v1(h, "GET", "/nodes/v1/builds", "", map[string]string{"Authorization": "Bearer " + testBuildsToken}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// The builds token reaches nothing else.
	for _, p := range []string{"/api/maintenance/bootstrap", "/api/maintenance/worker-slots"} {
		if w := v1(h, "GET", p, "", apiKey()); w.Code == 200 {
			t.Fatal("the builds token must not open", p)
		}
		if w := v1(h, "GET", p, "", map[string]string{"Authorization": "Bearer " + testBuildsToken}); w.Code == 200 {
			t.Fatal("the builds token must not open", p)
		}
	}
}

// The same scenario through the legacy routes and through /nodes/v1 leaves the
// same stored record (N4: the legacy routes are aliases onto the registry).
func TestLegacyAndV1StoreTheSameBuild(t *testing.T) {
	run := func(legacy bool) nodes.Build {
		s, h := buildsUI(t)
		id := "bld_same0001"
		var create, hb, fin, get func() *httptest.ResponseRecorder
		if legacy {
			base := "/api/maintenance/builds"
			bearer := map[string]string{"Authorization": "Bearer " + testBuildsToken}
			create = func() *httptest.ResponseRecorder {
				return v1(h, "POST", base, `{"id":"`+id+`","target":"laptop","source":"ci","title":"compile","kind":"gradle"}`, bearer)
			}
			hb = func() *httptest.ResponseRecorder {
				return v1(h, "POST", base+"/"+id+"/heartbeat", `{"progress":"half","log":["a","b"]}`, bearer)
			}
			fin = func() *httptest.ResponseRecorder {
				return v1(h, "POST", base+"/"+id+"/finish", `{"status":"succeeded","exit_code":0}`, bearer)
			}
			get = func() *httptest.ResponseRecorder { return v1(h, "GET", base+"/"+id, "", bearer) }
		} else {
			base := "/nodes/v1/builds"
			create = func() *httptest.ResponseRecorder {
				return v1(h, "POST", base, `{"client_id":"`+id+`","target":"laptop","source":"ci","title":"compile","kind":"gradle"}`, apiKey())
			}
			hb = func() *httptest.ResponseRecorder {
				return v1(h, "POST", base+"/"+id+"/heartbeat", `{"progress":"half","log":["a","b"]}`, apiKey())
			}
			fin = func() *httptest.ResponseRecorder {
				return v1(h, "POST", base+"/"+id+"/finish", `{"status":"succeeded","exit_code":0}`, apiKey())
			}
			get = func() *httptest.ResponseRecorder { return v1(h, "GET", base+"/"+id, "", apiKey()) }
		}
		for _, step := range []struct {
			n string
			f func() *httptest.ResponseRecorder
		}{{"create", create}, {"heartbeat", hb}, {"finish", fin}, {"get", get}} {
			if w := step.f(); w.Code != 200 {
				t.Fatalf("legacy=%v %s: %d %s", legacy, step.n, w.Code, w.Body.String())
			}
		}
		b, err := s.nodes.GetBuild(id)
		if err != nil {
			t.Fatal(err)
		}
		_, lines, _, _ := s.nodes.BuildLog(id)
		if len(lines) != 2 {
			t.Fatalf("legacy=%v log: %+v", legacy, lines)
		}
		return b
	}
	a, b := run(true), run(false)
	for _, x := range []*nodes.Build{&a, &b} { // the clock differs between runs
		x.CreatedAt, x.StartedAt, x.LastHeartbeatAt, x.ExpiresAt, x.EndedAt = "", "", "", "", ""
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("legacy %s\n   v1 %s", ja, jb)
	}
}

// N5: a cancel through /nodes/v1 reaches the worker exactly as before: the
// next forwarded heartbeat is answered "build already ended" with a cancelled build.
func TestN5CancelThroughV1ReachesTheNextHeartbeat(t *testing.T) {
	_, h := buildsUI(t)
	bearer := map[string]string{"Authorization": "Bearer " + testBuildsToken}
	if w := v1(h, "POST", "/nodes/v1/builds", `{"client_id":"bld_cancel001","target":"laptop","source":"ci","title":"t"}`, apiKey()); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := v1(h, "POST", "/nodes/v1/builds/bld_cancel001/cancel", "", apiKey()); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w := v1(h, "POST", "/api/maintenance/builds/bld_cancel001/heartbeat", `{}`, bearer)
	var out struct {
		Error string      `json:"error"`
		Build nodes.Build `json:"build"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 409 || out.Error != "build already ended" || out.Build.Status != nodes.BuildCancelled {
		t.Fatal(w.Code, w.Body.String())
	}
}

// GET /nodes/v1/nodes shows the hold the node's worker reports, the same as the
// legacy node view: held_by builds, the strongest mode among the running leases
// and the earliest start, whatever order the worker lists them in. An operator
// pause wins over the display of a lease.
func TestNodesV1ShowsTheWorkerReportedHold(t *testing.T) {
	s, h := buildsUI(t)
	node := transcode.DefaultWorkerName
	early, late := "2026-11-13T10:00:00Z", "2026-11-13T10:05:00Z"
	// Reported newest first: the earliest start and the strongest mode must not
	// depend on list order.
	reportLeases(t, s,
		transcode.WorkerLease{ID: "lease2", Status: "running", Mode: "suspend", StartedAt: late},
		transcode.WorkerLease{ID: "lease1", Status: "running", Mode: "graceful", StartedAt: early},
		transcode.WorkerLease{ID: "lease3", Status: "waiting", Mode: "suspend", StartedAt: "2026-11-13T09:00:00Z"},
	)
	type view struct {
		Name      string `json:"name"`
		Transcode *struct {
			Paused bool   `json:"paused"`
			Mode   string `json:"mode"`
			Since  string `json:"since"`
			HeldBy string `json:"held_by"`
		} `json:"transcode"`
	}
	read := func() view {
		t.Helper()
		w := v1(h, "GET", "/nodes/v1/nodes/"+node, "", apiKey())
		var v view
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Transcode == nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return v
	}
	v := read()
	if !v.Transcode.Paused || v.Transcode.HeldBy != "builds" || v.Transcode.Mode != "suspend" || v.Transcode.Since != early {
		t.Fatalf("the node view shows what the worker reports: %+v", v.Transcode)
	}
	// The list agrees with the single view, and with the legacy route.
	w := v1(h, "GET", "/nodes/v1/nodes", "", apiKey())
	var list struct {
		Nodes []view `json:"nodes"`
	}
	if json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Nodes) == 0 || list.Nodes[0].Transcode == nil || list.Nodes[0].Transcode.HeldBy != "builds" || list.Nodes[0].Transcode.Since != early {
		t.Fatal(w.Body.String())
	}
	w = withBuildsToken(h, "GET", "/api/maintenance/builds/nodes?node="+node, "")
	var legacy struct {
		Nodes []struct {
			TranscodePaused bool   `json:"transcode_paused"`
			TranscodeMode   string `json:"transcode_mode"`
		} `json:"nodes"`
	}
	if json.Unmarshal(w.Body.Bytes(), &legacy) != nil || len(legacy.Nodes) != 1 || !legacy.Nodes[0].TranscodePaused || legacy.Nodes[0].TranscodeMode != v.Transcode.Mode {
		t.Fatalf("the legacy view says the same: %s", w.Body.String())
	}
	// An operator pause wins over the lease hold.
	if w := request(h, "POST", pausePath, `{"node":"`+node+`","mode":"graceful"}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if v := read(); v.Transcode.HeldBy != "operator" {
		t.Fatalf("an operator pause wins: %+v", v.Transcode)
	}
}
