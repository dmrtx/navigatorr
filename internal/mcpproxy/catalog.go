package mcpproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// catalog caches the descriptors of the proxied tools. Each descriptor is
// kept as the raw bytes Transcode sent, so what the gateway publishes is
// byte for byte what the single process publishes.
//
// The cache is replaced atomically and only by a complete refresh. A failed
// refresh keeps the last good catalog and is not retried for a while. With no
// catalog at all there is nothing to serve, and every request tries again, so
// the gateway recovers as soon as Transcode is up.
type catalog struct {
	up       *upstream
	names    []string
	want     map[string]struct{}
	ttl      time.Duration
	budget   time.Duration
	backoff  time.Duration
	maxPages int
	now      func() time.Time
	logf     func(string, ...any)
	errorf   func(string, ...any)

	sf singleflight.Group

	mu      sync.Mutex
	tools   []json.RawMessage // in names order
	have    bool
	fetched time.Time
	retryAt time.Time
}

func (c *catalog) get(ctx context.Context) ([]json.RawMessage, error) {
	c.mu.Lock()
	if c.have {
		now := c.now()
		if now.Sub(c.fetched) < c.ttl || now.Before(c.retryAt) {
			t := c.tools
			c.mu.Unlock()
			return t, nil
		}
	}
	c.mu.Unlock()

	ch := c.sf.DoChan("refresh", c.refresh)
	select {
	case r := <-ch:
		if r.Err == nil {
			return r.Val.([]json.RawMessage), nil
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.have {
			return c.tools, nil
		}
		return nil, r.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *catalog) refresh() (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.budget)
	defer cancel()
	tools, err := c.fetch(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.logf("Transcode tool catalog refresh failed: %v", err)
		if c.have {
			c.retryAt = c.now().Add(c.backoff)
		}
		return nil, err
	}
	c.tools, c.have, c.fetched, c.retryAt = tools, true, c.now(), time.Time{}
	return tools, nil
}

// fetch reads every page of Transcode's tools/list. Any failure aborts the
// whole refresh; the next one starts again from the first page.
func (c *catalog) fetch(ctx context.Context) ([]json.RawMessage, error) {
	found := make(map[string]json.RawMessage, len(c.names))
	seen := map[string]struct{}{}
	cursor := ""
	for page := 0; ; page++ {
		if page >= c.maxPages {
			return nil, fmt.Errorf("catalog has more than %d pages", c.maxPages)
		}
		params := json.RawMessage(`{}`)
		if cursor != "" {
			params, _ = json.Marshal(map[string]string{"cursor": cursor})
		}
		// Only the first page may start over on a new session; a session
		// change in the middle would leave the cursor meaningless.
		raw, cerr := c.up.do(ctx, c.up.nextID(), "tools/list", params, page == 0)
		if cerr != nil {
			return nil, fmt.Errorf("page %d: %w", page+1, cerr)
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("page %d: %w", page+1, err)
		}
		if isNull(env.Result) {
			return nil, fmt.Errorf("page %d: upstream error %s", page+1, string(env.Error))
		}
		var res struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(env.Result, &res); err != nil {
			return nil, fmt.Errorf("page %d: %w", page+1, err)
		}
		for _, t := range res.Tools {
			var d struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(t, &d); err != nil || d.Name == "" {
				return nil, fmt.Errorf("page %d: tool without a name", page+1)
			}
			if _, ok := c.want[d.Name]; !ok {
				continue
			}
			if _, dup := found[d.Name]; dup {
				return nil, fmt.Errorf("tool %q listed twice", d.Name)
			}
			found[d.Name] = t
		}
		if res.NextCursor == "" {
			break
		}
		if _, dup := seen[res.NextCursor]; dup {
			return nil, errors.New("catalog cursor repeated")
		}
		seen[res.NextCursor] = struct{}{}
		cursor = res.NextCursor
	}

	var missing []string
	out := make([]json.RawMessage, 0, len(c.names))
	for _, n := range c.names {
		t, ok := found[n]
		if !ok {
			missing = append(missing, n)
			continue
		}
		out = append(out, t)
	}
	if len(missing) > 0 {
		c.errorf("Transcode catalog lacks proxied tools %v: keeping the previous catalog", missing)
		return nil, fmt.Errorf("catalog lacks %d proxied tools", len(missing))
	}
	return out, nil
}
