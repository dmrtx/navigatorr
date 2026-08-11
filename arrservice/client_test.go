package arrservice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jakenesler/navigatorr/config"
)

// A service that redirects an unauthenticated API request to its own login or
// setup page must not report ok, which is what following the redirect produces.
func TestPingDoesNotFollowRedirectToLoginPage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
	})
	mux.HandleFunc("/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>sign in</html>"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := NewService("prowlarr", config.ServiceConfig{
		URL:        srv.URL,
		APIVersion: "/api/v1",
		APIKey:     "wrong",
	})
	svc.StatusPath = "/status"

	if got := svc.Ping(context.Background()); got == "ok" {
		t.Errorf("Ping reported ok for a service that redirected to %s", "/auth/login")
	}
}

func TestPingReportsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	svc := NewService("prowlarr", config.ServiceConfig{
		URL:        srv.URL,
		APIVersion: "/api/v1",
		APIKey:     "wrong",
	})

	if got := svc.Ping(context.Background()); got != "unauthorized — check api_key" {
		t.Errorf("Ping = %q, want the unauthorized message", got)
	}
}

// A redirect that stays on the service's own API is the service working, not a
// bounce to a login page. SABnzbd sends /sabnzbd to /sabnzbd/ on every request.
func TestPingFollowsRedirectWithinTheAPI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sabnzbd", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/sabnzbd/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/sabnzbd/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := NewService("sabnzbd", config.ServiceConfig{
		URL:        srv.URL,
		APIVersion: "/sabnzbd",
		APIKey:     "key",
	})

	if got := svc.Ping(context.Background()); got != "ok" {
		t.Errorf("Ping = %q, want ok for a redirect that stays on the API", got)
	}
}

// DoRequest still follows redirects, so a service behind a proxy that upgrades
// or rewrites the request keeps working through call_api.
func TestDoRequestStillFollowsRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/series", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/v1/moved", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/api/v1/moved", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := NewService("sonarr", config.ServiceConfig{
		URL:        srv.URL,
		APIVersion: "/api/v1",
		APIKey:     "key",
	})

	_, code, err := svc.DoRequest(context.Background(), "GET", "/series", nil, nil)
	if err != nil {
		t.Fatalf("DoRequest: %v", err)
	}
	if code != http.StatusOK {
		t.Errorf("code = %d, want 200", code)
	}
}
