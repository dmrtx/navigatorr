package nodes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A long-poll test shortens the second so a timeout takes milliseconds.
func fastWait(t *testing.T) {
	t.Helper()
	old := logWaitUnit
	logWaitUnit = 20 * time.Millisecond
	t.Cleanup(func() { logWaitUnit = old })
}

type waitAnswer struct {
	code int
	body map[string]any
	took time.Duration
}

// pollLog runs a log call in the background and delivers its answer.
func pollLog(t *testing.T, h http.Handler, ctx context.Context, path string) <-chan waitAnswer {
	t.Helper()
	out := make(chan waitAnswer, 1)
	go func() {
		start := time.Now()
		r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		a := waitAnswer{code: w.Code, took: time.Since(start)}
		a.body = decodeBody(w.Body.Bytes())
		out <- a
	}()
	return out
}

func lineCount(a waitAnswer) int {
	ls, _ := a.body["lines"].([]any)
	return len(ls)
}

func registerLogBuild(t *testing.T, h http.Handler, id string) string {
	t.Helper()
	path := "/nodes/v1/builds/" + id
	if code, e := call(t, h, "POST", "/nodes/v1/builds", `{"client_id":"`+id+`","target":"remote-box","source":"ci","title":"t"}`); code != 200 {
		t.Fatal(code, e)
	}
	return path
}

func waitFor(t *testing.T, ch <-chan waitAnswer, within time.Duration) waitAnswer {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(within):
		t.Fatal("the long-poll did not answer")
		return waitAnswer{}
	}
}

