package nodesproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "synthetic-nodes-service-key-0123456789"

type recLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *recLog) Errorf(f string, a ...any) { l.add("ERROR", f, a) }
func (l *recLog) Warnf(f string, a ...any)  { l.add("WARN", f, a) }
func (l *recLog) add(level, f string, a []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+": "+sprintf(f, a))
}
func (l *recLog) count(level string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.HasPrefix(s, level+":") {
			n++
		}
	}
	return n
}
func (l *recLog) has(level string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.HasPrefix(s, level+":") {
			return true
		}
	}
	return false
}

// front serves the proxy over a real HTTP server, as Transcode does.
func front(t *testing.T, o Options) (*httptest.Server, *recLog) {
	t.Helper()
	lg := &recLog{}
	o.Logger = lg
	if o.Key == "" {
		o.Key = testKey
	}
	p, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(func() { srv.Close(); p.Close() })
	return srv, lg
}

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	return do(t, req)
}

func do(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func wantUnavailable(t *testing.T, res *http.Response, body []byte) {
	t.Helper()
	var e map[string]string
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("503 body is not JSON: %q", body)
	}
	if res.StatusCode != 503 || res.Header.Get("Retry-After") != "5" || e["code"] != "unavailable" || e["error"] != "the builds registry is not reachable" || e["hint"] == "" ||
		!strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("want the 503 unavailable answer, got %d %v %s", res.StatusCode, res.Header, body)
	}
}

func TestNewValidation(t *testing.T) {
	for _, tc := range []struct{ url, key string }{
		{"", testKey}, {"ftp://127.0.0.1:1", testKey}, {"http://", testKey}, {"http://127.0.0.1:1/x", testKey},
		{"http://127.0.0.1:1?q=1", testKey}, {"http://u:p@127.0.0.1:1", testKey}, {"http://127.0.0.1:1", ""}, {"http://127.0.0.1:1", "has space"},
	} {
		if _, err := New(Options{BaseURL: tc.url, Key: tc.key}); err == nil {
			t.Errorf("New(%q, %q) must fail", tc.url, tc.key)
		}
	}
	if _, err := New(Options{BaseURL: "http://127.0.0.1:1/", Key: testKey}); err != nil {
		t.Fatal(err)
	}
}

// The upstream sees the service key and nothing the client sent to identify
// itself or the chain it came through; method, path, query and body are kept.
func TestHeaderHygiene(t *testing.T) {
	var got *http.Request
	var body []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	srv, _ := front(t, Options{BaseURL: up.URL})

	req, _ := http.NewRequest("POST", srv.URL+"/api/maintenance/builds/b1/heartbeat?x=1&y=a%20b", strings.NewReader(`{"progress":"p"}`))
	for k, v := range map[string]string{
		"Authorization": "Bearer client-secret", "X-Api-Key": "client-key", "Cookie": "session=abc", "Origin": "https://example.test",
		"X-Navigatorr-Request": "1", "Forwarded": "for=192.0.2.1", "X-Forwarded-For": "192.0.2.1", "X-Forwarded-Host": "example.test",
		"X-Forwarded-Proto": "https", "X-Real-Ip": "192.0.2.1", "Cf-Access-Jwt-Assertion": "jwt", "Cf-Access-Authenticated-User-Email": "a@example.test",
		"Cf-Access-Client-Id": "id", "Content-Type": "application/json", "Accept": "application/json",
	} {
		req.Header.Set(k, v)
	}
	req.Header.Add("X-Api-Key", "second-client-key")
	res, out := do(t, req)
	if res.StatusCode != 201 || string(out) != `{"ok":true}` || res.Header.Get("X-Upstream") != "yes" {
		t.Fatal("the answer must pass through", res.StatusCode, string(out), res.Header)
	}
	if got.Method != "POST" || got.URL.Path != "/api/maintenance/builds/b1/heartbeat" || got.URL.RawQuery != "x=1&y=a%20b" || string(body) != `{"progress":"p"}` {
		t.Fatal("method, path, query and body must be unchanged", got.Method, got.URL, string(body))
	}
	for k, v := range got.Header {
		switch k {
		case "X-Api-Key":
			if len(v) != 1 || v[0] != testKey {
				t.Fatal("X-Api-Key must be the service key alone", v)
			}
		case "Content-Type", "Accept", "Content-Length", "User-Agent", "Accept-Encoding":
		default:
			t.Errorf("upstream saw an unexpected header %s: %v", k, v)
		}
	}
	if got.Host != strings.TrimPrefix(up.URL, "http://") {
		t.Fatal("the Host is the upstream's", got.Host)
	}
}

func TestKeyRejectedIs503(t *testing.T) {
	for _, status := range []int{401, 403} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", "x")
			http.Error(w, `{"error":"authentication required"}`, status)
		}))
		srv, lg := front(t, Options{BaseURL: up.URL})
		res, body := get(t, srv.URL+"/nodes/v1/builds")
		wantUnavailable(t, res, body)
		if strings.Contains(string(body), "authentication") || res.Header.Get("WWW-Authenticate") != "" {
			t.Fatal("the upstream's refusal must not reach the client")
		}
		if !lg.has("ERROR") {
			t.Fatal("a rejected key is logged as an error")
		}
		up.Close()
	}
}

