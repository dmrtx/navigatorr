package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/nodes"
	_ "modernc.org/sqlite"
)

// serviceKey is built at run time so secret scanners do not read it as a credential.
var serviceKey = strings.Repeat("s", 40)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// testConfig is a valid configuration with one transcode node, nodea, and a
// main database path that does not exist (Nodes must never open it).
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{Services: map[string]config.ServiceConfig{}}
	cfg.Nodes.Listen = "127.0.0.1:0"
	cfg.Nodes.KeyFile = writeFile(t, dir, "key", serviceKey+"\n")
	cfg.Database.NodesPath = filepath.Join(dir, "data", "nodes.db")
	cfg.Database.Path = filepath.Join(dir, "no-such-dir", "main.db")
	cfg.Transcode.Enabled = true
	cfg.Transcode.Executor = "http"
	cfg.Transcode.Workers = []config.TranscodeWorkerConfig{
		{Name: "nodea", HTTPExecutorConfig: config.HTTPExecutorConfig{BaseURL: "http://127.0.0.1:1"}},
	}
	return cfg
}

func mustBuild(t *testing.T, cfg *config.Config) *instance {
	t.Helper()
	in, err := build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// running is an instance serving on a loopback port.
type running struct {
	in     *instance
	base   string
	cancel context.CancelFunc
	done   chan error
}

func start(t *testing.T, in *instance, drain time.Duration) *running {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{in: in, base: "http://" + ln.Addr().String(), cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- in.serve(ctx, ln, drain) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Error("serve did not return")
		}
	})
	return r
}

func (r *running) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		r.done <- err
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return")
		return nil
	}
}

func do(t *testing.T, method, url, key, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestBuildRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(cfg *config.Config)
		want   string
	}{
		{"no key_file", func(c *config.Config) { c.Nodes.KeyFile = "" }, "nodes.key_file"},
		{"missing key file", func(c *config.Config) { c.Nodes.KeyFile += ".gone" }, "nodes.key_file"},
		{"short key", func(c *config.Config) { c.Nodes.KeyFile = writeFile(t, t.TempDir(), "k", "short") }, "shorter"},
		{"empty key file", func(c *config.Config) { c.Nodes.KeyFile = writeFile(t, t.TempDir(), "k", "\n") }, "empty"},
		{"empty nodes_path", func(c *config.Config) { c.Database.NodesPath = "" }, "database.nodes_path"},
		{"wildcard listen", func(c *config.Config) { c.Nodes.Listen = "0.0.0.0:9" }, "loopback"},
		{"hostless listen", func(c *config.Config) { c.Nodes.Listen = ":9" }, "loopback"},
		{"public listen", func(c *config.Config) { c.Nodes.Listen = "192.0.2.1:9" }, "loopback"},
		{"key file for a missing url", func(c *config.Config) { c.Nodes.TranscodeKeyFile = c.Nodes.KeyFile }, "transcode_url"},
		{"bad transcode key", func(c *config.Config) {
			c.Nodes.TranscodeURL = "http://127.0.0.1:1"
			c.Nodes.TranscodeKeyFile = writeFile(t, t.TempDir(), "k", "short")
		}, "transcode_key_file"},
		{"bad worker list", func(c *config.Config) { c.Transcode.Workers = append(c.Transcode.Workers, c.Transcode.Workers[0]) }, "transcode workers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			tc.mutate(cfg)
			in, err := build(cfg)
			if err == nil {
				in.svc.Close()
				t.Fatal("build succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q lacks %q", err, tc.want)
			}
			if strings.Contains(err.Error(), serviceKey) {
				t.Error("the error carries the key")
			}
			if exists(cfg.Database.NodesPath) {
				t.Error("a refused start still created the database")
			}
		})
	}
}

func TestBuildStartsWithAKeyAndLoopback(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:0", "[::1]:0", "localhost:0"} {
		cfg := testConfig(t)
		cfg.Nodes.Listen = listen
		in := mustBuild(t, cfg)
		in.svc.Close()
	}
}

func TestBuildAllowsANonLoopbackListenOnlyWhenSaid(t *testing.T) {
	cfg := testConfig(t)
	cfg.Nodes.Listen = "0.0.0.0:0"
	cfg.Nodes.AllowNonLoopback = true
	in := mustBuild(t, cfg)
	if in.listen != "0.0.0.0:0" {
		t.Errorf("listen = %q", in.listen)
	}
	in.svc.Close()
}

