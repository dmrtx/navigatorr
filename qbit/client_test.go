package qbit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeServer answers the login with the given status and body, and serves
// the endpoints with a session cookie check like qBittorrent does.
func fakeServer(t *testing.T, loginStatus int, loginBody, resumeStatus string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			if loginStatus == http.StatusOK || loginStatus == http.StatusNoContent {
				http.SetCookie(w, &http.Cookie{Name: "SID", Value: "s", Path: "/"})
			}
			w.WriteHeader(loginStatus)
			_, _ = w.Write([]byte(loginBody))
		case "/api/v2/torrents/resume":
			if _, err := r.Cookie("SID"); err != nil {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if resumeStatus == "204" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_, _ = w.Write([]byte(""))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestLoginAndCommandAcrossServerVersions(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		resume string
		wantOK bool
	}{
		{"older server, Ok.", 200, "Ok.", "200", true},
		{"newer server, empty 204", 204, "", "204", true},
		{"older server, wrong password", 200, "Fails.", "200", false},
		{"newer server, wrong password", 401, "Unauthorized", "200", false},
		{"banned address", 403, "Your IP address has been banned", "200", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeServer(t, tc.status, tc.body, tc.resume)
			defer srv.Close()
			err := NewClient(srv.URL, "u", "p").ResumeTorrents(context.Background(), []string{"abc"})
			if tc.wantOK && err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if !tc.wantOK && (err == nil || !strings.Contains(err.Error(), "login failed")) {
				t.Fatalf("expected a login failure, got %v", err)
			}
		})
	}
}