func TestPassThrough(t *testing.T) {
	for _, tc := range []struct {
		status int
		header string
	}{{200, ""}, {400, ""}, {404, ""}, {409, ""}, {429, "7"}, {500, ""}, {503, "9"}} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if tc.header != "" {
				w.Header().Set("Retry-After", tc.header)
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"code":"from_upstream"}`))
		}))
		srv, _ := front(t, Options{BaseURL: up.URL})
		res, body := get(t, srv.URL+"/nodes/v1/builds")
		if res.StatusCode != tc.status || string(body) != `{"code":"from_upstream"}` || res.Header.Get("Retry-After") != tc.header {
			t.Errorf("status %d must pass through unchanged: got %d %q %q", tc.status, res.StatusCode, body, res.Header.Get("Retry-After"))
		}
		up.Close()
	}
}

func TestUnreachableIs503(t *testing.T) {
	t.Run("closed port", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := ln.Addr().String()
		ln.Close()
		srv, lg := front(t, Options{BaseURL: "http://" + addr})
		res, body := get(t, srv.URL+"/api/maintenance/builds")
		wantUnavailable(t, res, body)
		if !lg.has("WARN") {
			t.Fatal("an unreachable Nodes is logged")
		}
	})
	t.Run("no headers within the timeout", func(t *testing.T) {
		release := make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer up.Close()
		defer close(release)
		srv, _ := front(t, Options{BaseURL: up.URL, ResponseHeaderTimeout: 150 * time.Millisecond})
		start := time.Now()
		res, body := get(t, srv.URL+"/nodes/v1/builds")
		wantUnavailable(t, res, body)
		if time.Since(start) > 3*time.Second {
			t.Fatal("the timeout did not apply", time.Since(start))
		}
	})
	t.Run("reset before the answer", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.SetLinger(0) // close sends a RST
				}
				_ = c.Close()
			}
		}()
		srv, _ := front(t, Options{BaseURL: "http://" + ln.Addr().String()})
		for _, m := range []string{"GET", "POST"} {
			req, _ := http.NewRequest(m, srv.URL+"/nodes/v1/builds", strings.NewReader("{}"))
			res, body := do(t, req)
			wantUnavailable(t, res, body)
		}
	})
	t.Run("closed without an answer", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_, _ = c.Read(make([]byte, 4096))
				_ = c.Close()
			}
		}()
		srv, _ := front(t, Options{BaseURL: "http://" + ln.Addr().String()})
		res, body := get(t, srv.URL+"/nodes/v1/builds")
		wantUnavailable(t, res, body)
	})
}

// Once the answer started, a failure must show as a broken connection, never
// as a short body that parses.
func TestFailureMidBodyAbortsTheConnection(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"short content-length": func(w http.ResponseWriter, r *http.Request) {
			conn, bw, _ := w.(http.Hijacker).Hijack()
			_, _ = bw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n{\"lines\":[\"a\"")
			_ = bw.Flush()
			_ = conn.Close()
		},
		"chunked": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"lines":["a"`))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			up := httptest.NewUnstartedServer(h)
			up.Config.ErrorLog = nil
			up.Start()
			defer up.Close()
			srv, lg := front(t, Options{BaseURL: up.URL})
			// Either outcome is a broken connection: net/http may not have sent
			// the buffered headers yet when the proxy aborts (EOF before them),
			// or the body read fails. Never a clean, parseable answer.
			res, err := http.Get(srv.URL + "/nodes/v1/builds/b1/log")
			if err == nil {
				b, rerr := io.ReadAll(res.Body)
				res.Body.Close()
				if rerr == nil {
					t.Fatalf("a failed answer must break the connection, the client got a clean %d %q", res.StatusCode, b)
				}
				if json.Valid(b) {
					t.Fatal("what arrived must not parse", string(b))
				}
			}
			// The proxy, not the client side, saw the failed copy.
			waitFor(t, func() bool { return lg.has("WARN") })
		})
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestIsLongPoll(t *testing.T) {
	for _, tc := range []struct {
		method, url string
		want        bool
	}{
		{"GET", "/api/maintenance/builds/b1/log?wait=5", true},
		{"GET", "/nodes/v1/builds/b1/log?after=3&wait=25", true},
		{"GET", "/api/maintenance/builds/b1/log", false},
		{"GET", "/api/maintenance/builds/b1/log?wait=0", false},
		{"GET", "/api/maintenance/builds/b1/log?wait=-3", false},
		{"GET", "/api/maintenance/builds/b1/log?wait=x", false},
		{"GET", "/api/maintenance/builds/b1/log?wait=", false},
		{"POST", "/api/maintenance/builds/b1/log?wait=5", false},
		{"GET", "/api/maintenance/builds/b1?wait=5", false},
		{"GET", "/api/maintenance/builds/b1/heartbeat?wait=5", false},
		{"GET", "/api/maintenance/builds//log?wait=5", false},
		{"GET", "/api/maintenance/builds/log?wait=5", false},
		{"GET", "/api/maintenance/builds/a/b/log?wait=5", false},
		{"GET", "/nodes/v1/nodes/b1/log?wait=5", false},
	} {
		r := httptest.NewRequest(tc.method, tc.url, nil)
		if got := IsLongPoll(r); got != tc.want {
			t.Errorf("%s %s: IsLongPoll = %v, want %v", tc.method, tc.url, got, tc.want)
		}
	}
}

