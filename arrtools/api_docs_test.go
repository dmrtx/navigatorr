package arrtools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/openapi"
	"github.com/mark3labs/mcp-go/mcp"
)

// list_services claims to report connection status, so it must actually probe.
func TestListServicesReportsStatus(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"version":"4.0"}`))
	}))
	defer ok.Close()

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer denied.Close()

	// Bound to a port nothing is listening on.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	cfg := &config.Config{Services: map[string]config.ServiceConfig{
		"sonarr": {URL: ok.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v3"},
		"radarr": {URL: denied.URL, APIKey: "bad", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v3"},
		"lidarr": {URL: deadURL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v1"},
	}}

	res, err := handleListServices(context.Background(), arrservice.NewRegistry(cfg), openapi.NewStore(cfg))
	if err != nil {
		t.Fatal(err)
	}

	var got []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, resultText(t, res))
	}

	want := map[string]string{
		"sonarr": "ok",
		"radarr": "unauthorized",
		"lidarr": "unreachable",
	}
	if len(got) != 3 {
		t.Fatalf("got %d services, want 3", len(got))
	}
	for _, g := range got {
		if !strings.HasPrefix(g.Status, want[g.Name]) {
			t.Errorf("%s status = %q, want prefix %q", g.Name, g.Status, want[g.Name])
		}
	}
}

// A url.Error stringifies the full request URL, which carries the key for
// query-auth services. The unreachable path must not echo it.
func TestPingDoesNotLeakQueryAuthKey(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	const secret = "super-secret-key"
	svc := arrservice.NewService("custom", config.ServiceConfig{
		URL: deadURL, APIKey: secret, AuthMethod: "query", APIVersion: "/api",
	})

	status := svc.Ping(context.Background())
	if !strings.HasPrefix(status, "unreachable") {
		t.Fatalf("status = %q, want unreachable", status)
	}
	if strings.Contains(status, secret) {
		t.Fatalf("Ping leaked the API key into its status: %q", status)
	}
}

// A truncated search says how many matches each service had, so a service
// crowded out of the first 50 is still visible.
func TestSearchAPITruncationListsMatchesPerService(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	specFor := func(n int) string {
		var sb strings.Builder
		sb.WriteString(`{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{`)
		for i := 0; i < n; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(`"/widget` + strings.Repeat("x", i+1) + `":{"get":{"summary":"List widgets","responses":{"200":{"description":"ok"}}}}`)
		}
		sb.WriteString("}}")
		return sb.String()
	}
	serve := func(n int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(specFor(n)))
		}))
	}
	a, b := serve(60), serve(14)
	defer a.Close()
	defer b.Close()
	cfg := &config.Config{Services: map[string]config.ServiceConfig{
		"alpha":   {OpenAPIURL: a.URL},
		"omegaxx": {OpenAPIURL: b.URL},
	}}
	store := openapi.NewStore(cfg)

	res, err := handleSearchAPI(context.Background(), store, "widget", "")
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(mcp.TextContent).Text
	for _, want := range []string{"showing the best 50 of 74", "alpha 60", "omegaxx 14", "service=<name>"} {
		if !strings.Contains(text, want) {
			t.Errorf("header lacks %q:\n%s", want, text[:300])
		}
	}
	if n := strings.Count(text, "**["); n != 50 {
		t.Errorf("listed %d results, want 50", n)
	}
}

// With service= given, the header has no per-service breakdown.
func TestSearchAPITruncationSingleServiceHeader(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var sb strings.Builder
	sb.WriteString(`{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{`)
	for i := 0; i < 60; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`"/widget` + strings.Repeat("x", i+1) + `":{"get":{"summary":"List widgets","responses":{"200":{"description":"ok"}}}}`)
	}
	sb.WriteString("}}")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(sb.String()))
	}))
	defer srv.Close()
	store := openapi.NewStore(&config.Config{Services: map[string]config.ServiceConfig{"alpha": {OpenAPIURL: srv.URL}}})

	for _, svc := range []string{"alpha", ""} {
		res, err := handleSearchAPI(context.Background(), store, "widget", svc)
		if err != nil {
			t.Fatal(err)
		}
		text := res.Content[0].(mcp.TextContent).Text
		if !strings.Contains(text, "showing the best 50 of 60)") || strings.Contains(text, "per service") || strings.Contains(text, "narrow") {
			t.Errorf("service=%q header: %s", svc, text[:200])
		}
	}
}
