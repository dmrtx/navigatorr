// Package mcpproxy is the frontier of the stateless MCP gateway. It sits in
// front of mcp-go's streamable-http server: the gateway's own tools are served
// by the SDK, and a fixed list of stateful tools is forwarded to Transcode
// over MCP. The rules mirror the reviewed connector proxy in front of it: one
// response per call, no progress, no server-initiated requests, and a call is
// only ever repeated when it is proven not to have run.
package mcpproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/jakenesler/navigatorr/internal"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	maxRequestBody = 1 << 20

	// MsgNotRun and MsgUnknown are what the model reads when a proxied call
	// fails. Only the first claims the call did not run.
	MsgNotRun  = "No ejecutada; Transcode no disponible, se puede repetir"
	MsgUnknown = "Resultado desconocido: puede haberse ejecutado; comprueba antes de repetir"
)

// bodyReadTimeout bounds how long a client may take to send a request body.
// The server has no ReadTimeout (it would also cut the SDK's long-lived GET
// streams), so a client that sends headers and then trickles or withholds the
// body would otherwise hold a goroutine and delay shutdown. A variable so
// tests can shorten it.
var bodyReadTimeout = 15 * time.Second

// Options configures a Frontier. Only Server, Next and UpstreamURL are
// required; the rest default to production values and exist for tests.
type Options struct {
	// Server holds the gateway's own tools; their descriptors are read from
	// it in memory.
	Server *server.MCPServer
	// Next is the SDK's streamable-http handler, which serves everything the
	// frontier does not intercept.
	Next http.Handler
	// UpstreamURL is Transcode's internal /mcp endpoint.
	UpstreamURL string
	// APIKey, if set, is sent upstream as X-Api-Key.
	APIKey string

	HTTPClient     *http.Client
	CallBudget     time.Duration // whole proxied call, reinit and retry included (55 s)
	InitTimeout    time.Duration // initialize + notifications/initialized (5 s)
	CatalogTTL     time.Duration // 60 s
	RefreshBudget  time.Duration // one catalog refresh, all pages (10 s)
	FailureBackoff time.Duration // pause after a failed refresh with a cache (60 s)
	MaxPages       int           // 10
	Now            func() time.Time
	Logf, Errorf   func(string, ...any)
}

// Frontier is the http.Handler placed in front of the SDK.
type Frontier struct {
	next     http.Handler
	srv      *server.MCPServer
	up       *upstream
	cat      *catalog
	proxied  map[string]struct{}
	sessions server.StatelessGeneratingSessionIdManager
	logf     func(string, ...any)
	errorf   func(string, ...any)
	budget   time.Duration
}

// New builds the frontier. It refuses to start when a tool the gateway
// registers itself is also in the proxy list.
func New(o Options) (*Frontier, error) {
	if o.Server == nil || o.Next == nil {
		return nil, fmt.Errorf("mcpproxy: Server and Next are required")
	}
	u, err := url.Parse(o.UpstreamURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("mcpproxy: upstream URL must be an absolute http(s) URL")
	}
	if o.Logf == nil {
		o.Logf = internal.Logf
	}
	if o.Errorf == nil {
		o.Errorf = internal.Errorf
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.CallBudget, 55*time.Second)
	def(&o.InitTimeout, 5*time.Second)
	def(&o.CatalogTTL, 60*time.Second)
	def(&o.RefreshBudget, 10*time.Second)
	def(&o.FailureBackoff, 60*time.Second)
	if o.MaxPages <= 0 {
		o.MaxPages = 10
	}

	f := &Frontier{
		next:    o.Next,
		srv:     o.Server,
		proxied: map[string]struct{}{},
		budget:  o.CallBudget,
		logf:    o.Logf,
		errorf:  o.Errorf,
	}
	names := ProxyTools()
	for _, n := range names {
		f.proxied[n] = struct{}{}
	}
	own, err := f.ownTools(context.Background())
	if err != nil {
		return nil, err
	}
	ownNames := make([]string, len(own))
	for i, d := range own {
		ownNames[i] = d.name
	}
	if err := CheckDisjoint(ownNames); err != nil {
		return nil, err
	}

	f.up = newUpstream(o.UpstreamURL, o.APIKey, o.HTTPClient, o.InitTimeout, o.Logf, o.Errorf)
	f.cat = &catalog{
		up: f.up, names: names, want: f.proxied,
		ttl: o.CatalogTTL, budget: o.RefreshBudget, backoff: o.FailureBackoff,
		maxPages: o.MaxPages, now: o.Now, logf: o.Logf, errorf: o.Errorf,
	}
	return f, nil
}

// Warm loads the catalog once at start-up. A failure is only logged: the
// catalog is retried lazily on the next tools/list.
func (f *Frontier) Warm(ctx context.Context) error {
	_, err := f.cat.get(ctx)
	return err
}

// Close ends the upstream session. Call it after the HTTP server has drained.
func (f *Frontier) Close(ctx context.Context) { f.up.terminate(ctx) }

