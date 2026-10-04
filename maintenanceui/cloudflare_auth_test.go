package maintenanceui

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
)

type accessRoundTripper func(*http.Request) (*http.Response, error)

func (f accessRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func accessFixture(t *testing.T) (*cloudflareAccessVerifier, *rsa.PrivateKey, *int) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	v := newCloudflareVerifier("https://example.cloudflareaccess.com", "application-audience")
	calls := new(int)
	jwks := fmt.Sprintf(`{"keys":[{"kid":"key-1","kty":"RSA","alg":"RS256","use":"sig","e":"AQAB","n":"%s"}]}`, base64.RawURLEncoding.EncodeToString(key.N.Bytes()))
	v.client.Transport = accessRoundTripper(func(r *http.Request) (*http.Response, error) {
		*calls++
		if r.URL.String() != v.issuer+"/cdn-cgi/access/certs" {
			t.Errorf("unexpected signing key source: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(jwks))}, nil
	})
	return v, key, calls
}
func accessJWT(t *testing.T, key *rsa.PrivateKey, header, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	digest := sha256.Sum256([]byte(payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return payload + "." + base64.RawURLEncoding.EncodeToString(sig)
}
func accessClaims(v *cloudflareAccessVerifier) map[string]any {
	return map[string]any{"iss": v.issuer, "aud": []string{v.audience}, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Add(-time.Minute).Unix(), "nbf": time.Now().Add(-time.Minute).Unix()}
}

func TestCloudflareAccessAssertions(t *testing.T) {
	v, key, calls := accessFixture(t)
	header := map[string]any{"alg": "RS256", "kid": "key-1"}
	token := accessJWT(t, key, header, accessClaims(v))
	if err := v.verify(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if err := v.verify(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("cached keys fetched %d times", *calls)
	}
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{"expired", "exp", time.Now().Add(-time.Second).Unix()},
		{"missing expiry", "exp", nil},
		{"wrong issuer", "iss", "https://attacker.cloudflareaccess.com"},
		{"wrong audience", "aud", []string{"another-app"}},
		{"future not-before", "nbf", time.Now().Add(time.Hour).Unix()},
		{"future issued-at", "iat", time.Now().Add(time.Hour).Unix()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := accessClaims(v)
			if tc.value == nil {
				delete(claims, tc.field)
			} else {
				claims[tc.field] = tc.value
			}
			if v.verify(context.Background(), accessJWT(t, key, header, claims)) == nil {
				t.Fatal("invalid assertion accepted")
			}
		})
	}
	forged := strings.Split(token, ".")
	forged[2] = base64.RawURLEncoding.EncodeToString(make([]byte, 256))
	if v.verify(context.Background(), strings.Join(forged, ".")) == nil {
		t.Fatal("forged signature accepted")
	}
	for _, bad := range []string{"", "eyJhbGciOiJub25lIn0.e30.", token + ".extra"} {
		if v.verify(context.Background(), bad) == nil {
			t.Fatal("malformed assertion accepted")
		}
	}
	unknown := accessJWT(t, key, map[string]any{"alg": "RS256", "kid": "unknown"}, accessClaims(v))
	for i := 0; i < 3; i++ {
		if v.verify(context.Background(), unknown) == nil {
			t.Fatal("unknown key accepted")
		}
	}
	if *calls != 1 {
		t.Fatal("unknown-key request bypassed fetch cooldown")
	}
	v.mu.Lock()
	v.expires = time.Now().Add(-time.Second)
	v.mu.Unlock()
	if v.verify(context.Background(), token) == nil {
		t.Fatal("expired signing-key cache accepted")
	}
	v.mu.Lock()
	v.nextFetch = time.Time{}
	v.mu.Unlock()
	if err := v.verify(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatal("expired cache not refreshed")
	}
}

func TestCloudflareUIAuthModeDoesNotAcceptLocalCredentials(t *testing.T) {
	original, _ := testUI(t)
	original.cfg.Web = config.WebConfig{Enabled: true, AuthMode: "cloudflare_access", CloudflareAccess: config.CloudflareAccessConfig{TeamDomain: "example.cloudflareaccess.com", Audience: "application-audience"}}
	s, err := New(original.cfg, original.registry, original.engine, original.mcp)
	if err != nil {
		t.Fatal(err)
	}
	verifier, key, _ := accessFixture(t)
	s.access = verifier
	h := s.Handler()
	info := request(h, "GET", "/api/maintenance/auth-info", "", false)
	if info.Code != 200 || strings.TrimSpace(info.Body.String()) != `{"auth_mode":"cloudflare_access"}` {
		t.Fatal(info.Body.String())
	}
	if w := request(h, "GET", "/api/maintenance/bootstrap", "", false); w.Code != 401 {
		t.Fatal("missing assertion accepted")
	}
	if w := request(h, "GET", "/api/maintenance/bootstrap", "", true); w.Code != 401 {
		t.Fatal("local bearer accepted")
	}
	cookie := httptest.NewRequest("GET", "/api/maintenance/bootstrap", nil)
	cookie.AddCookie(&http.Cookie{Name: "navigatorr_session", Value: "old-session"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, cookie)
	if rec.Code != 401 {
		t.Fatal("local session accepted")
	}
	if w := request(h, "POST", "/api/maintenance/login", `{"token":"`+testToken+`"}`, false); w.Code != 405 || len(w.Result().Cookies()) != 0 {
		t.Fatal("local login exposed")
	}
	token := accessJWT(t, key, map[string]any{"alg": "RS256", "kid": "key-1"}, accessClaims(verifier))
	for _, tc := range []struct {
		method, path string
		code         int
	}{{"GET", "/api/maintenance/bootstrap", 200}, {"POST", "/api/maintenance/logout", 405}} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("Cf-Access-Jwt-Assertion", token)
		r.Header.Set("X-Navigatorr-Request", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", "/api/maintenance/bootstrap", nil)
	r.Header.Set("Cf-Access-Jwt-Assertion", "forged")
	r.Header.Set("Cf-Access-Authenticated-User-Email", "allowed@example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("unverified forwarded identity accepted")
	}
}

func TestCloudflareKeyFetchFailsClosedAndIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{{"unavailable", 503, `{}`}, {"invalid", 200, `{}`}, {"oversized", 200, strings.Repeat("x", (1<<20)+1)}} {
		t.Run(tc.name, func(t *testing.T) {
			v, key, calls := accessFixture(t)
			v.client.Transport = accessRoundTripper(func(r *http.Request) (*http.Response, error) {
				*calls++
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			token := accessJWT(t, key, map[string]any{"alg": "RS256", "kid": "key-1"}, accessClaims(v))
			for i := 0; i < 2; i++ {
				if v.verify(context.Background(), token) == nil {
					t.Fatal("invalid key fetch accepted")
				}
			}
			if *calls != 1 {
				t.Fatal("failed-key requests not rate bounded")
			}
			if v.client.Timeout != 5*time.Second {
				t.Fatal("key fetch lacks timeout")
			}
			if v.client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
				t.Fatal("redirects allowed")
			}
		})
	}
}

func TestTokenModeAuthInfoPreservesLocalAuthentication(t *testing.T) {
	_, h := testUI(t)
	w := request(h, "GET", "/api/maintenance/auth-info", "", false)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"auth_mode":"token"}` {
		t.Fatal("auth-info disclosed configuration or changed default", w.Body.String())
	}
	if request(h, "GET", "/api/maintenance/bootstrap", "", false).Code != 401 {
		t.Fatal("anonymous token-mode API accepted")
	}
	if request(h, "GET", "/api/maintenance/bootstrap", "", true).Code != 200 {
		t.Fatal("token bearer no longer works")
	}
	login := request(h, "POST", "/api/maintenance/login", `{"token":"`+testToken+`"}`, false)
	if login.Code != 200 || len(login.Result().Cookies()) != 1 {
		t.Fatal("local session login changed")
	}
	r := httptest.NewRequest("POST", "/api/maintenance/logout", nil)
	r.Header.Set("X-Navigatorr-Request", "1")
	r.AddCookie(login.Result().Cookies()[0])
	logout := httptest.NewRecorder()
	h.ServeHTTP(logout, r)
	if logout.Code != 200 || len(logout.Result().Cookies()) != 1 || logout.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("local logout changed", logout.Body.String())
	}
}
