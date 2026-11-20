package mcpproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// goodSession has the shape the SDK's default session id manager accepts.
const goodSession = "mcp-session-123e4567-e89b-12d3-a456-426614174000"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Unix(1_700_000_000, 0)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type logs struct {
	mu    sync.Mutex
	errs  []string
	infos []string
}

func (l *logs) Errorf(f string, a ...any) {
	l.mu.Lock()
	l.errs = append(l.errs, fmt.Sprintf(f, a...))
	l.mu.Unlock()
}

func (l *logs) Logf(f string, a ...any) {
	l.mu.Lock()
	l.infos = append(l.infos, fmt.Sprintf(f, a...))
	l.mu.Unlock()
}

func (l *logs) errors() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.errs...)
}

// desc is a tool descriptor with characters that JSON escaping touches.
func desc(name string) string {
	return fmt.Sprintf(`{"name":%q,"description":"reads <x> & é for %s","inputSchema":{"type":"object","properties":{"q":{"type":"string"}}}}`, name, name)
}

func toolsResult(names []string, next string) string {
	ds := make([]string, len(names))
	for i, n := range names {
		ds[i] = desc(n)
	}
	s := `{"tools":[` + strings.Join(ds, ",") + `]`
	if next != "" {
		s += fmt.Sprintf(`,"nextCursor":%q`, next)
	}
	return s + `}`
}

// fake is a hand-written Transcode: it speaks just enough MCP over HTTP and
// lets a test override any step.
type fake struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	valid    map[string]bool
	nextSess int
	keys     []string
	callHdr  []http.Header
	cursors  []string
	pages    map[string]string // cursor -> tools/list result

	onInit  func(w http.ResponseWriter, r *http.Request) bool
	onInitd func(w http.ResponseWriter, r *http.Request) bool
	onList  func(w http.ResponseWriter, r *http.Request, env envelope) bool
	onCall  func(w http.ResponseWriter, r *http.Request, env envelope) bool

	inits, initds, lists, calls, deletes atomic.Int32
}

func newFake(t *testing.T) *fake { return newFakeOn(t, nil) }

// newFakeOn serves on ln, or on a free port when ln is nil.
func newFakeOn(t *testing.T, ln net.Listener) *fake {
	f := &fake{t: t, valid: map[string]bool{}, pages: map[string]string{"": toolsResult(ProxyTools(), "")}}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.handle))
	if ln != nil {
		f.srv.Listener.Close()
		f.srv.Listener = ln
	}
	f.srv.Start()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) url() string { return f.srv.URL + "/mcp" }

// expire invalidates every session, as a Transcode with a stateful session
// manager does after a restart.
func (f *fake) expire() {
	f.mu.Lock()
	f.valid = map[string]bool{}
	f.mu.Unlock()
}

func (f *fake) setPage(cursor, result string) {
	f.mu.Lock()
	f.pages[cursor] = result
	f.mu.Unlock()
}

func (f *fake) apiKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

func jsonOK(w http.ResponseWriter, id json.RawMessage, result string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", id, result)
}

func sessionGone(w http.ResponseWriter) {
	http.Error(w, "Invalid session ID", http.StatusNotFound)
}

func (f *fake) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.keys = append(f.keys, r.Header.Get("X-Api-Key"))
	f.mu.Unlock()
	if r.Method == http.MethodDelete {
		f.deletes.Add(1)
		return
	}
	var env envelope
	_ = json.Unmarshal(body, &env)
	if env.Method == "initialize" {
		f.inits.Add(1)
		if f.onInit != nil && f.onInit(w, r) {
			return
		}
		f.mu.Lock()
		f.nextSess++
		id := fmt.Sprintf("up-session-%d", f.nextSess)
		f.valid[id] = true
		f.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", id)
		jsonOK(w, env.ID, `{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1"}}`)
		return
	}
	f.mu.Lock()
	ok := f.valid[r.Header.Get("Mcp-Session-Id")]
	f.mu.Unlock()
	if !ok {
		sessionGone(w)
		return
	}
	switch env.Method {
	case "notifications/initialized":
		f.initds.Add(1)
		if f.onInitd != nil && f.onInitd(w, r) {
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		f.lists.Add(1)
		if f.onList != nil && f.onList(w, r, env) {
			return
		}
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(env.Params, &p)
		f.mu.Lock()
		f.cursors = append(f.cursors, p.Cursor)
		page, found := f.pages[p.Cursor]
		f.mu.Unlock()
		if !found {
			http.Error(w, "no such page", http.StatusInternalServerError)
			return
		}
		jsonOK(w, env.ID, page)
	case "tools/call":
		f.calls.Add(1)
		f.mu.Lock()
		f.callHdr = append(f.callHdr, r.Header.Clone())
		f.mu.Unlock()
		if f.onCall != nil && f.onCall(w, r, env) {
			return
		}
		jsonOK(w, env.ID, `{"content":[{"type":"text","text":"done"}]}`)
	default:
		http.Error(w, "unexpected "+env.Method, http.StatusBadRequest)
	}
}

