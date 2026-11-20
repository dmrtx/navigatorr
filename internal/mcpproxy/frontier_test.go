package mcpproxy

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestProxyListShape(t *testing.T) {
	names := ProxyTools()
	if len(names) != 49 {
		t.Fatalf("proxy list has %d names, want 49", len(names))
	}
	if !sort.StringsAreSorted(names) {
		t.Error("ProxyTools must be sorted")
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("%s listed twice", n)
		}
		seen[n] = true
		if strings.HasPrefix(n, "recipe_") {
			t.Errorf("%s: recipe_* is left out on purpose", n)
		}
	}
	names[0] = "mutated"
	if ProxyTools()[0] == "mutated" {
		t.Error("ProxyTools must return a copy")
	}
}

func TestCheckDisjoint(t *testing.T) {
	if err := CheckDisjoint([]string{"call_api", "list_services"}); err != nil {
		t.Fatal(err)
	}
	err := CheckDisjoint([]string{"call_api", "memory_set", "action_run"})
	if err == nil || !strings.Contains(err.Error(), "action_run") || !strings.Contains(err.Error(), "memory_set") {
		t.Fatalf("want both collisions named, got %v", err)
	}
}

func TestNewRefusesToStartOnCollision(t *testing.T) {
	s := ownServer()
	s.AddTool(mcp.NewTool("memory_get"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("x"), nil
	})
	_, err := New(Options{Server: s, Next: server.NewStreamableHTTPServer(s), UpstreamURL: "http://127.0.0.1:1/mcp"})
	if err == nil || !strings.Contains(err.Error(), "memory_get") {
		t.Fatalf("want a collision error naming memory_get, got %v", err)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	s := ownServer()
	next := server.NewStreamableHTTPServer(s)
	for _, u := range []string{"", "not a url", "ftp://host/mcp", "/mcp"} {
		if _, err := New(Options{Server: s, Next: next, UpstreamURL: u}); err == nil {
			t.Errorf("upstream %q: want an error", u)
		}
	}
	if _, err := New(Options{Next: next, UpstreamURL: "http://h/mcp"}); err == nil {
		t.Error("missing server: want an error")
	}
}

func TestListMergesOwnAndProxiedByteForByte(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), nil)
	r := g.list()
	names := listedNames(t, r)
	if len(names) != 50 || !sort.StringsAreSorted(names) {
		t.Fatalf("want 50 sorted tools (49 proxied + own_ping), got %d: %v", len(names), names)
	}
	raw := listedRaw(t, r)
	for _, n := range ProxyTools() {
		if raw[n] != desc(n) {
			t.Errorf("%s: descriptor changed in transit:\n got %s\nwant %s", n, raw[n], desc(n))
		}
	}
	if !strings.Contains(raw["own_ping"], `"own_ping"`) {
		t.Errorf("own descriptor missing: %s", raw["own_ping"])
	}
}

func TestBatchIsRejected(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), nil)
	r := g.post(goodSession, `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`)
	want200JSON(t, r)
	_, _, e := r.rpc(t)
	if !strings.Contains(string(e), "-32600") {
		t.Fatalf("want -32600, got %s", e)
	}
	if f.inits.Load() != 0 {
		t.Error("a rejected batch must not reach Transcode")
	}
}

func TestMissingOrNullIDIsInvalidRequest(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), nil)
	for _, c := range []struct{ name, body string }{
		{"list without id", `{"jsonrpc":"2.0","method":"tools/list"}`},
		{"list null id", `{"jsonrpc":"2.0","id":null,"method":"tools/list"}`},
		{"call without id", `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"memory_get"}}`},
		{"call null id", `{"jsonrpc":"2.0","id":null,"method":"tools/call","params":{"name":"memory_get"}}`},
		{"object id", `{"jsonrpc":"2.0","id":{},"method":"tools/call","params":{"name":"memory_get"}}`},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := g.post(goodSession, c.body)
			want200JSON(t, r)
			_, res, e := r.rpc(t)
			if res != nil || !strings.Contains(string(e), "-32600") {
				t.Fatalf("want -32600, got result=%s error=%s", res, e)
			}
		})
	}
	if f.inits.Load() != 0 {
		t.Error("invalid requests must not reach Transcode")
	}
}

