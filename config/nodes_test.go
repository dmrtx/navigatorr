package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNodesListenDefaultAndLoopback(t *testing.T) {
	if got := (NodesConfig{}).ListenAddr(); got != DefaultNodesListen {
		t.Errorf("default listen = %q", got)
	}
	for _, ok := range []string{"127.0.0.1:9", "[::1]:9", "localhost:9", "127.0.0.2:9"} {
		if err := (NodesConfig{Listen: ok}).CheckListen(); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:9", ":9", "192.0.2.1:9", "[::]:9", "example.invalid:9"} {
		if err := (NodesConfig{Listen: bad}).CheckListen(); err == nil {
			t.Errorf("%s accepted", bad)
		}
		if err := (NodesConfig{Listen: bad, AllowNonLoopback: true}).CheckListen(); err != nil {
			t.Errorf("%s refused with allow_non_loopback: %v", bad, err)
		}
	}
	if err := (NodesConfig{Listen: "nonsense"}).CheckListen(); err == nil {
		t.Error("an address without a port was accepted")
	}
}

func TestNodesBlockLoadsAndRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	body := "nodes:\n  listen: \"127.0.0.1:9\"\n  key_file: /k\n  url: \"http://nodes:9\"\n  transcode_url: \"http://engine:8\"\n  transcode_key_file: /t\n  allow_non_loopback: true\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	n := cfg.Nodes
	if n.Listen != "127.0.0.1:9" || n.KeyFile != "/k" || n.URL != "http://nodes:9" || n.TranscodeURL != "http://engine:8" || n.TranscodeKeyFile != "/t" || !n.AllowNonLoopback {
		t.Errorf("nodes block = %+v", n)
	}
	if err := os.WriteFile(p, []byte("nodes:\n  nonsense: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("unknown nodes keys must be rejected, got %v", err)
	}
	if err := os.WriteFile(p, []byte("nodes:\n  url: \"not a url\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "nodes.url") {
		t.Errorf("a bad nodes.url must be rejected, got %v", err)
	}
	if err := os.WriteFile(p, []byte("nodes:\n  transcode_url: \"ftp://engine\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "nodes.transcode_url") {
		t.Errorf("a bad nodes.transcode_url must be rejected, got %v", err)
	}
	if err := os.WriteFile(p, []byte("nodes:\n  nonsense: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "nodes.nonsense") {
		t.Errorf("an unknown nodes key must name its section, got %v", err)
	}
}