// gateway is a Frontier served over HTTP, with a spy on what it delegates.
type gateway struct {
	t     *testing.T
	f     *Frontier
	srv   *httptest.Server
	own   *server.MCPServer
	next  *spyNext
	logs  *logs
	clock *clock
}

type spyNext struct {
	h       http.Handler
	mu      sync.Mutex
	methods []string
}

func (s *spyNext) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.methods = append(s.methods, r.Method)
	s.mu.Unlock()
	s.h.ServeHTTP(w, r)
}

func (s *spyNext) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.methods...)
}

func ownServer() *server.MCPServer {
	s := server.NewMCPServer("gw", "1", server.WithToolCapabilities(true))
	s.AddTool(mcp.NewTool("own_ping", mcp.WithDescription("own")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("pong"), nil
	})
	return s
}

func newGateway(t *testing.T, upstream string, mod func(*Options)) *gateway {
	t.Helper()
	g := &gateway{t: t, own: ownServer(), logs: &logs{}, clock: newClock()}
	g.next = &spyNext{h: server.NewStreamableHTTPServer(g.own, server.WithEndpointPath("/mcp"))}
	o := Options{
		Server: g.own, Next: g.next, UpstreamURL: upstream,
		Now: g.clock.Now, Logf: g.logs.Logf, Errorf: g.logs.Errorf,
	}
	if mod != nil {
		mod(&o)
	}
	f, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.f = f
	g.srv = httptest.NewServer(f)
	t.Cleanup(g.srv.Close)
	return g
}

type reply struct {
	status int
	ctype  string
	body   []byte
}

// rpc decodes the body as one JSON-RPC response.
func (r reply) rpc(t *testing.T) (id json.RawMessage, result, errObj json.RawMessage) {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(r.body, &env); err != nil {
		t.Fatalf("not JSON-RPC (%d %q): %q", r.status, r.ctype, r.body)
	}
	return env.ID, env.Result, env.Error
}

// post sends a raw body with the given session header ("" sends none).
func (g *gateway) post(sessionID, body string) reply {
	g.t.Helper()
	return g.postCtx(context.Background(), sessionID, body)
}

func (g *gateway) postCtx(ctx context.Context, sessionID, body string) reply {
	g.t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, g.srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, ctype: resp.Header.Get("Content-Type"), body: b}
}

func (g *gateway) list() reply {
	g.t.Helper()
	return g.post(goodSession, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
}

func (g *gateway) call(id, name, args string) reply {
	g.t.Helper()
	if args == "" {
		args = "{}"
	}
	return g.post(goodSession, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, args))
}

// want200JSON checks the status and type every synthesized answer must have.
func want200JSON(t *testing.T, r reply) {
	t.Helper()
	if r.status != 200 || !strings.HasPrefix(r.ctype, "application/json") {
		t.Fatalf("want 200 application/json, got %d %q: %s", r.status, r.ctype, r.body)
	}
}

// toolResult parses a tools/call reply that must be a result.
func toolResult(t *testing.T, r reply) (isError bool, text string) {
	t.Helper()
	want200JSON(t, r)
	_, res, errObj := r.rpc(t)
	if res == nil {
		t.Fatalf("want a result, got error %s", errObj)
	}
	var v struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(res, &v); err != nil {
		t.Fatalf("result: %v", err)
	}
	if len(v.Content) > 0 {
		text = v.Content[0].Text
	}
	return v.IsError, text
}