func TestClientIDIsKeptAndSentUpstream(t *testing.T) {
	f := newFake(t)
	var got []string
	var mu sync.Mutex
	f.onCall = func(w http.ResponseWriter, r *http.Request, env envelope) bool {
		mu.Lock()
		got = append(got, string(env.ID))
		mu.Unlock()
		return false
	}
	g := newGateway(t, f.url(), nil)
	for _, id := range []string{`7`, `"abc-1"`} {
		r := g.call(id, "memory_get", "")
		gotID, _, _ := r.rpc(t)
		if string(gotID) != id {
			t.Errorf("reply id %s, want %s", gotID, id)
		}
	}
	r := g.post(goodSession, `{"jsonrpc":"2.0","id":"list-9","method":"tools/list"}`)
	if id, _, _ := r.rpc(t); string(id) != `"list-9"` {
		t.Errorf("list id %s", id)
	}
	if strings.Join(got, ",") != `7,"abc-1"` {
		t.Errorf("ids sent upstream: %v", got)
	}
}

func TestSessionHeaderIsCheckedLikeTheSDK(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), nil)
	for _, sid := range []string{"", "nope", "mcp-session-", "mcp-session-not-a-uuid"} {
		for _, body := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_get","arguments":{}}}`,
		} {
			mine := g.post(sid, body)
			// The SDK's answer for the same bad header, on a tool it serves.
			sdk := g.post(sid, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"own_ping"}}`)
			if mine.status != 404 || mine.status != sdk.status || mine.ctype != sdk.ctype || string(mine.body) != string(sdk.body) {
				t.Errorf("session %q: got %d %q %q, SDK gives %d %q %q", sid, mine.status, mine.ctype, mine.body, sdk.status, sdk.ctype, sdk.body)
			}
			if !strings.HasPrefix(mine.ctype, "text/plain") || strings.TrimSpace(string(mine.body)) != "Invalid session ID" {
				t.Errorf("session %q: want text/plain Invalid session ID, got %q %q", sid, mine.ctype, mine.body)
			}
		}
	}
	if f.inits.Load() != 0 {
		t.Error("a bad session must not reach Transcode")
	}
}

func TestSessionIssuedByAnotherServerIsAccepted(t *testing.T) {
	rt := newRealTranscode(t, false)
	// An id issued by a server like Transcode, not by the gateway.
	req, _ := http.NewRequest(http.MethodPost, rt.url(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	sid := resp.Header.Get("Mcp-Session-Id")
	if !strings.HasPrefix(sid, "mcp-session-") {
		t.Fatalf("unexpected session id %q", sid)
	}
	g := newGateway(t, rt.url(), nil)
	r := g.post(sid, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if n := listedNames(t, r); len(n) != 50 {
		t.Fatalf("want 50 tools, got %d", len(n))
	}
	if isErr, _ := toolResult(t, g.post(sid, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"memory_get","arguments":{"q":"x"}}}`)); isErr {
		t.Error("call with a foreign session id failed")
	}
}

func TestGetAndDeleteGoToTheSDK(t *testing.T) {
	f := newFake(t)
	var mu sync.Mutex
	var methods []string
	g := newGateway(t, f.url(), func(o *Options) {
		o.Next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			methods = append(methods, r.Method)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		})
	})
	for _, m := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		req, _ := http.NewRequest(m, g.srv.URL+"/mcp", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("%s: status %d, want the SDK's", m, resp.StatusCode)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "GET,DELETE,PUT" {
		t.Errorf("delegated %v", methods)
	}
	if f.inits.Load() != 0 {
		t.Error("nothing here should reach Transcode")
	}
}

func TestOwnAndOtherRequestsAreServedBySDK(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), nil)

	own := g.call("1", "own_ping", "")
	if isErr, text := toolResult(t, own); isErr || text != "pong" {
		t.Errorf("own tool: %v %q", isErr, text)
	}
	unknown := g.call("2", "no_such_tool", "")
	want200JSON(t, unknown)
	if _, res, e := unknown.rpc(t); res != nil || e == nil {
		t.Errorf("unknown tool: want a JSON-RPC error, got %s", unknown.body)
	}
	ping := g.post(goodSession, `{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	want200JSON(t, ping)
	notif := g.post(goodSession, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if notif.status != http.StatusAccepted {
		t.Errorf("notification: status %d", notif.status)
	}
	init := g.post("", `{"jsonrpc":"2.0","id":4,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	want200JSON(t, init)
	// Not JSON at all: the SDK's own 400.
	req, _ := http.NewRequest(http.MethodPost, g.srv.URL+"/mcp", strings.NewReader("x"))
	req.Header.Set("Content-Type", "text/plain")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("wrong content type: status %d, want the SDK's 400", resp.StatusCode)
	}
	// Invalid JSON: the SDK reports the parse error.
	bad := g.post(goodSession, `{"jsonrpc":`)
	if _, _, e := bad.rpc(t); !strings.Contains(string(e), "-32700") {
		t.Errorf("invalid JSON: %s", bad.body)
	}
	if f.inits.Load() != 0 {
		t.Error("own and unknown tools must not touch Transcode")
	}
}

