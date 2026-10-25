package tools

import (
	"context"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/internal/nashelper"
)

// NewNASHelperClient builds the client for the configured NAS helper. It returns
// nil without an error when no helper is configured: every check then keeps
// reading files over the network mount, as before.
func NewNASHelperClient(cfg config.NASHelperConfig) (*nashelper.Client, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	token, err := nashelper.ReadToken(cfg.TokenFile)
	if err != nil {
		return nil, err
	}
	mappings := make([]nashelper.PathMapping, 0, len(cfg.PathMappings))
	for _, m := range cfg.PathMappings {
		mappings = append(mappings, nashelper.PathMapping{Local: m.Local, Remote: m.Remote})
	}
	return nashelper.NewClient(nashelper.ClientConfig{URL: cfg.URL, Token: token, Mappings: mappings})
}

// inspectNASHelper reports the helper for diagnostics. A configured helper that
// cannot be reached is "degraded", not an outage: checks fall back to the mount
// and record that they did.
func inspectNASHelper(ctx context.Context, cfg config.NASHelperConfig, client *nashelper.Client, clientErr error, check bool) map[string]any {
	info := map[string]any{
		"configured": true,
		"url":        redactURL(cfg.URL),
		"token":      "***REDACTED***",
		"status":     "not_checked",
	}
	mappings := make([]map[string]string, 0, len(cfg.PathMappings))
	for _, m := range cfg.PathMappings {
		mappings = append(mappings, map[string]string{"local": m.Local, "remote": m.Remote})
	}
	info["path_mappings"] = mappings
	if clientErr != nil {
		info["status"] = "misconfigured"
		info["error"] = clientErr.Error()
		return info
	}
	if !check || client == nil {
		return info
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	h, err := client.Health(ctx)
	info["latency_ms"] = time.Since(start).Milliseconds()
	switch {
	case err == nil:
		info["status"] = "ok"
		info["version"] = h.Version
		info["instance_id"] = h.InstanceID
		info["roots"] = h.Roots
		info["resolver"] = h.Resolver
		// A mapping whose remote side the helper does not serve would make every
		// file look missing, so say so instead of reporting a healthy helper.
		var outside []string
		for _, m := range cfg.PathMappings {
			if !underAnyRoot(m.Remote, h.Roots) {
				outside = append(outside, m.Remote)
			}
		}
		if len(outside) > 0 {
			info["status"] = "misconfigured"
			info["error"] = "path_mappings remote outside the helper's roots"
			info["remote_outside_roots"] = outside
		}
	default:
		info["status"] = "unavailable"
		info["error"] = err.Error()
	}
	return info
}

func underAnyRoot(path string, roots []string) bool {
	for _, r := range roots {
		r = strings.TrimSuffix(r, "/")
		if path == r || strings.HasPrefix(path, r+"/") {
			return true
		}
	}
	return false
}
