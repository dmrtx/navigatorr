package nodesclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A synthetic key, built at run time so secret scanners have nothing to read.
var testKey = strings.Repeat("nodeskey", 5)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type logRec struct {
	mu            sync.Mutex
	errors, warns []string
}

func (l *logRec) Errorf(f string, a ...any) {
	l.mu.Lock()
	l.errors = append(l.errors, fmt.Sprintf(f, a...))
	l.mu.Unlock()
}
func (l *logRec) Warnf(f string, a ...any) {
	l.mu.Lock()
	l.warns = append(l.warns, fmt.Sprintf(f, a...))
	l.mu.Unlock()
}
func (l *logRec) errCount() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.errors) }

const twoNodes = `{"nodes":[
 {"name":"nodeA","builds_paused":true,"builds_paused_since":"2030-01-01T00:00:00Z","running_builds":[
  {"id":"bld_1","source":"ci","holder":"runner1","kind":"gradle","title":"run","started_at":"2030-01-01T01:00:00Z","status":"running","priority":"normal","preempt_mode":"graceful","created_at":"2030-01-01T00:59:00Z","last_heartbeat_at":"2030-01-01T01:01:00Z","expires_at":"2030-01-01T01:16:00Z"}]},
 {"name":"nodeB","builds_paused":false,"running_builds":[]}]}`

// fixture is a Nodes stand-in that counts GETs and POSTs.
type fixture struct {
	srv   *httptest.Server
	gets  atomic.Int32
	posts atomic.Int32
	h     atomic.Value // func(w, r)
	c     *Client
	clock *fakeClock
	log   *logRec
}

func newFixture(t *testing.T, h http.HandlerFunc, mod func(*Options)) *fixture {
	t.Helper()
	f := &fixture{clock: newClock(), log: &logRec{}}
	f.h.Store(h)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			f.gets.Add(1)
		} else {
			f.posts.Add(1)
		}
		f.h.Load().(http.HandlerFunc)(w, r)
	}))
	t.Cleanup(f.srv.Close)
	o := Options{BaseURL: f.srv.URL, Key: testKey, Logger: f.log, Now: f.clock.Now}
	if mod != nil {
		mod(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	f.c = c
	return f
}

func serveJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func TestOneSnapshotServesAllThreeReads(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nodes/v1/nodes" || r.URL.Query().Get("transcode") != "false" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if r.Header.Get("X-Api-Key") != testKey {
			t.Error("the service key was not sent")
		}
		serveJSON(twoNodes)(w, r)
	}, nil)
	recs := f.c.NodeBuildRecords("nodeA")
	if len(recs) != 1 || recs[0].ID != "bld_1" || recs[0].Holder != "runner1" || recs[0].Status != "running" ||
		recs[0].Priority != "normal" || recs[0].PreemptMode != "graceful" || recs[0].CreatedAt == "" || recs[0].LastHeartbeatAt == "" || recs[0].ExpiresAt == "" {
		t.Fatalf("records: %+v", recs)
	}
	if got := f.c.NodeBuildRecords("nodeB"); len(got) != 0 {
		t.Fatalf("nodeB: %+v", got)
	}
	m, err := f.c.BuildsPauses()
	if err != nil || len(m) != 1 || m["nodeA"] != "2030-01-01T00:00:00Z" {
		t.Fatalf("pauses: %v %v", m, err)
	}
	if p, err := f.c.BuildsPaused("nodeA"); !p || err != nil {
		t.Fatal(p, err)
	}
	if p, err := f.c.BuildsPaused("nodeB"); p || err != nil {
		t.Fatal(p, err)
	}
	if n := f.gets.Load(); n != 1 {
		t.Fatalf("%d requests for one snapshot", n)
	}
}