func (f *Frontier) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every request gets the read deadline: whatever body is left unread,
	// the server drains it after the handler, and a slow client must not
	// hold that drain. Only a JSON POST, read in full below, clears it before
	// a long call; the SDK answers the rest at once (GET streams are off).
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	if r.Method != http.MethodPost {
		f.next.ServeHTTP(w, r)
		return
	}
	// Let the SDK answer anything that is not JSON, with its own error. The
	// deadline stays: the SDK answers at once, and the server's drain of an
	// unread body must not wait on a slow client.
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		f.next.ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		// Keep the deadline and close the connection: otherwise the server
		// would wait, with no deadline, to drain the rest of the body.
		w.Header().Set("Connection", "close")
		writeJSON(w, rpcError(nil, mcp.PARSE_ERROR, "read request body error"))
		return
	}
	if len(body) > maxRequestBody {
		// The rest of the body is never read: keep the deadline and close
		// the connection, or the server would drain it with no deadline.
		w.Header().Set("Connection", "close")
		writeJSON(w, rpcError(nil, mcp.INVALID_REQUEST, "request body is larger than 1 MiB"))
		return
	}
	_ = rc.SetReadDeadline(time.Time{})
	// Whatever is delegated must see the body again.
	restore := func() {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	restore()

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		writeJSON(w, rpcError(nil, mcp.INVALID_REQUEST, "JSON-RPC batches are not supported"))
		return
	}
	var env envelope
	if json.Unmarshal(body, &env) != nil {
		f.next.ServeHTTP(w, r) // the SDK reports the parse error
		return
	}

	switch env.Method {
	case "tools/list":
	case "tools/call":
		var p struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(env.Params, &p) != nil {
			f.next.ServeHTTP(w, r)
			return
		}
		if _, ok := f.proxied[p.Name]; !ok {
			// Own or unknown: the SDK serves it, or answers tool-not-found,
			// without Transcode being involved.
			f.next.ServeHTTP(w, r)
			return
		}
	default:
		f.next.ServeHTTP(w, r)
		return
	}

	// Same session check, same answer, as the SDK's default manager.
	if _, err := f.sessions.Validate(r.Header.Get(server.HeaderKeySessionID)); err != nil {
		http.Error(w, "Invalid session ID", http.StatusNotFound)
		return
	}
	if !validID(env.ID) {
		writeJSON(w, rpcError(nil, mcp.INVALID_REQUEST, "a request needs jsonrpc 2.0 and a string or number id"))
		return
	}
	if env.JSONRPC != mcp.JSONRPC_VERSION {
		// The id is usable, so the client can match the answer.
		writeJSON(w, rpcError(env.ID, mcp.INVALID_REQUEST, "a request needs jsonrpc 2.0 and a string or number id"))
		return
	}

	if env.Method == "tools/list" {
		f.listTools(r.Context(), w, env.ID)
		return
	}
	f.callTool(r.Context(), w, env)
}

func writeJSON(w http.ResponseWriter, b []byte) {
	// Always 200: the connector turns any other status into a generic 502.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (f *Frontier) callTool(ctx context.Context, w http.ResponseWriter, env envelope) {
	ctx, cancel := context.WithTimeout(ctx, f.budget)
	defer cancel()
	raw, cerr := f.up.do(ctx, env.ID, "tools/call", env.Params, true)
	if cerr == nil {
		writeJSON(w, raw)
		return
	}
	f.logf("proxied tools/call failed (%s): %v", kindName(cerr.kind), cerr)
	msg := MsgUnknown
	if cerr.kind == failNotDispatched {
		msg = MsgNotRun
	}
	writeJSON(w, toolError(env.ID, msg))
}

func kindName(k failKind) string {
	if k == failNotDispatched {
		return "not dispatched"
	}
	return "outcome unknown"
}

type descriptor struct {
	name string
	raw  json.RawMessage
}

// ownTools reads the gateway's own descriptors from the SDK in memory.
func (f *Frontier) ownTools(ctx context.Context) ([]descriptor, error) {
	msg := f.srv.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":0,"method":"tools/list"}`))
	b, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	var res struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	if res.Error != nil {
		return nil, fmt.Errorf("listing own tools: %s", res.Error.Message)
	}
	out := make([]descriptor, 0, len(res.Result.Tools))
	for _, t := range res.Result.Tools {
		var d struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(t, &d); err != nil {
			return nil, err
		}
		out = append(out, descriptor{name: d.Name, raw: t})
	}
	return out, nil
}

// listTools answers tools/list with the own tools plus the cached catalog.
// Without a catalog it answers a JSON-RPC error rather than a short list: the
// connector announces listChanged=false and would never ask again.
func (f *Frontier) listTools(ctx context.Context, w http.ResponseWriter, id json.RawMessage) {
	proxied, err := f.cat.get(ctx)
	if err != nil {
		writeJSON(w, rpcError(id, mcp.INTERNAL_ERROR, "Transcode tool catalog is not available yet"))
		return
	}
	own, err := f.ownTools(ctx)
	if err != nil {
		f.errorf("listing own tools: %v", err)
		writeJSON(w, rpcError(id, mcp.INTERNAL_ERROR, "internal error"))
		return
	}
	all := own
	for _, raw := range proxied {
		var d struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &d)
		all = append(all, descriptor{name: d.Name, raw: raw})
	}
	// The SDK sorts by name; keep that order for the merged list.
	sort.SliceStable(all, func(i, j int) bool { return all[i].name < all[j].name })

	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.Write(id)
	b.WriteString(`,"result":{"tools":[`)
	for i, d := range all {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(d.raw)
	}
	b.WriteString(`]}}`)
	writeJSON(w, b.Bytes())
}
