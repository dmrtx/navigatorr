package maintenanceui

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/internal/mcpkey"
	"github.com/jakenesler/navigatorr/internal/nodesproxy"
	"github.com/jakenesler/navigatorr/nodes"
	"github.com/jakenesler/navigatorr/transcode"
)

const testNodesKey = "synthetic-nodes-service-key-0123456789"

// nodesBinaryHandler is the handler cmd/navigatorr-nodes builds (newHandler):
// /healthz open, the spec, /nodes/v1 and the legacy builds routes behind the
// service key. It is copied here because that function lives in package main.
func nodesBinaryHandler(svc *nodes.Service, key string) http.Handler {
	api := http.NewServeMux()
	api.Handle("GET "+nodes.SpecPath, nodes.SpecHandler())
	api.Handle(nodes.BasePath+"/", svc.Handler())
	legacy := svc.LegacyHandler()
	api.Handle(nodes.LegacyBasePath, legacy)
	api.Handle(nodes.LegacyBasePath+"/", legacy)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	deny := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "authentication required", "code": "unauthorized"})
	})
	mux.Handle("/", mcpkey.RequireWith(key, api, deny))
	return mux
}

// proxyStack is a Transcode server whose builds routes go through the proxy to
// a Nodes server (the binary's handler over the registry buildsUI attached).
type proxyStack struct {
	transcode *httptest.Server
	nodes     *httptest.Server
	srv       *Server
	svc       *nodes.Service
}

func newProxyStack(t *testing.T, opts nodesproxy.Options) *proxyStack {
	t.Helper()
	return newWrappedProxyStack(t, opts, nil)
}

// newWrappedProxyStack is newProxyStack with wrap around the Nodes handler,
// set before the Nodes server starts.
func newWrappedProxyStack(t *testing.T, opts nodesproxy.Options, wrap func(http.Handler) http.Handler) *proxyStack {
	t.Helper()
	s, h := buildsUI(t)
	st := &proxyStack{srv: s, svc: s.nodes}
	nh := nodesBinaryHandler(s.nodes, testNodesKey)
	if wrap != nil {
		nh = wrap(nh)
	}
	st.nodes = httptest.NewServer(nh)
	t.Cleanup(st.nodes.Close)
	if opts.BaseURL == "" {
		opts.BaseURL = st.nodes.URL
	}
	if opts.Key == "" {
		opts.Key = testNodesKey
	}
	p, err := nodesproxy.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	// Transcode keeps no registry of its own, as it will in production: only
	// the Nodes server above holds it, so a route that skipped the proxy fails.
	s.SetNodes(nil)
	s.SetNodesProxy(p)
	st.transcode = httptest.NewServer(h)
	t.Cleanup(st.transcode.Close)
	return st
}

func newInProcessStack(t *testing.T) string {
	t.Helper()
	_, h := buildsUI(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

var rawClient = &http.Client{
	Transport:     &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 100},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Timeout:       20 * time.Second,
}

type call struct {
	method, path, body string
	hdr                map[string]string
}

func bearer() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testBuildsToken}
}
func apikey() map[string]string { return map[string]string{"X-Api-Key": testBuildsToken} }

var (
	timeRe = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)`)
	genRe  = regexp.MustCompile(`"generation":"[^"]*"`)
)

