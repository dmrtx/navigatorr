package maintenanceui

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

type nodesAnswer struct {
	Nodes []struct {
		Node            string `json:"node"`
		TranscodePaused bool   `json:"transcode_paused"`
		TranscodeMode   string `json:"transcode_mode"`
		BuildsPaused    bool   `json:"builds_paused"`
		RunningBuilds   []struct {
			ID        string `json:"id"`
			Source    string `json:"source"`
			Label     string `json:"label"`
			StartedAt string `json:"started_at"`
		} `json:"running_builds"`
	} `json:"nodes"`
}

func getNodes(t *testing.T, w *httptest.ResponseRecorder) nodesAnswer {
	t.Helper()
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var out nodesAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err, w.Body.String())
	}
	return out
}

func TestBuildsNodesReportsStateAndRunningBuilds(t *testing.T) {
	_, h := buildsUI(t)
	node := transcode.DefaultWorkerName
	const path = "/api/maintenance/builds"

	idle := getNodes(t, withBuildsToken(h, "GET", path+"/nodes", ""))
	if len(idle.Nodes) != 1 || idle.Nodes[0].Node != node || idle.Nodes[0].TranscodePaused || idle.Nodes[0].TranscodeMode != "" || idle.Nodes[0].BuildsPaused || len(idle.Nodes[0].RunningBuilds) != 0 {
		t.Fatalf("an idle node: %+v", idle)
	}
	if !strings.Contains(withBuildsToken(h, "GET", path+"/nodes", "").Body.String(), `"running_builds":[]`) {
		t.Fatal("running_builds is an empty array, not null")
	}

	// A running build appears; a waiting one does not.
	running := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"`+node+`","source":"ci","title":"compile"}`))
	waiting := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"`+node+`","source":"warm","title":"later","priority":"low"}`))
	if waiting.Status != "waiting" {
		t.Fatal("the second build waits", waiting)
	}
	got := getNodes(t, withBuildsToken(h, "GET", path+"/nodes?node="+node, "")).Nodes[0]
	if len(got.RunningBuilds) != 1 || got.RunningBuilds[0].ID != running.ID || got.RunningBuilds[0].Source != "ci" || got.RunningBuilds[0].Label != "compile" || got.RunningBuilds[0].StartedAt == "" {
		t.Fatalf("only the running build is listed: %+v", got)
	}

	// An ended build leaves the list.
	if w := withBuildsToken(h, "POST", path+"/"+running.ID+"/finish", `{"status":"succeeded"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, b := range getNodes(t, withBuildsToken(h, "GET", path+"/nodes", "")).Nodes[0].RunningBuilds {
		if b.ID == running.ID {
			t.Fatal("an ended build is not running")
		}
	}

	// The literal route does not shadow a real build id.
	if w := withBuildsToken(h, "GET", path+"/"+waiting.ID, ""); w.Code != 200 || decodeBuild(t, w).ID != waiting.ID {
		t.Fatal("/builds/{id} still works", w.Code, w.Body.String())
	}
}

func TestBuildsNodesPausesAndFilter(t *testing.T) {
	_, h := buildsUI(t)
	node := transcode.DefaultWorkerName
	const path = "/api/maintenance/builds/nodes"

	postBuildsPause(t, h, node, true)
	got := getNodes(t, withBuildsToken(h, "GET", path, "")).Nodes[0]
	if !got.BuildsPaused || got.TranscodePaused || got.TranscodeMode != "" {
		t.Fatalf("builds paused only: %+v", got)
	}
	postBuildsPause(t, h, node, false)

	if w := request(h, "POST", pausePath, `{"node":"`+node+`","mode":"graceful"}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	got = getNodes(t, withBuildsToken(h, "GET", path, "")).Nodes[0]
	if !got.TranscodePaused || got.TranscodeMode != "graceful" || got.BuildsPaused {
		t.Fatalf("transcode paused only: %+v", got)
	}
	// The same effective mode worker-pause reports.
	var wp transcode.NodePauseStatus
	if err := json.Unmarshal(request(h, "GET", pausePath+"?node="+node, "", true).Body.Bytes(), &wp); err != nil || wp.Mode != got.TranscodeMode || wp.Paused != got.TranscodePaused {
		t.Fatal("matches worker-pause", err, wp)
	}

	if n := getNodes(t, withBuildsToken(h, "GET", path+"?node="+node, "")).Nodes; len(n) != 1 || n[0].Node != node {
		t.Fatal("filter by node", n)
	}
	w := withBuildsToken(h, "GET", path+"?node=nope", "")
	if w.Code != 404 || strings.TrimSpace(w.Body.String()) != `{"error":"unknown node nope"}` {
		t.Fatal("unknown node", w.Code, w.Body.String())
	}
}

func TestBuildsNodesAuthAndMethods(t *testing.T) {
	_, h := buildsUI(t)
	const path = "/api/maintenance/builds/nodes"
	if w := withBuildsToken(h, "GET", path, ""); w.Code != 200 {
		t.Fatal("builds token reads it", w.Code)
	}
	if w := withBuildsToken(h, "GET", "/api/maintenance/worker-pause", ""); w.Code != 401 {
		t.Fatal("the builds token is still refused elsewhere", w.Code)
	}
	if w := request(h, "GET", path, "", true); w.Code != 200 {
		t.Fatal("the web token reads it", w.Code)
	}
	if w := request(h, "GET", path, "", false); w.Code != 401 {
		t.Fatal("no credentials", w.Code)
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if w := withBuildsToken(h, m, path, "{}"); w.Code != 405 {
			t.Fatal(m, "is a 405", w.Code, w.Body.String())
		}
	}
}
