package transcodeworker

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/server"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/internal/mcpkey"
	"github.com/jakenesler/navigatorr/internal/nodesproxy"
	"github.com/jakenesler/navigatorr/maintenanceui"
	"github.com/jakenesler/navigatorr/nodes"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/tools"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// These tests drive the worker's real lease reporter (httpLeaseReporter, which
// is unexported, so they live here) against the whole chain that 3e-e2 puts in
// front of it: the Transcode server (maintenanceui, its own authentication),
// the Nodes proxy, and the Nodes binary's handler over a registry. The
// worker package cannot be driven from outside without exporting the reporter,
// so the chain is built here from the packages' public constructors.

type singleNode struct{}

func (singleNode) Node(target string) (string, bool) { return target, target == "nodeA" }
func (singleNode) Nodes() []string                   { return []string{"nodeA"} }

const (
	chainBuildsToken = "synth-builds-" + "tok-0123456789"
	chainNodesKey    = "synthetic-nodes-service-key-0123456789"
)

type leaseChain struct {
	w         *Worker
	transcode *httptest.Server
	svc       atomic.Pointer[nodes.Service] // the registry Nodes serves; replaced to simulate a restart that lost it
	down      atomic.Bool                   // Nodes drops every connection
	srvSvc    func() *nodes.Service
}

func newLeaseChain(t *testing.T) *leaseChain {
	t.Helper()
	c := &leaseChain{}
	openRegistry := func() *nodes.Service {
		s, err := nodes.Open(nodes.Options{Path: filepath.Join(t.TempDir(), "nodes.db"), Topology: singleNode{}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	c.srvSvc = openRegistry
	c.svc.Store(openRegistry())

	// Nodes: the binary's mux over whichever registry is current.
	nodesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.down.Load() {
			panic(http.ErrAbortHandler)
		}
		svc := c.svc.Load()
		api := http.NewServeMux()
		api.Handle("GET "+nodes.SpecPath, nodes.SpecHandler())
		api.Handle(nodes.BasePath+"/", svc.Handler())
		legacy := svc.LegacyHandler()
		api.Handle(nodes.LegacyBasePath, legacy)
		api.Handle(nodes.LegacyBasePath+"/", legacy)
		mcpkey.Require(chainNodesKey, api).ServeHTTP(w, r)
	}))
	t.Cleanup(nodesSrv.Close)
	proxy, err := nodesproxy.New(nodesproxy.Options{BaseURL: nodesSrv.URL, Key: chainNodesKey})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)

	// Transcode: a real maintenanceui.Server with a builds token.
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "builds-token")
	if err := os.WriteFile(tokenFile, []byte(chainBuildsToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		Web:       config.WebConfig{Enabled: true, Token: "maint-test-" + "tok-0123456789", BuildsTokenFile: tokenFile},
		Transcode: config.TranscodeConfig{Enabled: true, Executor: "http", HTTP: config.HTTPExecutorConfig{BaseURL: "http://127.0.0.1:1", Token: "synth-" + "worker-tok"}},
	}
	workerConfigs, err := cfg.Transcode.BuildWorkerConfigs()
	if err != nil {
		t.Fatal(err)
	}
	exec, err := httpclient.NewHTTPExecutor(workerConfigs[0].HTTP)
	if err != nil {
		t.Fatal(err)
	}
	m := server.NewMCPServer("test", "1")
	reg := arrservice.NewRegistry(cfg)
	e := tools.RegisterMaintenance(m, cfg, reg, nil, st, transcode.SingleWorker(exec))
	ui, err := maintenanceui.New(cfg, reg, e, m)
	if err != nil {
		t.Fatal(err)
	}
	ui.SetNodesProxy(proxy)
	c.transcode = httptest.NewServer(ui.Handler())
	t.Cleanup(c.transcode.Close)

	c.w, _, _ = newLeaseWorker(t, 2, newStubSpawn())
	c.w.leases.reporter = newHTTPLeaseReporter(c.transcode.URL, tokenFile, "nodeA")
	return c
}

