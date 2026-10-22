package maintenanceui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// twoWorkerUI configures two nodes: m1 answers every check and reports its
// activity, m4 points at a closed port and only keeps a slot split.
func twoWorkerUI(t *testing.T) (*Server, http.Handler, *fakeSlotWorker) {
	t.Helper()
	s, h := testUI(t)
	online := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ready":
			writeJSON(w, 200, map[string]bool{"ready": true})
		case "/v1/doctor":
			writeJSON(w, 200, map[string]bool{"ok": true})
		case "/v1/activity":
			writeJSON(w, 200, map[string]any{"worker_slots_total": 2, "worker_slots_used": 0, "jobs": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(online.Close)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	m1, err := httpclient.NewHTTPExecutor(httpclient.HTTPConfig{BaseURL: online.URL, Token: "synthetic-worker-token"})
	if err != nil {
		t.Fatal(err)
	}
	offline, err := httpclient.NewHTTPExecutor(httpclient.HTTPConfig{BaseURL: closed.URL, Token: "synthetic-worker-token"})
	if err != nil {
		t.Fatal(err)
	}
	m4 := &fakeSlotWorker{Executor: offline, ceiling: 3, cpus: 10}
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{
		{Name: "m1", Executor: m1, Address: "192.0.2.10", Podcast: true},
		{Name: "m4", Executor: m4, Address: "192.0.2.11"},
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := s.engine.Deps()
	deps.Transcode, deps.Workers = nil, set
	s.engine = action.NewEngine(deps)
	return s, h, m4
}

func getJSON(t *testing.T, h http.Handler, path string) map[string]any {
	t.Helper()
	w := request(h, "GET", path, "", true)
	var out map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal(path, w.Code, w.Body.String())
	}
	return out
}

func TestWorkersListsEachNodeWithOneOffline(t *testing.T) {
	_, h, _ := twoWorkerUI(t)
	out := getJSON(t, h, "/api/maintenance/workers")
	if out["ready"] != true {
		t.Fatal("one ready node must keep the server ready", out)
	}
	nodes, _ := out["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatal("expected one node per worker", out)
	}
	want := map[string]string{"m1": "ready", "m4": "unavailable"}
	for _, raw := range nodes {
		node := raw.(map[string]any)
		name, _ := node["worker"].(string)
		if node["status"] != want[name] {
			t.Fatal("node status", name, node)
		}
	}

	activity := getJSON(t, h, "/api/maintenance/worker-activity")
	if activity["available"] != true {
		t.Fatal("the offline node hid the online node's activity", activity)
	}
	workers, _ := activity["workers"].([]any)
	if len(workers) != 2 {
		t.Fatal("expected activity for each worker", activity)
	}
	for _, raw := range workers {
		node := raw.(map[string]any)
		if (node["name"] == "m1") != (node["available"] == true) {
			t.Fatal("activity availability", node)
		}
	}
}

func TestWorkerSlotsAddressOneNode(t *testing.T) {
	s, h, m4 := twoWorkerUI(t)
	if w := request(h, "POST", "/api/maintenance/worker-slots?node=m4", `{"video_slots":2,"podcast_slots":1}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if m4.puts != 1 || m4.lastPush.VideoSlots != 2 {
		t.Fatal("the split did not reach m4", m4.puts, m4.lastPush)
	}
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings("m4"); row == nil || row.VideoSlots != 2 || row.PodcastSlots != 1 {
		t.Fatal("m4 split not stored under its own name", row)
	}
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings("m1"); row != nil {
		t.Fatal("m4 split leaked into m1", row)
	}
	if request(h, "GET", "/api/maintenance/worker-slots?node=m9", "", true).Code != 404 {
		t.Fatal("an unknown node must be refused")
	}
}
