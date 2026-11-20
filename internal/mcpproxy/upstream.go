package mcpproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"golang.org/x/sync/singleflight"
)

const (
	// maxBody caps one upstream response, and also one SSE event.
	maxBody = 4 << 20

	headerSession  = "Mcp-Session-Id"
	headerProtocol = "MCP-Protocol-Version"
	headerAPIKey   = "X-Api-Key"
)

// failKind says whether a failed call is known not to have run.
type failKind int

const (
	// failUnknown: the request may have been dispatched.
	failUnknown failKind = iota
	// failNotDispatched: there is proof the tools/call never reached a handler.
	failNotDispatched
)

// callError is a failed upstream exchange. retryable marks the only two cases
// that are proven not to have been dispatched and are worth one more try
// after a new session: connection refused and the exact session 404.
type callError struct {
	kind      failKind
	retryable bool
	cause     error
}

func (e *callError) Error() string { return e.cause.Error() }
func (e *callError) Unwrap() error { return e.cause }

func notDispatched(err error) *callError { return &callError{kind: failNotDispatched, cause: err} }
func unknown(err error) *callError       { return &callError{kind: failUnknown, cause: err} }

// session is one upstream MCP session. A new value is created per
// (re)initialization, so pointer identity is the compare-and-swap token.
type session struct {
	id    string
	proto string
}

// upstream is the client to Transcode's /mcp: one shared session, independent
// of the sessions of incoming clients.
type upstream struct {
	url         string
	apiKey      string
	hc          *http.Client
	initTimeout time.Duration
	logf        func(string, ...any)
	errorf      func(string, ...any)

	mu     sync.Mutex
	closed bool // set by terminate; later handshakes are ended, not published
	cur    *session
	init   singleflight.Group
	ids    atomic.Int64

	// afterCheck is a test hook, run between the fast-path check and the flight.
	afterCheck func()
}

func newUpstream(url, apiKey string, hc *http.Client, initTimeout time.Duration, logf, errorf func(string, ...any)) *upstream {
	if hc == nil {
		// No connection reuse: after Transcode restarts, a stale pooled
		// connection would fail with an ambiguous reset instead of a clean
		// refusal or 404. On the local Docker network a new connection is cheap.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DisableKeepAlives = true
		tr.Proxy = nil
		hc = &http.Client{Transport: tr}
	} else {
		c := *hc // never mutate the caller's client
		hc = &c
	}
	// A redirect would replay a tools/call (a 307 keeps the method and body)
	// at a place nobody configured, with the key attached. A 3xx is the
	// answer: outcome unknown.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &upstream{url: url, apiKey: apiKey, hc: hc, initTimeout: initTimeout, logf: logf, errorf: errorf}
}

func (u *upstream) nextID() json.RawMessage {
	return json.RawMessage(itoa(int(u.ids.Add(1))))
}

// ensure returns a published session, creating one when there is none or when
// the current one is the failed one (compare-and-swap: if another caller has
// already replaced it, that replacement is used and nothing is re-initialized).
// The handshake runs in its own context, so a caller that gives up does not
// abort it for the callers waiting on the same flight.
// errShuttingDown is returned once Close has run: no session is opened after it.
var errShuttingDown = errors.New("gateway is shutting down")

