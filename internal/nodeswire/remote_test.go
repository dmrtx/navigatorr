package nodeswire

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/internal/mcpkey"
	"github.com/jakenesler/navigatorr/internal/nodesclient"
	"github.com/jakenesler/navigatorr/nodes"
)

// A synthetic service key, built at run time so secret scanners have nothing to read.
var remoteTestKey = strings.Repeat("remotekey", 4)

// nodesBinaryHandler is the handler cmd/navigatorr-nodes builds (newHandler):
// the spec and /nodes/v1 behind the service key. Copied here because that
// function lives in package main.
func nodesBinaryHandler(svc *nodes.Service, key string) http.Handler {
	api := http.NewServeMux()
	api.Handle("GET "+nodes.SpecPath, nodes.SpecHandler())
	api.Handle(nodes.BasePath+"/", svc.Handler())
	deny := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "authentication required", "code": "unauthorized"})
	})
	mux := http.NewServeMux()
	mux.Handle("/", mcpkey.RequireWith(key, api, deny))
	return mux
}

func remoteStack(t *testing.T) (*nodes.Service, action.BuildRecords, *nodesclient.Client) {
	t.Helper()
	svc, err := nodes.Open(nodes.Options{Path: filepath.Join(t.TempDir(), "nodes.db"), Topology: oneNode{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	srv := httptest.NewServer(nodesBinaryHandler(svc, remoteTestKey))
	t.Cleanup(srv.Close)
	// A short TTL so a test sees a change made behind the client's back.
	c, err := nodesclient.New(nodesclient.Options{BaseURL: srv.URL, Key: remoteTestKey, TTL: time.Millisecond, KnownNodes: oneNode{}.Nodes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return svc, RemoteRecords(c), c
}

// The engine's view through the client is the registry's own: a pause set
// through the client reads back, and a running build carries the same fields
// the in-process adapter gives.
func TestRemoteRecordsEndToEnd(t *testing.T) {
	svc, remote, _ := remoteStack(t)
	ctx := context.Background()

	if p, err := remote.BuildsPaused("nodeA"); p || err != nil {
		t.Fatalf("fresh registry: %v %v", p, err)
	}
	if err := remote.SetBuildsPause(ctx, "nodeA", true); err != nil {
		t.Fatal(err)
	}
	if p, err := remote.BuildsPaused("nodeA"); !p || err != nil {
		t.Fatalf("after pause: %v %v", p, err)
	}
	m, err := remote.BuildsPauses()
	if err != nil || m["nodeA"] == "" {
		t.Fatalf("pauses: %v %v", m, err)
	}
	if local, _ := Records(svc).BuildsPauses(); local["nodeA"] != m["nodeA"] {
		t.Fatalf("since differs: remote %q local %q", m["nodeA"], local["nodeA"])
	}
	if err := remote.SetBuildsPause(ctx, "nodeA", false); err != nil {
		t.Fatal(err)
	}
	if p, _ := remote.BuildsPaused("nodeA"); p {
		t.Fatal("resume did not read back")
	}
	err = remote.SetBuildsPause(ctx, "nodeZ", true)
	if !errors.Is(err, nodesclient.ErrUnknownNode) {
		t.Fatalf("unknown node: %v", err)
	}

	if _, err := svc.CreateBuild(ctx, nodes.BuildInput{Target: "nodeA", Source: "ci-tool", Holder: "runner1", Kind: "gradle", Title: "run", PreemptMode: nodes.PreemptGraceful, Via: nodes.BuildViaNodeLease}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond) // past the client's TTL
	got, want := remote.NodeBuildRecords("nodeA"), Records(svc).NodeBuildRecords("nodeA")
	if len(got) != 1 || len(want) != 1 {
		t.Fatalf("remote %+v local %+v", got, want)
	}
	if got[0] != want[0] {
		t.Fatalf("a running build differs:\nremote %+v\nlocal  %+v", got[0], want[0])
	}
	if got[0].Status != "running" || got[0].Priority == "" || got[0].PreemptMode != "graceful" || got[0].CreatedAt == "" || got[0].LastHeartbeatAt == "" || got[0].ExpiresAt == "" || got[0].Holder != "runner1" {
		t.Fatalf("fields missing: %+v", got[0])
	}
}

// With Nodes gone, the pause reads fail with an error the engine recognises,
// and the build list is empty.
func TestRemoteRecordsUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String()
	ln.Close()
	c, err := nodesclient.New(nodesclient.Options{BaseURL: closed, Key: remoteTestKey})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := RemoteRecords(c)
	if got := r.NodeBuildRecords("nodeA"); len(got) != 0 {
		t.Fatal(got)
	}
	if _, err := r.BuildsPaused("nodeA"); !errors.Is(err, action.ErrBuildRecordsUnavailable) || !errors.Is(err, nodesclient.ErrUnavailable) {
		t.Fatalf("BuildsPaused: %v", err)
	}
	if _, err := r.BuildsPauses(); !errors.Is(err, action.ErrBuildRecordsUnavailable) {
		t.Fatalf("BuildsPauses: %v", err)
	}
	if _, err := r.(action.BuildRecordsLister).ListNodeBuildRecords("nodeA"); !errors.Is(err, action.ErrBuildRecordsUnavailable) {
		t.Fatalf("ListNodeBuildRecords: %v", err)
	}
}