func TestTTLThenFetchAgain(t *testing.T) {
	f := newFixture(t, serveJSON(twoNodes), nil)
	_, _ = f.c.BuildsPauses()
	f.clock.Advance(1400 * time.Millisecond)
	_, _ = f.c.BuildsPauses()
	if f.gets.Load() != 1 {
		t.Fatalf("within the TTL: %d requests", f.gets.Load())
	}
	f.clock.Advance(200 * time.Millisecond)
	_, _ = f.c.BuildsPauses()
	if f.gets.Load() != 2 {
		t.Fatalf("after the TTL: %d requests", f.gets.Load())
	}
}

// gate lets a handler hold its answer until the test releases it.
type gate struct {
	entered chan struct{}
	release chan struct{}
}

func newGate() *gate { return &gate{entered: make(chan struct{}, 16), release: make(chan struct{})} }
func (g *gate) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.entered <- struct{}{}
		<-g.release
		h(w, r)
	}
}

func TestConcurrentReadersShareOneFlight(t *testing.T) {
	g := newGate()
	f := newFixture(t, g.wrap(serveJSON(twoNodes)), nil)
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := f.c.BuildsPaused("nodeA")
			if err != nil || !p {
				errs <- fmt.Errorf("%v %v", p, err)
			}
		}()
	}
	<-g.entered
	time.Sleep(50 * time.Millisecond) // let the others reach the flight
	close(g.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if f.gets.Load() != 1 {
		t.Fatalf("%d requests for %d readers", f.gets.Load(), n)
	}
}

func TestCancelledCallerGetsItsOwnErrorAndNothingElseChanges(t *testing.T) {
	g := newGate()
	f := newFixture(t, g.wrap(serveJSON(twoNodes)), nil)
	ctxA, cancelA := context.WithCancel(context.Background())
	aErr := make(chan error, 1)
	go func() { _, err := f.c.buildsPaused(ctxA, "nodeA"); aErr <- err }()
	<-g.entered
	bRes := make(chan bool, 1)
	go func() { p, _ := f.c.BuildsPaused("nodeA"); bRes <- p }()
	time.Sleep(30 * time.Millisecond)
	cancelA()
	if err := <-aErr; !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("cancelled caller: %v", err)
	}
	close(g.release)
	if !<-bRes {
		t.Fatal("the other caller lost the result")
	}
	// No backoff and no failure: the next read is served from the cache.
	if p, err := f.c.BuildsPaused("nodeA"); !p || err != nil {
		t.Fatal(p, err)
	}
	if f.gets.Load() != 1 {
		t.Fatalf("%d requests", f.gets.Load())
	}
}

func TestACancelAloneIsNeverAFailureOrABackoff(t *testing.T) {
	g := newGate()
	f := newFixture(t, g.wrap(serveJSON(twoNodes)), nil)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := f.c.buildsPauses(ctx); errc <- err }()
	<-g.entered
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(g.release)
	// The flight the cancelled caller started finishes and fills the cache.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if m, err := f.c.BuildsPauses(); err == nil && len(m) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no result after the cancel")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if f.gets.Load() != 1 {
		t.Fatalf("%d requests", f.gets.Load())
	}
}

func TestFailureHasNoStaleWindowAndBacksOffFiveSeconds(t *testing.T) {
	f := newFixture(t, serveJSON(twoNodes), nil)
	if p, _ := f.c.BuildsPaused("nodeA"); !p {
		t.Fatal("first read")
	}
	f.h.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }))
	f.clock.Advance(2 * time.Second) // TTL over
	if got := f.c.NodeBuildRecords("nodeA"); len(got) != 0 {
		t.Fatalf("a failed read must show no records, got %+v", got)
	}
	if _, err := f.c.BuildsPauses(); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := f.c.BuildsPaused("nodeA"); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if f.gets.Load() != 2 {
		t.Fatalf("%d requests; the failure should have cost one", f.gets.Load())
	}
	f.clock.Advance(4900 * time.Millisecond)
	if _, err := f.c.BuildsPauses(); !errors.Is(err, ErrUnavailable) || f.gets.Load() != 2 {
		t.Fatalf("inside the backoff: %v, %d requests", err, f.gets.Load())
	}
	f.h.Store(http.HandlerFunc(serveJSON(twoNodes)))
	f.clock.Advance(200 * time.Millisecond)
	if m, err := f.c.BuildsPauses(); err != nil || len(m) != 1 || f.gets.Load() != 3 {
		t.Fatalf("after the backoff: %v %v, %d requests", m, err, f.gets.Load())
	}
	if !strings.Contains(ErrUnavailable.Error(), "builds registry not available") {
		t.Fatal(ErrUnavailable)
	}
}