// A log read with wait gets the long header timeout; the same read without it,
// or anything else, the short one.
func TestLongPollPastItsTimeoutIs503(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	srv, _ := front(t, Options{BaseURL: up.URL, ResponseHeaderTimeout: 50 * time.Millisecond, LongPollHeaderTimeout: 100 * time.Millisecond})
	res, body := get(t, srv.URL+"/nodes/v1/builds/b1/log?wait=2")
	wantUnavailable(t, res, body)
}

func TestRequestDeadlineStillAnswers503(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer up.Close()
	p, err := New(Options{BaseURL: up.URL, Key: testKey, Logger: &recLog{}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// A deadline set by something in front of the proxy, not a client that left.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
		defer cancel()
		p.ServeHTTP(w, r.WithContext(ctx))
	}))
	defer srv.Close()
	res, body := get(t, srv.URL+"/nodes/v1/builds")
	wantUnavailable(t, res, body)
}

func TestStalledTLSHandshakeIs503(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accept TCP, never answer the handshake
		}
	}()
	// The long-poll transport waits up to an hour for headers here, so only the
	// short handshake timeout can end the long-poll case in time.
	srv, _ := front(t, Options{BaseURL: "https://" + ln.Addr().String(),
		ResponseHeaderTimeout: 100 * time.Millisecond, LongPollHeaderTimeout: time.Hour})
	for _, path := range []string{"/nodes/v1/builds", "/nodes/v1/builds/b1/log?wait=5"} {
		start := time.Now()
		res, body := get(t, srv.URL+path) // the client gives up at 10 s
		wantUnavailable(t, res, body)
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("%s: a stalled TLS handshake took %v to end in a 503", path, d)
		}
	}
}

func TestNotReachableWarnsOncePerInterval(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens there now
	srv, lg := front(t, Options{BaseURL: "http://" + addr})
	for i := 0; i < 5; i++ {
		res, body := get(t, srv.URL+"/nodes/v1/builds")
		wantUnavailable(t, res, body)
	}
	if n := lg.count("WARN"); n != 1 {
		t.Fatalf("five failures inside the interval logged %d warnings, want 1", n)
	}
}

func TestNotReachableWarnsAgainAfterTheInterval(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	lg := &recLog{}
	h, err := New(Options{BaseURL: "http://" + addr, Key: "k", Logger: lg})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.warnEvery = 50 * time.Millisecond
	srv := httptest.NewServer(h)
	defer srv.Close()
	for i := 0; i < 2; i++ {
		if i > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		res, body := get(t, srv.URL+"/nodes/v1/builds")
		wantUnavailable(t, res, body)
	}
	if n := lg.count("WARN"); n != 2 {
		t.Fatalf("two failures an interval apart logged %d warnings, want 2", n)
	}
}

func TestLongPollUsesTheLongTransport(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	srv, _ := front(t, Options{BaseURL: up.URL, ResponseHeaderTimeout: 100 * time.Millisecond, LongPollHeaderTimeout: 3 * time.Second})
	for _, p := range []string{"/api/maintenance/builds/b1/log?wait=2", "/nodes/v1/builds/b1/log?wait=2&after=0"} {
		if res, body := get(t, srv.URL+p); res.StatusCode != 200 || string(body) != `{}` {
			t.Errorf("%s must wait for the answer: %d %s", p, res.StatusCode, body)
		}
	}
	for _, p := range []string{"/api/maintenance/builds/b1/log", "/api/maintenance/builds/b1/log?wait=0", "/nodes/v1/builds/b1", "/nodes/v1/builds/b1/log?wait=nope"} {
		res, body := get(t, srv.URL+p)
		wantUnavailable(t, res, body)
	}
}

func TestClientCancelCancelsUpstream(t *testing.T) {
	arrived, done := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done()
		close(done)
	}))
	defer up.Close()
	srv, lg := front(t, Options{BaseURL: up.URL, LongPollHeaderTimeout: 20 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/nodes/v1/builds/b1/log?wait=20", nil)
	errc := make(chan error, 1)
	go func() {
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
		}
		errc <- err
	}()
	<-arrived
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream request was not cancelled")
	}
	if err := <-errc; err == nil {
		t.Fatal("the client call must fail")
	}
	time.Sleep(50 * time.Millisecond)
	if lg.has("ERROR") || lg.has("WARN") {
		t.Fatal("a client that left is not a failure to log", lg.lines)
	}
}

func sprintf(f string, a []any) string { return fmt.Sprintf(f, a...) }