func wantNotRun(t *testing.T, r reply) {
	t.Helper()
	isErr, text := toolResult(t, r)
	if !isErr || text != MsgNotRun {
		t.Fatalf("want not-run error, got isError=%v %q", isErr, text)
	}
}

func wantUnknown(t *testing.T, r reply) {
	t.Helper()
	isErr, text := toolResult(t, r)
	if !isErr || text != MsgUnknown {
		t.Fatalf("want unknown-outcome error, got isError=%v %q", isErr, text)
	}
}

func listedNames(t *testing.T, r reply) []string {
	t.Helper()
	want200JSON(t, r)
	_, res, errObj := r.rpc(t)
	if res == nil {
		t.Fatalf("tools/list failed: %s", errObj)
	}
	var v struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res, &v); err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(v.Tools))
	for i, d := range v.Tools {
		out[i] = d.Name
	}
	return out
}

func listedRaw(t *testing.T, r reply) map[string]string {
	t.Helper()
	_, res, _ := r.rpc(t)
	var v struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(res, &v); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, d := range v.Tools {
		var n struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(d, &n)
		out[n.Name] = string(d)
	}
	return out
}

// closedAddr returns an address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.Listener.Addr().String()
	srv.Close()
	return addr
}

// realTranscode is a real mcp-go server with the proxied tools, plus a few
// stateless ones the gateway must never forward.
type realTranscode struct {
	srv      *httptest.Server
	mu       sync.RWMutex
	h        http.Handler
	mcp      *server.MCPServer
	stateful bool
	effects  atomic.Int32
}

func newRealTranscode(t *testing.T, stateful bool) *realTranscode {
	rt := &realTranscode{stateful: stateful}
	rt.mcp = server.NewMCPServer("transcode", "1", server.WithToolCapabilities(true))
	reg := func(name string) {
		rt.mcp.AddTool(
			mcp.NewTool(name,
				mcp.WithDescription("Does <something> & more with é: "+name),
				mcp.WithString("q", mcp.Required(), mcp.Description("a query")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				switch name {
				case "memory_set":
					rt.effects.Add(1)
					return mcp.NewToolResultText("stored"), nil
				case "memory_get":
					return mcp.NewToolResultStructured(map[string]any{"k": "v"}, "k=v"), nil
				case "memory_list":
					return mcp.NewToolResultError("boom"), nil
				}
				return mcp.NewToolResultText("ok:" + name), nil
			})
	}
	for _, n := range ProxyTools() {
		reg(n)
	}
	reg("list_services") // a tool the gateway serves itself
	reg("recipe_list")   // not part of the connector catalog
	rt.restart()
	rt.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt.mu.RLock()
		h := rt.h
		rt.mu.RUnlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(rt.srv.Close)
	return rt
}

// restart swaps in a fresh streamable-http server, so a stateful session
// manager forgets every session.
func (rt *realTranscode) restart() {
	var opts []server.StreamableHTTPOption
	opts = append(opts, server.WithEndpointPath("/mcp"))
	if rt.stateful {
		opts = append(opts, server.WithSessionIdManager(&server.InsecureStatefulSessionIdManager{}))
	}
	rt.mu.Lock()
	rt.h = server.NewStreamableHTTPServer(rt.mcp, opts...)
	rt.mu.Unlock()
}

func (rt *realTranscode) url() string { return rt.srv.URL + "/mcp" }

func jsonEq(a, b string) bool {
	var x, y bytes.Buffer
	if json.Compact(&x, []byte(a)) != nil || json.Compact(&y, []byte(b)) != nil {
		return false
	}
	return x.String() == y.String()
}

func listenOn(t *testing.T, addr string) net.Listener {
	t.Helper()
	var ln net.Listener
	var err error
	for i := 0; i < 20; i++ {
		if ln, err = net.Listen("tcp", addr); err == nil {
			return ln
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("listen on %s: %v", addr, err)
	return nil
}
