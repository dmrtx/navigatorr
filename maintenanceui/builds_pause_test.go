package maintenanceui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

const pausePath = "/api/maintenance/worker-pause"

func postBuildsPause(t *testing.T, h http.Handler, node string, paused bool) transcode.NodePauseStatus {
	t.Helper()
	flag := "false"
	if paused {
		flag = "true"
	}
	w := request(h, "POST", pausePath, `{"node":"`+node+`","builds_paused":`+flag+`}`, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var st transcode.NodePauseStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err, w.Body.String())
	}
	return st
}

func TestBuildsPausedRefusesCreateWith409AndACode(t *testing.T) {
	s, h := buildsUI(t)
	node := transcode.DefaultWorkerName
	const path = "/api/maintenance/builds"
	create := func(target string) *httptest.ResponseRecorder {
		return withBuildsToken(h, "POST", path, `{"target":"`+target+`","source":"ci","title":"run"}`)
	}
	running := decodeBuild(t, create(node))
	if running.ID == "" {
		t.Fatal("a build before the pause is accepted")
	}

	if st := postBuildsPause(t, h, node, true); !st.BuildsPaused || st.HeldBy == "operator" {
		t.Fatalf("the answer carries the flag and no operator pause: %+v", st)
	}
	w := create(node)
	var refusal map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if w.Code != 409 || refusal["error"] != "builds paused on "+node || refusal["code"] != "builds_paused" {
		t.Fatal("a build on a builds-paused node is a 409 with its code", w.Code, w.Body.String())
	}
	if list, _ := s.engine.ListBuilds(store.BuildFilter{}); len(list) != 1 {
		t.Fatal("a refused create records nothing", list)
	}
	// A non-node target is unaffected; a bad request is still a 400.
	if w := create("laptop"); w.Code != 200 {
		t.Fatal("a non-node target is accepted", w.Code, w.Body.String())
	}
	if w := withBuildsToken(h, "POST", path, `{"target":"`+node+`","source":"ci"}`); w.Code != 400 {
		t.Fatal("validation still comes first", w.Code)
	}
	// The registered build carries on: heartbeat, then finish.
	if w := withBuildsToken(h, "POST", path+"/"+running.ID+"/heartbeat", `{}`); w.Code != 200 {
		t.Fatal("a registered build still heartbeats", w.Code, w.Body.String())
	}
	if got := decodeBuild(t, withBuildsToken(h, "GET", path+"/"+running.ID, "")); got.Status != store.BuildRunning {
		t.Fatalf("pausing builds does not touch a running build: %+v", got)
	}

	// Resume: accepted again, and the running build is untouched.
	if st := postBuildsPause(t, h, node, false); st.BuildsPaused {
		t.Fatalf("%+v", st)
	}
	if w := create(node); w.Code != 200 {
		t.Fatal("resumed", w.Code, w.Body.String())
	}
	if got := decodeBuild(t, withBuildsToken(h, "GET", path+"/"+running.ID, "")); got.Status != store.BuildRunning {
		t.Fatalf("resuming does not touch a running build: %+v", got)
	}
}

func TestBuildsTokenCannotToggleTheBuildsPause(t *testing.T) {
	s, h := buildsUI(t)
	node := transcode.DefaultWorkerName
	body := `{"node":"` + node + `","builds_paused":true}`
	// The way a script uses the token: a bearer, no same-origin header.
	if w := withBuildsToken(h, "POST", pausePath, body); w.Code != 403 {
		t.Fatal("the builds token is not accepted on the toggle (no header)", w.Code, w.Body.String())
	}
	// With the header too, the token is still no session.
	r := httptest.NewRequest("POST", pausePath, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testBuildsToken)
	r.Header.Set("X-Navigatorr-Request", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("the builds token must not toggle the builds pause", w.Code, w.Body.String())
	}
	if s.engine.NodeBuildsPaused(node) {
		t.Fatal("nothing was paused")
	}
	// The web session can.
	if request(h, "POST", pausePath, body, true).Code != 200 || !s.engine.NodeBuildsPaused(node) {
		t.Fatal("the web session toggles it")
	}
	// And a session without the header cannot, like the transcode pause.
	r = httptest.NewRequest("POST", pausePath, strings.NewReader(`{"node":"`+node+`","builds_paused":false}`))
	r.Header.Set("Authorization", "Bearer "+testToken)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 || !s.engine.NodeBuildsPaused(node) {
		t.Fatal("the same-origin header is required", w.Code)
	}
}

