package openapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
)

const syntheticSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "t", "version": "1"},
  "paths": {
    "/transcode/v1/pauses/{name}": {"put": {"summary": "Pause or resume a worker", "description": "Send a mode.", "tags": ["workers"], "responses": {"200": {"description": "ok"}}}},
    "/transcode/v1/pauses": {"get": {"summary": "List every worker's pause", "tags": ["workers"], "responses": {"200": {"description": "ok"}}}},
    "/batches/{id}/goal": {"post": {"summary": "Change a batch goal", "tags": ["batches"], "responses": {"200": {"description": "ok"}}}},
    "/backups/{id}/restore": {"post": {"summary": "Restore a backup", "tags": ["backups"], "responses": {"200": {"description": "ok"}}}},
    "/backups": {"get": {"summary": "List retained backups", "tags": ["backups"], "responses": {"200": {"description": "ok"}}}},
    "/activity": {"get": {"summary": "Live slots and jobs on every worker", "description": "What each worker is encoding right now.", "tags": ["workers"], "responses": {"200": {"description": "ok"}}}},
    "/recipe-bundle/rollback": {"post": {"summary": "Roll the recipe bundle back", "tags": ["recipes"], "responses": {"200": {"description": "ok"}}}},
    "/podcasts/{id}/classify": {"post": {"summary": "Submit the classification of podcast blocks", "description": "Marks ads.", "tags": ["podcasts"], "responses": {"200": {"description": "ok"}}}},
    "/drain": {"put": {"summary": "Turn the deploy drain on or off", "description": "Stops new work on the workers.", "tags": ["workers"], "responses": {"200": {"description": "ok"}}}}
  }
}`

func parseSynthetic(t *testing.T) *Index {
	t.Helper()
	idx, err := Parse(context.Background(), "svc", []byte(syntheticSpec))
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func firstPath(res []EndpointSummary) string {
	if len(res) == 0 {
		return ""
	}
	return res[0].Path
}

func TestNaturalQueriesSynthetic(t *testing.T) {
	idx := parseSynthetic(t)
	for query, want := range map[string]string{
		"pause a worker":              "/transcode/v1/pauses/{name}",
		"change batch goal":           "/batches/{id}/goal",
		"restore backup":              "/backups/{id}/restore",
		"what is encoding now":        "/activity",
		"rollback recipe":             "/recipe-bundle/rollback",
		"classify podcast ads":        "/podcasts/{id}/classify",
		"drain workers before deploy": "/drain",
		"pause":                       "/transcode/v1/pauses",
		"Restore a backup":            "/backups/{id}/restore",
	} {
		res := idx.Search(query)
		found := false
		for _, r := range res {
			if r.Path == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: %s not in %v", query, want, res)
		}
	}
	// An exact phrase ranks on top.
	if got := firstPath(idx.Search("restore a backup")); got != "/backups/{id}/restore" {
		t.Errorf("exact phrase first = %s", got)
	}
	if got := idx.Search("zzzqq nothing"); len(got) != 0 {
		t.Errorf("unrelated query matched %v", got)
	}
}

func TestAllTokensBeatPartialMatches(t *testing.T) {
	idx := parseSynthetic(t)
	res := idx.Search("restore backup")
	if len(res) != 1 || res[0].Path != "/backups/{id}/restore" {
		t.Fatalf("want only the operation matching both tokens, got %v", res)
	}
	// No endpoint has all three: those matching at least half (2) remain.
	res = idx.Search("restore backup unicorn")
	if firstPath(res) != "/backups/{id}/restore" {
		t.Fatalf("partial fallback: %v", res)
	}
	// One of three tokens is below half: nothing.
	if res := idx.Search("restore unicorn dragon"); len(res) != 0 {
		t.Fatalf("below half must not match: %v", res)
	}
}

func TestQueryTokens(t *testing.T) {
	if got := queryTokens("Encoding?"); len(got) != 1 || got[0] != "encoding" {
		t.Fatalf("a one-word query is not stemmed: %v", got)
	}
	got := queryTokens("What is encoding now?")
	if len(got) != 1 || got[0] != "encod" {
		t.Fatalf("stop words dropped, rest stemmed: %v", got)
	}
	got = queryTokens("pausing workers before deploy")
	want := []string{"paus", "worker", "deploy"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

// Every natural query finds the right operation in the real transcode spec.
func TestNaturalQueriesRealTranscodeSpec(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "transcodeapi", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Parse(context.Background(), "transcode", data)
	if err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]string{
		"pause a worker":              "/transcode/v1/pauses/{name}",
		"change batch goal":           "/transcode/v1/batches/{id}/goal",
		"restore backup":              "/transcode/v1/backups/{id}/restore",
		"what is encoding now":        "/transcode/v1/activity",
		"rollback recipe":             "/transcode/v1/recipe-bundle/rollback",
		"classify podcast ads":        "/transcode/v1/actions/{id}/podcast/classify",
		"drain workers before deploy": "/transcode/v1/drain",
	} {
		res := idx.Search(query)
		pos := -1
		for i, r := range res {
			if r.Path == want {
				pos = i
				break
			}
		}
		t.Logf("%-30q -> %d results, %s at position %d (first: %s %s)", query, len(res), want, pos, firstMethod(res), firstPath(res))
		if pos < 0 || pos > 4 {
			t.Errorf("%q: %s at position %d, want within the top 5", query, want, pos)
		}
	}
}

func firstMethod(res []EndpointSummary) string {
	if len(res) == 0 {
		return ""
	}
	return res[0].Method
}

func TestStoreSearchOrdersAcrossServices(t *testing.T) {
	// aaa matches both tokens only in a description (score 2); bbb matches
	// them in the path and summary (score 6). Alphabetical order would put aaa
	// first, so bbb leading proves the cross-service score ordering.
	a, _ := Parse(context.Background(), "aaa", []byte(`{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{"/x":{"get":{"summary":"Other","description":"Restore the backup","responses":{"200":{"description":"ok"}}}}}}`))
	b, _ := Parse(context.Background(), "bbb", []byte(`{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{"/restore-backup":{"get":{"summary":"Restore a backup","responses":{"200":{"description":"ok"}}}}}}`))
	s := &Store{cfg: &config.Config{}, indices: map[string]*Index{"aaa": a, "bbb": b}}
	res := s.Search("restore backup", "")
	if len(res) != 2 || res[0].Service != "bbb" || res[1].Service != "aaa" {
		t.Fatalf("higher score in bbb must come first: %v", res)
	}
	if got := s.Search("restore backup", "aaa"); len(got) != 1 || got[0].Service != "aaa" {
		t.Fatalf("service filter: %v", got)
	}
}

// A full match comes before a partial one even when the partial one hits the
// primary fields (path, summary) and the full one only the description.
func TestFullMatchBeatsHigherScoringPartialMatch(t *testing.T) {
	const spec = `{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{
	"/alpha/beta":{"get":{"summary":"Alpha beta","responses":{"200":{"description":"ok"}}}},
	"/other":{"get":{"summary":"Other","description":"Handles alpha, beta and gamma together.","responses":{"200":{"description":"ok"}}}}}}`
	idx, err := Parse(context.Background(), "svc", []byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	res := idx.Search("alpha beta gamma")
	if len(res) != 2 || res[0].Path != "/other" {
		t.Fatalf("full match must lead: %v", res)
	}
}

func newLazyStore(t *testing.T, url string) (*Store, *time.Time) {
	t.Helper()
	cfg := &config.Config{Services: map[string]config.ServiceConfig{"transcode": {OpenAPIURL: url}}}
	clock := time.Unix(1000, 0)
	s := &Store{
		cfg:           cfg,
		cache:         NewCache(t.TempDir()),
		indices:       map[string]*Index{},
		attempts:      map[string]time.Time{},
		inflight:      map[string]chan struct{}{},
		retryInterval: 30 * time.Second,
		fetchTimeout:  2 * time.Second,
		now:           func() time.Time { return clock },
	}
	return s, &clock
}

func TestLazyLoadAfterFailedStartup(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "not up yet", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(syntheticSpec))
	}))
	defer srv.Close()

	s, clock := newLazyStore(t, srv.URL)
	ctx := context.Background()
	s.LoadAll(ctx) // fails: the server is not ready
	if s.GetIndex("transcode") != nil {
		t.Fatal("index must be empty after the failed load")
	}

	// Inside the retry window no new fetch is made.
	s.EnsureAll(ctx)
	if calls.Load() != 1 || s.GetIndex("transcode") != nil {
		t.Fatalf("rate limit broken: %d calls", calls.Load())
	}

	*clock = clock.Add(31 * time.Second)
	s.EnsureAll(ctx)
	if calls.Load() != 2 {
		t.Fatalf("want a retry after the interval, calls=%d", calls.Load())
	}
	if res := s.Search("pause a worker", ""); len(res) == 0 {
		t.Fatalf("search after lazy load found nothing")
	}

	// Loaded: never fetched again.
	*clock = clock.Add(time.Hour)
	s.EnsureLoaded(ctx, "transcode")
	if calls.Load() != 2 {
		t.Fatalf("loaded service refetched: %d", calls.Load())
	}
}

func TestLazyLoadFailureIsRateLimitedAndBounded(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	s, clock := newLazyStore(t, srv.URL)
	for i := 0; i < 5; i++ {
		s.EnsureLoaded(context.Background(), "transcode")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d, want 1 within the interval", calls.Load())
	}
	*clock = clock.Add(time.Minute)
	s.EnsureLoaded(context.Background(), "transcode")
	if calls.Load() != 2 {
		t.Fatalf("calls=%d, want 2", calls.Load())
	}
	// Unknown services are ignored.
	s.EnsureLoaded(context.Background(), "nope")
}

func TestRetryMissingLoadsWhenServerComesUp(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "not up yet", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(syntheticSpec))
	}))
	defer srv.Close()
	s, _ := newLazyStore(t, srv.URL)
	s.RetryMissing(context.Background(), 5*time.Millisecond, 5*time.Second)
	if s.GetIndex("transcode") == nil {
		t.Fatalf("not loaded after %d calls", calls.Load())
	}
}

func TestEnsureWaitsForLoadInFlightAndForceNeverRunsTwo(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		w.Write([]byte(syntheticSpec))
	}))
	defer srv.Close()
	s, _ := newLazyStore(t, srv.URL)
	s.fetchTimeout = 10 * time.Second

	results := make(chan bool, 3)
	go func() { results <- s.ensure(context.Background(), "transcode", false) }()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	go func() { results <- s.ensure(context.Background(), "transcode", false) }()
	go func() { results <- s.ensure(context.Background(), "transcode", true) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	for i := 0; i < 3; i++ {
		if !<-results {
			t.Fatal("every caller must see the loaded index")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d, want a single fetch", calls.Load())
	}
}

func TestCancelledCallerDoesNotPoisonTheLoad(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.Write([]byte(syntheticSpec))
	}))
	defer srv.Close()
	s, _ := newLazyStore(t, srv.URL)
	s.fetchTimeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for calls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel() // the caller gives up while the load is in flight
	}()
	if s.ensure(ctx, "transcode", false) {
		t.Log("loaded before the cancelled caller returned")
	}
	// The detached load finishes anyway and no failure window was recorded.
	deadline := time.Now().Add(5 * time.Second)
	for s.GetIndex("transcode") == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.GetIndex("transcode") == nil {
		t.Fatal("load must complete despite the cancelled caller")
	}
	s.mu.RLock()
	failures := len(s.attempts)
	s.mu.RUnlock()
	if failures != 0 {
		t.Fatalf("a cancelled caller must not record a failure: %d", failures)
	}
}

func TestRetryMissingStopsOnCancelDuringSleep(t *testing.T) {
	s, _ := newLazyStore(t, "http://127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RetryMissing(ctx, time.Hour, time.Hour)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RetryMissing did not stop on cancel")
	}
}

func TestRetryMissingDoesNotOvershootDeadline(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	s, _ := newLazyStore(t, srv.URL)
	start := time.Now()
	// The first sleep (1 h) is clamped to the 100 ms budget.
	s.RetryMissing(context.Background(), time.Hour, 100*time.Millisecond)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("overshot the deadline: %v", d)
	}
	// A non-positive initial delay is treated as one second, still clamped.
	start = time.Now()
	s.RetryMissing(context.Background(), 0, 50*time.Millisecond)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("overshot with initial=0: %v", d)
	}
}

func TestRetryMissingReturnsAtDeadlineWithHungServer(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		http.Error(w, "released", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	defer close(release) // runs first: unblocks the handlers
	s, _ := newLazyStore(t, srv.URL)
	s.fetchTimeout = 5 * time.Second

	start := time.Now()
	s.RetryMissing(context.Background(), 10*time.Millisecond, 300*time.Millisecond)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("returned after %v, want about 300ms", d)
	}
	if calls.Load() == 0 {
		t.Fatal("the round never reached the hung server")
	}
}

func TestRefreshWinsOverStaleInFlightLoad(t *testing.T) {
	const specA = `{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{"/version-a":{"get":{"summary":"a","responses":{"200":{"description":"ok"}}}}}}`
	const specB = `{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{"/version-b":{"get":{"summary":"b","responses":{"200":{"description":"ok"}}}}}}`
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-release
			w.Write([]byte(specA))
			return
		}
		w.Write([]byte(specB))
	}))
	defer srv.Close()
	s, _ := newLazyStore(t, srv.URL)
	s.fetchTimeout = 10 * time.Second

	go s.EnsureLoaded(context.Background(), "transcode")
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	refreshed := make(chan error, 1)
	go func() { refreshed <- s.Refresh(context.Background(), "transcode") }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	idx := s.GetIndex("transcode")
	if idx == nil || idx.Endpoints["/version-b"] == nil || idx.Endpoints["/version-a"] != nil {
		t.Fatalf("final index must be version B: %+v", idx)
	}
}

func TestCancelledContextStartsNoLoads(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(syntheticSpec))
	}))
	defer srv.Close()
	s, _ := newLazyStore(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.LoadAll(ctx)
	s.EnsureAll(ctx)
	s.EnsureLoaded(ctx, "transcode")
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 0 || s.GetIndex("transcode") != nil {
		t.Fatalf("a cancelled ctx started %d fetches", calls.Load())
	}
}

func TestFailedRefreshDoesNotBlockLazyLoad(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "not up yet", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(syntheticSpec))
	}))
	defer srv.Close()

	s, _ := newLazyStore(t, srv.URL)
	if err := s.Refresh(context.Background(), "transcode"); err == nil {
		t.Fatal("first refresh must fail")
	}
	// No time passes: the lazy load must still try at once.
	s.EnsureLoaded(context.Background(), "transcode")
	if calls.Load() != 2 || s.GetIndex("transcode") == nil {
		t.Fatalf("lazy load blocked by a failed refresh: calls=%d", calls.Load())
	}
}

func TestRefreshWithCancelledContextKeepsCache(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(syntheticSpec))
	}))
	defer srv.Close()
	s, _ := newLazyStore(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Refresh(ctx, "transcode"); err == nil {
		t.Fatal("refresh with a cancelled context must fail")
	}
	s.mu.RLock()
	n, busy := len(s.attempts), len(s.inflight)
	s.mu.RUnlock()
	if calls.Load() != 0 || n != 0 || busy != 0 {
		t.Fatalf("cancelled refresh touched state: calls=%d attempts=%d inflight=%d", calls.Load(), n, busy)
	}
}