// settle waits until a waiter is registered on the build.
func settle(t *testing.T, s *Service, id string) {
	t.Helper()
	for i := 0; i < 500; i++ {
		s.logs.mu.Lock()
		n := s.logs.notify[id]
		ok := n != nil && n.n > 0
		s.logs.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no reader started waiting")
}

func TestLogWaitReturnsAtOnceWhenLinesExist(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	p := registerLogBuild(t, h, "bld_wait0001")
	call(t, h, "POST", p+"/heartbeat", `{"log":["one","two"]}`)
	a := waitFor(t, pollLog(t, h, context.Background(), p+"/log?after=1&wait=25"), 2*time.Second)
	if a.code != 200 || lineCount(a) != 1 || a.took > time.Second {
		t.Fatalf("%+v", a)
	}
	// wait=0 and no wait are the old behaviour: immediate even with nothing new.
	a = waitFor(t, pollLog(t, h, context.Background(), p+"/log?after=2&wait=0"), 2*time.Second)
	if a.code != 200 || lineCount(a) != 0 {
		t.Fatalf("%+v", a)
	}
	// A position past the end is a reset, answered at once.
	a = waitFor(t, pollLog(t, h, context.Background(), p+"/log?after=99&wait=25"), 2*time.Second)
	if a.code != 200 || a.body["reset"] != true || lineCount(a) != 2 {
		t.Fatalf("%+v", a)
	}
}

func TestLogWaitBlocksUntilANewLine(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	p := registerLogBuild(t, h, "bld_wait0002")
	call(t, h, "POST", p+"/heartbeat", `{"log":["one"]}`)
	ch := pollLog(t, h, context.Background(), p+"/log?after=1&wait=25")
	settle(t, s, "bld_wait0002")
	select {
	case a := <-ch:
		t.Fatalf("answered before a new line: %+v", a)
	case <-time.After(50 * time.Millisecond):
	}
	// A heartbeat without lines does not wake the answer into emptiness.
	call(t, h, "POST", p+"/heartbeat", `{"progress":"x"}`)
	call(t, h, "POST", p+"/heartbeat", `{"log":["two"]}`)
	a := waitFor(t, ch, 2*time.Second)
	if a.code != 200 || lineCount(a) != 1 || a.body["next_after"].(float64) != 2 {
		t.Fatalf("%+v", a)
	}
}

func TestLogWaitBeforeAnyOutputAndOnFinish(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	p := registerLogBuild(t, h, "bld_wait0003")
	// No buffer yet: the first line wakes the reader.
	ch := pollLog(t, h, context.Background(), p+"/log?wait=25")
	settle(t, s, "bld_wait0003")
	call(t, h, "POST", p+"/heartbeat", `{"log":["first"]}`)
	if a := waitFor(t, ch, 2*time.Second); lineCount(a) != 1 {
		t.Fatalf("%+v", a)
	}
	// The end of the build wakes it, with no new line.
	ch = pollLog(t, h, context.Background(), p+"/log?after=1&wait=25")
	settle(t, s, "bld_wait0003")
	call(t, h, "POST", p+"/finish", `{"status":"succeeded"}`)
	a := waitFor(t, ch, 2*time.Second)
	if a.code != 200 || lineCount(a) != 0 || a.body["status"] != "succeeded" || a.body["ended_at"] == nil {
		t.Fatalf("%+v", a)
	}
	// An ended build is never waited on.
	a = waitFor(t, pollLog(t, h, context.Background(), p+"/log?after=1&wait=25"), 2*time.Second)
	if a.took > time.Second || a.body["status"] != "succeeded" {
		t.Fatalf("%+v", a)
	}
}

func TestLogWaitReturnsWhenTheBuildIsLost(t *testing.T) {
	s, clock := buildSvc(t)
	h := s.Handler()
	p := registerLogBuild(t, h, "bld_wait0004")
	call(t, h, "POST", p+"/heartbeat", `{"log":["one"],"ttl_seconds":30}`)
	ch := pollLog(t, h, context.Background(), p+"/log?after=1&wait=25")
	settle(t, s, "bld_wait0004")
	clock.advance(time.Minute)
	s.Reconcile(context.Background())
	if a := waitFor(t, ch, 2*time.Second); a.body["status"] != "lost" {
		t.Fatalf("%+v", a)
	}
}

func TestLogWaitTimesOutWithNoLines(t *testing.T) {
	fastWait(t)
	s, _ := buildSvc(t)
	h := s.Handler()
	p := registerLogBuild(t, h, "bld_wait0005")
	a := waitFor(t, pollLog(t, h, context.Background(), p+"/log?wait=2"), 2*time.Second)
	if a.code != 200 || lineCount(a) != 0 || a.took < 30*time.Millisecond {
		t.Fatalf("a wait of 2 units must last about 40 ms: %+v", a)
	}
	if s.logs.waiters != 0 || len(s.logs.notify) != 0 {
		t.Fatalf("a finished wait must leave nothing registered: %d %d", s.logs.waiters, len(s.logs.notify))
	}
}

func TestLogWaitEndsWithTheRequestContext(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	p := registerLogBuild(t, h, "bld_wait0006")
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		ch := pollLog(t, h, ctx, p+"/log?wait=25")
		settle(t, s, "bld_wait0006")
		cancel()
		if a := waitFor(t, ch, 2*time.Second); a.took > time.Second {
			t.Fatalf("%+v", a)
		}
	}
	if s.logs.waiters != 0 || len(s.logs.notify) != 0 {
		t.Fatalf("cancelled waits left %d registered", s.logs.waiters)
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines grew from %d to %d", before, after)
	}
}

func TestLogWaitWakesWhenTheBufferIsDropped(t *testing.T) {
	s, clock := buildSvc(t)
	h := s.Handler()
	p := registerLogBuild(t, h, "bld_wait0007")
	call(t, h, "POST", p+"/heartbeat", `{"log":["one"]}`)
	ch := pollLog(t, h, context.Background(), p+"/log?after=1&wait=25")
	settle(t, s, "bld_wait0007")
	clock.advance(time.Second)
	s.logs.drop("bld_wait0007") // as when the buffer is recreated, evicted or expires
	a := waitFor(t, ch, 2*time.Second)
	// The reader's position is past the (now empty) end: a reset.
	if a.code != 200 || a.body["reset"] != true {
		t.Fatalf("%+v", a)
	}
}