func TestWorkerPauseRequestsForTheBuildsSwitch(t *testing.T) {
	s, h := testUI(t)
	node := transcode.DefaultWorkerName
	for _, bad := range []string{
		`{"node":"` + node + `","builds_paused":"yes"}`,
		`{"builds_paused":true}`,
		// One request, one switch: never half applied.
		`{"node":"` + node + `","mode":"graceful","builds_paused":true}`,
	} {
		if w := request(h, "POST", pausePath, bad, true); w.Code != 400 {
			t.Fatal("must be refused", bad, w.Code)
		}
	}
	if w := request(h, "POST", pausePath, `{"node":"nowhere","builds_paused":true}`, true); w.Code != 404 {
		t.Fatal("an unknown node is a 404", w.Code)
	}
	if pauses, _ := s.engine.Deps().Store.ListBuildPauses(); len(pauses) != 0 {
		t.Fatal("a refused request pauses nothing", pauses)
	}

	// The two pauses are independent through the endpoint too.
	postBuildsPause(t, h, node, true)
	w := request(h, "POST", pausePath, `{"node":"`+node+`","mode":"graceful"}`, true)
	var st transcode.NodePauseStatus
	if json.Unmarshal(w.Body.Bytes(), &st) != nil || !st.Paused || !st.BuildsPaused || st.Mode != "graceful" {
		t.Fatal("a transcode pause leaves the builds pause alone", w.Code, w.Body.String())
	}
	w = request(h, "POST", pausePath, `{"node":"`+node+`","mode":"off"}`, true)
	if json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Paused || !st.BuildsPaused {
		t.Fatal("a transcode resume leaves the builds pause alone", w.Body.String())
	}
	if st := postBuildsPause(t, h, node, false); st.BuildsPaused || st.Paused {
		t.Fatalf("%+v", st)
	}

	// Every place that shows node pause state carries the flag.
	postBuildsPause(t, h, node, true)
	var one transcode.NodePauseStatus
	w = request(h, "GET", pausePath+"?node="+node, "", true)
	if json.Unmarshal(w.Body.Bytes(), &one) != nil || !one.BuildsPaused || one.BuildsPausedSince == "" {
		t.Fatal("GET worker-pause?node=", w.Body.String())
	}
	var all struct {
		Nodes map[string]transcode.NodePauseStatus `json:"nodes"`
	}
	w = request(h, "GET", pausePath, "", true)
	if json.Unmarshal(w.Body.Bytes(), &all) != nil || !all.Nodes[node].BuildsPaused {
		t.Fatal("GET worker-pause", w.Body.String())
	}
	var list struct {
		Nodes []struct {
			Pause *transcode.NodePauseStatus `json:"pause"`
		} `json:"nodes"`
		Observation transcode.WorkerObservation `json:"observation"`
	}
	w = request(h, "GET", "/api/maintenance/workers", "", true)
	if json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Nodes) == 0 || list.Nodes[0].Pause == nil || !list.Nodes[0].Pause.BuildsPaused {
		t.Fatal("workers must carry each node's flag", w.Body.String())
	}
	if len(list.Observation.Workers) == 0 || !list.Observation.Workers[0].BuildsPaused {
		t.Fatal("the observation carries it too", w.Body.String())
	}
	var activity struct {
		Workers []struct {
			BuildsPaused bool `json:"builds_paused"`
		} `json:"workers"`
	}
	w = request(h, "GET", "/api/maintenance/worker-activity", "", true)
	if json.Unmarshal(w.Body.Bytes(), &activity) != nil || len(activity.Workers) == 0 || !activity.Workers[0].BuildsPaused {
		t.Fatal("worker activity (the slots) carries it", w.Body.String())
	}
}

// The mode and the builds switch are never in one request: presence decides,
// whatever the mode's value is.
func TestWorkerPauseRefusesModeAndBuildsPausedTogetherByPresence(t *testing.T) {
	s, h := testUI(t)
	node := transcode.DefaultWorkerName
	for _, bad := range []string{
		`{"node":"` + node + `","mode":"","builds_paused":true}`,
		`{"node":"` + node + `","mode":"off","builds_paused":false}`,
		`{"node":"` + node + `","mode":"graceful","builds_paused":true}`,
	} {
		if w := request(h, "POST", pausePath, bad, true); w.Code != 400 {
			t.Fatal("must be a 400", bad, w.Code, w.Body.String())
		}
	}
	if s.engine.NodeBuildsPaused(node) {
		t.Fatal("a refused request changes nothing")
	}
	if pauses, _ := s.engine.Deps().Store.ListWorkerPauses(); len(pauses) != 0 {
		t.Fatal(pauses)
	}
}
