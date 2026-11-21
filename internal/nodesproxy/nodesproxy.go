// Package nodesproxy is the reverse proxy the Transcode server uses to reach
// the separate Nodes process (cmd/navigatorr-nodes): the builds routes,
// legacy (/api/maintenance/builds*) and successor (/nodes/v1/*), are forwarded
// as they came in, once Transcode's own authentication has accepted them.
//
// It imports nothing of the module except package internal (logging); the
// import guard (internal/importguard) keeps it from linking the registry
// (nodes), the engine (action, transcode*), the store or the UI. It therefore
// knows the two build-log paths by value, and a test in maintenanceui pins them
// to the constants of package nodes.
//
// What it does to a request:
//   - strips every credential and proxy header the client sent (Authorization,
//     X-Api-Key, Cookie, Origin, X-Navigatorr-Request, Forwarded, X-Forwarded-*,
//     X-Real-IP, Cf-Access-*), then sets X-Api-Key to the Nodes service key;
//   - keeps method, path, query and body unchanged;
//   - cancels the upstream request when the client goes away.
//
// What it does to an answer:
//   - passes every status and body through unchanged (429 and 5xx included),
//     except 401 and 403: Nodes only answers those when the service key is out
//     of sync, which is an operator problem, not the caller's, so the client
//     gets a 503 (Retry-After: 5) and the proxy logs an ERROR;
//   - turns "Nodes cannot be reached" (refused, reset, timed out before the
//     response headers) into the same 503 with Retry-After: 5, so a worker
//     that treats 5xx as "try later" (N3) never sees a 400, 404 or 409 that
//     Nodes did not send;
//   - once the answer has started, a failure aborts the client connection
//     (http.ErrAbortHandler, raised by httputil.ReverseProxy) instead of
//     leaving a truncated body that looks complete. Nothing between the proxy
//     and net/http may recover that panic.
package nodesproxy

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jakenesler/navigatorr/internal"
)

// Defaults of Options.
const (
	DefaultDialTimeout           = 500 * time.Millisecond
	DefaultResponseHeaderTimeout = 10 * time.Second
	// DefaultLongPollHeaderTimeout covers the longest build-log long-poll
	// (25 s in Nodes) with room to answer.
	DefaultLongPollHeaderTimeout = 30 * time.Second
	DefaultIdleConnTimeout       = 30 * time.Second
	// retryAfter is the Retry-After (seconds) of a 503 the proxy writes.
	retryAfter = "5"
	// warnEvery limits the "not reachable" warning to one per interval, so a
	// stopped Nodes does not fill the log at the workers' retry rate.
	warnEvery = 30 * time.Second
)

// clockBase anchors the warning limiter to the monotonic clock.
var clockBase = time.Now()

// Logger is where the proxy reports. Messages carry no request data beyond the
// method and path (a query can carry names).
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

	// Timeouts, injectable for tests; zero values take the Default constants.
	DialTimeout           time.Duration
	ResponseHeaderTimeout time.Duration
	LongPollHeaderTimeout time.Duration
}

// errKeyRejected is what ModifyResponse returns for a 401 or 403 from Nodes.
var errKeyRejected = errors.New("nodes rejected the service key")

// Handler is the proxy. Close releases its idle connections.
type Handler struct {
	rp        *httputil.ReverseProxy
	tr        *router
	log       Logger
	last      atomic.Int64 // unix nano of the last "not reachable" warning
	warnEvery time.Duration
}

// New builds the proxy. The base URL must be http or https with a host and no
// path, query or user info; the key must be visible ASCII (it travels in a
// header).
func New(o Options) (*Handler, error) {
	u, err := url.Parse(strings.TrimSpace(o.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("nodesproxy: base URL must be http(s)://host[:port]")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("nodesproxy: base URL must have no path, query or user info")
	}
	key := strings.TrimSpace(o.Key)
	if key == "" {
		return nil, errors.New("nodesproxy: the service key is empty")
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return nil, errors.New("nodesproxy: the service key must be visible ASCII")
		}
	}
	h := &Handler{log: o.Logger, warnEvery: warnEvery}
	if h.log == nil {
		h.log = defaultLogger{}
	}
	h.tr = newRouter(or(o.DialTimeout, DefaultDialTimeout), or(o.ResponseHeaderTimeout, DefaultResponseHeaderTimeout), or(o.LongPollHeaderTimeout, DefaultLongPollHeaderTimeout))
	target := &url.URL{Scheme: u.Scheme, Host: u.Host}
	h.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
			pr.Out.Host = target.Host
			scrub(pr.Out.Header)
			pr.Out.Header.Set("X-Api-Key", key)
		},
		Transport: h.tr,
		ModifyResponse: func(res *http.Response) error {
			if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
				return errKeyRejected
			}
			return nil
		},
		ErrorHandler: h.fail,
		// httputil.ReverseProxy reports a failed copy of the answer here (and
		// then aborts the connection).
		ErrorLog: log.New(logWriter{h.log}, "", 0),
	}
	return h, nil
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Close drops the idle connections to Nodes.
func (h *Handler) Close() { h.tr.close() }

