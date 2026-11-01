package arrservice

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/config"
)

func TestPlexPolicy(t *testing.T) {
	p := plexPolicy{service: "plex"}
	for _, c := range [][2]string{
		{"GET", "/library/sections"},
		{"GET", "/library/sections/3/refresh?force=1"},
		{"PUT", "/library/sections/3?agent=x"},
		{"POST", "/library/sections"},
		{"PUT", "/library/metadata/9/match"},
		{"get", "library/sections/"},
		{"DELETE", "/library/metadata/9"},
		{"delete", "/library/sections/3"},
	} {
		if err := p.Check(c[0], c[1], nil); err != nil {
			t.Errorf("%s %s should pass: %v", c[0], c[1], err)
		}
	}
	for _, c := range [][2]string{
		{"DELETE", "/status/sessions/1"},
		{" Delete ", "/playlists/9"},
		{"DELETE", "/"},
		{"DELETE", "/myplex/account"},
		{"DELETE", "/library/../playlists/9"},
		{"GET", "/myplex/account"},
		{"GET", "/MyPlex/account"},
		{"GET", "/security/token"},
		{"GET", "/accounts"},
		{"GET", "/library/../myplex/account"},
		{"GET", "/%6dyplex/account"},
		{"GET", "//myplex/account"},
		{"GET", "/library/sections?_method=DELETE"},
		{"GET", "/library\\..\\myplex"},
	} {
		var pe *PolicyError
		if err := p.Check(c[0], c[1], nil); !errors.As(err, &pe) {
			t.Errorf("%s %q should be blocked, got %v", c[0], c[1], err)
		}
	}
	if p.Check("GET", "/library/sections", map[string]string{"X-Plex-Token": "x"}) == nil {
		t.Error("a token in the query must be blocked")
	}
}

// The policy sits below every entry point, so a blocked request never reaches Plex.
func TestPlexServiceNeverSendsBlockedRequests(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	key := strings.Repeat("ab", 6) // made up at run time, nothing for secret scanners to flag
	svc := NewService("plex", config.ServiceConfig{URL: srv.URL, APIKey: key, AuthMethod: "header", AuthHeader: "X-Plex-Token"})
	ctx := context.Background()

	if _, _, err := svc.DoRequest(ctx, "DELETE", "/playlists/1", nil, nil); err == nil {
		t.Error("DoRequest DELETE outside /library must be blocked")
	}
	if _, _, err := svc.DoRequestOnce(ctx, "delete", "/playlists/1", nil, nil); err == nil {
		t.Error("DoRequestOnce delete outside /library must be blocked")
	}
	if _, _, err := svc.DoRequest(ctx, "GET", "/myplex/account", nil, nil); err == nil {
		t.Error("/myplex must be blocked")
	}
	if hits.Load() != 0 {
		t.Fatalf("blocked requests reached the server %d times", hits.Load())
	}
	if _, _, err := svc.DoRequest(ctx, "PUT", "/library/metadata/9/match", nil, nil); err != nil || hits.Load() != 1 {
		t.Errorf("a write should pass: %v hits %d", err, hits.Load())
	}
	if _, _, err := svc.DoRequest(ctx, "DELETE", "/library/metadata/9", nil, nil); err != nil || hits.Load() != 2 {
		t.Errorf("a library delete should pass: %v hits %d", err, hits.Load())
	}
	// Other services are untouched.
	other := NewService("sonarr", config.ServiceConfig{URL: srv.URL, AuthMethod: "header"})
	if _, _, err := other.DoRequest(ctx, "DELETE", "/series/1", nil, nil); err != nil {
		t.Errorf("sonarr must not get the Plex policy: %v", err)
	}
}
