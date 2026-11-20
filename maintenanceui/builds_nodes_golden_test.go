package maintenanceui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/nodes"
	"github.com/jakenesler/navigatorr/transcode"
)

// referenceBuildsNodes is GET /builds/nodes as it was computed before the route
// moved into package nodes: from the engine's stored pause states with the
// builds attached. The new answer must match it byte for byte on a healthy
// registry.
func referenceBuildsNodes(s *Server, r *http.Request, want string) (int, string) {
	type runningBuild struct {
		ID        string `json:"id"`
		Source    string `json:"source"`
		Label     string `json:"label"`
		StartedAt string `json:"started_at"`
	}
	type nodeState struct {
		Node            string         `json:"node"`
		TranscodePaused bool           `json:"transcode_paused"`
		TranscodeMode   string         `json:"transcode_mode"`
		BuildsPaused    bool           `json:"builds_paused"`
		RunningBuilds   []runningBuild `json:"running_builds"`
	}
	statuses := s.engine.AttachNodeBuilds(s.engine.NodePauseStatesStored(r.Context()))
	out := make([]nodeState, 0, len(statuses))
	for name, st := range statuses {
		if want != "" && name != want {
			continue
		}
		n := nodeState{Node: name, TranscodePaused: st.Paused, TranscodeMode: st.Mode, BuildsPaused: st.BuildsPaused, RunningBuilds: []runningBuild{}}
		for _, b := range st.Builds {
			if b.Status == nodes.BuildRunning {
				n.RunningBuilds = append(n.RunningBuilds, runningBuild{ID: b.ID, Source: b.Source, Label: b.Title, StartedAt: b.StartedAt})
			}
		}
		out = append(out, n)
	}
	if want != "" && len(out) == 0 {
		return 404, `{"error":"unknown node ` + want + `"}` + "\n"
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]any{"nodes": out})
	return 200, buf.String()
}

func TestBuildsNodesMatchesTheEngineReading(t *testing.T) {
	s, h := buildsUI(t)
	node := transcode.DefaultWorkerName
	const path = "/api/maintenance/builds"
	same := func(step string) {
		t.Helper()
		for _, q := range []string{"", "?node=" + node, "?node=nope"} {
			want := strings.TrimPrefix(q, "?node=")
			code, body := referenceBuildsNodes(s, httptestRequest(), want)
			w := withBuildsToken(h, "GET", path+"/nodes"+q, "")
			if w.Code != code || w.Body.String() != body {
				t.Fatalf("%s %q:\n got %d %s\nwant %d %s", step, q, w.Code, w.Body.String(), code, body)
			}
		}
	}
	same("idle")
	b := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"`+node+`","via":"node_lease","source":"ci","title":"compile"}`))
	same("a running build")
	postBuildsPause(t, h, node, true)
	same("builds paused")
	postBuildsPause(t, h, node, false)
	if w := request(h, "POST", pausePath, `{"node":"`+node+`","mode":"graceful"}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	same("transcode paused, build running")
	postBuildsPause(t, h, node, true)
	same("both paused")
	withBuildsToken(h, "POST", path+"/"+b.ID+"/finish", `{"status":"succeeded"}`)
	same("build finished")
}

func TestBuildsNodesMatchesTheEngineReadingWithTwoNodes(t *testing.T) {
	h := twoNodeBuildsUI(t)
	const path = "/api/maintenance/builds"
	decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"m1","via":"node_lease","source":"ci","title":"first"}`))
	decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"m4","via":"node_lease","source":"ci","title":"second"}`))
	w := withBuildsToken(h, "GET", path+"/nodes", "")
	var got struct {
		Nodes []struct {
			Node string `json:"node"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Nodes) != 2 || got.Nodes[0].Node != "m1" || got.Nodes[1].Node != "m4" {
		t.Fatal(w.Body.String())
	}
}

func httptestRequest() *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	return r
}

// Without a registry every builds route is a 503, the machine picker included
// (it answers from the registry now); never 200 with "builds paused".
func TestBuildsRoutesAre503WithoutARegistry(t *testing.T) {
	s, _ := buildsUI(t)
	s.SetNodes(nil)
	h := s.Handler()
	const path = "/api/maintenance/builds"
	for _, c := range [][2]string{
		{"GET", path}, {"POST", path}, {"GET", path + "/nodes"}, {"GET", path + "/nodes?node=x"},
		{"GET", path + "/bld_x"}, {"POST", path + "/bld_x/heartbeat"}, {"GET", path + "/bld_x/log"}, {"POST", path + "/bld_x/finish"},
	} {
		w := withBuildsToken(h, c[0], c[1], `{}`)
		if w.Code != 503 || strings.TrimSpace(w.Body.String()) != `{"error":"builds are not available"}` || strings.Contains(w.Body.String(), "builds_paused") {
			t.Fatal(c, w.Code, w.Body.String())
		}
	}
	if w := request(h, "GET", path+"/nodes", "", false); w.Code != 401 {
		t.Fatal("auth still comes first", w.Code)
	}
}

// A registry that cannot be read is a 503 through the server too (N3).
func TestBuildsNodesUnreadableRegistryIs503(t *testing.T) {
	s, h := buildsUI(t)
	postBuildsPause(t, h, transcode.DefaultWorkerName, true)
	if err := s.nodes.Close(); err != nil {
		t.Fatal(err)
	}
	w := withBuildsToken(h, "GET", "/api/maintenance/builds/nodes", "")
	if w.Code != 503 || strings.Contains(w.Body.String(), "builds_paused") {
		t.Fatal(w.Code, w.Body.String())
	}
}