func TestBuildListenDefault(t *testing.T) {
	cfg := testConfig(t)
	cfg.Nodes.Listen = ""
	in := mustBuild(t, cfg)
	defer in.svc.Close()
	if in.listen != config.DefaultNodesListen {
		t.Errorf("listen = %q", in.listen)
	}
}

// A second Nodes on the same database fails at start-up, on the registry's lock.
func TestBuildFailsWhenTheDatabaseIsHeld(t *testing.T) {
	cfg := testConfig(t)
	first := mustBuild(t, cfg)
	defer first.svc.Close()
	if _, err := build(cfg); err == nil || !strings.Contains(err.Error(), "held by another process") {
		t.Fatalf("second build: %v", err)
	}
}

func TestEveryRouteButHealthzNeedsTheKey(t *testing.T) {
	in := mustBuild(t, testConfig(t))
	defer in.svc.Close()
	srv := httptest.NewServer(in.handler)
	defer srv.Close()

	routes := []struct{ method, path string }{
		{"GET", nodes.SpecPath},
		{"GET", nodes.BasePath + "/nodes"},
		{"GET", nodes.BasePath + "/nodes/nodea"},
		{"GET", nodes.BasePath + "/builds"},
		{"POST", nodes.BasePath + "/builds"},
		{"GET", nodes.BasePath + "/nope"},
		{"GET", nodes.LegacyBasePath},
		{"POST", nodes.LegacyBasePath},
		{"GET", nodes.LegacyBasePath + "/nodes"},
		{"GET", nodes.LegacyBasePath + "/bld_x/log"},
		{"GET", "/"},
		{"GET", "/metrics"},
	}
	for _, rt := range routes {
		for name, key := range map[string]string{"no key": "", "wrong key": strings.Repeat("w", 40)} {
			code, body := do(t, rt.method, srv.URL+rt.path, key, "")
			if code != 401 {
				t.Errorf("%s %s with %s: %d, want 401", rt.method, rt.path, name, code)
				continue
			}
			var v map[string]string
			if err := json.Unmarshal([]byte(body), &v); err != nil || v["code"] != "unauthorized" || v["error"] == "" {
				t.Errorf("%s %s: 401 body %q is not the repo's JSON shape", rt.method, rt.path, body)
			}
		}
	}
	// Authorization: Bearer is accepted like the other services.
	req, _ := http.NewRequest("GET", srv.URL+nodes.BasePath+"/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+serviceKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("bearer: %d", resp.StatusCode)
	}

	for _, p := range []string{nodes.SpecPath, nodes.BasePath + "/nodes", nodes.BasePath + "/nodes/nodea", nodes.BasePath + "/builds", nodes.LegacyBasePath, nodes.LegacyBasePath + "/nodes"} {
		if code, body := do(t, "GET", srv.URL+p, serviceKey, ""); code != 200 {
			t.Errorf("GET %s with the key: %d %s", p, code, body)
		}
	}
	if code, _ := do(t, "GET", srv.URL+"/metrics", serviceKey, ""); code != 404 {
		t.Errorf("an unknown path with the key: %d, want 404", code)
	}
	if code, body := do(t, "GET", srv.URL+"/healthz", "", ""); code != 200 || body != "ok\n" {
		t.Errorf("healthz without a key: %d %q", code, body)
	}
}

func TestHealthzFailsWithTheDatabaseClosed(t *testing.T) {
	in := mustBuild(t, testConfig(t))
	srv := httptest.NewServer(in.handler)
	defer srv.Close()
	if code, _ := do(t, "GET", srv.URL+"/healthz", "", ""); code != 200 {
		t.Fatalf("healthz = %d", code)
	}
	if err := in.svc.Close(); err != nil {
		t.Fatal(err)
	}
	code, body := do(t, "GET", srv.URL+"/healthz", "", "")
	if code != 503 {
		t.Fatalf("healthz with the database closed = %d", code)
	}
	if strings.Contains(body, in.dbPath) {
		t.Errorf("healthz body leaks the path: %q", body)
	}
}

