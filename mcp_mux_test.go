package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPMuxGuardsOnlyMCP(t *testing.T) {
	key := strings.Repeat("k", 32) // built at run time so secret scanners skip it
	mark := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Handler", name)
		})
	}
	call := func(mux http.Handler, method, path string, withKey bool) (int, string) {
		r := httptest.NewRequest(method, path, nil)
		if withKey {
			r.Header.Set("X-Api-Key", key)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code, w.Header().Get("X-Handler")
	}

	open := httpMux("/mcp", mark("mcp"), "", mark("web"))
	for _, p := range []string{"/mcp", "/mcp/x"} {
		if code, h := call(open, "POST", p, false); code != 200 || h != "mcp" {
			t.Errorf("no key configured, %s: got %d %q, want open MCP", p, code, h)
		}
	}

	guarded := httpMux("/mcp", mark("mcp"), key, mark("web"))
	for _, m := range []string{"POST", "GET", "DELETE", "OPTIONS"} {
		for _, p := range []string{"/mcp", "/mcp/x"} {
			if code, h := call(guarded, m, p, false); code != 401 || h != "" {
				t.Errorf("%s %s without key: got %d %q, want 401", m, p, code, h)
			}
			if code, h := call(guarded, m, p, true); code != 200 || h != "mcp" {
				t.Errorf("%s %s with key: got %d %q, want MCP", m, p, code, h)
			}
		}
	}
	for _, p := range []string{"/", "/api/maintenance/x", "/mcpx"} {
		if code, h := call(guarded, "GET", p, false); code != 200 || h != "web" {
			t.Errorf("%s must reach the web without the MCP key: got %d %q", p, code, h)
		}
	}
	if code, _ := call(httpMux("/mcp", mark("mcp"), key, nil), "GET", "/", false); code != 404 {
		t.Errorf("no web: / got %d, want 404", code)
	}
}