func TestRequestBodyLimit(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), nil)
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_set","arguments":{"v":"` + strings.Repeat("a", maxRequestBody) + `"}}}`
	r := g.post(goodSession, big)
	want200JSON(t, r)
	if _, _, e := r.rpc(t); !strings.Contains(string(e), "-32600") {
		t.Errorf("want -32600 for a body over 1 MiB, got %s", r.body)
	}
	if f.calls.Load() != 0 {
		t.Error("an oversized request must not be forwarded")
	}
	ok := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_set","arguments":{"v":"` + strings.Repeat("a", maxRequestBody-200) + `"}}}`
	if isErr, _ := toolResult(t, g.post(goodSession, ok)); isErr {
		t.Error("a body just under the limit must go through")
	}
}

func TestSynthesizedAnswersAreAlwaysHTTP200JSON(t *testing.T) {
	// A closed upstream makes every proxied answer synthesized.
	g := newGateway(t, "http://"+closedAddr(t)+"/mcp", nil)
	for name, r := range map[string]reply{
		"list":  g.list(),
		"call":  g.call("1", "memory_get", ""),
		"batch": g.post(goodSession, `[]`),
		"noid":  g.post(goodSession, `{"jsonrpc":"2.0","method":"tools/list"}`),
	} {
		t.Run(name, func(t *testing.T) { want200JSON(t, r) })
	}
}

func TestUpstreamHeaders(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), nil)
	g.call("1", "memory_get", "")
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.callHdr[0]
	if h.Get("Mcp-Session-Id") != "up-session-1" {
		t.Errorf("session header %q", h.Get("Mcp-Session-Id"))
	}
	if h.Get("MCP-Protocol-Version") != "2025-03-26" {
		t.Errorf("protocol version %q", h.Get("MCP-Protocol-Version"))
	}
	if a := h.Get("Accept"); !strings.Contains(a, "application/json") || !strings.Contains(a, "text/event-stream") {
		t.Errorf("accept %q", a)
	}
	if h.Get("Mcp-Session-Id") == goodSession {
		t.Error("the client's session id must never be forwarded")
	}
}

func TestAPIKeySentOnlyWhenConfigured(t *testing.T) {
	key := strings.Repeat("k", 32)
	f := newFake(t)
	g := newGateway(t, f.url(), func(o *Options) { o.APIKey = key })
	g.list()
	g.call("1", "memory_get", "")
	g.f.Close(context.Background())
	keys := f.apiKeys()
	if len(keys) < 4 { // initialize, initialized, list, call, delete
		t.Fatalf("saw %d requests", len(keys))
	}
	for i, k := range keys {
		if k != key {
			t.Errorf("request %d: X-Api-Key %q", i, k)
		}
	}

	f2 := newFake(t)
	g2 := newGateway(t, f2.url(), nil)
	g2.list()
	for i, k := range f2.apiKeys() {
		if k != "" {
			t.Errorf("request %d carried a key without one configured", i)
		}
	}
}

func TestCloseDeletesTheUpstreamSession(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), nil)
	g.f.Close(context.Background()) // no session yet: nothing to delete
	if f.deletes.Load() != 0 {
		t.Fatal("no session, no DELETE")
	}

	f2 := newFake(t)
	g2 := newGateway(t, f2.url(), nil)
	if isErr, text := toolResult(t, g2.call("1", "memory_get", "")); isErr {
		t.Fatalf("call before Close failed: %s", text)
	}
	g2.f.Close(context.Background())
	if f2.deletes.Load() != 1 {
		t.Errorf("DELETE count %d, want 1", f2.deletes.Load())
	}
	// After Close no new session is opened: the call is refused unrun.
	inits := f2.inits.Load()
	wantNotRun(t, g2.call("2", "memory_get", ""))
	if f2.inits.Load() != inits {
		t.Errorf("a session was opened after Close")
	}
}

func TestWrongVersionKeepsTheClientID(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), nil)
	r := g.post(goodSession, `{"jsonrpc":"1.0","id":"abc","method":"tools/list"}`)
	want200JSON(t, r)
	id, res, e := r.rpc(t)
	if string(id) != `"abc"` || res != nil || !strings.Contains(string(e), "-32600") {
		t.Errorf("id=%s result=%s error=%s", id, res, e)
	}
	// No usable id: null.
	r = g.post(goodSession, `{"jsonrpc":"1.0","method":"tools/list"}`)
	if id, _, _ := r.rpc(t); string(id) != "null" && len(id) != 0 {
		t.Errorf("id %s, want null", id)
	}
}