func TestRefusedKeyIsUnavailableAndLogsError(t *testing.T) {
	for _, code := range []int{401, 403} {
		f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"x","code":"unauthorized"}`, code)
		}, nil)
		if _, err := f.c.BuildsPaused("nodeA"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%d: %v", code, err)
		}
		if len(f.c.NodeBuildRecords("nodeA")) != 0 {
			t.Fatal("records")
		}
		if f.log.errCount() != 1 || !strings.Contains(f.log.errors[0], "service key") || strings.Contains(f.log.errors[0], testKey) {
			t.Fatalf("%d: log %q", code, f.log.errors)
		}
		// Rate limited: more failures after the backoff, inside the minute, add nothing.
		f.clock.Advance(6 * time.Second)
		_, _ = f.c.BuildsPaused("nodeA")
		if f.log.errCount() != 1 {
			t.Fatalf("%d: logged again: %q", code, f.log.errors)
		}
		f.clock.Advance(time.Minute)
		_, _ = f.c.BuildsPaused("nodeA")
		if f.log.errCount() != 2 {
			t.Fatalf("%d: no new log after a minute: %q", code, f.log.errors)
		}
	}
}

func TestBadAnswersAreUnavailable(t *testing.T) {
	for name, body := range map[string]string{"not json": "<html>", "no nodes": `{}`, "truncated": `{"nodes":[{"name":`} {
		f := newFixture(t, serveJSON(body), nil)
		if _, err := f.c.BuildsPauses(); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSlowAnswerTimesOutAsUnavailable(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, func(o *Options) { o.ReadTimeout = 80 * time.Millisecond })
	began := time.Now()
	if _, err := f.c.BuildsPauses(); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if time.Since(began) > 2*time.Second {
		t.Fatal("did not time out")
	}
}

func TestUnreachableRegistryIsUnavailable(t *testing.T) {
	f := newFixture(t, serveJSON(twoNodes), nil)
	f.srv.Close()
	if _, err := f.c.BuildsPauses(); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

// --- generation ---

func pauseOK(w http.ResponseWriter, r *http.Request) { serveJSON(`{"name":"nodeA"}`)(w, r) }

// serveRoutes answers GET with the first handler and POST with the second.
func serveRoutes(get, post http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			get(w, r)
		} else {
			post(w, r)
		}
	}
}

func TestReadInFlightDuringASuccessfulWriteDoesNotStoreItsOldResult(t *testing.T) {
	g := newGate()
	f := newFixture(t, serveRoutes(g.wrap(serveJSON(twoNodes)), pauseOK), nil)
	got := make(chan bool, 1)
	go func() { p, _ := f.c.BuildsPaused("nodeA"); got <- p }()
	<-g.entered // the old read is on its way
	if err := f.c.SetBuildsPause(context.Background(), "nodeA", false); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	<-got
	// The read that began after the write must go to Nodes again.
	if _, err := f.c.BuildsPauses(); err != nil {
		t.Fatal(err)
	}
	if f.gets.Load() != 2 {
		t.Fatalf("%d GETs: the old result was kept", f.gets.Load())
	}
}

func TestReadInFlightDuringAnUnknownWriteDoesNotStoreItsOldResult(t *testing.T) {
	g := newGate()
	hang := func(w http.ResponseWriter, r *http.Request) {
		c, _, _ := w.(http.Hijacker).Hijack()
		_ = c.Close()
	}
	f := newFixture(t, serveRoutes(g.wrap(serveJSON(twoNodes)), hang), nil)
	got := make(chan struct{}, 1)
	go func() { _, _ = f.c.BuildsPaused("nodeA"); got <- struct{}{} }()
	<-g.entered
	if err := f.c.SetBuildsPause(context.Background(), "nodeA", true); err == nil || err.Error() != msgNoAnswer {
		t.Fatalf("%v", err)
	}
	close(g.release)
	<-got
	if _, err := f.c.BuildsPauses(); err != nil {
		t.Fatal(err)
	}
	if f.gets.Load() != 2 {
		t.Fatalf("%d GETs", f.gets.Load())
	}
}

func TestAReadAfterASuccessfulWriteOpensANewFlight(t *testing.T) {
	f := newFixture(t, serveRoutes(serveJSON(twoNodes), pauseOK), nil)
	_, _ = f.c.BuildsPauses() // cached
	if err := f.c.SetBuildsPause(context.Background(), "nodeA", false); err != nil {
		t.Fatal(err)
	}
	_, _ = f.c.BuildsPauses()
	if f.gets.Load() != 2 {
		t.Fatalf("%d GETs: the cache survived a write", f.gets.Load())
	}
}

func TestAWriteClearsTheBackoff(t *testing.T) {
	f := newFixture(t, serveRoutes(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 500) }, pauseOK), nil)
	_, _ = f.c.BuildsPauses() // fails, backs off
	if err := f.c.SetBuildsPause(context.Background(), "nodeA", true); err != nil {
		t.Fatal(err)
	}
	_, _ = f.c.BuildsPauses()
	if f.gets.Load() != 2 {
		t.Fatalf("%d GETs: the backoff survived a write", f.gets.Load())
	}
}

func TestWritesThatChangeNothingLeaveTheCacheAlone(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"401": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 401) },
		"404": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"unknown node","code":"unknown_node"}`))
		},
		"500": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) },
	}
	for name, post := range cases {
		f := newFixture(t, serveRoutes(serveJSON(twoNodes), post), nil)
		_, _ = f.c.BuildsPauses()
		if err := f.c.SetBuildsPause(context.Background(), "nodeA", true); err == nil {
			t.Fatalf("%s: no error", name)
		}
		_, _ = f.c.BuildsPauses()
		if f.gets.Load() != 1 {
			t.Errorf("%s: %d GETs: a refused write dropped the cache", name, f.gets.Load())
		}
	}
	// A dial that is refused too.
	f := newFixture(t, serveJSON(twoNodes), nil)
	_, _ = f.c.BuildsPauses()
	c2, _ := New(Options{BaseURL: closedPortURL(t), Key: testKey, Logger: f.log, Now: f.clock.Now})
	c2.snap, c2.snapUntil = f.c.snap, f.c.snapUntil
	if err := c2.SetBuildsPause(context.Background(), "nodeA", true); err == nil || err.Error() != msgUnreachable {
		t.Fatal(err)
	}
	if c2.snap == nil || c2.gen != 0 {
		t.Fatal("a refused dial dropped the cache")
	}
}

