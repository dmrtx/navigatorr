package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// NASHelperConfig points at the read-only helper that runs on the NAS next to
// the library (docs/nas-helper.md). It is off when URL is empty: nothing changes
// and every check keeps reading files over the network mount.
type NASHelperConfig struct {
	URL string `yaml:"url"`
	// TokenFile holds the bearer token; it is read when the client is built.
	TokenFile string `yaml:"token_file"`
	// PathMappings translate a path as the coordinator sees it into the path the
	// helper sees. Both are absolute directories; the longest Local prefix wins.
	PathMappings []NASHelperPathMapping `yaml:"path_mappings"`
}

// NASHelperPathMapping maps a directory in the coordinator's namespace to the same
// directory in the helper's.
type NASHelperPathMapping struct {
	Local  string `yaml:"local"`
	Remote string `yaml:"remote"`
}

// Enabled reports whether a helper is configured.
func (c NASHelperConfig) Enabled() bool { return strings.TrimSpace(c.URL) != "" }

// Validate rejects a half-configured helper at load time, so a typo cannot leave
// the feature silently off.
func (c NASHelperConfig) Validate() error {
	if !c.Enabled() {
		if c.TokenFile != "" || len(c.PathMappings) > 0 {
			return fmt.Errorf("nas_helper.token_file and nas_helper.path_mappings need nas_helper.url")
		}
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(c.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("nas_helper.url must be an http(s) URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("nas_helper.url must be a base URL without credentials, path or query")
	}
	if strings.TrimSpace(c.TokenFile) == "" {
		return fmt.Errorf("nas_helper.token_file is required when nas_helper.url is set")
	}
	if len(c.PathMappings) == 0 {
		return fmt.Errorf("nas_helper.path_mappings needs at least one mapping")
	}
	seen := map[string]bool{}
	for _, m := range c.PathMappings {
		for name, p := range map[string]string{"local": m.Local, "remote": m.Remote} {
			if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
				return fmt.Errorf("nas_helper.path_mappings %s %q must be a clean absolute directory other than /", name, p)
			}
		}
		if seen[m.Local] {
			return fmt.Errorf("nas_helper.path_mappings lists %q twice", m.Local)
		}
		seen[m.Local] = true
	}
	return nil
}
