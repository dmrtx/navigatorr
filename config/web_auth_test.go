package config

import "testing"

func TestWebAuthModes(t *testing.T) {
	if (WebConfig{}).AuthModeValue() != "token" {
		t.Fatal("default auth changed")
	}
	valid := WebConfig{Enabled: true, AuthMode: "cloudflare_access", CloudflareAccess: CloudflareAccessConfig{TeamDomain: "https://example.cloudflareaccess.com/", Audience: "application-audience"}}
	if err := valid.ValidateAuth(); err != nil {
		t.Fatal(err)
	}
	issuer, err := valid.CloudflareIssuer()
	if err != nil || issuer != "https://example.cloudflareaccess.com" {
		t.Fatal(issuer, err)
	}
	for _, domain := range []string{"http://example.cloudflareaccess.com", "https://example.com", "https://example.cloudflareaccess.com.evil.com", "https://example.cloudflareaccess.com:8443", "https://user@example.cloudflareaccess.com", "https://example.cloudflareaccess.com/path", "https://example.cloudflareaccess.com?query=1", "https://example.cloudflareaccess.com#fragment", "https://127.0.0.1"} {
		w := valid
		w.CloudflareAccess.TeamDomain = domain
		if w.ValidateAuth() == nil {
			t.Fatalf("unsafe domain accepted %s", domain)
		}
	}
	for _, mutate := range []func(*WebConfig){func(w *WebConfig) { w.AuthMode = "unknown" }, func(w *WebConfig) { w.Token = "ambiguous" }, func(w *WebConfig) { w.TokenFile = "secret" }, func(w *WebConfig) { w.CloudflareAccess.Audience = "" }} {
		w := valid
		mutate(&w)
		if w.ValidateAuth() == nil {
			t.Fatal("invalid auth configuration accepted")
		}
	}
}
