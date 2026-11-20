package openapi

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var fetchClient = &http.Client{Timeout: 30 * time.Second}

// Fetch downloads an OpenAPI spec, using cache if available.
//
// A spec on a loopback address is never cached: it is served by this server
// itself (transcode, nodes), changes with every deploy, and costs nothing to
// fetch. Cached on disk, it would outlive the deploy that changed it by up to
// a day.
func Fetch(ctx context.Context, url string, cache *Cache) ([]byte, error) {
	cacheable := !isLoopbackURL(url)
	// Try cache first
	if cacheable {
		if data := cache.Get(url); data != nil {
			return data, nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/json, application/x-yaml, text/yaml")

	resp, err := fetchClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching spec from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetching spec from %s: HTTP %d", url, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading spec: %w", err)
	}

	// Cache to disk
	if cacheable {
		if err := cache.Put(url, data); err != nil {
			// Non-fatal, just log
			fmt.Fprintf(os.Stderr, "warning: failed to cache spec: %v\n", err)
		}
	}

	return data, nil
}

// isLoopbackURL reports whether raw points at this machine: localhost, a
// name under .localhost, or a loopback IP. An unparsable URL is not loopback.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