// post sends a call the way an operator's script does: the builds token as
// X-Api-Key.
func (c *leaseChain) post(t *testing.T, path, body string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", c.transcode.URL+path, strings.NewReader(body))
	req.Header.Set("X-Api-Key", chainBuildsToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// beat marks the lease's output dirty and runs a report pass: one heartbeat.
func (c *leaseChain) beat(t *testing.T, id string) {
	t.Helper()
	if _, err := c.w.LeaseHeartbeat(id, ptr("working"), 0, nil); err != nil {
		t.Fatal(err)
	}
	c.w.leaseReportPass()
}

// A build the operator cancels through /nodes/v1 is learned by the worker from
// its next heartbeat, answered 409 by Nodes and passed through Transcode and
// the proxy: the lease ends as cancelled (N5).
func TestWorkerLearnsACancelThroughTheProxy(t *testing.T) {
	c := newLeaseChain(t)
	l, err := c.w.LeaseCreate(leaseReq("compile"))
	if err != nil {
		t.Fatal(err)
	}
	c.w.leaseReportPass() // registers it, if the create did not
	got, _ := c.w.LeaseGet(l.ID)
	if got.ServerID == "" {
		t.Fatalf("the lease was not registered through the chain: %+v", got)
	}
	if b, err := c.svc.Load().GetBuild(got.ServerID); err != nil || b.Status != nodes.BuildRunning {
		t.Fatalf("Nodes must hold the running build: %+v %v", b, err)
	}
	c.beat(t, l.ID)
	if got, _ = c.w.LeaseGet(l.ID); got.Status != leaseStatusRunning {
		t.Fatalf("a heartbeat the chain accepts changes nothing: %+v", got)
	}

	if code := c.post(t, "/nodes/v1/builds/"+got.ServerID+"/cancel", `{}`); code != 200 {
		t.Fatalf("cancel = %d", code)
	}
	c.beat(t, l.ID)
	if got, _ = c.w.LeaseGet(l.ID); got.Status != leaseStatusCancelled {
		t.Fatalf("the 409 cancelled must end the lease: %+v", got)
	}
}

// A heartbeat Nodes answers 404 (it lost the registry) makes the worker
// register the lease again under a new id; the build is not ended.
func TestWorkerRegistersAgainAfterANotFoundThroughTheProxy(t *testing.T) {
	c := newLeaseChain(t)
	l, err := c.w.LeaseCreate(leaseReq("compile"))
	if err != nil {
		t.Fatal(err)
	}
	c.w.leaseReportPass()
	first, _ := c.w.LeaseGet(l.ID)
	if first.ServerID == "" {
		t.Fatalf("not registered: %+v", first)
	}

	c.svc.Store(c.srvSvc()) // Nodes restarts without its data
	c.beat(t, l.ID)
	got, _ := c.w.LeaseGet(l.ID)
	if got.Status != leaseStatusRunning || got.ServerID != "" {
		t.Fatalf("after the 404 the lease runs and waits to be registered again: %+v", got)
	}
	c.w.leases.pending = nil // do not wait out the retry gap
	c.w.leaseReportPass()
	got, _ = c.w.LeaseGet(l.ID)
	if got.ServerID == "" || got.ServerID == first.ServerID || got.Status != leaseStatusRunning {
		t.Fatalf("it must be registered under a new id: %+v (was %s)", got, first.ServerID)
	}
	if b, err := c.svc.Load().GetBuild(got.ServerID); err != nil || b.Status != nodes.BuildRunning {
		t.Fatalf("the new registry holds the build: %+v %v", b, err)
	}
}

// Nodes down never stops a lease (N3): the proxy answers 503, the worker keeps
// the lease running and the heartbeat goes through once Nodes is back.
func TestWorkerLeaseSurvivesNodesBeingDownThroughTheProxy(t *testing.T) {
	c := newLeaseChain(t)
	c.down.Store(true)
	l, err := c.w.LeaseCreate(leaseReq("compile"))
	if err != nil {
		t.Fatalf("a lease is granted with Nodes down: %v", err)
	}
	if got, _ := c.w.LeaseGet(l.ID); got.Status != leaseStatusRunning || got.ServerID != "" {
		t.Fatalf("running and not recorded yet: %+v", got)
	}
	if err := c.w.leases.reporter.CheckPaused(); err == nil || err == errLeasesPaused {
		t.Fatalf("an unreachable Nodes is an error that is not 'paused': %v", err)
	}
	c.beat(t, l.ID)
	c.down.Store(false)
	c.w.leases.pending = nil
	c.w.leaseReportPass()
	got, _ := c.w.LeaseGet(l.ID)
	if got.Status != leaseStatusRunning || got.ServerID == "" {
		t.Fatalf("registered once Nodes is back: %+v", got)
	}
	c.down.Store(true)
	c.beat(t, l.ID)
	if got, _ = c.w.LeaseGet(l.ID); got.Status != leaseStatusRunning || got.ServerID == "" {
		t.Fatalf("a failed heartbeat keeps the lease and its record: %+v", got)
	}
}
