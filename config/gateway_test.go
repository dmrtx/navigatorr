package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayConfigDefaultsAndValidation(t *testing.T) {
	if got := (GatewayConfig{}).ListenAddr(); got != DefaultGatewayListen {
		t.Errorf("default listen %q", got)
	}
	if got := (GatewayConfig{Listen: "0.0.0.0:1"}).ListenAddr(); got != "0.0.0.0:1" {
		t.Errorf("listen %q", got)
	}
	for _, u := range []string{"", "transcode:8098/mcp", "ftp://h/mcp", "http://"} {
		if err := (GatewayConfig{UpstreamURL: u}).Validate(); err == nil {
			t.Errorf("upstream %q should be rejected", u)
		}
	}
	if err := (GatewayConfig{UpstreamURL: "http://transcode:8098/mcp"}).Validate(); err != nil {
		t.Errorf("valid upstream rejected: %v", err)
	}
}

// The shipped example must load with the server's strict loader.
func TestGatewayExampleLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "config.gateway.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Gateway.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.ListenAddr() == "" || cfg.QBittorrent.URL == "" || len(cfg.Services) == 0 {
		t.Errorf("example did not populate the config: %+v", cfg.Gateway)
	}
}

func TestGatewayBlockParses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	body := "gateway:\n  listen: \"127.0.0.1:9\"\n  upstream_url: \"http://t:1/mcp\"\n  key_file: /k\n  cache_dir: /c\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Gateway
	if g.Listen != "127.0.0.1:9" || g.UpstreamURL != "http://t:1/mcp" || g.KeyFile != "/k" || g.CacheDir != "/c" {
		t.Errorf("got %+v", g)
	}
	if err := os.WriteFile(p, []byte("gateway:\n  nonsense: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("unknown gateway keys must be rejected, got %v", err)
	}
}
