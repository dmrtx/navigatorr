package tools

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/internal/nashelper"
	"github.com/mark3labs/mcp-go/server"
)

const diagHelperToken = "0123456789abcdef0123456789abcdef-token"

func diagHelperConfig(t *testing.T, url string, tokenFile string, remote ...string) config.NASHelperConfig {
	t.Helper()
	r := "/data"
	if len(remote) > 0 {
		r = remote[0]
	}
	return config.NASHelperConfig{
		URL:          url,
		TokenFile:    tokenFile,
		PathMappings: []config.NASHelperPathMapping{{Local: "/data", Remote: r}},
	}
}

func writeHelperToken(t *testing.T, token string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func diagnosticsFor(t *testing.T, cfg *config.Config) map[string]any {
	t.Helper()
	s := server.NewMCPServer("test", "0.0.0")
	RegisterDiagnostics(s, cfg, nil, nil, nil, nil, nil, nil)
	txt := callDiagnosticsWithContext(t, s, context.Background(), map[string]any{"check_connectivity": true})
	var out map[string]any
	if err := json.Unmarshal([]byte(txt), &out); err != nil {
		t.Fatalf("decoding diagnostics: %v", err)
	}
	return out
}

func TestDiagnosticsOmitsNASHelperWhenNotConfigured(t *testing.T) {
	out := diagnosticsFor(t, &config.Config{})
	if _, ok := out["nas_helper"]; ok {
		t.Fatalf("an unconfigured helper must not appear: %v", out["nas_helper"])
	}
}

func TestDiagnosticsReportsAHealthyNASHelper(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := nashelper.NewServer(nashelper.Config{Roots: []string{root}, Token: diagHelperToken})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer func() { ts.Close(); srv.Close() }()

	cfg := &config.Config{NASHelper: diagHelperConfig(t, ts.URL, writeHelperToken(t, diagHelperToken), root+"/library")}
	out := diagnosticsFor(t, cfg)
	info, _ := out["nas_helper"].(map[string]any)
	if info["status"] != "ok" || info["instance_id"] != srv.InstanceID() || info["resolver"] == "" {
		t.Fatalf("nas_helper: %v", info)
	}
	if info["token"] != "***REDACTED***" {
		t.Fatalf("token not redacted: %v", info["token"])
	}
	if strings.Contains(mustJSON(t, out), diagHelperToken) {
		t.Fatal("the token appears in diagnostics")
	}
}

func TestDiagnosticsFlagsAMappingTheHelperDoesNotServe(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := nashelper.NewServer(nashelper.Config{Roots: []string{root}, Token: diagHelperToken})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer func() { ts.Close(); srv.Close() }()
	cfg := &config.Config{NASHelper: diagHelperConfig(t, ts.URL, writeHelperToken(t, diagHelperToken), "/somewhere/else")}
	out := diagnosticsFor(t, cfg)
	info, _ := out["nas_helper"].(map[string]any)
	if info["status"] != "misconfigured" || out["status"] != "degraded" {
		t.Fatalf("status %v, nas_helper %v", out["status"], info)
	}
}

func TestDiagnosticsReportsAnUnreachableNASHelperAsDegraded(t *testing.T) {
	ts := httptest.NewServer(nil)
	url := ts.URL
	ts.Close()
	cfg := &config.Config{NASHelper: diagHelperConfig(t, url, writeHelperToken(t, diagHelperToken))}
	out := diagnosticsFor(t, cfg)
	info, _ := out["nas_helper"].(map[string]any)
	if info["status"] != "unavailable" || out["status"] != "degraded" {
		t.Fatalf("status %v, nas_helper %v", out["status"], info)
	}
}

func TestDiagnosticsReportsAMisconfiguredNASHelper(t *testing.T) {
	cfg := &config.Config{NASHelper: diagHelperConfig(t, "http://nas.invalid:8099", filepath.Join(t.TempDir(), "missing"))}
	out := diagnosticsFor(t, cfg)
	info, _ := out["nas_helper"].(map[string]any)
	if info["status"] != "misconfigured" || out["status"] != "degraded" {
		t.Fatalf("status %v, nas_helper %v", out["status"], info)
	}
}

func TestNewNASHelperClientIsNilWhenOff(t *testing.T) {
	c, err := NewNASHelperClient(config.NASHelperConfig{})
	if c != nil || err != nil {
		t.Fatalf("got %v, %v", c, err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
