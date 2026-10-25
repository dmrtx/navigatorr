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
		idResp, err := c.Identity(ctx, path)
		id := idResp.Identity
		if err != nil || id.Size != int64(len(data)) || id.Ino == 0 || idResp.InstanceID != f.srv.InstanceID() {
			t.Fatalf("identity: %+v, %v", idResp, err)
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
		if res.InstanceID != f.srv.InstanceID() || res.SHA256 != hex.EncodeToString(sum[:]) || res.Bytes != int64(len(data)) {
			t.Fatalf("hash: %+v", res)
		}
		if res.Identity.Ino != id.Ino {
			t.Fatalf("hash identity %+v differs from %+v", res.Identity, id)
		}
	}
}

func TestClientAnswersVersusFallbacks(t *testing.T) {
	f := newFixture(t, 2)
	c := f.client(t)
	ctx := context.Background()
	dir := filepath.Join(f.root, "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// The helper looked and said something: not a reason to read through the mount.
	for name, path := range map[string]string{
		"missing":  filepath.Join(f.root, "missing.bin"),
		"not file": dir,
	} {
		_, err := c.Identity(ctx, path)
		if !IsAnswer(err) || ShouldFallBack(err) {
			t.Errorf("%s: want an answer, got %v", name, err)
		}
	}
	// The helper cannot help with these: read through the mount.
	_, err := c.Identity(ctx, "/elsewhere/a.bin")
	if !errors.Is(err, ErrNotMapped) || IsAnswer(err) || !ShouldFallBack(err) {
		t.Errorf("unmapped path: %v", err)
	}
	outside, err := NewClient(ClientConfig{URL: f.http.URL, Token: testToken, Mappings: []PathMapping{{Local: "/data", Remote: "/not/served"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = outside.Identity(ctx, "/data/a.bin")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != CodeOutside || IsAnswer(err) || !ShouldFallBack(err) {
		t.Errorf("path the helper does not serve: %v", err)
	}
	// The caller's own cancellation is not a reason to fall back either.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	path, _ := f.write("a.bin", 10)
	if _, err := c.Identity(cctx, path); !errors.Is(err, context.Canceled) || ShouldFallBack(err) {
		t.Errorf("caller cancelled: %v", err)
	}
	if ShouldFallBack(nil) {
		t.Error("no error, no fallback")
	}
	for _, code := range []string{CodeDenied, CodeBadInput, CodeCancelled, CodeAuth, CodeInternal, CodeBusy, "some_future_code"} {
		if e := (&Error{Code: code}); IsAnswer(e) || !ShouldFallBack(e) {
			t.Errorf("%s must fall back", code)
		}
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
	if err == nil || !ShouldFallBack(err) {
		t.Fatalf("a refused token must fall back: %v", err)
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
		if err := call(); !errors.Is(err, ErrUnavailable) || !ShouldFallBack(err) {
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

func TestClientIdentityCarriesTheInstance(t *testing.T) {
	_, ts, root, restart := newRestartable(t)
	c, err := NewClient(ClientConfig{URL: ts.URL, Token: testToken, Mappings: []PathMapping{{Local: root, Remote: root}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "a.bin")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := c.Identity(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	second := restart()
	after, err := c.Identity(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if before.InstanceID == after.InstanceID || after.InstanceID != second.InstanceID() {
		t.Fatalf("a caller must be able to tell the instances apart: %q then %q", before.InstanceID, after.InstanceID)
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
		if !errors.Is(err, ErrRestarted) || !ShouldFallBack(err) {
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
	if !errors.As(err, &apiErr) || apiErr.Code != CodeChanged || ShouldFallBack(err) {
		t.Fatalf("a file changed mid-read is an answer, not an outage: %v", err)
	}
}

// fakeHelper is a scripted helper for the paths a real one cannot be made to take.
type fakeHelper struct {
	t        *testing.T
	instance func() string
	polls    atomic.Int64
	deletes  atomic.Int64
	// poll answers the nth poll (1-based); return ok=false to drop the connection.
	poll func(n int64) (job Job, status int)
	// start answers the hash start.
	start func() Job
}

func (f *fakeHelper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/hash":
		writeJSON(w, http.StatusAccepted, f.start())
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
		n := f.polls.Add(1)
		job, status := f.poll(n)
		if status == 0 {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		writeJSON(w, status, job)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
		f.deletes.Add(1)
		writeJSON(w, http.StatusOK, Job{})
	default:
		http.NotFound(w, r)
	}
}

func fakeClient(t *testing.T, f *fakeHelper) *Client {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	c, err := NewClient(ClientConfig{URL: ts.URL, Token: testToken, Mappings: []PathMapping{{Local: "/data", Remote: "/data"}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func waitDeletes(t *testing.T, f *fakeHelper, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.deletes.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("the job was not cancelled on the helper (deletes=%d)", f.deletes.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClientHashDetectsAnInstanceChangeBetweenPolls(t *testing.T) {
	f := &fakeHelper{t: t}
	f.start = func() Job { return Job{ID: "j1", InstanceID: "A", State: JobRunning} }
	f.poll = func(int64) (Job, int) { return Job{ID: "j1", InstanceID: "B", State: JobRunning}, http.StatusOK }
	c := fakeClient(t, f)
	_, err := c.Hash(context.Background(), "/data/a.bin", nil)
	if !errors.Is(err, ErrRestarted) || !ShouldFallBack(err) {
		t.Fatalf("got %v", err)
	}
	waitDeletes(t, f, 1)
}

func TestClientHashSurvivesOneFailedPoll(t *testing.T) {
	f := &fakeHelper{t: t}
	f.start = func() Job { return Job{ID: "j1", InstanceID: "A", State: JobRunning} }
	f.poll = func(n int64) (Job, int) {
		switch n {
		case 1:
			return Job{}, 0 // connection dropped
		case 2:
			return Job{ID: "j1", InstanceID: "A", State: JobRunning}, http.StatusOK
		}
		return Job{ID: "j1", InstanceID: "A", State: JobDone, Result: &HashResult{SHA256: "ab", Bytes: 1}}, http.StatusOK
	}
	c := fakeClient(t, f)
	res, err := c.Hash(context.Background(), "/data/a.bin", nil)
	if err != nil || res.SHA256 != "ab" || res.InstanceID != "A" {
		t.Fatalf("%+v, %v", res, err)
	}
	if f.deletes.Load() != 0 {
		t.Fatal("a finished job must not be cancelled")
	}
}

func TestClientHashGivesUpAfterRepeatedFailedPollsAndCancels(t *testing.T) {
	f := &fakeHelper{t: t}
	f.start = func() Job { return Job{ID: "j1", InstanceID: "A", State: JobRunning} }
	f.poll = func(int64) (Job, int) { return Job{}, 0 }
	c := fakeClient(t, f)
	_, err := c.Hash(context.Background(), "/data/a.bin", nil)
	if !errors.Is(err, ErrUnavailable) || !ShouldFallBack(err) {
		t.Fatalf("got %v", err)
	}
	// The transport may retry a dropped GET itself, so this is a floor.
	if got := f.polls.Load(); got < maxPollFailures {
		t.Fatalf("polled %d times, want at least %d", got, maxPollFailures)
	}
	waitDeletes(t, f, 1)
}

func TestClientHashOddJobsFallBackAndCancelOnlyWhenTheyShould(t *testing.T) {
	cases := []struct {
		name    string
		job     Job
		deletes int64
	}{
		{"unknown state", Job{ID: "j1", InstanceID: "A", State: "exploded"}, 1},
		{"id without instance", Job{ID: "j1", State: JobRunning}, 1},
		{"cancelled by the helper", Job{ID: "j1", InstanceID: "A", State: JobCancelled}, 0},
		{"failed without an error", Job{ID: "j1", InstanceID: "A", State: JobFailed}, 0},
		{"done without a result", Job{ID: "j1", InstanceID: "A", State: JobDone}, 1},
	}
	for _, tc := range cases {
		f := &fakeHelper{t: t}
		f.start = func() Job { return tc.job }
		c := fakeClient(t, f)
		if _, err := c.Hash(context.Background(), "/data/a.bin", nil); !ShouldFallBack(err) {
			t.Errorf("%s: got %v", tc.name, err)
		}
		if tc.deletes > 0 {
			waitDeletes(t, f, tc.deletes)
		} else {
			time.Sleep(50 * time.Millisecond)
			if f.deletes.Load() != 0 {
				t.Errorf("%s: a job the helper already ended must not be cancelled", tc.name)
			}
		}
	}
}

func TestClientHashToleratesPollsThatTheHelperRefuses(t *testing.T) {
	f := &fakeHelper{t: t}
	f.start = func() Job { return Job{ID: "j1", InstanceID: "A", State: JobRunning} }
	f.poll = func(n int64) (Job, int) {
		if n == 1 {
			return Job{}, http.StatusServiceUnavailable
		}
		return Job{ID: "j1", InstanceID: "A", State: JobDone, Result: &HashResult{SHA256: "ab", Bytes: 1}}, http.StatusOK
	}
	c := fakeClient(t, f)
	if _, err := c.Hash(context.Background(), "/data/a.bin", nil); err != nil {
		t.Fatal(err)
	}
}

func TestClientHashPassesOnAnAnswerFromAFailedJob(t *testing.T) {
	f := &fakeHelper{t: t}
	f.start = func() Job { return Job{ID: "j1", InstanceID: "A", State: JobRunning} }
	f.poll = func(int64) (Job, int) {
		return Job{ID: "j1", InstanceID: "A", State: JobFailed, Error: &Error{Code: CodeChanged, Message: "x"}}, http.StatusOK
	}
	c := fakeClient(t, f)
	_, err := c.Hash(context.Background(), "/data/a.bin", nil)
	if !IsAnswer(err) || ShouldFallBack(err) {
		t.Fatalf("got %v", err)
	}
	if f.deletes.Load() != 0 {
		t.Fatal("a failed job is already over")
	}
}

func TestClientHashCancelsTheJobWhenTheCallerGivesUp(t *testing.T) {
	f := &fakeHelper{t: t}
	f.start = func() Job { return Job{ID: "j1", InstanceID: "A", State: JobRunning} }
	f.poll = func(int64) (Job, int) { return Job{ID: "j1", InstanceID: "A", State: JobRunning}, http.StatusOK }
	c := fakeClient(t, f)
	// Long enough for the first poll to be in flight when the deadline passes.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_, err := c.Hash(ctx, "/data/a.bin", nil)
	if !errors.Is(err, context.DeadlineExceeded) || ShouldFallBack(err) {
		t.Fatalf("the caller's deadline is the caller's: %v", err)
	}
	waitDeletes(t, f, 1)
}

// A helper that accepts the connection and never answers is unavailable, not a
// deadline of the caller's: the caller must read through the mount.
func TestClientOwnTimeoutFallsBack(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer ts.Close()
	defer close(release)
	c, err := NewClient(ClientConfig{URL: ts.URL, Token: testToken, Mappings: []PathMapping{{Local: "/data", Remote: "/data"}}})
	if err != nil {
		t.Fatal(err)
	}
	c.timeout = 100 * time.Millisecond
	_, err = c.Identity(context.Background(), "/data/a.bin")
	if !errors.Is(err, ErrUnavailable) || !ShouldFallBack(err) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestClientErrorsNeverCarryTheToken(t *testing.T) {
	// A helper that echoes the Authorization header into its answers.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "echo "+r.Header.Get("Authorization"), http.StatusInternalServerError)
	}))
	c, err := NewClient(ClientConfig{URL: ts.URL, Token: testToken, Mappings: []PathMapping{{Local: "/data", Remote: "/data"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, errEcho := c.Identity(context.Background(), "/data/a.bin")
	ts.Close()
	_, errDown := c.Identity(context.Background(), "/data/a.bin")
	for _, err := range []error{errEcho, errDown} {
		if err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatalf("token leaked or no error: %v", err)
		}
	}
}