func TestHealthzFailsWithoutTheMetaTable(t *testing.T) {
	cfg := testConfig(t)
	in := mustBuild(t, cfg)
	defer in.svc.Close()
	srv := httptest.NewServer(in.handler)
	defer srv.Close()
	db, err := sql.Open("sqlite", cfg.Database.NodesPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE meta`); err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, "GET", srv.URL+"/healthz", "", ""); code != 503 {
		t.Fatalf("healthz without meta = %d", code)
	}
}

func TestNodesComeFromTheConfiguredWorkers(t *testing.T) {
	cfg := testConfig(t)
	cfg.Transcode.Workers = []config.TranscodeWorkerConfig{
		{Name: "nodeb", HTTPExecutorConfig: config.HTTPExecutorConfig{BaseURL: "http://127.0.0.1:1"}},
		{Name: "nodea", HTTPExecutorConfig: config.HTTPExecutorConfig{BaseURL: "http://127.0.0.1:2"}},
	}
	in := mustBuild(t, cfg)
	defer in.svc.Close()
	srv := httptest.NewServer(in.handler)
	defer srv.Close()
	_, body := do(t, "GET", srv.URL+nodes.BasePath+"/nodes", serviceKey, "")
	var v struct{ Nodes []struct{ Name string } }
	if err := json.Unmarshal([]byte(body), &v); err != nil || len(v.Nodes) != 2 || v.Nodes[0].Name != "nodea" || v.Nodes[1].Name != "nodeb" {
		t.Fatalf("nodes: %s (%v)", body, err)
	}

	// Without the transcode engine configured there are no nodes, as in the server.
	cfg = testConfig(t)
	cfg.Transcode.Enabled = false
	in2 := mustBuild(t, cfg)
	defer in2.svc.Close()
	srv2 := httptest.NewServer(in2.handler)
	defer srv2.Close()
	_, body = do(t, "GET", srv2.URL+nodes.BasePath+"/nodes", serviceKey, "")
	if err := json.Unmarshal([]byte(body), &v); err != nil || len(v.Nodes) != 0 {
		t.Fatalf("nodes with transcode disabled: %s (%v)", body, err)
	}
}

// engineStub serves /transcode/v1/node-states and counts what it was asked.
type engineStub struct {
	status  atomic.Int32
	gotKey  atomic.Value
	hits    atomic.Int32
	nodesJS string
}

func (e *engineStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.hits.Add(1)
	e.gotKey.Store(r.Header.Get("X-Api-Key"))
	if r.URL.Path != "/transcode/v1/node-states" {
		http.NotFound(w, r)
		return
	}
	if s := int(e.status.Load()); s != 200 {
		w.WriteHeader(s)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(e.nodesJS))
}

func TestTranscodeViewAdapter(t *testing.T) {
	engine := &engineStub{nodesJS: `{"nodes":[{"node":"nodea","paused":true,"mode":"suspend","since":"2026-11-13T10:00:00Z","held_by":"operator","message":"synthetic"}]}`}
	engine.status.Store(200)
	es := httptest.NewServer(engine)
	defer es.Close()

	cfg := testConfig(t)
	cfg.Nodes.TranscodeURL = es.URL
	cfg.Nodes.TranscodeKeyFile = writeFile(t, t.TempDir(), "tk", strings.Repeat("t", 40))
	in := mustBuild(t, cfg)
	defer in.svc.Close()
	srv := httptest.NewServer(in.handler)
	defer srv.Close()

	code, body := do(t, "GET", srv.URL+nodes.BasePath+"/nodes/nodea", serviceKey, "")
	var v struct {
		Transcode *struct {
			Paused bool   `json:"paused"`
			Mode   string `json:"mode"`
			HeldBy string `json:"held_by"`
		} `json:"transcode"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &v) != nil || v.Transcode == nil || !v.Transcode.Paused || v.Transcode.Mode != "suspend" || v.Transcode.HeldBy != "operator" {
		t.Fatalf("node view: %d %s", code, body)
	}
	if got, _ := engine.gotKey.Load().(string); got != strings.Repeat("t", 40) {
		t.Errorf("the engine saw key %q", got)
	}
	// The legacy view reads the same state.
	if _, body := do(t, "GET", srv.URL+nodes.LegacyBasePath+"/nodes", serviceKey, ""); !strings.Contains(body, `"transcode_paused":true`) {
		t.Errorf("legacy nodes: %s", body)
	}
}

