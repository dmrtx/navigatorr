package maintenanceui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/tools"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
	"github.com/mark3labs/mcp-go/server"
)

// twoNodeBuildsUI is buildsUI with two configured transcode nodes, "m4" listed
// before "m1" so a sorted answer is not just config order.
func twoNodeBuildsUI(t *testing.T) http.Handler {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	worker := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(worker.Close)
	tokenPath := filepath.Join(t.TempDir(), "builds-token")
	if err := os.WriteFile(tokenPath, []byte(testBuildsToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Web: config.WebConfig{Enabled: true, Token: testToken, BuildsTokenFile: tokenPath}, Media: config.MediaConfig{AllowedReadRoots: []string{"/media"}}, Transcode: config.TranscodeConfig{Enabled: true, Executor: "http", HTTP: config.HTTPExecutorConfig{BaseURL: worker.URL, Token: "synthetic-worker-token"}}}
	configs, err := cfg.Transcode.BuildWorkerConfigs()
	if err != nil {
		t.Fatal(err)
	}
	ex, err := httpclient.NewHTTPExecutor(configs[0].HTTP)
	if err != nil {
		t.Fatal(err)
	}
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: "m4", Executor: ex, Podcast: true}, {Name: "m1", Executor: ex}})
	if err != nil {
		t.Fatal(err)
	}
	m := server.NewMCPServer("test", "1")
	reg := arrservice.NewRegistry(cfg)
	e := tools.RegisterMaintenance(m, cfg, reg, nil, st, set)
	s, err := New(cfg, reg, e, m)
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

func TestBuildsNodesTwoNodesKeepTheirOwnBuilds(t *testing.T) {
	h := twoNodeBuildsUI(t)
	const path = "/api/maintenance/builds"
	on1 := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"m1","source":"ci","title":"first"}`))
	on4 := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"m4","source":"ci","title":"second"}`))
	if on1.ID == "" || on4.ID == "" {
		t.Fatal("both builds start")
	}

	all := getNodes(t, withBuildsToken(h, "GET", path+"/nodes", "")).Nodes
	if len(all) != 2 || all[0].Node != "m1" || all[1].Node != "m4" {
		t.Fatalf("sorted by name: %+v", all)
	}
	if len(all[0].RunningBuilds) != 1 || all[0].RunningBuilds[0].ID != on1.ID || len(all[1].RunningBuilds) != 1 || all[1].RunningBuilds[0].ID != on4.ID {
		t.Fatalf("each node lists only its own build: %+v", all)
	}
	one := getNodes(t, withBuildsToken(h, "GET", path+"/nodes?node=m4", "")).Nodes
	if len(one) != 1 || one[0].Node != "m4" || len(one[0].RunningBuilds) != 1 || one[0].RunningBuilds[0].ID != on4.ID {
		t.Fatalf("?node= returns only that node: %+v", one)
	}
	if !sort.SliceIsSorted(all, func(i, j int) bool { return all[i].Node < all[j].Node }) {
		t.Fatal("sorted")
	}
}

func TestBuildsNodesExactJSONKeys(t *testing.T) {
	_, h := buildsUI(t)
	const path = "/api/maintenance/builds"
	decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"`+transcode.DefaultWorkerName+`","source":"ci","title":"compile"}`))

	var body map[string]any
	if err := json.Unmarshal(withBuildsToken(h, "GET", path+"/nodes", "").Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if keys(body) != "nodes" {
		t.Fatal("top-level keys", body)
	}
	nodes := body["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatal(nodes)
	}
	node := nodes[0].(map[string]any)
	if got, want := keys(node), "builds_paused,node,running_builds,transcode_mode,transcode_paused"; got != want {
		t.Fatalf("node keys %s, want %s", got, want)
	}
	running := node["running_builds"].([]any)
	if len(running) != 1 {
		t.Fatal(running)
	}
	b := running[0].(map[string]any)
	if got, want := keys(b), "id,label,source,started_at"; got != want {
		t.Fatalf("build keys %s, want %s", got, want)
	}
	if _, err := time.Parse(time.RFC3339, b["started_at"].(string)); err != nil {
		t.Fatal("started_at is RFC3339", b["started_at"])
	}
	if reflect.TypeOf(node["transcode_mode"]).Kind() != reflect.String {
		t.Fatal("transcode_mode is always a string")
	}
}

func keys(m map[string]any) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	s := ""
	for i, k := range out {
		if i > 0 {
			s += ","
		}
		s += k
	}
	return s
}
