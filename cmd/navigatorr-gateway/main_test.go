package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/internal/mcpkey"
	"github.com/jakenesler/navigatorr/internal/mcpproxy"
	"github.com/jakenesler/navigatorr/openapi"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const session = "mcp-session-123e4567-e89b-12d3-a456-426614174000"

// connectorTools is the reviewed 59-tool catalog (names only).
func connectorTools(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "mcpproxy", "testdata", "connector_tools.txt"))
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Fields(string(b))
	if len(names) != 59 {
		t.Fatalf("fixture has %d names, want 59", len(names))
	}
	return names
}

func testConfig(qbit, tx, sab bool) *config.Config {
	cfg := &config.Config{Services: map[string]config.ServiceConfig{}, MaxResponseSizeKB: 50}
	if qbit {
		cfg.QBittorrent.URL = "http://127.0.0.1:1"
	}
	if tx {
		cfg.Transmission.URL = "http://127.0.0.1:1"
	}
	if sab {
		cfg.SABnzbd.URL = "http://127.0.0.1:1"
	}
	return cfg
}

func ownServer(t *testing.T, cfg *config.Config) *server.MCPServer {
	t.Helper()
	s := server.NewMCPServer("navigatorr", "1.0.0", server.WithToolCapabilities(true))
	registerOwnTools(s, cfg, arrservice.NewRegistry(cfg), openapi.NewStoreAt(cfg, t.TempDir()))
	return s
}

