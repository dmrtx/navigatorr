package mcpproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

func TestRealTranscodeResultsAndErrors(t *testing.T) {
	rt := newRealTranscode(t, false)
	g := newGateway(t, rt.url(), nil)

	if isErr, text := toolResult(t, g.call("1", "memory_set", `{"q":"x"}`)); isErr || text != "stored" {
		t.Errorf("memory_set: %v %q", isErr, text)
	}
	if rt.effects.Load() != 1 {
		t.Errorf("effects %d, want 1", rt.effects.Load())
	}

	r := g.call("2", "memory_get", `{"q":"x"}`)
	_, res, _ := r.rpc(t)
	var v struct {
		Structured map[string]string `json:"structuredContent"`
	}
	_ = json.Unmarshal(res, &v)
	if v.Structured["k"] != "v" {
		t.Errorf("structuredContent lost: %s", res)
	}

	if isErr, text := toolResult(t, g.call("3", "memory_list", `{"q":"x"}`)); !isErr || text != "boom" {
		t.Errorf("a tool's own error must survive: %v %q", isErr, text)
	}

	// Transcode answers with a JSON-RPC error (-32602 for an unknown tool).
	rt.mcp.DeleteTools("action_run")
	r = g.call("4", "action_run", "")
	want200JSON(t, r)
	_, res, e := r.rpc(t)
	if res != nil || !strings.Contains(string(e), "-32602") {
		t.Errorf("want Transcode's -32602 untouched, got %s", r.body)
	}
}

func TestRealTranscodeStatefulRestart(t *testing.T) {
	rt := newRealTranscode(t, true)
	g := newGateway(t, rt.url(), nil)

	toolResult(t, g.call("1", "memory_set", `{"q":"x"}`))
	if rt.effects.Load() != 1 {
		t.Fatalf("effects %d", rt.effects.Load())
	}
	rt.restart() // every session is forgotten: the next request gets a 404
	if isErr, text := toolResult(t, g.call("2", "memory_set", `{"q":"x"}`)); isErr || text != "stored" {
		t.Fatalf("after a restart: %v %q", isErr, text)
	}
	if rt.effects.Load() != 2 {
		t.Errorf("effects %d, want exactly one more (no repeat)", rt.effects.Load())
	}

	// tools/list after a restart: 404, reinitialize, and the page is served.
	g.clock.Advance(61 * time.Second)
	rt.restart()
	if n := listedNames(t, g.list()); len(n) != 50 {
		t.Errorf("tools/list after a restart: %d tools", len(n))
	}
}

func TestRealTranscodeStatelessRestartNeedsNoReinit(t *testing.T) {
	rt := newRealTranscode(t, false)
	g := newGateway(t, rt.url(), nil)
	toolResult(t, g.call("1", "memory_set", `{"q":"x"}`))
	rt.restart()
	toolResult(t, g.call("2", "memory_set", `{"q":"x"}`))
	if rt.effects.Load() != 2 {
		t.Errorf("effects %d", rt.effects.Load())
	}
}

// The retry rule depends on the exact text mcp-go answers with before it
// dispatches anything. If an upgrade changes it, this fails and the rule has
// to be reviewed instead of silently never (or wrongly) retrying.
func TestMCPGoSessionErrorTextIsPinned(t *testing.T) {
	post := func(h http.Handler, sid string) (int, string, string) {
		srv := httptest.NewServer(h)
		defer srv.Close()
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}

	s := ownServer()
	// The default manager: a missing or malformed id.
	code, ctype, body := post(server.NewStreamableHTTPServer(s), "")
	if code != 404 || ctype != "text/plain; charset=utf-8" || body != "Invalid session ID\n" {
		t.Errorf("default manager: %d %q %q", code, ctype, body)
	}

	// A stateful manager: an unknown id, and a terminated one.
	mgr := &server.InsecureStatefulSessionIdManager{}
	h := server.NewStreamableHTTPServer(s, server.WithSessionIdManager(mgr))
	code, ctype, body = post(h, "mcp-session-123e4567-e89b-12d3-a456-426614174000")
	if code != 404 || ctype != "text/plain; charset=utf-8" || body != "Invalid session ID\n" {
		t.Errorf("unknown session: %d %q %q", code, ctype, body)
	}
	sid := mgr.Generate()
	if _, err := mgr.Terminate(sid); err != nil {
		t.Fatal(err)
	}
	code, ctype, body = post(h, sid)
	if code != 404 || ctype != "text/plain; charset=utf-8" || body != "Session terminated\n" {
		t.Errorf("terminated session: %d %q %q", code, ctype, body)
	}
}
