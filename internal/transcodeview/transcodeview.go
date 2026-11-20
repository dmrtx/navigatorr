// Package transcodeview reads the transcode state of each node over HTTP, from
// GET /transcode/v1/node-states of the transcode engine, for a Nodes process
// that runs apart from the engine.
//
// It imports nothing of the module (internal/importguard checks it): not
// action, nodes, store or transcode*. It therefore cannot implement
// nodes.TranscodeView itself, whose method returns nodes.TranscodeState. It
// offers States instead, and the nodes binary adapts it in a few lines:
//
//	func (a adapter) TranscodeStates(ctx context.Context) map[string]nodes.TranscodeState {
//		out := map[string]nodes.TranscodeState{}
//		states, _ := a.c.States(ctx) // an error is a soft failure: no transcode block
//		for name, s := range states {
//			out[name] = nodes.TranscodeState{Paused: s.Paused, Mode: s.Mode, Since: s.Since, HeldBy: s.HeldBy, Message: s.Message}
//		}
//		return out
//	}
//
// A failure is never an error for Nodes: States returns a nil map and an
// error, the adapter shows the nodes without a transcode block. After a
// failure the client makes no request for BackoffTTL, so a stopped engine
// costs one short attempt per backoff and nothing in between.
package transcodeview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	// Path is the engine endpoint this client reads.
	Path = "/transcode/v1/node-states"

	// RequestTimeout bounds one request, connection included.
	RequestTimeout = 750 * time.Millisecond
	// DialTimeout bounds opening the connection.
	DialTimeout = 500 * time.Millisecond
	// CacheTTL is how long a good answer is reused.
	CacheTTL = 2 * time.Second
	// BackoffTTL is how long after a failure no request is made.
	BackoffTTL = 5 * time.Second

	maxBody = 1 << 20
)

// ErrBackoff is returned, without a request, while a recent failure is backing
// off.
var ErrBackoff = errors.New("transcodeview: the engine failed recently, not asking again yet")

// State is the transcode state of one node.
type State struct {
	Paused  bool
	Mode    string
	Since   string
	HeldBy  string
	Message string
}

// wire is the JSON of the endpoint (kept apart from State so a renamed Go
// field cannot change the contract; the tests run real HTTP against the
// handler to catch drift).
type wire struct {
	Nodes []struct {
		Node    string `json:"node"`
		Paused  bool   `json:"paused"`
		Mode    string `json:"mode"`
		Since   string `json:"since"`
		HeldBy  string `json:"held_by"`
		Message string `json:"message"`
	} `json:"nodes"`
}

// Options configures a Client. BaseURL is required.
type Options struct {
	BaseURL string // e.g. http://engine:8080, no path
	APIKey  string // sent as X-Api-Key; empty sends none
	// HTTPClient replaces the default client (tests). Its timeouts are then
	// its own; the per-request timeout still applies through the context.
	HTTPClient *http.Client
	// Now is the clock (default time.Now), for the cache and the backoff.
	Now func() time.Time
}

// Client reads node states with a short cache, a single in-flight request and
// a backoff after failures. It is safe for concurrent use.
type Client struct {
	url    string
	key    string
	hc     *http.Client
	now    func() time.Time
	flight singleflight.Group

	mu       sync.Mutex
	cached   map[string]State
	cachedAt time.Time
	haveGood bool
	failedAt time.Time
	failed   bool
}

// New builds a Client.
func New(o Options) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(o.BaseURL), "/")
	if base == "" {
		return nil, errors.New("transcodeview: base URL is required")
	}
	c := &Client{url: base + Path, key: o.APIKey, hc: o.HTTPClient, now: o.Now}
	if c.now == nil {
		c.now = time.Now
	}
	if c.hc == nil {
		c.hc = &http.Client{Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: DialTimeout}).DialContext,
			TLSHandshakeTimeout: DialTimeout,
			MaxIdleConns:        2,
			IdleConnTimeout:     30 * time.Second,
		}}
	}
	return c, nil
}

// States is the transcode state of every node the engine reports, by node
// name. It returns a copy each time. On any failure (error, timeout, a status
// other than 200, a body that is not the contract) it returns a nil map and an
// error and remembers the failure for BackoffTTL. A caller whose own ctx ends
// gets ctx.Err() and nothing is recorded: the request itself goes on for the
// others.
func (c *Client) States(ctx context.Context) (map[string]State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	now := c.now()
	if c.haveGood && now.Sub(c.cachedAt) < CacheTTL {
		out := copyStates(c.cached)
		c.mu.Unlock()
		return out, nil
	}
	if c.failed && now.Sub(c.failedAt) < BackoffTTL {
		c.mu.Unlock()
		return nil, ErrBackoff
	}
	c.mu.Unlock()

	// The flight outlives any one caller: it keeps its own timeout and is not
	// cancelled when the caller that started it gives up.
	ch := c.flight.DoChan("states", func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RequestTimeout)
		defer cancel()
		got, err := c.fetch(fctx)
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			c.failed, c.failedAt = true, c.now()
			return nil, err
		}
		c.failed, c.haveGood = false, true
		c.cached, c.cachedAt = got, c.now()
		return got, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return copyStates(r.Val.(map[string]State)), nil
	}
}

func (c *Client) fetch(ctx context.Context) (map[string]State, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("transcodeview: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("transcodeview: engine answered %d", resp.StatusCode)
	}
	var w wire
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&w); err != nil {
		return nil, fmt.Errorf("transcodeview: unreadable answer: %w", err)
	}
	out := make(map[string]State, len(w.Nodes))
	for _, n := range w.Nodes {
		if n.Node == "" {
			continue
		}
		out[n.Node] = State{Paused: n.Paused, Mode: n.Mode, Since: n.Since, HeldBy: n.HeldBy, Message: n.Message}
	}
	return out, nil
}

func copyStates(in map[string]State) map[string]State {
	out := make(map[string]State, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