// A failing engine is "no view", never an error for the caller (N3).
func TestTranscodeViewFailureIsNoView(t *testing.T) {
	engine := &engineStub{}
	engine.status.Store(500)
	es := httptest.NewServer(engine)
	defer es.Close()
	cfg := testConfig(t)
	cfg.Nodes.TranscodeURL = es.URL
	in := mustBuild(t, cfg)
	defer in.svc.Close()
	srv := httptest.NewServer(in.handler)
	defer srv.Close()
	for _, p := range []string{nodes.BasePath + "/nodes/nodea", nodes.BasePath + "/nodes", nodes.LegacyBasePath + "/nodes"} {
		code, body := do(t, "GET", srv.URL+p, serviceKey, "")
		if code != 200 {
			t.Fatalf("GET %s with the engine failing: %d %s", p, code, body)
		}
		if strings.Contains(body, `"transcode":`) {
			t.Errorf("GET %s shows a transcode block without a view: %s", p, body)
		}
	}
	if engine.hits.Load() == 0 {
		t.Error("the engine was never asked")
	}

	// An engine that is not there at all is the same.
	es.Close()
	if code, _ := do(t, "GET", srv.URL+nodes.BasePath+"/nodes/nodea", serviceKey, ""); code != 200 {
		t.Errorf("engine down: %d", code)
	}
}

func TestNoTranscodeURLMeansNoView(t *testing.T) {
	in := mustBuild(t, testConfig(t))
	defer in.svc.Close()
	srv := httptest.NewServer(in.handler)
	defer srv.Close()
	code, body := do(t, "GET", srv.URL+nodes.BasePath+"/nodes/nodea", serviceKey, "")
	if code != 200 || strings.Contains(body, `"transcode":`) {
		t.Fatalf("%d %s", code, body)
	}
}

func TestHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer(http.NewServeMux())
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v: a long-poll would be cut", srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout != 5*time.Second || srv.IdleTimeout != 60*time.Second {
		t.Errorf("ReadHeaderTimeout %v, IdleTimeout %v", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
}

func TestDrainOutlastsTheLongestLongPoll(t *testing.T) {
	if drainTimeout <= time.Duration(nodes.BuildLogMaxWait)*time.Second {
		t.Fatalf("drain %v does not outlast a %d s long-poll", drainTimeout, nodes.BuildLogMaxWait)
	}
	if drainTimeout != 35*time.Second {
		t.Errorf("drain = %v", drainTimeout)
	}
}

// The main database path points nowhere on purpose: Nodes starts, serves and
// stops without ever creating or opening it.
func TestNeverOpensTheMainDatabase(t *testing.T) {
	cfg := testConfig(t)
	missingDir := filepath.Dir(cfg.Database.Path)
	in := mustBuild(t, cfg)
	r := start(t, in, time.Second)
	if code, _ := do(t, "POST", r.base+nodes.BasePath+"/builds", serviceKey, `{"target":"other","source":"synthetic","title":"t"}`); code != 200 {
		t.Fatalf("create: %d", code)
	}
	if code, _ := do(t, "GET", r.base+"/healthz", "", ""); code != 200 {
		t.Fatalf("healthz: %d", code)
	}
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	if exists(cfg.Database.Path) || exists(missingDir) {
		t.Errorf("the main database path or its directory was created")
	}
	if !exists(cfg.Database.NodesPath) {
		t.Errorf("nodes.db was not created at nodes_path")
	}
}

// Whatever happens to the drain, the registry is closed before the process
// exits: no -wal is left and the lock is free.
func TestShutdownThatExhaustsTheDrainStillClosesTheDatabase(t *testing.T) {
	cfg := testConfig(t)
	in := mustBuild(t, cfg)
	real := in.handler
	release := make(chan struct{})
	entered := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/stuck", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	mux.Handle("/", real)
	in.handler = mux

	r := start(t, in, 100*time.Millisecond)
	if code, _ := do(t, "POST", r.base+nodes.BasePath+"/builds", serviceKey, `{"target":"other","source":"synthetic","title":"t"}`); code != 200 {
		t.Fatalf("create: %d", code)
	}
	if !exists(cfg.Database.NodesPath + "-wal") {
		t.Fatal("no -wal while the registry is open: the test would prove nothing")
	}
	go func() {
		resp, err := http.Get(r.base + "/stuck")
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the stuck request never arrived")
	}
	err := r.stop(t)
	close(release)
	if err == nil {
		t.Error("an exhausted drain should be reported")
	}
	if exists(cfg.Database.NodesPath + "-wal") {
		t.Error("a -wal file is left after exit")
	}
	again, err := nodes.Open(nodes.Options{Path: cfg.Database.NodesPath})
	if err != nil {
		t.Fatalf("the registry was not closed (lock still held?): %v", err)
	}
	again.Close()
}

