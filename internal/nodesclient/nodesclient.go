// Package nodesclient is how the Transcode engine reads and writes the builds
// registry when Nodes runs as its own process (cmd/navigatorr-nodes): it
// mirrors what the engine needs of the registry (a node's running builds, the
// per-node "builds paused" switch) over GET /nodes/v1/nodes and
// POST /nodes/v1/nodes/{name}/builds-pause, sent with the Nodes service key.
//
// It imports nothing of the module except package internal (logging); the
// import guard (internal/importguard) keeps it from linking the registry
// (nodes), the engine (action, transcode*) or the store, so it has DTOs of its
// own. internal/nodeswire adapts it to action.BuildRecords.
//
// Reads:
//   - the three read methods all come from one snapshot of
//     GET /nodes/v1/nodes?transcode=false, cached for TTL (1.5 s) and fetched
//     by one flight at a time (golang.org/x/sync/singleflight, keyed by the
//     cache generation), so a screen that polls every second and a tool call
//     together cost one request;
//   - a failure (network, timeout, non-200, bad JSON, 401/403) has no stale
//     window: records read as empty, the pause reads return ErrUnavailable,
//     and no new request is sent for FailBackoff (5 s);
//   - the request inside a flight never uses a caller's context: a caller that
//     gives up gets its own ctx error and nothing else changes.
//
// Writes (SetBuildsPause) are one request, no retry. The error says whether
// the pause may have changed (see the messages below), and only a success, or
// an answer that leaves it unknown, bumps the generation so no read that was
// already in flight can store its old result.
package nodesclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/jakenesler/navigatorr/internal"
)

// Defaults of Options.
const (
	DefaultTTL          = 1500 * time.Millisecond
	DefaultFailBackoff  = 5 * time.Second
	DefaultReadTimeout  = 2 * time.Second
	DefaultWriteTimeout = 5 * time.Second
	DefaultDialTimeout  = 500 * time.Millisecond
	// logEvery limits the repeated ERROR and WARN lines (refused key, name
	// drift, unreachable registry) to one per interval each.
	logEvery = time.Minute
	// maxBody bounds what is read from Nodes.
	maxBody = 4 << 20

	nodesPath = "/nodes/v1/nodes"
)

// ErrUnavailable is returned by the pause reads when the registry could not be
// read. Test it with errors.Is.
var ErrUnavailable = errors.New("builds registry not available")

// ErrUnknownNode is returned (wrapped) by SetBuildsPause when Nodes does not
// know the node.
var ErrUnknownNode = errors.New("node unknown to the builds registry")

// Messages of SetBuildsPause, exact.
const (
	msgUnreachable = "builds registry unreachable; nothing changed"
	msgNoAnswer    = "builds registry did not answer; the pause may or may not have changed, check again"
	msgRefusedKey  = "builds registry refused the service key; nothing changed"
)