// exchange runs one call and returns a normalised transcript of the answer:
// status, every header but Date and Content-Length, and the body with
// timestamps blanked (their digits vary run to run).
func exchange(t *testing.T, base string, c call) string {
	t.Helper()
	req, err := http.NewRequest(c.method, base+c.path, strings.NewReader(c.body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range c.hdr {
		req.Header.Set(k, v)
	}
	res, err := rawClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", c.method, c.path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	var hs []string
	for k, v := range res.Header {
		if k == "Date" || k == "Content-Length" {
			continue
		}
		hs = append(hs, k+": "+strings.Join(v, ","))
	}
	sort.Strings(hs)
	body := timeRe.ReplaceAllString(string(b), "<t>")
	body = genRe.ReplaceAllString(body, `"generation":"<g>"`)
	return fmt.Sprintf("%s %s\n%d\n%s\n%s", c.method, c.path, res.StatusCode, strings.Join(hs, "\n"), body)
}

func bigTitle() string {
	return `{"target":"laptop","source":"ci","title":"` + strings.Repeat("x", 300<<10) + `"}`
}

// equivalenceCalls is the builds API exercised through Transcode, call by call.
func equivalenceCalls() []call {
	node := transcode.DefaultWorkerName
	const lg = "/api/maintenance/builds"
	const v1 = "/nodes/v1"
	return []call{
		// auth is Transcode's, before the proxy
		{"GET", lg, "", nil},
		{"GET", lg, "", map[string]string{"Authorization": "Bearer wrong-token-0123456789abcdef"}},
		{"GET", lg, "", map[string]string{"Authorization": "Bearer " + testToken, "X-Navigatorr-Request": "1"}},
		{"POST", lg, `{"target":"laptop","source":"ci","title":"x"}`, map[string]string{"Authorization": "Bearer " + testToken}},
		{"GET", v1 + "/builds", "", nil},
		{"GET", v1 + "/builds", "", map[string]string{"X-Api-Key": "wrong-key-" + "0123456789abcdef"}},
		{"GET", v1 + "/openapi.json", "", nil},

		// legacy lifecycle
		{"POST", lg, `{"id":"bld_eq000001","target":"laptop","source":"ci","holder":"runner1","kind":"gradle","title":"unit run"}`, bearer()},
		{"POST", lg, `{"id":"bld_eq000001","target":"laptop","source":"ci","title":"again"}`, bearer()},
		{"POST", lg, ``, bearer()},
		{"POST", lg, `nope`, bearer()},
		{"POST", lg, `{}`, bearer()},
		{"POST", lg, `{"target":"laptop","source":"ci","title":"x","extra":1}`, bearer()},
		{"POST", lg, `{"target":"bad name","source":"ci","title":"x"}`, bearer()},
		{"POST", lg, bigTitle(), bearer()},
		{"GET", lg, "", bearer()},
		{"GET", lg + "?target=laptop&status=running&limit=5", "", bearer()},
		{"GET", lg + "?limit=zero", "", bearer()},
		{"GET", lg + "/bld_eq000001", "", bearer()},
		{"GET", lg + "/bld_unknown", "", bearer()},
		{"GET", lg + "/bad%20id", "", bearer()},
		{"POST", lg + "/bld_eq000001/heartbeat", `{"progress":"half","log":["compile","test"]}`, bearer()},
		{"POST", lg + "/bld_eq000001/heartbeat", `{"progress":`, bearer()},
		{"POST", lg + "/bld_eq000001/heartbeat", `{"log":["` + strings.Repeat("y", 2<<20) + `"]}`, bearer()},
		{"POST", lg + "/bld_unknown/heartbeat", `{}`, bearer()},
		{"GET", lg + "/bld_eq000001/log", "", bearer()},
		{"GET", lg + "/bld_eq000001/log?after=1", "", bearer()},
		{"GET", lg + "/bld_eq000001/log?after=2&wait=1", "", bearer()},
		{"GET", lg + "/bld_eq000001/log?wait=abc", "", bearer()},
		{"GET", lg + "/bld_eq000001/log?wait=99", "", bearer()},
		{"GET", lg + "/bld_unknown/log", "", bearer()},
		{"GET", lg + "/nodes", "", bearer()},
		{"GET", lg + "/nodes?node=" + node, "", bearer()},
		{"GET", lg + "/nodes?node=nodeZ", "", bearer()},
		{"DELETE", lg + "/bld_eq000001", "", bearer()},
		{"POST", lg + "/bld_eq000001/finish", `{"status":"succeeded","exit_code":0}`, bearer()},
		{"POST", lg + "/bld_eq000001/finish", `{"status":"failed"}`, bearer()},
		{"POST", lg + "/bld_eq000001/finish", `{"status":"sideways"}`, bearer()},
		{"POST", lg + "/bld_eq000001/heartbeat", `{}`, bearer()},
		{"GET", lg + "/bld_eq000001/log", "", bearer()},

		// builds pause on a transcode node (409), then resume
		{"POST", v1 + "/nodes/" + node + "/builds-pause", `{"paused":true}`, apikey()},
		{"POST", lg, `{"id":"bld_eq000002","target":"` + node + `","source":"ci","title":"held","via":"node_lease"}`, bearer()},
		{"GET", lg + "/nodes?node=" + node, "", bearer()},
		{"POST", v1 + "/nodes/" + node + "/builds-pause", `{"paused":false}`, apikey()},
		{"POST", lg, `{"id":"bld_eq000002","target":"` + node + `","source":"ci","title":"held","via":"node_lease"}`, bearer()},
		{"POST", lg, `{"target":"` + node + `","source":"ci","title":"no lease"}`, bearer()},
		{"POST", lg + "/bld_eq000002/finish", `{"status":"succeeded"}`, bearer()},

		// /nodes/v1
		{"POST", v1 + "/builds", `{"id":"bld_eq000003","target":"laptop","source":"ci","title":"v1 run"}`, apikey()},
		{"POST", v1 + "/builds", `{"id":"bld_eq000003","target":"laptop","source":"ci","title":"v1 run"}`, apikey()},
		{"POST", v1 + "/builds", `{"target":"laptop"}`, apikey()},
		{"GET", v1 + "/builds", "", apikey()},
		{"GET", v1 + "/builds?status=bogus", "", apikey()},
		{"GET", v1 + "/builds/bld_eq000003", "", apikey()},
		{"GET", v1 + "/builds/bld_unknown", "", apikey()},
		{"GET", v1 + "/nodes", "", apikey()},
		{"GET", v1 + "/nodes/" + node, "", apikey()},
		{"GET", v1 + "/nodes/nodeZ", "", apikey()},
		{"GET", v1 + "/nope", "", apikey()},
		{"POST", v1 + "/builds/bld_eq000003/heartbeat", `{"progress":"1/2","log":["a","b","c"]}`, apikey()},
		{"GET", v1 + "/builds/bld_eq000003/log?after=1&wait=1", "", apikey()},
		{"POST", v1 + "/builds/bld_eq000003/cancel", `{}`, apikey()},
		{"POST", v1 + "/builds/bld_eq000003/heartbeat", `{"progress":"2/2"}`, apikey()},
		{"POST", v1 + "/builds/bld_eq000003/finish", `{"status":"cancelled"}`, apikey()},
		{"GET", v1 + "/builds/bld_eq000003", "", map[string]string{"Authorization": "Bearer " + testBuildsToken}},
	}
}

// The same calls through Transcode with the in-process registry and with the
// proxy to the Nodes binary's handler answer alike: status, headers and body
// (N4), including a cancel the next heartbeat reports (N5).
func TestProxyAnswersLikeTheInProcessRegistry(t *testing.T) {
	inProc := newInProcessStack(t)
	proxied := newProxyStack(t, nodesproxy.Options{}).transcode.URL
	for i, c := range equivalenceCalls() {
		want := exchange(t, inProc, c)
		got := exchange(t, proxied, c)
		if want != got {
			t.Fatalf("call %d differs through the proxy\n--- in process\n%s\n--- proxied\n%s", i, clip(want), clip(got))
		}
	}
}

func clip(s string) string {
	if len(s) > 1500 {
		return s[:1500] + "..."
	}
	return s
}

// A cancel made through /nodes/v1 is what the worker's next heartbeat learns
// (N5): 409 with the build's status "cancelled".
func TestCancelReachesTheNextHeartbeatThroughTheProxy(t *testing.T) {
	st := newProxyStack(t, nodesproxy.Options{})
	base := st.transcode.URL
	body := func(c call) (int, map[string]any) {
		req, _ := http.NewRequest(c.method, base+c.path, strings.NewReader(c.body))
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		res, err := rawClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(res.Body).Decode(&m)
		return res.StatusCode, m
	}
	if code, _ := body(call{"POST", "/api/maintenance/builds", `{"id":"bld_cn000001","target":"laptop","source":"ci","title":"x"}`, bearer()}); code != 200 {
		t.Fatal(code)
	}
	if code, _ := body(call{"POST", "/nodes/v1/builds/bld_cn000001/cancel", `{}`, apikey()}); code != 200 {
		t.Fatal(code)
	}
	code, m := body(call{"POST", "/api/maintenance/builds/bld_cn000001/heartbeat", `{}`, bearer()})
	b, _ := m["build"].(map[string]any)
	if code != 409 || b["status"] != "cancelled" {
		t.Fatal("the heartbeat after a cancel is a 409 with the build cancelled", code, m)
	}
}

// 64 readers parked in a long-poll make the next one a 429, and the proxy
// passes the registry's answer through like the in-process route does.
func TestProxyPassesTooManyWaitersThrough(t *testing.T) {
	probe := func(base string) string {
		if code := exchangeStatus(t, base, call{"POST", "/api/maintenance/builds", `{"id":"bld_wt000001","target":"laptop","source":"ci","title":"x"}`, bearer()}); code != 200 {
			t.Fatal(code)
		}
		var wg sync.WaitGroup
		for i := 0; i < 64; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequest("GET", base+"/nodes/v1/builds/bld_wt000001/log?wait=6", nil)
				req.Header.Set("X-Api-Key", testBuildsToken)
				if res, err := rawClient.Do(req); err == nil {
					_, _ = io.Copy(io.Discard, res.Body)
					res.Body.Close()
				}
			}()
		}
		defer wg.Wait()
		var last string
		time.Sleep(500 * time.Millisecond) // the readers are parked by now
		for try := 0; try < 3; try++ {
			last = exchange(t, base, call{"GET", "/nodes/v1/builds/bld_wt000001/log?wait=1", "", apikey()})
			if strings.Contains(last, "\n429\n") {
				return last
			}
		}
		t.Fatal("never saw a 429", last)
		return ""
	}
	want := probe(newInProcessStack(t))
	got := probe(newProxyStack(t, nodesproxy.Options{}).transcode.URL)
	if want != got {
		t.Fatalf("429 differs\n--- in process\n%s\n--- proxied\n%s", want, got)
	}
}

