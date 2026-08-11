package arrservice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jakenesler/navigatorr/config"
)

// Audiobookshelf serves /ping at the root rather than under /api, so a status
// path of "/ping" made Ping request /api/ping and report a perfectly healthy
// instance as "http 404". Ping can only reach paths under the API prefix, so
// the default status path has to be one of those.
func TestAudiobookshelfPingUsesPathUnderAPIPrefix(t *testing.T) {
	var requested string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.Path
		// Mirrors a real Audiobookshelf: /ping and /healthcheck sit at the
		// root, and everything else lives under /api.
		switch r.URL.Path {
		case "/api/me", "/ping", "/healthcheck":
			w.Write([]byte(`{"id":"root"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	svc := NewService("audiobookshelf", config.ServiceConfig{
		URL:        srv.URL,
		APIVersion: config.DefaultAPIVersions["audiobookshelf"],
		APIKey:     "token",
		AuthMethod: config.DefaultAuthMethods["audiobookshelf"],
		AuthHeader: config.DefaultAuthHeaders["audiobookshelf"],
		AuthPrefix: config.DefaultAuthPrefixes["audiobookshelf"],
	})

	if got := svc.Ping(context.Background()); got != "ok" {
		t.Errorf("Ping() = %q, want \"ok\" (requested %q)", got, requested)
	}
	if want := "/api/me"; requested != want {
		t.Errorf("Ping requested %q, want %q", requested, want)
	}
}

// The status path has to be authenticated, otherwise Ping reports ok for an
// instance whose API key is wrong. Audiobookshelf's root /ping is anonymous,
// which is the second reason it is the wrong endpoint to use here.
func TestAudiobookshelfPingReportsBadToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"id":"root"}`))
	}))
	defer srv.Close()

	svc := NewService("audiobookshelf", config.ServiceConfig{
		URL:        srv.URL,
		APIVersion: config.DefaultAPIVersions["audiobookshelf"],
		APIKey:     "wrong-token",
		AuthMethod: config.DefaultAuthMethods["audiobookshelf"],
		AuthHeader: config.DefaultAuthHeaders["audiobookshelf"],
		AuthPrefix: config.DefaultAuthPrefixes["audiobookshelf"],
	})

	if got := svc.Ping(context.Background()); got != "unauthorized — check api_key" {
		t.Errorf("Ping() with a bad token = %q, want unauthorized", got)
	}
}