// Build is a build record on a node, as Nodes lists it (RunningBuild in
// GET /nodes/v1/nodes).
type Build struct {
	ID              string `json:"id"`
	Source          string `json:"source"`
	Holder          string `json:"holder,omitempty"`
	Kind            string `json:"kind,omitempty"`
	Title           string `json:"title"`
	StartedAt       string `json:"started_at,omitempty"`
	Progress        string `json:"progress,omitempty"`
	Status          string `json:"status,omitempty"`
	Priority        string `json:"priority,omitempty"`
	PreemptMode     string `json:"preempt_mode,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
	LastHeartbeatAt string `json:"last_heartbeat_at,omitempty"`
	ExpiresAt       string `json:"expires_at,omitempty"`
}

type nodeView struct {
	Name              string  `json:"name"`
	BuildsPaused      bool    `json:"builds_paused"`
	BuildsPausedSince string  `json:"builds_paused_since,omitempty"`
	RunningBuilds     []Build `json:"running_builds"`
}

// Logger is where the client reports. Messages carry node names and statuses,
// never the key.
type Logger interface {
	Errorf(format string, args ...any)
	Warnf(format string, args ...any)
}

type defaultLogger struct{}

func (defaultLogger) Errorf(format string, args ...any) { internal.Errorf(format, args...) }
func (defaultLogger) Warnf(format string, args ...any)  { internal.Warnf(format, args...) }

// Options configures New.
type Options struct {
	// BaseURL is the Nodes server, scheme and host only (http://127.0.0.1:PORT).
	BaseURL string
	// Key is the Nodes service key sent as X-Api-Key.
	Key string
	// Logger defaults to the repository's stderr logger.
	Logger Logger
	// KnownNodes is the node names this server has configured. When Nodes
	// reports a different set, the client logs an ERROR (at most once a
	// minute). Nil turns the check off.
	KnownNodes func() []string

	// Now is the clock of the cache, the backoff and the log limiter.
	Now func() time.Time
	// Timeouts, injectable for tests; zero values take the Default constants.
	TTL, FailBackoff, ReadTimeout, WriteTimeout, DialTimeout time.Duration
}

type snapshot struct {
	nodes  map[string]nodeView
	names  []string
	pauses map[string]string
}

// Client reads and writes the builds registry. Safe for concurrent use.
type Client struct {
	base       string
	key        string
	log        Logger
	known      func() []string
	now        func() time.Time
	ttl, back  time.Duration
	rto, wto   time.Duration
	httpClient *http.Client
	tr         *http.Transport

	sf singleflight.Group

	mu          sync.Mutex
	gen         uint64
	snap        *snapshot
	snapUntil   time.Time
	failUntil   time.Time
	lastAuthLog time.Time
	lastWarnLog time.Time
	lastDrift   time.Time
}

// New builds the client. The base URL must be http or https with a host and no
// path, query or user info; the key must be visible ASCII.
func New(o Options) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(o.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("nodesclient: base URL must be http(s)://host[:port]")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("nodesclient: base URL must have no path, query or user info")
	}
	key := strings.TrimSpace(o.Key)
	if key == "" {
		return nil, errors.New("nodesclient: the service key is empty")
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return nil, errors.New("nodesclient: the service key must be visible ASCII")
		}
	}
	c := &Client{
		base: u.Scheme + "://" + u.Host, key: key, log: o.Logger, known: o.KnownNodes, now: o.Now,
		ttl: or(o.TTL, DefaultTTL), back: or(o.FailBackoff, DefaultFailBackoff),
		rto: or(o.ReadTimeout, DefaultReadTimeout), wto: or(o.WriteTimeout, DefaultWriteTimeout),
	}
	if c.log == nil {
		c.log = defaultLogger{}
	}
	if c.now == nil {
		c.now = time.Now
	}
	c.tr = &http.Transport{
		// No proxy from the environment: Nodes is a neighbour, not a site.
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: or(o.DialTimeout, DefaultDialTimeout)}).DialContext,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 0, // each request carries its own deadline
	}
	c.httpClient = &http.Client{Transport: c.tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c, nil
}

func or(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// Close releases the idle connections.
func (c *Client) Close() { c.tr.CloseIdleConnections() }

// NodeBuildRecords is the node's running builds as Nodes lists them, oldest
// first as Nodes sends them. Empty when the registry cannot be read.
func (c *Client) NodeBuildRecords(node string) []Build {
	out, _ := c.nodeBuildRecords(context.Background(), node)
	return out
}

func (c *Client) nodeBuildRecords(ctx context.Context, node string) ([]Build, error) {
	s, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	v, ok := s.nodes[node]
	if !ok || len(v.RunningBuilds) == 0 {
		return nil, nil
	}
	return append([]Build(nil), v.RunningBuilds...), nil
}

// BuildsPauses is the nodes whose builds are paused, with the time each pause
// began. ErrUnavailable when the registry cannot be read.
func (c *Client) BuildsPauses() (map[string]string, error) {
	return c.buildsPauses(context.Background())
}

func (c *Client) buildsPauses(ctx context.Context) (map[string]string, error) {
	s, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(s.pauses))
	for k, v := range s.pauses {
		out[k] = v
	}
	return out, nil
}

// BuildsPaused reports whether new builds on the node are refused.
// ErrUnavailable when the registry cannot be read.
func (c *Client) BuildsPaused(node string) (bool, error) {
	return c.buildsPaused(context.Background(), node)
}

func (c *Client) buildsPaused(ctx context.Context, node string) (bool, error) {
	s, err := c.read(ctx)
	if err != nil {
		return false, err
	}
	_, paused := s.pauses[node]
	return paused, nil
}

// read returns the cached snapshot, or the one a flight fetches. A caller whose
// ctx ends gets ctx.Err() and nothing else: the flight goes on for the others
// and the cache and the backoff are untouched.
func (c *Client) read(ctx context.Context) (*snapshot, error) {
	c.mu.Lock()
	now := c.now()
	if c.snap != nil && now.Before(c.snapUntil) {
		s := c.snap
		c.mu.Unlock()
		return s, nil
	}
	if now.Before(c.failUntil) {
		c.mu.Unlock()
		return nil, ErrUnavailable
	}
	gen := c.gen
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ch := c.sf.DoChan(strconv.FormatUint(gen, 10), func() (any, error) { return c.fetch(gen) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.(*snapshot), nil
	}
}

// fetch is the body of a flight: one request, its own timeout, never a caller's
// context. It stores its result only if the generation is still gen.
func (c *Client) fetch(gen uint64) (any, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), c.rto)
	defer cancel()
	s, err := c.request(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if err != nil {
		if c.gen == gen {
			c.snap, c.failUntil = nil, now.Add(c.back)
		}
		return nil, ErrUnavailable
	}
	if c.gen == gen {
		c.snap, c.snapUntil, c.failUntil = s, now.Add(c.ttl), time.Time{}
		c.checkDriftLocked(s, now)
	}
	return s, nil
}

type statusError struct{ code int }

func (e *statusError) Error() string { return "builds registry answered HTTP " + strconv.Itoa(e.code) }

func (c *Client) request(ctx context.Context) (*snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+nodesPath+"?transcode=false", nil)
	if err != nil {
		c.failed(err)
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("Accept", "application/json")
	res, err := c.httpClient.Do(req)
	if err != nil {
		c.failed(err)
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		err := &statusError{res.StatusCode}
		c.failed(err)
		return nil, err
	}
	var body struct {
		Nodes *[]nodeView `json:"nodes"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxBody)).Decode(&body); err != nil || body.Nodes == nil {
		if err == nil {
			err = errors.New("no nodes in the answer")
		}
		err = fmt.Errorf("builds registry sent a body that cannot be read: %w", err)
		c.failed(err)
		return nil, err
	}
	s := &snapshot{nodes: make(map[string]nodeView, len(*body.Nodes)), pauses: map[string]string{}}
	for _, n := range *body.Nodes {
		s.nodes[n.Name] = n
		s.names = append(s.names, n.Name)
		if n.BuildsPaused {
			s.pauses[n.Name] = n.BuildsPausedSince
		}
	}
	sort.Strings(s.names)
	return s, nil
}