func toolNames(t *testing.T, s *server.MCPServer) []string {
	t.Helper()
	b, _ := json.Marshal(s.HandleMessage(context.Background(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	var v struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range v.Result.Tools {
		out = append(out, d.Name)
	}
	sort.Strings(out)
	return out
}

// The deployment config has qBittorrent only: what the gateway registers
// plus the proxy list must be exactly the connector's 59 tools.
func TestOwnPlusProxyIsTheConnectorCatalog(t *testing.T) {
	own := toolNames(t, ownServer(t, testConfig(true, false, false)))
	if err := mcpproxy.CheckDisjoint(own); err != nil {
		t.Fatal(err)
	}
	union := append(append([]string(nil), own...), mcpproxy.ProxyTools()...)
	sort.Strings(union)
	want := connectorTools(t)
	sort.Strings(want)
	if strings.Join(union, ",") != strings.Join(want, ",") {
		t.Errorf("own ∪ proxy differs from the 59:\n got %v\nwant %v", union, want)
	}
}

// With every download client configured the gateway has more tools of its
// own; none may ever be in the proxy list.
func TestOwnToolsWithAllClientsNeverOverlapTheProxyList(t *testing.T) {
	own := toolNames(t, ownServer(t, testConfig(true, true, true)))
	if len(own) <= 10 {
		t.Fatalf("expected the transmission and sabnzbd tools too, got %v", own)
	}
	if err := mcpproxy.CheckDisjoint(own); err != nil {
		t.Fatal(err)
	}
}

// transcode is a real mcp-go server with the gateway's own tools and the
// proxied ones: the single process the gateway must be indistinguishable from.
func transcode(t *testing.T, cfg *config.Config, wrap func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	s := ownServer(t, cfg)
	for _, n := range mcpproxy.ProxyTools() {
		name := n
		s.AddTool(
			mcp.NewTool(name,
				mcp.WithDescription("Stateful tool <"+name+"> & more, é"),
				mcp.WithString("q", mcp.Required(), mcp.Description("query")),
				mcp.WithNumber("limit", mcp.Description("max rows")),
				mcp.WithDestructiveHintAnnotation(false),
			),
			func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("ran " + name), nil
			})
	}
	var h http.Handler = server.NewStreamableHTTPServer(s, server.WithEndpointPath("/mcp"))
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func gatewayFor(t *testing.T, cfg *config.Config, upstream string) *httptest.Server {
	t.Helper()
	cfg.Gateway.UpstreamURL = upstream + "/mcp"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h, _, err := build(ctx, cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func rpc(t *testing.T, base, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func callResult(t *testing.T, base, name string) (isError bool, text string) {
	t.Helper()
	code, b := rpc(t, base, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{"q":"x"}}}`, name))
	var v struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if code != 200 || json.Unmarshal(b, &v) != nil || len(v.Result.Content) == 0 {
		t.Fatalf("call %s: %d %s", name, code, b)
	}
	return v.Result.IsError, v.Result.Content[0].Text
}

// Every one of the 59 descriptors, and the whole tools/list, is byte for
// byte what the single process publishes.
func TestDescriptorsEqualTheSingleProcess(t *testing.T) {
	cfg := testConfig(true, false, false)
	single := transcode(t, cfg, nil)
	gw := gatewayFor(t, testConfig(true, false, false), single.URL)

	const list = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	_, want := rpc(t, single.URL, list)
	code, got := rpc(t, gw.URL, list)
	if code != 200 {
		t.Fatalf("gateway tools/list: %d %s", code, got)
	}
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		t.Errorf("tools/list differs from the single process (%d vs %d bytes)", len(got), len(want))
	}

	var parse struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(got, &parse); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	wantRaw := descriptors(t, want)
	gotRaw := descriptors(t, got)
	for _, n := range connectorTools(t) {
		if gotRaw[n] == "" {
			t.Errorf("%s missing from the gateway's list", n)
			continue
		}
		seen[n] = true
		if gotRaw[n] != wantRaw[n] {
			t.Errorf("%s: descriptor differs\n got %s\nwant %s", n, gotRaw[n], wantRaw[n])
		}
	}
	if len(seen) != 59 {
		t.Errorf("checked %d of 59", len(seen))
	}
}

func descriptors(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var v struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, raw := range v.Result.Tools {
		var d struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &d)
		out[d.Name] = string(raw)
	}
	return out
}

func TestOwnAndProxiedCallsThroughTheGateway(t *testing.T) {
	cfg := testConfig(true, false, false)
	up := transcode(t, cfg, nil)
	gw := gatewayFor(t, testConfig(true, false, false), up.URL)
	if isErr, text := callResult(t, gw.URL, "memory_get"); isErr || text != "ran memory_get" {
		t.Errorf("proxied: %v %q", isErr, text)
	}
	// list_services is the gateway's own: answered without Transcode.
	if isErr, _ := callResult(t, gw.URL, "list_services"); isErr {
		t.Error("own tool failed")
	}
}

func (s *keyState) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Api-Key")
		s.mu.Lock()
		s.seen = append(s.seen, got)
		s.mu.Unlock()
		if s.require != "" && got != s.require {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

type keyState struct {
	require string
	mu      sync.Mutex
	seen    []string
}

func writeKey(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKeyEndToEnd(t *testing.T) {
	key := strings.Repeat("k", 32)
	ks := &keyState{require: key}
	up := transcode(t, testConfig(true, false, false), ks.wrap)

	cfg := testConfig(true, false, false)
	cfg.Gateway.KeyFile = writeKey(t, key+"\n")
	gw := gatewayFor(t, cfg, up.URL)
	if isErr, text := callResult(t, gw.URL, "memory_get"); isErr {
		t.Fatalf("with the key: %q", text)
	}
	if code, b := rpc(t, gw.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); code != 200 || !strings.Contains(string(b), `"memory_get"`) {
		t.Errorf("tools/list with the key: %d", code)
	}
	ks.mu.Lock()
	for i, k := range ks.seen {
		if k != key {
			t.Errorf("request %d carried %q, want the trimmed key", i, k)
		}
	}
	ks.mu.Unlock()

	// No key configured: nothing is sent, and a keyed Transcode refuses.
	noKey := gatewayFor(t, testConfig(true, false, false), up.URL)
	isErr, text := callResult(t, noKey.URL, "memory_get")
	if !isErr || text != mcpproxy.MsgNotRun {
		t.Errorf("without the key: %v %q, want the not-run error", isErr, text)
	}
	ks.mu.Lock()
	if last := ks.seen[len(ks.seen)-1]; last != "" {
		t.Errorf("a key header was sent with no key configured: %q", last)
	}
	ks.mu.Unlock()
}

func TestNoKeyConfiguredSendsNoHeader(t *testing.T) {
	ks := &keyState{}
	up := transcode(t, testConfig(true, false, false), ks.wrap)
	gw := gatewayFor(t, testConfig(true, false, false), up.URL)
	callResult(t, gw.URL, "memory_get")
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if len(ks.seen) == 0 {
		t.Fatal("nothing reached Transcode")
	}
	for _, k := range ks.seen {
		if k != "" {
			t.Errorf("X-Api-Key %q sent with no key configured", k)
		}
	}
}

func TestInvalidKeyFileStopsStartup(t *testing.T) {
	for name, p := range map[string]string{
		"missing":      filepath.Join(t.TempDir(), "nope"),
		"empty":        writeKey(t, ""),
		"blank":        writeKey(t, "  \n"),
		"short":        writeKey(t, strings.Repeat("k", 31)+"\n"),
		"short padded": writeKey(t, "  "+strings.Repeat("k", 20)+strings.Repeat(" ", 20)+"\n"),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(true, false, false)
			cfg.Gateway.UpstreamURL = "http://127.0.0.1:1/mcp"
			cfg.Gateway.KeyFile = p
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if _, _, err := build(ctx, cfg); err == nil {
				t.Fatal("want a start-up error")
			} else if strings.Contains(err.Error(), "kkkk") {
				t.Errorf("the key must not appear in the error: %v", err)
			}
		})
	}
	// Exactly 32 characters after trimming is fine.
	if k, err := mcpkey.Load(writeKey(t, strings.Repeat("k", 32)+"\r\n")); err != nil || len(k) != 32 {
		t.Errorf("32-character key: %v", err)
	}
}

func TestHealthzDoesNotDependOnTranscode(t *testing.T) {
	cfg := testConfig(true, false, false)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close() // nothing listens there
	gw := gatewayFor(t, cfg, "http://"+addr)
	resp, err := http.Get(gw.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.TrimSpace(string(b)) != "ok" {
		t.Errorf("healthz: %d %q", resp.StatusCode, b)
	}
	// And the tools list says it cannot answer, rather than listing fewer tools.
	code, body := rpc(t, gw.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if code != 200 || !strings.Contains(string(body), `"error"`) {
		t.Errorf("tools/list with Transcode down: %d %s", code, body)
	}
}

func TestHTTPServerHasNoShortWriteTimeout(t *testing.T) {
	if s := newHTTPServer(http.NotFoundHandler()); s.WriteTimeout != 0 && s.WriteTimeout < 60*time.Second {
		t.Errorf("WriteTimeout %s would cut a 55 s call", s.WriteTimeout)
	}
	if drainTimeout <= 55*time.Second {
		t.Errorf("drain %s must exceed the 55 s call budget", drainTimeout)
	}
}

// A call already running when the shutdown signal arrives finishes and
// answers before the upstream session is closed.
func TestShutdownDrainsCallsBeforeClosingTheSession(t *testing.T) {
	var (
		started  = make(chan struct{})
		once     sync.Once
		callDone atomic.Int64
		deleteAt atomic.Int64
		deletes  atomic.Int32
	)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
			deleteAt.Store(time.Now().UnixNano())
			return
		}
		b, _ := io.ReadAll(r.Body)
		var env struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(b, &env)
		w.Header().Set("Content-Type", "application/json")
		switch env.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "up-1")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`, env.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			once.Do(func() { close(started) })
			time.Sleep(400 * time.Millisecond)
			callDone.Store(time.Now().UnixNano())
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"finished"}]}}`, env.ID)
		}
	}))
	defer up.Close()

	cfg := testConfig(true, false, false)
	cfg.Gateway.UpstreamURL = up.URL + "/mcp"
	bctx, bcancel := context.WithCancel(context.Background())
	defer bcancel()
	h, fr, err := build(bctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, signal := context.WithCancel(context.Background()) // stands in for SIGTERM
	served := make(chan error, 1)
	go func() { served <- serve(sctx, ln, h, fr, 5*time.Second, 2*time.Second) }()

	type out struct {
		isErr bool
		text  string
	}
	res := make(chan out, 1)
	go func() {
		isErr, text := callResult(t, "http://"+ln.Addr().String(), "memory_get")
		res <- out{isErr, text}
	}()
	<-started
	signal()

	select {
	case o := <-res:
		if o.isErr || o.text != "finished" {
			t.Errorf("the in-flight call got %+v, want its result", o)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight call never answered")
	}
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after draining")
	}
	if deletes.Load() != 1 {
		t.Fatalf("DELETE count %d, want 1", deletes.Load())
	}
	if deleteAt.Load() < callDone.Load() {
		t.Error("the upstream session was closed before the call finished")
	}
	if _, err := http.Get("http://" + ln.Addr().String() + "/healthz"); err == nil {
		t.Error("the listener should be closed after shutdown")
	}
}

// The gateway never sends notifications, so it refuses the GET stream: an
// open stream would otherwise hold http.Server.Shutdown for the whole drain.
func TestGetStreamIsRefused(t *testing.T) {
	tx := transcode(t, testConfig(false, true, false), nil)
	gw := gatewayFor(t, testConfig(false, true, false), tx.URL+"/mcp")
	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp = %d, want 405", resp.StatusCode)
	}
}