func exchangeStatus(t *testing.T, base string, c call) int {
	t.Helper()
	req, _ := http.NewRequest(c.method, base+c.path, strings.NewReader(c.body))
	for k, v := range c.hdr {
		req.Header.Set(k, v)
	}
	res, err := rawClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// What Nodes receives from Transcode is the service key and nothing of the
// caller's: not the builds token, session cookie, same-origin header or Access
// identity, even when the caller sent them.
func TestProxyShowsNodesOnlyTheServiceKey(t *testing.T) {
	var mu sync.Mutex
	var seen []http.Header
	st := newWrappedProxyStack(t, nodesproxy.Options{}, func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.Header.Clone())
			mu.Unlock()
			inner.ServeHTTP(w, r)
		})
	})
	for _, c := range []call{
		{"GET", "/api/maintenance/builds/nodes", "", map[string]string{
			"Authorization": "Bearer " + testBuildsToken, "Cookie": "navigatorr_session=abc", "Origin": "https://example.test",
			"X-Navigatorr-Request": "1", "X-Forwarded-For": "192.0.2.1", "Cf-Access-Jwt-Assertion": "jwt"}},
		// A Connection header naming the key must not take the service key out.
		{"GET", "/nodes/v1/builds", "", map[string]string{"X-Api-Key": testBuildsToken, "Connection": "X-Api-Key"}},
		{"GET", "/nodes/v1/nodes", "", map[string]string{"X-Api-Key": testBuildsToken, "Forwarded": "for=192.0.2.1", "X-Real-Ip": "192.0.2.1"}},
	} {
		if code := exchangeStatus(t, st.transcode.URL, c); code != 200 {
			t.Fatal(c.path, code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatal("Nodes must have been called three times", len(seen))
	}
	for _, h := range seen {
		for k, v := range h {
			switch k {
			case "X-Api-Key":
				if len(v) != 1 || v[0] != testNodesKey {
					t.Fatal("X-Api-Key must be the service key", v)
				}
			case "User-Agent", "Accept-Encoding":
			default:
				t.Errorf("Nodes saw %s: %v", k, v)
			}
		}
	}
}

// With Nodes unreachable or refusing the key, the builds routes answer 503 with
// Retry-After, never an error a worker would take for a decision (N3); the spec
// is still served here, and other API routes are untouched.
func TestProxyWhenNodesIsDown(t *testing.T) {
	check := func(t *testing.T, st *proxyStack) {
		t.Helper()
		for _, c := range []call{
			{"POST", "/api/maintenance/builds", `{"target":"laptop","source":"ci","title":"x"}`, bearer()},
			{"GET", "/api/maintenance/builds/nodes", "", bearer()},
			{"POST", "/api/maintenance/builds/bld_x/heartbeat", `{}`, bearer()},
			{"POST", "/api/maintenance/builds/bld_x/finish", `{"status":"succeeded"}`, bearer()},
			{"GET", "/api/maintenance/builds/bld_x/log?wait=1", "", bearer()},
			{"GET", "/nodes/v1/builds", "", apikey()},
		} {
			req, _ := http.NewRequest(c.method, st.transcode.URL+c.path, strings.NewReader(c.body))
			for k, v := range c.hdr {
				req.Header.Set(k, v)
			}
			res, err := rawClient.Do(req)
			if err != nil {
				t.Fatal(c.path, err)
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			var e map[string]string
			_ = json.Unmarshal(b, &e)
			if res.StatusCode != 503 || res.Header.Get("Retry-After") != "5" || e["code"] != "unavailable" || e["error"] != "the builds registry is not reachable" {
				t.Fatalf("%s %s: want 503 with Retry-After, got %d %v %s", c.method, c.path, res.StatusCode, res.Header, b)
			}
		}
		if code := exchangeStatus(t, st.transcode.URL, call{"GET", "/nodes/v1/openapi.json", "", nil}); code != 200 {
			t.Fatal("the spec is served locally", code)
		}
		if code := exchangeStatus(t, st.transcode.URL, call{"GET", "/api/maintenance/auth-info", "", nil}); code != 200 {
			t.Fatal("the rest of the API is unaffected", code)
		}
	}
	t.Run("stopped", func(t *testing.T) {
		st := newProxyStack(t, nodesproxy.Options{})
		st.nodes.Close()
		check(t, st)
	})
	t.Run("key out of sync", func(t *testing.T) {
		check(t, newProxyStack(t, nodesproxy.Options{Key: "another-service-key-0123456789abcdef"}))
	})
}

// No registry and no proxy: the routes keep their 503.
func TestBuildsRoutesWithoutRegistryOrProxy(t *testing.T) {
	base, _ := testUI(t)
	s, err := New(base.cfg, base.registry, base.engine, base.mcp)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	if w := request(h, "GET", "/api/maintenance/builds", "", true); w.Code != 503 || !strings.Contains(w.Body.String(), "builds are not available") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(h, "GET", "/nodes/v1/builds", "", true); w.Code != 503 || !strings.Contains(w.Body.String(), "nodes is not available") {
		t.Fatal(w.Code, w.Body.String())
	}
}

// The proxy decides its long transport by the log paths, which it knows by
// value (it must not import package nodes): pin them to the real routes.
func TestProxyLongPollPathsMatchTheRegistryRoutes(t *testing.T) {
	for _, base := range []string{nodes.LegacyBasePath, nodes.BasePath + "/builds"} {
		r := httptest.NewRequest("GET", base+"/bld_x/log?wait=5", nil)
		if !nodesproxy.IsLongPoll(r) {
			t.Errorf("%s/{id}/log must be a long-poll path", base)
		}
	}
}

// A Nodes answer that breaks after its headers must break the client's
// connection through Transcode's whole handler (auth, headers, gzip), not end
// as a short body that looks complete.
func TestProxyMidBodyFailureAbortsThroughTheServer(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, bw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = bw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 400\r\n\r\n{\"lines\":[\"a\"")
		_ = bw.Flush()
		_ = conn.Close()
	}))
	defer up.Close()
	st := newProxyStack(t, nodesproxy.Options{BaseURL: up.URL})
	for _, enc := range []string{"", "gzip"} {
		t.Run("accept-encoding="+enc, func(t *testing.T) {
			req, _ := http.NewRequest("GET", st.transcode.URL+"/api/maintenance/builds/bld_x/log", nil)
			for k, v := range bearer() {
				req.Header.Set(k, v)
			}
			if enc != "" {
				req.Header.Set("Accept-Encoding", enc)
			}
			// EOF before the headers (still buffered when the proxy aborted) or a
			// failed body read: both are a broken connection, never a clean answer.
			res, err := rawClient.Do(req)
			if err != nil {
				return
			}
			defer res.Body.Close()
			if _, err := io.ReadAll(res.Body); err == nil {
				t.Fatalf("a broken answer must break the connection, the client got a clean %d", res.StatusCode)
			}
		})
	}
}