// limited reports whether a log line guarded by *last may be written now.
func (c *Client) limited(last *time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if !last.IsZero() && now.Sub(*last) < logEvery {
		return false
	}
	*last = now
	return true
}

// failed logs why a read failed: ERROR for a refused key, WARN otherwise, each
// limited to one a minute.
func (c *Client) failed(err error) {
	var se *statusError
	if errors.As(err, &se) && (se.code == http.StatusUnauthorized || se.code == http.StatusForbidden) {
		if c.limited(&c.lastAuthLog) {
			c.log.Errorf("nodes: the builds registry refused the service key (HTTP %d); check nodes.key_file on both sides", se.code)
		}
		return
	}
	if c.limited(&c.lastWarnLog) {
		c.log.Warnf("nodes: the builds registry could not be read: %v", err)
	}
}

// checkDriftLocked logs an ERROR (once a minute) when the node names Nodes
// reports are not the ones this server has configured. c.mu is held.
func (c *Client) checkDriftLocked(s *snapshot, now time.Time) {
	if c.known == nil {
		return
	}
	want := append([]string(nil), c.known()...)
	sort.Strings(want)
	if equalStrings(want, s.names) {
		return
	}
	if !c.lastDrift.IsZero() && now.Sub(c.lastDrift) < logEvery {
		return
	}
	c.lastDrift = now
	c.log.Errorf("nodes: node names differ between this server %v and the builds registry %v; builds are matched by name, so a node missing on either side shows no builds or pause", want, s.names)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// SetBuildsPause pauses or resumes new builds on a node: one request, no retry.
// It forgets the cache (and the backoff) when the pause changed or may have
// changed, so the next read goes to Nodes.
func (c *Client) SetBuildsPause(ctx context.Context, node string, paused bool) error {
	ctx, cancel := context.WithTimeout(ctx, c.wto)
	defer cancel()
	payload, _ := json.Marshal(map[string]bool{"paused": paused})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+nodesPath+"/"+url.PathEscape(node)+"/builds-pause", strings.NewReader(string(payload)))
	if err != nil {
		return errors.New(msgUnreachable)
	}
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// The request counts as sent once its head has gone out: from then on the
	// registry may have acted on it.
	var sent atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteHeaders: func() { sent.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) { sent.Store(true) },
	}))
	res, err := c.httpClient.Do(req)
	if err != nil {
		if !sent.Load() {
			return errors.New(msgUnreachable)
		}
		c.forget()
		return errors.New(msgNoAnswer)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		c.forget()
		return nil
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		if c.limited(&c.lastAuthLog) {
			c.log.Errorf("nodes: the builds registry refused the service key (HTTP %d); check nodes.key_file on both sides", res.StatusCode)
		}
		return errors.New(msgRefusedKey)
	}
	var env struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(raw, &env)
	if res.StatusCode == http.StatusNotFound && env.Code == "unknown_node" {
		return fmt.Errorf("%w: %s; nothing changed", ErrUnknownNode, node)
	}
	msg := strings.TrimSpace(env.Error)
	if msg == "" {
		msg = "no message"
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return fmt.Errorf("builds registry refused the change (HTTP %d: %s); nothing changed", res.StatusCode, msg)
}

// forget drops the snapshot and the backoff and opens a new generation, so a
// flight that began before this call stores nothing and the next read asks
// Nodes.
func (c *Client) forget() {
	c.mu.Lock()
	c.gen++
	c.snap, c.snapUntil, c.failUntil = nil, time.Time{}, time.Time{}
	c.mu.Unlock()
}
