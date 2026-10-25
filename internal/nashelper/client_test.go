package nashelper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/internal/sampledigest"
)

func (f *fixture) client(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{
		URL:      f.http.URL,
		Token:    testToken,
		Mappings: []PathMapping{{Local: f.root, Remote: f.root}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientRemoteMapping(t *testing.T) {
	c, err := NewClient(ClientConfig{
		URL:   "http://nas.lan:8099",
		Token: testToken,
		Mappings: []PathMapping{
			{Local: "/data", Remote: "/volume1/Media"},
			{Local: "/data/tv", Remote: "/volume1/Media/Series"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"/data/movies/a.mkv", "/volume1/Media/movies/a.mkv", true},
		{"/data/tv/show/a.mkv", "/volume1/Media/Series/show/a.mkv", true},
		{"/data/tvshow/a.mkv", "/volume1/Media/tvshow/a.mkv", true},
		{"/database/a.mkv", "", false},
		{"/other/a.mkv", "", false},
		{"data/a.mkv", "", false},
		{"/data/../etc/passwd", "", false},
		{"/data//a.mkv", "", false},
	}
	for _, tc := range cases {
		got, ok := c.Remote(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Remote(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNewClientRejectsBadConfig(t *testing.T) {
	good := ClientConfig{URL: "http://nas:8099", Token: testToken, Mappings: []PathMapping{{Local: "/a", Remote: "/a"}}}
	for name, mutate := range map[string]func(*ClientConfig){
		"scheme":       func(c *ClientConfig) { c.URL = "ftp://nas" },
		"short token":  func(c *ClientConfig) { c.Token = "short" },
		"no mappings":  func(c *ClientConfig) { c.Mappings = nil },
		"relative map": func(c *ClientConfig) { c.Mappings = []PathMapping{{Local: "a", Remote: "/a"}} },
	} {
		c := good
		mutate(&c)
		if _, err := NewClient(c); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte("  "+testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadToken(p)
	if err != nil || got != testToken {
		t.Fatalf("got %q, %v", got, err)
	}
	if err := os.WriteFile(p, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadToken(p); err == nil {
		t.Fatal("a short token must be refused")
	}
	if _, err := ReadToken(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing file must be refused")
	}
}

func TestClientHealthIdentitySampleHash(t *testing.T) {
	for _, walk := range []bool{false, true} {
		f := newFixtureWith(t, 2, walk)
		c := f.client(t)
		if err := os.MkdirAll(filepath.Join(f.root, "show"), 0o700); err != nil {
			t.Fatal(err)
		}
		path, data := f.write("show/a.bin", 3<<20+123)
		ctx := context.Background()

		h, err := c.Health(ctx)
		if err != nil || h.InstanceID != f.srv.InstanceID() {
			t.Fatalf("health: %+v, %v", h, err)
		}
		id, err := c.Identity(ctx, path)
		if err != nil || id.Size != int64(len(data)) || id.Ino == 0 {
			t.Fatalf("identity: %+v, %v", id, err)
		}
		sample, err := c.Sample(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		want, err := sampledigest.Digest(ctx, strings.NewReader(string(data)), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if sample.Digest != want {
			t.Fatalf("sample digest %s, want %s", sample.Digest, want)
		}
		var reads atomic.Int64
		res, err := c.Hash(ctx, path, func(read, total int64) { reads.Store(read) })
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if res.SHA256 != hex.EncodeToString(sum[:]) || res.Bytes != int64(len(data)) {
			t.Fatalf("hash: %+v", res)
		}
		if res.Identity.Ino != id.Ino {
			t.Fatalf("hash identity %+v differs from %+v", res.Identity, id)
		}
	}
}

func TestClientAPIErrorsAreTypedAndNotUnavailable(t *testing.T) {
	f := newFixture(t, 2)
	c := f.client(t)
	ctx := context.Background()

	_, err := c.Identity(ctx, filepath.Join(f.root, "missing.bin"))
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != CodeNotFound || IsUnavailable(err) {
		t.Fatalf("missing file: %v", err)
	}
	// A path no mapping covers is refused before any request is made.
	_, err = c.Identity(ctx, "/elsewhere/a.bin")
	if !errors.As(err, &apiErr) || apiErr.Code != CodeOutside || IsUnavailable(err) {
		t.Fatalf("unmapped path: %v", err)
	}
}

func TestClientWrongTokenIsUnavailableAndNeverLeaks(t *testing.T) {
	f := newFixture(t, 2)
	c, err := NewClient(ClientConfig{
		URL:      f.http.URL,
		Token:    strings.Repeat("x", 40),
		Mappings: []PathMapping{{Local: f.root, Remote: f.root}},
	})
	if err != nil {
		t.Fatal(err)
	}
	path, _ := f.write("a.bin", 10)
	_, err = c.Identity(context.Background(), path)
	if err == nil || !IsUnavailable(err) {
		t.Fatalf("a refused token must count as unavailable: %v", err)
	}
	if strings.Contains(err.Error(), strings.Repeat("x", 40)) {
		t.Fatal("the token leaked into an error")
	}
}

func TestClientUnreachableHelperIsUnavailable(t *testing.T) {
	f := newFixture(t, 2)
	c := f.client(t)
	path, _ := f.write("a.bin", 10)
	f.http.Close()
	for name, call := range map[string]func() error{
		"health":   func() error { _, err := c.Health(context.Background()); return err },
		"identity": func() error { _, err := c.Identity(context.Background(), path); return err },
		"sample":   func() error { _, err := c.Sample(context.Background(), path); return err },
		"hash":     func() error { _, err := c.Hash(context.Background(), path, nil); return err },
	} {
		if err := call(); !errors.Is(err, ErrUnavailable) || !IsUnavailable(err) {
			t.Errorf("%s: want ErrUnavailable, got %v", name, err)
		}
	}
}

func TestClientDoesNotFollowRedirectsWithTheToken(t *testing.T) {
	var hits atomic.Int64
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	c, err := NewClient(ClientConfig{URL: redirector.URL, Token: testToken, Mappings: []PathMapping{{Local: "/a", Remote: "/a"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Health(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a redirect must be unavailable: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the client followed a redirect")
	}
}

func TestClientRejectsNonProtocolAnswers(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not the helper</html>"))
	}))
	defer ts.Close()
	c, err := NewClient(ClientConfig{URL: ts.URL, Token: testToken, Mappings: []PathMapping{{Local: "/a", Remote: "/a"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Health(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

// switchable serves whichever helper it currently points at, so a test can
// "restart" the helper under a client.
type switchable struct {
	mu sync.Mutex
	h  http.Handler
}

func (s *switchable) set(h http.Handler) { s.mu.Lock(); s.h = h; s.mu.Unlock() }
func (s *switchable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	h := s.h
	s.mu.Unlock()
	h.ServeHTTP(w, r)
}

func newRestartable(t *testing.T) (*Server, *httptest.Server, string, func() *Server) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sw := &switchable{}
	mk := func() *Server {
		srv, err := NewServer(Config{Roots: []string{root}, Token: testToken, MaxHashes: 2})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(srv.Close)
		return srv
	}
	first := mk()
	sw.set(first.Handler())
	ts := httptest.NewServer(sw)
	t.Cleanup(ts.Close)
	return first, ts, root, func() *Server {
		s := mk()
		sw.set(s.Handler())
		return s
	}
}

func TestClientNoticesARestart(t *testing.T) {
	_, ts, root, restart := newRestartable(t)
	c, err := NewClient(ClientConfig{URL: ts.URL, Token: testToken, Mappings: []PathMapping{{Local: root, Remote: root}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "a.bin")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Identity(ctx, path); err != nil {
		t.Fatal(err)
	}
	restart()
	if _, err := c.Identity(ctx, path); !errors.Is(err, ErrRestarted) || !IsUnavailable(err) {
		t.Fatalf("an identity from a new instance must be reported: %v", err)
	}
	// After the report the client follows the new instance.
	if _, err := c.Identity(ctx, path); err != nil {
		t.Fatalf("the new instance should be accepted once reported: %v", err)
	}
}

func TestClientHashAcrossARestart(t *testing.T) {
	first, ts, root, restart := newRestartable(t)
	c, err := NewClient(ClientConfig{URL: ts.URL, Token: testToken, Mappings: []PathMapping{{Local: root, Remote: root}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "a.bin")
	if err := os.WriteFile(path, make([]byte, 4<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	first.afterChunk = func(string) { once.Do(func() { close(held); <-release }) }
	done := make(chan error, 1)
	go func() { _, err := c.Hash(context.Background(), path, nil); done <- err }()
	<-held
	restart()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, ErrRestarted) || !IsUnavailable(err) {
			t.Fatalf("a hash whose helper restarted must say so: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("hash did not finish")
	}
}

func TestClientCancelStopsTheJobOnTheHelper(t *testing.T) {
	f := newFixture(t, 2)
	c := f.client(t)
	path, _ := f.write("big.bin", 8<<20)
	reading := make(chan struct{}, 1)
	f.srv.afterChunk = func(string) {
		select {
		case reading <- struct{}{}:
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := c.Hash(ctx, path, nil); errc <- err }()
	<-reading
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.srv.mu.Lock()
		running := f.srv.activeLocked()
		f.srv.mu.Unlock()
		if running == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper kept reading after the client gave up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestClientHashReportsAChangedFile(t *testing.T) {
	f := newFixture(t, 2)
	c := f.client(t)
	path, _ := f.write("a.bin", 4<<20)
	var once sync.Once
	f.srv.afterChunk = func(p string) {
		once.Do(func() {
			fh, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err == nil {
				_, _ = fh.Write([]byte("x"))
				fh.Close()
			}
		})
	}
	_, err := c.Hash(context.Background(), path, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != CodeChanged || IsUnavailable(err) {
		t.Fatalf("a file changed mid-read is an answer, not an outage: %v", err)
	}
}
