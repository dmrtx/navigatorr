package maintenanceui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/tools"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const testToken = "maintenance-test-token-0123456789"

func testUI(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{Web: config.WebConfig{Enabled: true, Token: testToken}}
	m := server.NewMCPServer("test", "1")
	reg := arrservice.NewRegistry(cfg)
	e := tools.RegisterMaintenance(m, cfg, reg, nil, st)
	s, err := New(cfg, reg, e, m)
	if err != nil {
		t.Fatal(err)
	}
	return s, s.Handler()
}
func request(h http.Handler, method, path, body string, auth bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-Navigatorr-Request", "1")
	if auth {
		r.Header.Set("Authorization", "Bearer "+testToken)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestUIAuthenticationAndOrigin(t *testing.T) {
	_, h := testUI(t)
	if w := request(h, "GET", "/api/maintenance/bootstrap", "", false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(h, "POST", "/api/maintenance/login", `{"token":"bad"}`, false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := request(h, "POST", "/api/maintenance/login", `{"token":"`+testToken+`"}`, false)
	if w.Code != 200 || len(w.Result().Cookies()) != 1 || !w.Result().Cookies()[0].HttpOnly {
		t.Fatal("missing secure session", w.Body.String())
	}
	r := httptest.NewRequest("GET", "/api/maintenance/bootstrap", nil)
	r.AddCookie(w.Result().Cookies()[0])
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	r = httptest.NewRequest("POST", "/api/maintenance/tool", bytes.NewBufferString(`{"name":"action_list","arguments":{}}`))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("X-Navigatorr-Request", "1")
	r.Header.Set("Origin", "https://attacker.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatal("cross-origin accepted")
	}
	if w := request(h, "POST", "/api/maintenance/tool", `{"name":"call_api","arguments":{}}`, true); w.Code != 403 {
		t.Fatal("generic service mutations exposed")
	}
	if w := request(h, "POST", "/api/maintenance/tool", `{} {}`, true); w.Code != 400 {
		t.Fatal("trailing JSON accepted")
	}
}

func TestUIQueueAndMCPShareHistory(t *testing.T) {
	s, h := testUI(t)
	body := `{"name":"action_run","arguments":{"action":"transcode_media","inputs":"{\"path\":\"/missing\"}","idempotency_key":"browser-receipt"}}`
	w := request(h, "POST", "/api/maintenance/tool", body, true)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var receipt struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if w := request(h, "POST", "/api/maintenance/tool", body, true); w.Code != 202 || !strings.Contains(w.Body.String(), receipt.ID) {
		t.Fatal("duplicate request lost identity")
	}
	if err := s.engine.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	w = request(h, "POST", "/api/maintenance/tool", `{"name":"action_status","arguments":{"id":"`+receipt.ID+`"}}`, true)
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Content) != 1 {
		t.Fatal(w.Body.String())
	}
	var status struct {
		Action struct {
			Origin string `json:"origin"`
			Status string `json:"status"`
		} `json:"action"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &status); err != nil || status.Action.Origin != "web" || status.Action.Status != "failed" {
		t.Fatal(result.Content[0].Text)
	}
	// An agent calling MCP writes to the same ledger with independent provenance.
	_, err := s.mcp.GetTool("action_run").Handler(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "action_run", Arguments: map[string]any{"action": "transcode_media", "inputs": `{"path":"/mcp-missing"}`}}})
	if err != nil {
		t.Fatal(err)
	}
	w = request(h, "POST", "/api/maintenance/tool", `{"name":"action_list","arguments":{}}`, true)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Content) != 1 {
		t.Fatal(w.Body.String())
	}
	var jobs []struct {
		Origin string `json:"origin"`
	}
	if json.Unmarshal([]byte(result.Content[0].Text), &jobs) != nil || len(jobs) != 2 {
		t.Fatal(result.Content[0].Text)
	}
	origins := map[string]bool{}
	for _, job := range jobs {
		origins[job.Origin] = true
	}
	if !origins["web"] || !origins["mcp"] {
		t.Fatal("UI and MCP histories diverged", jobs)
	}
	if w := request(h, "GET", "/", "", false); w.Code != 200 || !strings.Contains(w.Body.String(), "Navigatorr · Maintenance") {
		t.Fatal("embedded UI missing")
	}
}

func TestUILibraryBoundedAndNoCredentials(t *testing.T) {
	s, h := testUI(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/movie" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "title": "Movie A", "path": "/library/A", "api_key": "secret"}, {"id": 2, "title": "Movie B"}})
	}))
	defer upstream.Close()
	s.registry = arrservice.NewRegistry(&config.Config{Services: map[string]config.ServiceConfig{"radarr": {URL: upstream.URL, APIVersion: "/api/v3", APIKey: "server-only-secret"}}})
	w := request(h, "GET", "/api/maintenance/library?service=radarr&limit=1", "", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"has_more":true`) || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "Movie B") {
		t.Fatal(w.Body.String())
	}
	if w := request(h, "GET", "/api/maintenance/library?service=radarr&id=../x", "", true); w.Code != 400 {
		t.Fatal("invalid media id accepted")
	}
}