func TestLogWaitRefusesBadValuesAndTooManyReaders(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	p := registerLogBuild(t, h, "bld_wait0008")
	for _, q := range []string{"wait=-1", "wait=26", "wait=x", "wait=1.5", "wait=999999999999999999999"} {
		code, e := call(t, h, "GET", p+"/log?"+q, "")
		if code != 400 || e["code"] != "invalid_request" || e["error"] == nil || e["hint"] == nil {
			t.Errorf("%s: %d %v", q, code, e)
		}
	}
	var releases []func()
	for i := 0; i < buildLogMaxWaiters; i++ {
		rel, err := s.logs.watch("bld_wait0008")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, rel)
	}
	code, e := call(t, h, "GET", p+"/log?wait=5", "")
	if code != 429 || e["code"] != "too_many_waiters" {
		t.Fatalf("%d %v", code, e)
	}
	// Reading without wait is never refused.
	if code, _ := call(t, h, "GET", p+"/log", ""); code != 200 {
		t.Fatal(code)
	}
	// With the cap full, a reader that would not wait still gets its answer:
	// lines already past after, or an unknown build.
	call(t, h, "POST", p+"/heartbeat", `{"log":["one"]}`)
	code, e = call(t, h, "GET", p+"/log?after=0&wait=5", "")
	if ls, _ := e["lines"].([]any); code != 200 || len(ls) != 1 {
		t.Fatalf("cap full, lines ready: %d %v", code, e)
	}
	if code, _ := call(t, h, "GET", "/nodes/v1/builds/nope/log?wait=5", ""); code != 404 {
		t.Fatalf("cap full, unknown build: %d", code)
	}
	if code, e := call(t, h, "GET", p+"/log?after=1&wait=5", ""); code != 429 || e["code"] != "too_many_waiters" {
		t.Fatalf("cap full, would wait: %d %v", code, e)
	}
	for _, r := range releases {
		r()
		r() // releasing twice is harmless
	}
	if s.logs.waiters != 0 {
		t.Fatal(s.logs.waiters)
	}
	if code, _ := call(t, h, "GET", "/nodes/v1/builds/nope/log?wait=1", ""); code != 404 {
		t.Fatal(code)
	}
}

func TestHeartbeatBatchOf256IsAcceptedAnd257Refused(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	p := registerLogBuild(t, h, "bld_batch001")
	mk := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = `"` + strings.Repeat("x", BuildLogMaxLine) + `"`
		}
		return `{"log":[` + strings.Join(parts, ",") + `]}`
	}
	if code, e := call(t, h, "POST", p+"/heartbeat", mk(256)); code != 200 {
		t.Fatal(code, e)
	}
	if code, e := call(t, h, "POST", p+"/heartbeat", mk(257)); code != 400 || e["code"] != "invalid_request" {
		t.Fatal(code, e)
	}
	_, all := call(t, h, "GET", p+"/log", "")
	if ls, _ := all["lines"].([]any); len(ls) != 256 {
		t.Fatalf("%d lines", len(ls))
	}
}

func decodeBody(b []byte) map[string]any {
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// A reader whose generation is gone (buffer dropped, nothing new since) gets
// its reset at once, as without wait, rather than after the whole wait.
func TestLogWaitStaleGenerationOnEmptyBufferAnswersAtOnce(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	p := registerLogBuild(t, h, "bld_wait0009")
	s.logs.drop("bld_wait0009")
	ch := pollLog(t, h, context.Background(), p+"/log?after=0&generation=old&wait=25")
	a := waitFor(t, ch, 2*time.Second)
	if a.code != 200 || a.body["reset"] != true {
		t.Fatalf("%+v", a)
	}
}
