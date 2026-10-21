package config

import "testing"

func TestTokenModeRejectsIncompleteAccessConfig(t *testing.T) {
	for _, access := range []CloudflareAccessConfig{
		{TeamDomain: "example.cloudflareaccess.com"},
		{Audience: "application-audience"},
		{TeamDomain: "example.com", Audience: "application-audience"},
	} {
		web := WebConfig{Enabled: true, Token: "a-local-token-of-at-least-24-chars", CloudflareAccess: access}
		if web.ValidateAuth() == nil {
			t.Fatalf("incomplete Access config accepted: %+v", access)
		}
	}
}

func TestTokenModeAcceptsCompleteAccessConfig(t *testing.T) {
	web := WebConfig{Enabled: true, Token: "a-local-token-of-at-least-24-chars", CloudflareAccess: CloudflareAccessConfig{TeamDomain: "https://example.cloudflareaccess.com", Audience: "application-audience"}}
	if err := web.ValidateAuth(); err != nil || !web.CloudflareAccessConfigured() {
		t.Fatalf("complete Access config refused: %v", err)
	}
	if (WebConfig{CloudflareAccess: CloudflareAccessConfig{TeamDomain: " ", Audience: "\t"}}).CloudflareAccessConfigured() {
		t.Fatal("blank Access fields counted as configured")
	}
}
