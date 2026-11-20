package transcodeview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const goodBody = `{"nodes":[{"node":"nodeA","paused":true,"mode":"graceful","since":"2026-11-13T10:00:00Z","held_by":"operator","message":"paused"},{"node":"nodeB","paused":false,"message":"idle"}]}`

// fake is an engine stand-in that counts requests and can be told how to answer.
type fake struct {
	srv    *httptest.Server
	hits   atomic.Int64
	status atomic.Int64
	gate   atomic.Pointer[chan struct{}] // when set, requests wait on it
	key    atomic.Value
}

func newFake(t *testing.T) *fake {
	f := &fake{}
	f.status.Store(200)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.key.Store(r.Header.Get("X-Api-Key"))
		if g := f.gate.Load(); g != nil {
			select {
			case <-*g:
			case <-r.Context().Done():
				return
			}
		}
		if r.URL.Path != Path {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(f.status.Load()))
		if f.status.Load() == 200 {
			_, _ = w.Write([]byte(goodBody))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newClient(t *testing.T, f *fake, clk *clock) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: f.srv.URL + "/", APIKey: "synthetic-key", Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStatesDecodeAndSendTheKey(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, &clock{t: time.Unix(1000, 0)})
	got, err := c.States(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	a, b := got["nodeA"], got["nodeB"]
	if len(got) != 2 || !a.Paused || a.Mode != "graceful" || a.HeldBy != "operator" || a.Since != "2026-11-13T10:00:00Z" || a.Message != "paused" || b.Paused || b.Message != "idle" {
		t.Fatalf("%+v", got)
	}
	if f.key.Load() != "synthetic-key" {
		t.Fatalf("key %v", f.key.Load())
	}
	if _, err := New(Options{}); err == nil {
		t.Fatal("a base URL is required")
	}
}

func TestCacheServesWithinTheTTL(t *testing.T) {
	f := newFake(t)
	clk := &clock{t: time.Unix(1000, 0)}
	c := newClient(t, f, clk)
	first, _ := c.States(t.Context())
	first["nodeA"] = State{} // a caller changing its copy must not change the cache
	clk.add(CacheTTL - time.Millisecond)
	second, err := c.States(t.Context())
	if err != nil || f.hits.Load() != 1 || !second["nodeA"].Paused {
		t.Fatalf("hits=%d err=%v %+v", f.hits.Load(), err, second)
	}
	clk.add(2 * time.Millisecond)
	if _, err := c.States(t.Context()); err != nil || f.hits.Load() != 2 {
		t.Fatalf("expired: hits=%d err=%v", f.hits.Load(), err)
	}
}

func TestConcurrentCallersShareOneRequest(t *testing.T) {
	f := newFake(t)
	gate := make(chan struct{})
	f.gate.Store(&gate)
	c := newClient(t, f, &clock{t: time.Unix(1000, 0)})
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.States(context.Background())
			if err == nil && len(got) != 2 {
				err = context.DeadlineExceeded
			}
			errs <- err
		}()
	}
	// Wait for the request to arrive and the others to queue behind it.
	for f.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.hits.Load() != 1 {
		t.Fatalf("%d requests for %d callers", f.hits.Load(), n)
	}
}

// One caller giving up neither stops the request the others wait on nor
// counts as a failure.
func TestACancelledCallerBreaksNobodyAndStartsNoBackoff(t *testing.T) {
	f := newFake(t)
	gate := make(chan struct{})
	f.gate.Store(&gate)
	c := newClient(t, f, &clock{t: time.Unix(1000, 0)})

	cctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() { _, err := c.States(cctx); cancelled <- err }()
	for f.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	other := make(chan error, 1)
	go func() { _, err := c.States(context.Background()); other <- err }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	if err := <-cancelled; err != context.Canceled {
		t.Fatalf("the cancelled caller gets its own error, got %v", err)
	}
	close(gate)
	if err := <-other; err != nil {
		t.Fatalf("the other caller must still get the answer: %v", err)
	}
	// No backoff: a call after the cache expires asks again at once.
	c.mu.Lock()
	failed := c.failed
	c.mu.Unlock()
	if failed || f.hits.Load() != 1 {
		t.Fatalf("failed=%v hits=%d", failed, f.hits.Load())
	}
	// A caller already cancelled makes no request and records nothing.
	dead, stop := context.WithCancel(context.Background())
	stop()
	clk := &clock{t: time.Unix(5000, 0)}
	c2 := newClient(t, f, clk)
	hits := f.hits.Load()
	if _, err := c2.States(dead); err != context.Canceled || f.hits.Load() != hits {
		t.Fatalf("err=%v hits=%d", err, f.hits.Load())
	}
}

func TestAHungEngineAnswersSoftWithinTheTimeout(t *testing.T) {
	f := newFake(t)
	gate := make(chan struct{}) // never opened
	f.gate.Store(&gate)
	c := newClient(t, f, &clock{t: time.Unix(1000, 0)})
	start := time.Now()
	got, err := c.States(context.Background())
	took := time.Since(start)
	if err == nil || got != nil {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if took < RequestTimeout-100*time.Millisecond || took > RequestTimeout+400*time.Millisecond {
		t.Fatalf("took %v, want about %v", took, RequestTimeout)
	}
}

func TestFailureBacksOffFiveSecondsWithoutRequests(t *testing.T) {
	f := newFake(t)
	f.status.Store(500)
	clk := &clock{t: time.Unix(1000, 0)}
	c := newClient(t, f, clk)
	if got, err := c.States(t.Context()); err == nil || got != nil {
		t.Fatalf("a 500 is a soft failure: %v %v", got, err)
	}
	if f.hits.Load() != 1 {
		t.Fatal(f.hits.Load())
	}
	f.status.Store(200) // the engine is back, but we do not ask yet
	for _, d := range []time.Duration{0, time.Second, BackoffTTL - time.Second - time.Millisecond} {
		clk.add(d)
		got, err := c.States(t.Context())
		if err != ErrBackoff || got != nil || f.hits.Load() != 1 {
			t.Fatalf("during the backoff: err=%v hits=%d", err, f.hits.Load())
		}
	}
	clk.add(2 * time.Millisecond)
	got, err := c.States(t.Context())
	if err != nil || len(got) != 2 || f.hits.Load() != 2 {
		t.Fatalf("after the backoff: err=%v hits=%d", err, f.hits.Load())
	}
}

func TestNon200AndBadBodiesAreSoftFailures(t *testing.T) {
	for _, status := range []int{401, 404, 503} {
		f := newFake(t)
		f.status.Store(int64(status))
		c := newClient(t, f, &clock{t: time.Unix(1000, 0)})
		if got, err := c.States(t.Context()); err == nil || got != nil {
			t.Fatalf("%d: %v %v", status, got, err)
		}
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) }))
	defer bad.Close()
	c, _ := New(Options{BaseURL: bad.URL})
	if got, err := c.States(t.Context()); err == nil || got != nil {
		t.Fatalf("%v %v", got, err)
	}
	// Nothing listens: a refused connection is a soft failure too.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	c, _ = New(Options{BaseURL: url})
	if got, err := c.States(t.Context()); err == nil || got != nil {
		t.Fatalf("%v %v", got, err)
	}
}
