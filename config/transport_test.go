package config

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestResolveTransport_Defaults(t *testing.T) {
	opts, err := ResolveTransport(nil, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Transport != "stdio" {
		t.Errorf("expected default transport stdio, got %q", opts.Transport)
	}
	if opts.Listen != "127.0.0.1:8098" {
		t.Errorf("expected default listen 127.0.0.1:8098, got %q", opts.Listen)
	}
}

func TestResolveTransport_ConfigDriven(t *testing.T) {
	cfg := &Config{
		MCP: MCPConfig{
			Transport: "streamable-http",
			Listen:    "0.0.0.0:8098",
		},
	}
	opts, err := ResolveTransport(cfg, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Transport != "streamable-http" {
		t.Errorf("expected streamable-http, got %q", opts.Transport)
	}
	if opts.Listen != "0.0.0.0:8098" {
		t.Errorf("expected 0.0.0.0:8098, got %q", opts.Listen)
	}
}

func TestResolveTransport_FlagOverridesConfig(t *testing.T) {
	cfg := &Config{
		MCP: MCPConfig{
			Transport: "stdio",
			Listen:    "127.0.0.1:8098",
		},
	}
	opts, err := ResolveTransport(cfg, "streamable-http", "192.0.2.1:9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Transport != "streamable-http" {
		t.Errorf("expected flag override to streamable-http, got %q", opts.Transport)
	}
	if opts.Listen != "192.0.2.1:9000" {
		t.Errorf("expected flag override to 192.0.2.1:9000, got %q", opts.Listen)
	}
}

func TestResolveTransport_CaseInsensitive(t *testing.T) {
	opts, err := ResolveTransport(nil, "Streamable-HTTP", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Transport != "streamable-http" {
		t.Errorf("expected normalized streamable-http, got %q", opts.Transport)
	}
}

func TestResolveTransport_Invalid(t *testing.T) {
	_, err := ResolveTransport(nil, "websocket", "")
	if err == nil {
		t.Fatalf("expected error for invalid transport 'websocket'")
	}
	if !strings.Contains(err.Error(), "invalid transport") {
		t.Errorf("expected error message mentioning invalid transport, got %v", err)
	}
}

func TestMCPConfig_YAMLDecodingStrict(t *testing.T) {
	yamlContent := `
allow_destructive: false
mcp:
  transport: streamable-http
  listen: 0.0.0.0:8098
`
	cfg := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(yamlContent)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		t.Fatalf("strict YAML decode failed for mcp config: %v", err)
	}
	if cfg.MCP.Transport != "streamable-http" {
		t.Errorf("expected mcp.transport=streamable-http, got %q", cfg.MCP.Transport)
	}
	if cfg.MCP.Listen != "0.0.0.0:8098" {
		t.Errorf("expected mcp.listen=0.0.0.0:8098, got %q", cfg.MCP.Listen)
	}
}
