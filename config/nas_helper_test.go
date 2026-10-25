package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNASHelperIsOffByDefault(t *testing.T) {
	var c NASHelperConfig
	if c.Enabled() || c.Validate() != nil {
		t.Fatal("an empty block must be off and valid")
	}
}

func TestNASHelperValidation(t *testing.T) {
	good := NASHelperConfig{
		URL:          "http://nas.lan:8099",
		TokenFile:    "/run/secrets/helper",
		PathMappings: []NASHelperPathMapping{{Local: "/volume1/Media", Remote: "/volume1/Media"}},
	}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*NASHelperConfig){
		"no token file":     func(c *NASHelperConfig) { c.TokenFile = "" },
		"relative token":    func(c *NASHelperConfig) { c.TokenFile = "secrets/helper" },
		"no mappings":       func(c *NASHelperConfig) { c.PathMappings = nil },
		"bad scheme":        func(c *NASHelperConfig) { c.URL = "ftp://nas:8099" },
		"no host":           func(c *NASHelperConfig) { c.URL = "http://" },
		"credentials":       func(c *NASHelperConfig) { c.URL = "http://u:p@nas:8099" },
		"path in url":       func(c *NASHelperConfig) { c.URL = "http://nas:8099/v1" },
		"relative local":    func(c *NASHelperConfig) { c.PathMappings[0].Local = "Media" },
		"unclean remote":    func(c *NASHelperConfig) { c.PathMappings[0].Remote = "/volume1/../Media" },
		"root mapping":      func(c *NASHelperConfig) { c.PathMappings[0].Local = "/" },
		"duplicate local":   func(c *NASHelperConfig) { c.PathMappings = append(c.PathMappings, c.PathMappings[0]) },
		"token without url": func(c *NASHelperConfig) { c.URL = "" },
	}
	for name, mutate := range cases {
		c := good
		c.PathMappings = append([]NASHelperPathMapping(nil), good.PathMappings...)
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestNASHelperLoadsFromYAML(t *testing.T) {
	path := writeTempConfig(t, `
nas_helper:
  url: "http://nas.lan:8099"
  token_file: "/run/secrets/helper"
  path_mappings:
    - local: /volume1/Media
      remote: /volume1/Media
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.NASHelper.Enabled() || cfg.NASHelper.PathMappings[0].Remote != "/volume1/Media" {
		t.Fatalf("%+v", cfg.NASHelper)
	}
	bad := writeTempConfig(t, "nas_helper:\n  url: \"http://nas.lan:8099\"\n")
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "token_file") {
		t.Fatalf("a helper without a token file must fail to load: %v", err)
	}
}
