package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// HTTP daemon transport configuration. Automatic execution is HTTP-only: an
// enabled transcode section that omits `executor` defaults to "http". Explicit
// SSH still parses for admin/backward compatibility but is never automatic.

func writeHTTPConfig(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTranscodeHTTP_EffectiveConfigAlias(t *testing.T) {
	tc := TranscodeConfig{
		HTTP:       HTTPExecutorConfig{BaseURL: "http://a:8097"},
		WorkerHTTP: HTTPExecutorConfig{BaseURL: "http://b:8097"},
	}
	if got := tc.EffectiveHTTPConfig().BaseURL; got != "http://a:8097" {
		t.Errorf("explicit http must win, got %q", got)
	}
	tc = TranscodeConfig{WorkerHTTP: HTTPExecutorConfig{BaseURL: "http://b:8097"}}
	if got := tc.EffectiveHTTPConfig().BaseURL; got != "http://b:8097" {
		t.Errorf("alias must apply, got %q", got)
	}
}

func TestTranscodeHTTP_TimeoutDefaults(t *testing.T) {
	var hc HTTPExecutorConfig
	if hc.RequestTimeoutDuration() != 15*time.Second {
		t.Errorf("default request timeout: %v", hc.RequestTimeoutDuration())
	}
	if hc.SubmitTimeoutDuration() != 60*time.Second {
		t.Errorf("default submit timeout: %v", hc.SubmitTimeoutDuration())
	}
	hc = HTTPExecutorConfig{RequestTimeoutSec: 3, SubmitTimeoutSec: 7}
	if hc.RequestTimeoutDuration() != 3*time.Second || hc.SubmitTimeoutDuration() != 7*time.Second {
		t.Errorf("explicit timeouts: %+v", hc)
	}
}

func TestTranscodeHTTP_LoadAndBuild(t *testing.T) {
	p := writeHTTPConfig(t, "http.yaml", `
transcode:
  enabled: true
  executor: "http"
  http:
    base_url: "http://192.0.2.10:8097"
    token_file: "/run/secrets/tok"
    request_timeout_sec: 10
    submit_timeout_sec: 30
    path_mappings:
      - local_prefix: "/media"
        remote_prefix: "/Volumes/media"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Transcode.Executor != "http" {
		t.Fatalf("executor=%q", cfg.Transcode.Executor)
	}
	built, err := cfg.Transcode.BuildHTTPExecutorConfig()
	if err != nil {
		t.Fatalf("BuildHTTPExecutorConfig: %v", err)
	}
	if built.BaseURL != "http://192.0.2.10:8097" {
		t.Errorf("base=%q", built.BaseURL)
	}
	if built.TokenFile != "/run/secrets/tok" {
		t.Errorf("token_file=%q", built.TokenFile)
	}
	if built.RequestTimeout != 10*time.Second || built.SubmitTimeout != 30*time.Second {
		t.Errorf("timeouts: %+v", built)
	}
	if len(built.PathMappings) != 1 || built.PathMappings[0].Local != "/media" || built.PathMappings[0].Remote != "/Volumes/media" {
		t.Errorf("mappings: %+v", built.PathMappings)
	}
}

func TestTranscodeHTTP_Validation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		wantErr string
	}{
		{"http executor requires base_url", "transcode:\n  executor: \"http\"\n", "base_url is required"},
		{"bad scheme fails closed", "transcode:\n  executor: \"http\"\n  http:\n    base_url: \"ftp://x/y\"\n", "absolute http(s) URL"},
		{"negative timeout fails", "transcode:\n  executor: \"http\"\n  http:\n    base_url: \"http://h:8097\"\n    request_timeout_sec: -1\n", "must not be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := writeHTTPConfig(t, "case.yaml", tc.content)
			_, err := Load(p)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}

	t.Run("explicit ssh still parses for admin compatibility", func(t *testing.T) {
		p := writeHTTPConfig(t, "ssh.yaml", "transcode:\n  executor: \"ssh\"\n")
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Transcode.Executor != "ssh" {
			t.Errorf("executor=%q", cfg.Transcode.Executor)
		}
	})

	t.Run("dormant malformed http block still fails closed", func(t *testing.T) {
		p := writeHTTPConfig(t, "dormant.yaml", "transcode:\n  executor: \"ssh\"\n  http:\n    base_url: \"notaurl\"\n")
		_, err := Load(p)
		if err == nil || !strings.Contains(err.Error(), "absolute http(s) URL") {
			t.Fatalf("dormant malformed block must fail closed, got %v", err)
		}
	})
}

func TestTranscodeHTTP_ExampleUsesHTTPDefault(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "config.yaml.example"))
	if err != nil {
		t.Fatalf("example: %v", err)
	}
	if cfg.Transcode.Executor != "http" {
		t.Errorf("example default executor must be http, got %q", cfg.Transcode.Executor)
	}
	hc := cfg.Transcode.EffectiveHTTPConfig()
	if hc.BaseURL == "" {
		t.Error("example should document an http block (commented values still parse)")
	}
	if err := hc.ValidateHTTPConfig(); err != nil {
		t.Errorf("example http block must be well-formed: %v", err)
	}
}

func TestTranscodeHTTP_EnabledOmittingExecutorDefaultsHTTP(t *testing.T) {
	p := writeHTTPConfig(t, "default.yaml", `
transcode:
  enabled: true
  http:
    base_url: "http://192.0.2.10:8097"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Transcode.Executor != "http" {
		t.Fatalf("omitted executor must default to http, got %q", cfg.Transcode.Executor)
	}
	if _, err := cfg.Transcode.BuildHTTPExecutorConfig(); err != nil {
		t.Fatalf("defaulted http executor must build: %v", err)
	}
}

func TestTranscodeHTTP_DisabledOmittingExecutorNotForcedHTTP(t *testing.T) {
	p := writeHTTPConfig(t, "disabled.yaml", "transcode:\n  enabled: false\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Transcode.Executor == "http" {
		t.Errorf("disabled transcode must not imply the http executor, got %q", cfg.Transcode.Executor)
	}
}