// A long-poll in flight at shutdown is answered before the registry closes.
func TestLongPollInFlightFinishesBeforeClose(t *testing.T) {
	cfg := testConfig(t)
	in := mustBuild(t, cfg)
	r := start(t, in, 10*time.Second)

	code, body := do(t, "POST", r.base+nodes.BasePath+"/builds", serviceKey, `{"target":"other","source":"synthetic","title":"t"}`)
	var b struct {
		ID string `json:"id"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &b) != nil || b.ID == "" {
		t.Fatalf("create: %d %s", code, body)
	}
	type result struct {
		code int
		body string
		took time.Duration
	}
	got := make(chan result, 1)
	started := time.Now()
	go func() {
		c, bd := do(t, "GET", r.base+nodes.BasePath+"/builds/"+b.ID+"/log?wait=1", serviceKey, "")
		got <- result{c, bd, time.Since(started)}
	}()
	time.Sleep(150 * time.Millisecond) // the poll is parked on the server by now
	stopped := make(chan error, 1)
	go func() { stopped <- r.stop(t) }()
	res := <-got
	if res.code != 200 {
		t.Fatalf("the long-poll was cut by the shutdown: %d %s", res.code, res.body)
	}
	if res.took < 900*time.Millisecond {
		t.Errorf("the long-poll answered after %v: it did not wait", res.took)
	}
	if err := <-stopped; err != nil {
		t.Errorf("shutdown: %v", err)
	}
	if exists(cfg.Database.NodesPath + "-wal") {
		t.Error("a -wal file is left after exit")
	}
}

// --- check ---

func cleanDB(t *testing.T, mark bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nodes.db")
	s, err := nodes.Open(nodes.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if mark {
		db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES('legacy_import', '3 builds, 0 pauses')`); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	return path
}

func check(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := runCheck(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCheckCommand(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		path := cleanDB(t, true)
		code, out, errs := check("--path", path)
		if code != 0 || !strings.Contains(out, "ok\n") || !strings.Contains(out, "legacy_import: 3 builds, 0 pauses") || !strings.Contains(out, "-wal present:  false") {
			t.Fatalf("exit %d\n%s\n%s", code, out, errs)
		}
		for _, want := range []string{"file owner:", "dir owner:", "integrity:     ok"} {
			if !strings.Contains(out, want) {
				t.Errorf("report lacks %q:\n%s", want, out)
			}
		}
	})
	t.Run("usage", func(t *testing.T) {
		if code, _, _ := check(); code != 2 {
			t.Errorf("no path: exit %d", code)
		}
		if code, _, _ := check("--path", cleanDB(t, true), "extra"); code != 2 {
			t.Errorf("extra argument: exit %d", code)
		}
		if code, _, _ := check("--bogus"); code != 2 {
			t.Errorf("unknown flag: exit %d", code)
		}
	})
	t.Run("missing database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nodes.db")
		if code, _, _ := check("--path", path); code != 1 {
			t.Errorf("exit %d", code)
		}
		if exists(path) {
			t.Error("check created the database")
		}
	})
	t.Run("lock held", func(t *testing.T) {
		path := cleanDB(t, true)
		s, err := nodes.Open(nodes.Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		code, _, errs := check("--path", path, "--checkpoint")
		if code != 1 || !strings.Contains(errs, "held by another process") {
			t.Fatalf("exit %d: %s", code, errs)
		}
	})
	t.Run("wal present", func(t *testing.T) {
		path := cleanDB(t, true)
		writeFile(t, filepath.Dir(path), "nodes.db-wal", "")
		code, _, errs := check("--path", path)
		if code != 1 || !strings.Contains(errs, "-wal") {
			t.Fatalf("exit %d: %s", code, errs)
		}
		if code, out, errs := check("--path", path, "--checkpoint"); code != 0 || !strings.Contains(out, "checkpointed:  true") {
			t.Fatalf("with --checkpoint: exit %d\n%s\n%s", code, out, errs)
		}
		if exists(path + "-wal") {
			t.Error("the -wal file is still there after --checkpoint")
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nodes.db")
		writeFile(t, filepath.Dir(path), "nodes.db", strings.Repeat("not a database ", 500))
		if code, _, errs := check("--path", path); code != 1 || !strings.Contains(errs, "integrity_check") {
			t.Fatalf("exit %d: %s", code, errs)
		}
	})
	t.Run("missing legacy_import", func(t *testing.T) {
		code, out, errs := check("--path", cleanDB(t, false))
		if code != 1 || !strings.Contains(errs, "legacy_import") || !strings.Contains(out, "(missing)") {
			t.Fatalf("exit %d\n%s\n%s", code, out, errs)
		}
	})
}
