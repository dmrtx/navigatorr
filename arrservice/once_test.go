package arrservice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/config"
)

func TestDoRequestOnceDoesNotReplayRedirectedMutation(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/command" {
					w.Header().Set("Location", "/accepted-again")
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			svc := NewService("sonarr", config.ServiceConfig{URL: srv.URL})
			_, code, err := svc.DoRequestOnce(context.Background(), http.MethodPost, "/command", nil, []byte(`{"name":"ManualImport"}`))
			if err != nil || code != status || calls.Load() != 1 {
				t.Fatalf("redirect replay: code=%d calls=%d err=%v", code, calls.Load(), err)
			}
		})
	}
}