func (u *upstream) ensure(ctx context.Context, failed *session) (*session, error) {
	for attempt := 0; attempt < 2; attempt++ {
		u.mu.Lock()
		if u.closed {
			u.mu.Unlock()
			return nil, errShuttingDown
		}
		if u.cur != nil && u.cur != failed {
			s := u.cur
			u.mu.Unlock()
			return s, nil
		}
		u.mu.Unlock()
		if u.afterCheck != nil {
			u.afterCheck()
		}

		ch := u.init.DoChan("init", func() (any, error) {
			// A flight that finished between the check above and this one
			// has already replaced the session: use it, do not handshake again.
			// Close may also have run in between: then open nothing.
			u.mu.Lock()
			if u.closed {
				u.mu.Unlock()
				return nil, errShuttingDown
			}
			if u.cur != nil && u.cur != failed {
				s := u.cur
				u.mu.Unlock()
				return s, nil
			}
			u.mu.Unlock()

			hctx, cancel := context.WithTimeout(context.Background(), u.initTimeout)
			defer cancel()
			fresh, err := u.handshake(hctx)
			if err != nil {
				return nil, err
			}
			u.mu.Lock()
			if u.closed {
				// Close ran while this handshake was in flight: end the
				// session it just opened instead of publishing it.
				u.mu.Unlock()
				dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer dcancel()
				u.delete(dctx, fresh)
				return nil, errShuttingDown
			}
			defer u.mu.Unlock()
			if u.cur == nil || u.cur == failed {
				u.cur = fresh
			}
			return u.cur, nil
		})
		select {
		case r := <-ch:
			if r.Err != nil {
				return nil, r.Err
			}
			if s := r.Val.(*session); s != failed {
				return s, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("upstream session could not be replaced")
}

func (u *upstream) current() *session {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cur
}

// handshake runs initialize and notifications/initialized. The session is
// only returned, and so only published, when both succeed.
func (u *upstream) handshake(ctx context.Context) (*session, error) {
	id := u.nextID()
	params := mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "navigatorr-gateway", Version: "1"},
	}
	pb, _ := json.Marshal(params)
	body := buildRequest(id, "initialize", pb)
	raw, hdr, cerr := u.exchange(ctx, nil, body, id)
	if cerr != nil {
		return nil, fmt.Errorf("initialize: %w", cerr)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || isNull(env.Result) {
		return nil, errors.New("initialize: no result")
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(env.Result, &res); err != nil || res.ProtocolVersion == "" {
		return nil, errors.New("initialize: no protocol version")
	}
	s := &session{id: hdr.Get(headerSession), proto: res.ProtocolVersion}

	req, err := u.newRequest(ctx, http.MethodPost, s, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		return nil, err
	}
	resp, err := u.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("notifications/initialized: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Read the body for the key check before anything discards it.
		if resp.StatusCode == http.StatusUnauthorized && isKeyRejection(resp) {
			u.errorf("Transcode rejected the gateway key (HTTP 401): check the shared key file")
		}
		return nil, fmt.Errorf("notifications/initialized: HTTP %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return s, nil
}

func buildRequest(id json.RawMessage, method string, params json.RawMessage) []byte {
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.Write(id)
	b.WriteString(`,"method":`)
	m, _ := json.Marshal(method)
	b.Write(m)
	if len(params) > 0 {
		b.WriteString(`,"params":`)
		b.Write(params)
	}
	b.WriteString(`}`)
	return b.Bytes()
}

func (u *upstream) newRequest(ctx context.Context, method string, s *session, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	if u.apiKey != "" {
		req.Header.Set(headerAPIKey, u.apiKey)
	}
	if s != nil {
		if s.id != "" {
			req.Header.Set(headerSession, s.id)
		}
		if s.proto != "" {
			req.Header.Set(headerProtocol, s.proto)
		}
	}
	return req, nil
}

// do sends one JSON-RPC request through the shared session and returns the
// raw JSON-RPC response message (a result or an error object, untouched).
// With retry set, a call proven not to have been dispatched is repeated once
// on a fresh session. ctx carries the whole time budget.
func (u *upstream) do(ctx context.Context, id json.RawMessage, method string, params json.RawMessage, retry bool) ([]byte, *callError) {
	raw, _, cerr := u.doOn(ctx, nil, id, method, params, retry)
	return raw, cerr
}

// doOn is do, and also reports the session the answer came from. A non-nil
// pin keeps the request on that session: it never starts a new one, and it
// fails if the shared session is no longer the pinned one. Paged reads use it
// so a cursor is only ever sent to the session that issued it.
func (u *upstream) doOn(ctx context.Context, pin *session, id json.RawMessage, method string, params json.RawMessage, retry bool) ([]byte, *session, *callError) {
	sess := pin
	if pin != nil {
		retry = false
		if u.current() != pin {
			return nil, pin, notDispatched(errors.New("upstream session changed"))
		}
	} else {
		var err error
		if sess, err = u.ensure(ctx, nil); err != nil {
			return nil, nil, notDispatched(fmt.Errorf("upstream session: %w", err))
		}
	}
	body := buildRequest(id, method, params)
	for attempt := 0; ; attempt++ {
		raw, _, cerr := u.exchange(ctx, sess, body, id)
		if cerr == nil {
			return raw, sess, nil
		}
		if !cerr.retryable {
			return nil, sess, cerr
		}
		if !retry || attempt > 0 {
			return nil, sess, notDispatched(cerr.cause)
		}
		var err error
		if sess, err = u.ensure(ctx, sess); err != nil {
			return nil, nil, notDispatched(fmt.Errorf("upstream session: %w", err))
		}
	}
}

// exchange posts one request and reads its response. wantID is the id the
// response must carry.
func (u *upstream) exchange(ctx context.Context, s *session, body []byte, wantID json.RawMessage) ([]byte, http.Header, *callError) {
	req, err := u.newRequest(ctx, http.MethodPost, s, body)
	if err != nil {
		return nil, nil, notDispatched(err)
	}
	resp, err := u.hc.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			ce := notDispatched(err)
			ce.retryable = true
			return nil, nil, ce
		}
		return nil, nil, unknown(err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// Only the key middleware's own answer proves the request never
		// reached the MCP layer; any other 401 (a proxy, a handler) may not.
		if isKeyRejection(resp) {
			u.errorf("Transcode rejected the gateway key (HTTP 401): check the shared key file")
			return nil, nil, notDispatched(errors.New("upstream authentication failed (HTTP 401)"))
		}
		return nil, nil, unknown(errors.New("upstream HTTP 401"))
	case resp.StatusCode == http.StatusNotFound:
		if isSessionGone(resp) {
			ce := notDispatched(errors.New("upstream session no longer valid (HTTP 404)"))
			ce.retryable = true
			return nil, nil, ce
		}
		return nil, nil, unknown(errors.New("upstream HTTP 404"))
	case resp.StatusCode != http.StatusOK:
		return nil, nil, unknown(fmt.Errorf("upstream HTTP %d", resp.StatusCode))
	}

	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, nil, unknown(fmt.Errorf("upstream content type: %w", err))
	}
	lr := &io.LimitedReader{R: resp.Body, N: maxBody + 1}
	var raw []byte
	switch mt {
	case "application/json":
		b, err := io.ReadAll(lr)
		if err != nil {
			return nil, nil, unknown(err)
		}
		if lr.N <= 0 {
			return nil, nil, unknown(errors.New("upstream response over 4 MiB"))
		}
		if err := checkResponse(b, wantID); err != nil {
			return nil, nil, unknown(err)
		}
		raw = b
	case "text/event-stream":
		raw, err = readSSE(lr, wantID)
		if err != nil {
			return nil, nil, unknown(err)
		}
		if lr.N <= 0 {
			return nil, nil, unknown(errors.New("upstream response over 4 MiB"))
		}
	default:
		return nil, nil, unknown(fmt.Errorf("upstream content type %q", mt))
	}
	return raw, resp.Header, nil
}

// isSessionGone matches the 404 mcp-go answers, before dispatching anything,
// when a session id is not valid: text/plain and exactly one of two bodies, as
// http.Error writes them. Any other 404 may come from elsewhere (a proxy, a
// wrong path) and is never retried.
func isSessionGone(resp *http.Response) bool {
	b, ok := plainBody(resp, 64)
	return ok && (b == "Invalid session ID\n" || b == "Session terminated\n")
}

// isKeyRejection matches what mcpkey.Require writes: 401, text/plain and the
// body "unauthorized\n".
func isKeyRejection(resp *http.Response) bool {
	b, ok := plainBody(resp, 64)
	return ok && b == "unauthorized\n"
}

// plainBody returns the body of a text/plain response, read up to max bytes
// (a longer body is reported as not plain, so it never matches).
func plainBody(resp *http.Response, max int64) (string, bool) {
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mt != "text/plain" {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil || int64(len(b)) > max {
		return "", false
	}
	return string(b), true
}

// checkResponse validates a JSON-RPC response message: version, exactly one
// of result and error, and the expected id.
func checkResponse(raw []byte, wantID json.RawMessage) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("malformed upstream JSON: %w", err)
	}
	return checkEnvelope(&env, wantID)
}

func checkEnvelope(env *envelope, wantID json.RawMessage) error {
	if env.JSONRPC != "2.0" {
		return errors.New("upstream response is not JSON-RPC 2.0")
	}
	if env.Method != "" {
		return errors.New("upstream sent a request instead of a response")
	}
	hasResult, hasError := !isNull(env.Result), !isNull(env.Error)
	if hasResult == hasError {
		return errors.New("upstream response needs exactly one of result and error")
	}
	if !sameID(env.ID, wantID) {
		return errors.New("upstream response id does not match the request")
	}
	return nil
}

var errServerRequest = errors.New("upstream asked the client for something (sampling, elicitation or roots), which the gateway does not support")

// readSSE reads an event stream until the response to wantID arrives.
// Notifications are skipped; a request from the server aborts the call.
func readSSE(r io.Reader, wantID json.RawMessage) ([]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxBody)
	var data []byte
	pending := false
	eventType := ""

	// dispatch follows the SSE rules: an event with no data is not
	// dispatched, and neither is one whose type is set and is not "message"
	// (pings and the like). Either way the buffers start over.
	dispatch := func() ([]byte, bool, error) {
		msg, typ, had := data, eventType, pending
		data, pending, eventType = nil, false, ""
		if !had || len(msg) == 0 || (typ != "" && typ != "message") {
			return nil, false, nil
		}
		var env envelope
		if err := json.Unmarshal(msg, &env); err != nil {
			return nil, false, fmt.Errorf("malformed upstream event: %w", err)
		}
		if env.Method != "" {
			// Any id makes it a request the gateway cannot answer, however
			// odd the id; only a missing or null id is a notification.
			if !isNull(env.ID) {
				return nil, false, errServerRequest
			}
			return nil, false, nil // notification
		}
		if err := checkEnvelope(&env, wantID); err != nil {
			return nil, false, err
		}
		return msg, true, nil
	}

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			if msg, ok, err := dispatch(); err != nil || ok {
				return msg, err
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "data":
			if pending {
				data = append(data, '\n')
			}
			data = append(data, value...)
			pending = true
		case "event":
			eventType = string(value)
		} // id, retry and unknown fields are ignored
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading upstream stream: %w", err)
	}
	if msg, ok, err := dispatch(); err != nil || ok {
		return msg, err
	}
	return nil, errors.New("upstream stream ended without a response")
}

// terminate closes the upstream session. Best effort: the caller bounds ctx.
func (u *upstream) terminate(ctx context.Context) {
	u.mu.Lock()
	u.closed = true
	s := u.cur
	u.cur = nil
	u.mu.Unlock()
	u.delete(ctx, s)
}

// delete sends DELETE /mcp for one session. Best effort.
func (u *upstream) delete(ctx context.Context, s *session) {
	if s == nil || s.id == "" {
		return
	}
	req, err := u.newRequest(ctx, http.MethodDelete, s, nil)
	if err != nil {
		return
	}
	resp, err := u.hc.Do(req)
	if err != nil {
		u.logf("closing the upstream session: %v", err)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
}