// The same failure once Transcode has already sent the headers and a large part
// of the body (past every buffer, gzip's included): the client must still see
// the read fail, not a gzip trailer or a short body that ends cleanly.
func TestProxyFailureAfterAFlushedBodyAbortsThroughTheServer(t *testing.T) {
	// Incompressible: gzip must not shrink it back under its own buffer.
	raw := make([]byte, 96<<10)
	rand.New(rand.NewSource(1)).Read(raw)
	chunk := hex.EncodeToString(raw) // 192 KiB that gzip only halves
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lines":[` + chunk))
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		panic(http.ErrAbortHandler)
	}))
	defer up.Close()
	st := newProxyStack(t, nodesproxy.Options{BaseURL: up.URL})
	for _, enc := range []string{"", "gzip"} {
		t.Run("accept-encoding="+enc, func(t *testing.T) {
			req, _ := http.NewRequest("GET", st.transcode.URL+"/api/maintenance/builds/bld_x/log", nil)
			for k, v := range bearer() {
				req.Header.Set(k, v)
			}
			if enc != "" {
				req.Header.Set("Accept-Encoding", enc)
			}
			res, err := rawClient.Do(req)
			if err != nil {
				t.Fatalf("headers and part of the body were flushed, the client must get them: %v", err)
			}
			defer res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatalf("status %d", res.StatusCode)
			}
			raw, err := io.ReadAll(res.Body)
			if err == nil {
				t.Fatal("the body ended cleanly although Nodes broke mid-answer")
			}
			if enc != "gzip" {
				return
			}
			if ce := res.Header.Get("Content-Encoding"); ce != "gzip" {
				t.Fatalf("Content-Encoding %q, want gzip", ce)
			}
			// What a client that only reads the gzip stream sees: no trailer, so
			// the stream must fail too, never end cleanly.
			zr, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			zr.Multistream(false)
			if _, err := io.ReadAll(zr); err == nil {
				t.Fatal("the gzip stream ended cleanly although Nodes broke mid-answer")
			}
		})
	}
}