// ServeHTTP forwards the request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.rp.ServeHTTP(w, r) }

// scrub removes what must not reach Nodes: the client's credentials, its
// browser-context headers and everything that names the proxy chain.
func scrub(h http.Header) {
	for _, k := range []string{"Authorization", "X-Api-Key", "Cookie", "Origin", "X-Navigatorr-Request", "Forwarded", "X-Real-Ip"} {
		h.Del(k)
	}
	for k := range h {
		l := strings.ToLower(k)
		if strings.HasPrefix(l, "x-forwarded-") || strings.HasPrefix(l, "cf-access-") {
			delete(h, k)
		}
	}
}

// unavailable is the one body every "Nodes is not usable" answer carries.
type unavailable struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	Hint  string `json:"hint"`
}

// WriteUnavailable writes the 503 the builds routes answer when the registry
// cannot be reached: the JSON body, Retry-After: 5.
func WriteUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", retryAfter)
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(unavailable{
		Error: "the builds registry is not reachable",
		Code:  "unavailable",
		Hint:  "builds are recorded by a separate service that did not answer; retry in a few seconds (a running build is not affected)",
	})
}

// fail is the ReverseProxy's ErrorHandler: it runs only before any byte of the
// answer was written.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errKeyRejected) {
		h.log.Errorf("nodesproxy: the Nodes service answered 401/403 to %s %s: its key does not match nodes.key_file", r.Method, r.URL.Path)
		WriteUnavailable(w)
		return
	}
	if errors.Is(r.Context().Err(), context.Canceled) {
		// The client left; nobody reads an answer. Any other cause (a deadline
		// on the request, a transport that reports Canceled on its own) still
		// gets the 503: a caller that is waiting never sees an empty 200.
		return
	}
	now := int64(time.Since(clockBase)) + 1 // monotonic, never 0 (0 means "never warned")
	if last := h.last.Load(); (last == 0 || now-last > int64(h.warnEvery)) && h.last.CompareAndSwap(last, now) {
		h.log.Warnf("nodesproxy: the Nodes service is not reachable for %s %s: %s", r.Method, r.URL.Path, describe(err))
	}
	WriteUnavailable(w)
}

// describe is a log-safe cause: the error text of a dial failure carries the
// address, which is configuration and fine, but never a request value.
func describe(err error) string {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return "timed out"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	return err.Error()
}

type logWriter struct{ l Logger }

func (w logWriter) Write(p []byte) (int, error) {
	w.l.Warnf("nodesproxy: %s", strings.TrimSpace(string(p)))
	return len(p), nil
}

// router picks the transport of a request: the long one for a build-log
// long-poll, the short one for everything else.
type router struct{ short, long *http.Transport }

func newRouter(dial, header, longHeader time.Duration) *router {
	mk := func(rht time.Duration) *http.Transport {
		// The handshake gets the short timeout on both transports: a long-poll
		// waits for Nodes' answer, never for a connection that stalls.
		return &http.Transport{
			Proxy:       nil, // Nodes is local; never through an environment proxy
			DialContext: (&net.Dialer{Timeout: dial}).DialContext,
			// An https Nodes that accepts TCP and stalls in the handshake is
			// "not reachable" too, not a request that hangs.
			TLSHandshakeTimeout:   header,
			ResponseHeaderTimeout: rht,
			IdleConnTimeout:       DefaultIdleConnTimeout,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			// Bytes go through as they are: the client's Accept-Encoding decides.
			DisableCompression: true,
		}
	}
	return &router{short: mk(header), long: mk(longHeader)}
}

func (t *router) RoundTrip(r *http.Request) (*http.Response, error) {
	if IsLongPoll(r) {
		return t.long.RoundTrip(r)
	}
	return t.short.RoundTrip(r)
}

func (t *router) close() {
	t.short.CloseIdleConnections()
	t.long.CloseIdleConnections()
}

// Build-log paths, by value (see the package comment): the legacy route is
// {legacyBase}/{id}/log and the successor {successorBase}/{id}/log.
const (
	legacyBase    = "/api/maintenance/builds"
	successorBase = "/nodes/v1/builds"
)

// IsLongPoll is whether r is a build-log read that may hold the answer: a GET
// on a build's log path with a valid wait (a whole number above zero).
func IsLongPoll(r *http.Request) bool {
	if r.Method != http.MethodGet || !isLogPath(r.URL.Path) {
		return false
	}
	n, err := strconv.Atoi(r.URL.Query().Get("wait"))
	return err == nil && n > 0
}

func isLogPath(p string) bool {
	for _, base := range []string{legacyBase, successorBase} {
		rest, ok := strings.CutPrefix(p, base+"/")
		if !ok {
			continue
		}
		id, tail, ok := strings.Cut(rest, "/")
		return ok && id != "" && tail == "log"
	}
	return false
}

var _ http.Handler = (*Handler)(nil)