// --- write messages ---

func closedPortURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + ln.Addr().String()
	_ = ln.Close()
	return u
}

func TestSetBuildsPauseSuccessSendsTheRightRequest(t *testing.T) {
	var gotPath, gotBody, gotKey, gotType string
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotType = r.URL.EscapedPath(), r.Header.Get("X-Api-Key"), r.Header.Get("Content-Type")
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		pauseOK(w, r)
	}, nil)
	if err := f.c.SetBuildsPause(context.Background(), "node A/b", true); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/nodes/v1/nodes/node%20A%2Fb/builds-pause" || gotBody != `{"paused":true}` || gotKey != testKey || gotType != "application/json" {
		t.Fatalf("%q %q %q %q", gotPath, gotBody, gotKey, gotType)
	}
}

func TestSetBuildsPauseErrors(t *testing.T) {
	t.Run("dial refused", func(t *testing.T) {
		c, _ := New(Options{BaseURL: closedPortURL(t), Key: testKey, Logger: &logRec{}})
		defer c.Close()
		if err := c.SetBuildsPause(context.Background(), "nodeA", true); err == nil || err.Error() != "builds registry unreachable; nothing changed" {
			t.Fatal(err)
		}
	})
	t.Run("timeout after send", func(t *testing.T) {
		f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body) // the server notices a closed connection once the body is read
			<-r.Context().Done()
		}, func(o *Options) { o.WriteTimeout = 150 * time.Millisecond })
		if err := f.c.SetBuildsPause(context.Background(), "nodeA", true); err == nil || err.Error() != "builds registry did not answer; the pause may or may not have changed, check again" {
			t.Fatal(err)
		}
	})
	t.Run("reset after send", func(t *testing.T) {
		f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			c, _, _ := w.(http.Hijacker).Hijack()
			_ = c.Close()
		}, nil)
		if err := f.c.SetBuildsPause(context.Background(), "nodeA", true); err == nil || err.Error() != "builds registry did not answer; the pause may or may not have changed, check again" {
			t.Fatal(err)
		}
	})
	t.Run("unknown node", func(t *testing.T) {
		f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"node is not configured","code":"unknown_node"}`))
		}, nil)
		err := f.c.SetBuildsPause(context.Background(), "nodeZ", true)
		if !errors.Is(err, ErrUnknownNode) || !strings.Contains(err.Error(), "unknown to the builds registry") || !strings.Contains(err.Error(), "nodeZ") {
			t.Fatal(err)
		}
	})
	for _, code := range []int{401, 403} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", code) }, nil)
			if err := f.c.SetBuildsPause(context.Background(), "nodeA", true); err == nil || err.Error() != "builds registry refused the service key; nothing changed" {
				t.Fatal(err)
			}
			if f.log.errCount() != 1 || strings.Contains(f.log.errors[0], testKey) {
				t.Fatalf("log %q", f.log.errors)
			}
		})
	}
	t.Run("other status", func(t *testing.T) {
		f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"paused is required","code":"invalid_request"}`))
		}, nil)
		err := f.c.SetBuildsPause(context.Background(), "nodeA", true)
		if err == nil || !strings.Contains(err.Error(), "paused is required") || !strings.Contains(err.Error(), "nothing changed") {
			t.Fatal(err)
		}
	})
	t.Run("caller cancels before dialling", func(t *testing.T) {
		f := newFixture(t, serveJSON(`{}`), nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := f.c.SetBuildsPause(ctx, "nodeA", true); err == nil || err.Error() != msgUnreachable || f.posts.Load() != 0 {
			t.Fatal(err, f.posts.Load())
		}
	})
}

