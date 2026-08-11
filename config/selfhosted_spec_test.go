package config

import (
	"os"
	"path/filepath"
	"testing"
)

func loadFromYAML(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// Bazarr publishes no spec to GitHub but serves its own at /api/swagger.json,
// so its spec URL has to be built from the configured instance.
func TestSelfHostedSpecURLResolvesAgainstInstance(t *testing.T) {
	cfg := loadFromYAML(t, `
services:
  bazarr:
    url: "http://10.0.0.100:6767"
    api_key: "k"
`)
	got := cfg.Services["bazarr"].OpenAPIURL
	want := "http://10.0.0.100:6767/api/swagger.json"
	if got != want {
		t.Errorf("bazarr OpenAPIURL = %q, want %q", got, want)
	}
}

// The spec URL is derived from the instance URL, so it has to be built after
// resolveURL has filled in the scheme and port. Building it from the raw config
// value yields a URL with no scheme, which cannot be fetched.
func TestSelfHostedSpecURLUsesResolvedInstanceURL(t *testing.T) {
	cfg := loadFromYAML(t, `
services:
  bazarr:
    api_key: "k"
`)
	got := cfg.Services["bazarr"].OpenAPIURL
	want := "http://localhost:6767/api/swagger.json"
	if got != want {
		t.Errorf("bazarr OpenAPIURL = %q, want %q", got, want)
	}

	cfg = loadFromYAML(t, `
services:
  bazarr:
    url: "10.0.0.100"
    api_key: "k"
`)
	got = cfg.Services["bazarr"].OpenAPIURL
	want = "http://10.0.0.100:6767/api/swagger.json"
	if got != want {
		t.Errorf("bazarr OpenAPIURL with a bare host = %q, want %q", got, want)
	}
}

// An explicit openapi_url in the config wins over the self-hosted default.
func TestExplicitSpecURLOverridesSelfHosted(t *testing.T) {
	cfg := loadFromYAML(t, `
services:
  bazarr:
    url: "http://10.0.0.100:6767"
    api_key: "k"
    openapi_url: "http://specs.example.com/bazarr.json"
`)
	got := cfg.Services["bazarr"].OpenAPIURL
	want := "http://specs.example.com/bazarr.json"
	if got != want {
		t.Errorf("bazarr OpenAPIURL = %q, want %q", got, want)
	}
}

// A service with a GitHub-hosted spec must not pick up an instance-relative
// URL, and a self-hosted path must not leak onto services that have neither.
func TestSelfHostedSpecDoesNotAffectOtherServices(t *testing.T) {
	cfg := loadFromYAML(t, `
services:
  sonarr:
    url: "http://10.0.0.100:8989"
    api_key: "k"
`)
	got := cfg.Services["sonarr"].OpenAPIURL
	if got != DefaultOpenAPIURLs["sonarr"] {
		t.Errorf("sonarr OpenAPIURL = %q, want the GitHub default %q", got, DefaultOpenAPIURLs["sonarr"])
	}
}
