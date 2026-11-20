package config

import (
	"fmt"
	"net/url"
)

// GatewayConfig holds the settings only the stateless MCP gateway
// (cmd/navigatorr-gateway) reads. The server ignores this block, so both
// binaries can be given the same file. Services, download clients and
// max_response_size_kb come from the usual top-level keys.
type GatewayConfig struct {
	// Listen is the gateway's address. Defaults to 127.0.0.1:8099.
	Listen string `yaml:"listen"`
	// UpstreamURL is the internal /mcp endpoint of the Transcode server.
	UpstreamURL string `yaml:"upstream_url"`
	// KeyFile optionally holds the shared key sent to Transcode as X-Api-Key.
	// Empty means no key is sent.
	KeyFile string `yaml:"key_file"`
	// CacheDir is where downloaded OpenAPI specs are cached. Empty means
	// ~/.cache/navigatorr.
	CacheDir string `yaml:"cache_dir"`
}

// DefaultGatewayListen is used when gateway.listen is empty.
const DefaultGatewayListen = "127.0.0.1:8099"

// ListenAddr returns the configured listen address or the default.
func (g GatewayConfig) ListenAddr() string {
	if g.Listen == "" {
		return DefaultGatewayListen
	}
	return g.Listen
}

// Validate checks the settings the gateway cannot start without.
func (g GatewayConfig) Validate() error {
	u, err := url.Parse(g.UpstreamURL)
	if g.UpstreamURL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("gateway.upstream_url must be the absolute http(s) URL of the Transcode /mcp endpoint")
	}
	return nil
}