// --- drift ---

func TestNameDriftLogsOncePerMinute(t *testing.T) {
	known := []string{"nodeA", "nodeC"}
	f := newFixture(t, serveJSON(twoNodes), func(o *Options) { o.KnownNodes = func() []string { return known } })
	read := func() { f.clock.Advance(2 * time.Second); _, _ = f.c.BuildsPauses() }
	read()
	if f.log.errCount() != 1 || !strings.Contains(f.log.errors[0], "differ") {
		t.Fatalf("%q", f.log.errors)
	}
	read()
	read()
	if f.log.errCount() != 1 {
		t.Fatalf("again within a minute: %q", f.log.errors)
	}
	f.clock.Advance(time.Minute)
	read()
	if f.log.errCount() != 2 {
		t.Fatalf("after a minute: %q", f.log.errors)
	}
	known = []string{"nodeB", "nodeA"} // same set, any order
	f.clock.Advance(time.Minute)
	read()
	if f.log.errCount() != 2 {
		t.Fatalf("equal names logged: %q", f.log.errors)
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	for _, o := range []Options{
		{BaseURL: "", Key: testKey}, {BaseURL: "ftp://x", Key: testKey}, {BaseURL: "http://127.0.0.1:1/path", Key: testKey},
		{BaseURL: "http://u:p@127.0.0.1:1", Key: testKey}, {BaseURL: "http://127.0.0.1:1", Key: ""}, {BaseURL: "http://127.0.0.1:1", Key: "a b"},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}
