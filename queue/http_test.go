package queue

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	store := openTemp(t)
	srv, err := NewServer(store, "s3cret")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store
}

func post(t *testing.T, ts *httptest.Server, auth, ctype, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+"/request", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// An empty token used to disable auth entirely, and the shipped example config
// paired listen: ":8099" with token: "". Anything posted here is later read and
// acted on by an agent holding write credentials, so it has to fail closed.
func TestNewServerRequiresToken(t *testing.T) {
	store := openTemp(t)
	if _, err := NewServer(store, ""); err == nil {
		t.Fatal("NewServer with an empty token should fail")
	}
}

func TestAuthRejectsBadAndMissingTokens(t *testing.T) {
	ts, _ := newTestServer(t)
	body := `{"text":"boston legal"}`

	for _, tt := range []struct {
		name string
		auth string
		want int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		// TrimPrefix is a no-op when the prefix is absent, so a bare credential
		// used to authenticate.
		{"no scheme", "s3cret", http.StatusUnauthorized},
		{"correct", "Bearer s3cret", http.StatusCreated},
		// RFC 7235 makes the scheme case-insensitive; a shortcut app or shell
		// script will produce these eventually.
		{"lowercase scheme", "bearer s3cret", http.StatusCreated},
		{"extra whitespace", "Bearer   s3cret", http.StatusCreated},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp := post(t, ts, tt.auth, "application/json", body)
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestHealthzNeedsNoAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200", resp.StatusCode)
	}
}

// The path exemption for /healthz must not be a way past the middleware.
func TestHealthzExemptionIsNotBypassable(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, path := range []string{"/healthz/../request", "//healthz", "/healthz/../../request"} {
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(`{"text":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue // client may normalise the path away entirely
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated {
			t.Errorf("%s enqueued without auth", path)
		}
	}
}

// The body was decoded with no limit, so a 5MB text was accepted and then
// re-marshaled on every subsequent queue operation.
func TestRequestBodyIsCapped(t *testing.T) {
	ts, store := newTestServer(t)
	huge := `{"text":"` + strings.Repeat("a", maxBodyBytes+1024) + `"}`
	resp := post(t, ts, "Bearer s3cret", "application/json", huge)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
	if got := store.List(""); len(got) != 0 {
		t.Errorf("oversized request was stored: %+v", got)
	}
}

func TestRequestTextLengthIsCapped(t *testing.T) {
	ts, _ := newTestServer(t)
	body := `{"text":"` + strings.Repeat("a", MaxTextLen+1) + `"}`
	resp := post(t, ts, "Bearer s3cret", "application/json", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// Without a content-type check this is a CORS simple request, which any page
// the user visits can send to localhost with no preflight.
func TestRequestRequiresJSONContentType(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts, "Bearer s3cret", "text/plain", `{"text":"boston legal"}`)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
	// A charset parameter is still JSON.
	resp = post(t, ts, "Bearer s3cret", "application/json; charset=utf-8", `{"text":"boston legal"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status with charset = %d, want 201", resp.StatusCode)
	}
}

func TestRequestRejectsBlankText(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts, "Bearer s3cret", "application/json", `{"text":"   "}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestQueueRejectsUnknownStatusFilter(t *testing.T) {
	ts, _ := newTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/queue?status=complete", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMethodChecks(t *testing.T) {
	ts, _ := newTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/request", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /request = %d, want 405", resp.StatusCode)
	}
}
