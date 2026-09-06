package arrservice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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

// Setting CheckRedirect opts out of the 10-redirect cap net/http applies by
// default, so a server that alternates /x and /x/ — which sameResource treats
// as one resource on every hop — is followed as fast as the network allows
// until the status timeout fires. That turns a status check into thousands of
// requests against an already-misconfigured service.
func TestPingStopsFollowingRedirectLoop(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if r.URL.Path == "/api/v1/status" {
			http.Redirect(w, r, "/api/v1/status/", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/api/v1/status", http.StatusFound)
	}))
	defer srv.Close()

	svc := NewService("prowlarr", config.ServiceConfig{
		URL:        srv.URL,
		APIVersion: "/api/v1",
		APIKey:     "k",
	})
	svc.StatusPath = "/status"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := svc.Ping(ctx)

	if n := atomic.LoadInt64(&hits); n > maxPingRedirects+1 {
		t.Errorf("Ping made %d requests, want at most %d", n, maxPingRedirects+1)
	}
	if got != "http 302" {
		t.Errorf("Ping() = %q, want %q", got, "http 302")
	}
}

func TestDoRequestWithContentType(t *testing.T) {
	var gotCT, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("X-Api-Key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	svc := NewService("bazarr", config.ServiceConfig{
		URL:        srv.URL,
		APIVersion: "/api",
		APIKey:     "test-key-123",
		AuthMethod: "header",
		AuthHeader: "X-Api-Key",
	})

	ctx := context.Background()

	// 1. Default DoRequest sets application/json
	_, code, err := svc.DoRequest(ctx, "POST", "/test", nil, []byte(`{"hello":"world"}`))
	if err != nil || code != 200 {
		t.Fatalf("DoRequest failed: code=%d, err=%v", code, err)
	}
	if gotCT != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", gotCT)
	}
	if gotAuth != "test-key-123" {
		t.Errorf("expected X-Api-Key test-key-123, got %q", gotAuth)
	}
	if gotBody != `{"hello":"world"}` {
		t.Errorf("expected body %q, got %q", `{"hello":"world"}`, gotBody)
	}

	// 2. DoRequestWithContentType sets application/x-www-form-urlencoded
	formBody := []byte("foo=bar&baz=1")
	_, code, err = svc.DoRequestWithContentType(ctx, "POST", "/test", nil, formBody, "application/x-www-form-urlencoded")
	if err != nil || code != 200 {
		t.Fatalf("DoRequestWithContentType failed: code=%d, err=%v", code, err)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("expected Content-Type application/x-www-form-urlencoded, got %q", gotCT)
	}
	if gotAuth != "test-key-123" {
		t.Errorf("expected X-Api-Key test-key-123, got %q", gotAuth)
	}
	if gotBody != "foo=bar&baz=1" {
		t.Errorf("expected form body %q, got %q", "foo=bar&baz=1", gotBody)
	}
}
