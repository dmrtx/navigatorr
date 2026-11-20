package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// NodesConfig holds the settings of the separate Nodes binary
// (cmd/navigatorr-nodes), which runs the builds registry on its own and owns
// nodes.db. The Transcode server reads url and key_file in the step that
// switches it to the remote registry; until then it ignores this block, so one
// file can be given to every binary. The database path is database.nodes_path:
// Nodes never derives it from the main database.
type NodesConfig struct {
	// Listen is the address Nodes serves on. Defaults to 127.0.0.1:8097.
	Listen string `yaml:"listen"`
	// KeyFile holds the service key (X-Api-Key) Nodes requires on every route
	// but /healthz and its callers send.
	KeyFile string `yaml:"key_file"`
	// URL is where Transcode reaches Nodes (base URL, no path). Not read by the
	// Nodes binary itself.
	URL string `yaml:"url"`
	// TranscodeURL is the base URL of the transcode engine, whose
	// GET /transcode/v1/node-states Nodes reads. Empty means no transcode view.
	TranscodeURL string `yaml:"transcode_url"`
	// TranscodeKeyFile holds the key sent to the engine as X-Api-Key. Empty
	// sends none.
	TranscodeKeyFile string `yaml:"transcode_key_file"`
	// AllowNonLoopback lets Listen be an address outside loopback.
	AllowNonLoopback bool `yaml:"allow_non_loopback"`
}

// DefaultNodesListen is used when nodes.listen is empty.
const DefaultNodesListen = "127.0.0.1:8097"

// ListenAddr returns the configured listen address or the default.
func (n NodesConfig) ListenAddr() string {
	if strings.TrimSpace(n.Listen) == "" {
		return DefaultNodesListen
	}
	return strings.TrimSpace(n.Listen)
}

// CheckListen refuses a listen address that is not on loopback, unless
// allow_non_loopback is set. A wildcard or empty host is not loopback.
func (n NodesConfig) CheckListen() error {
	addr := n.ListenAddr()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("nodes.listen %q must be host:port: %w", addr, err)
	}
	if n.AllowNonLoopback {
		return nil
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("nodes.listen %q is not a loopback address; use 127.0.0.1 or set nodes.allow_non_loopback", addr)
}

// ValidateURLs checks the two base URLs when they are set.
func (n NodesConfig) ValidateURLs() error {
	for name, v := range map[string]string{"nodes.url": n.URL, "nodes.transcode_url": n.TranscodeURL} {
		if strings.TrimSpace(v) == "" {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(v))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return fmt.Errorf("%s must be an absolute http(s) URL", name)
		}
	}
	return nil
}
