package mcpproxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// sse writes events as one text/event-stream response.
func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", e)
	}
}

func callWith(t *testing.T, h func(w http.ResponseWriter, r *http.Request, env envelope), mod func(*Options)) (*fake, *gateway, reply) {
	t.Helper()
	f := newFake(t)
	f.onCall = func(w http.ResponseWriter, r *http.Request, env envelope) bool { h(w, r, env); return true }
	g := newGateway(t, f.url(), mod)
	return f, g, g.call("5", "memory_set", `{"q":"x"}`)
}

func TestResultsComeBackUntouched(t *testing.T) {
	const result = `{"content":[{"type":"text","text":"a < b & c"}],"structuredContent":{"n":1,"ok":true},"isError":true,"_meta":{"x":[1,2]}}`
	_, _, r := callWith(t, func(w http.ResponseWriter, _ *http.Request, env envelope) {
		jsonOK(w, env.ID, result)
	}, nil)
	want200JSON(t, r)
	if want := `{"jsonrpc":"2.0","id":5,"result":` + result + "}\n"; string(r.body) != want {
		t.Fatalf("body changed:\n got %s\nwant %s", r.body, want)
	}
}

func TestJSONRPCErrorFromTranscodeIsKeptWholeAndNotRetried(t *testing.T) {
	const e = `{"code":-32602,"message":"bad argument","data":{"field":"q"}}`
	f, _, r := callWith(t, func(w http.ResponseWriter, _ *http.Request, env envelope) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":%s}`, env.ID, e)
	}, nil)
	want200JSON(t, r)
	if want := `{"jsonrpc":"2.0","id":5,"error":` + e + `}`; string(r.body) != want {
		t.Fatalf("got %s want %s", r.body, want)
	}
	if f.calls.Load() != 1 {
		t.Errorf("calls %d, an upstream error is final", f.calls.Load())
	}
}

func TestSSEResponses(t *testing.T) {
	notif := `{"jsonrpc":"2.0","method":"notifications/message","params":{"data":"hi"}}`
	big := strings.Repeat("x", 200<<10) // well over 64 KiB
	cases := []struct {
		name   string
		events func(id string) []string
		want   string // "" means MsgUnknown
	}{
		{"notification then response", func(id string) []string {
			return []string{notif, `{"jsonrpc":"2.0","id":` + id + `,"result":{"content":[{"type":"text","text":"fin"}]}}`}
		}, `{"jsonrpc":"2.0","id":5,"result":{"content":[{"type":"text","text":"fin"}]}}`},
		{"large event", func(id string) []string {
			return []string{`{"jsonrpc":"2.0","id":` + id + `,"result":{"content":[{"type":"text","text":"` + big + `"}]}}`}
		}, `{"jsonrpc":"2.0","id":5,"result":{"content":[{"type":"text","text":"` + big + `"}]}}`},
		{"no final response", func(id string) []string { return []string{notif, notif} }, ""},
		{"server request aborts the call", func(id string) []string {
			return []string{`{"jsonrpc":"2.0","id":99,"method":"sampling/createMessage","params":{}}`,
				`{"jsonrpc":"2.0","id":` + id + `,"result":{"content":[]}}`}
		}, ""},
		{"other id", func(id string) []string { return []string{`{"jsonrpc":"2.0","id":6,"result":{}}`} }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, _, r := callWith(t, func(w http.ResponseWriter, _ *http.Request, env envelope) {
				sse(w, c.events(string(env.ID))...)
			}, nil)
			if c.want == "" {
				wantUnknown(t, r)
			} else {
				want200JSON(t, r)
				if string(r.body) != c.want {
					t.Fatalf("got %.200s", r.body)
				}
			}
			if f.calls.Load() != 1 {
				t.Errorf("calls %d, a dispatched call is never repeated", f.calls.Load())
			}
		})
	}
}

func TestSSEWithoutTrailingBlankLine(t *testing.T) {
	_, _, r := callWith(t, func(w http.ResponseWriter, _ *http.Request, env envelope) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, `data: {"jsonrpc":"2.0","id":%s,"result":{"content":[]}}`, env.ID)
	}, nil)
	want200JSON(t, r)
	if !strings.Contains(string(r.body), `"result"`) {
		t.Fatalf("got %s", r.body)
	}
}

func TestResponseSizeLimit(t *testing.T) {
	pad := func(n int) string { return strings.Repeat("a", n) }
	reply := func(n int) func(w http.ResponseWriter, _ *http.Request, env envelope) {
		return func(w http.ResponseWriter, _ *http.Request, env envelope) {
			jsonOK(w, env.ID, `{"content":[{"type":"text","text":"`+pad(n)+`"}]}`)
		}
	}
	_, _, ok := callWith(t, reply(maxBody-1024), nil)
	if isErr, _ := toolResult(t, ok); isErr {
		t.Error("a response just under 4 MiB must pass")
	}
	f, _, over := callWith(t, reply(maxBody), nil)
	wantUnknown(t, over)
	if f.calls.Load() != 1 {
		t.Error("an oversized response is never repeated")
	}
	_, _, overSSE := callWith(t, func(w http.ResponseWriter, _ *http.Request, env envelope) {
		sse(w, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"%s"}]}}`, env.ID, pad(maxBody)))
	}, nil)
	wantUnknown(t, overSSE)
}

func TestErrorClassification(t *testing.T) {
	text404 := func(w http.ResponseWriter, _ *http.Request, _ envelope) { http.NotFound(w, nil) }
	status := func(code int) func(http.ResponseWriter, *http.Request, envelope) {
		return func(w http.ResponseWriter, _ *http.Request, _ envelope) { http.Error(w, "x", code) }
	}
	bodyCode := func(code int, ctype, b string) func(http.ResponseWriter, *http.Request, envelope) {
		return func(w http.ResponseWriter, _ *http.Request, _ envelope) {
			w.Header().Set("Content-Type", ctype)
			if code != 0 {
				w.WriteHeader(code)
			}
			fmt.Fprint(w, b)
		}
	}
	body := func(ctype, b string) func(http.ResponseWriter, *http.Request, envelope) {
		return bodyCode(0, ctype, b)
	}
	cases := map[string]func(http.ResponseWriter, *http.Request, envelope){
		"403":                 status(403),
		"400":                 status(400),
		"404 route":           text404,
		"404 other body":      func(w http.ResponseWriter, _ *http.Request, _ envelope) { http.Error(w, "Session not found", 404) },
		"404 json body":       bodyCode(404, "application/json", "Invalid session ID\n"),
		"500":                 status(500),
		"502":                 status(502),
		"malformed JSON":      body("application/json", `{"jsonrpc":"2.0","id":5,"result":`),
		"empty body":          body("application/json", ``),
		"html":                body("text/html", `<html></html>`),
		"different id":        body("application/json", `{"jsonrpc":"2.0","id":6,"result":{}}`),
		"id as text":          body("application/json", `{"jsonrpc":"2.0","id":"5","result":{}}`),
		"no id":               body("application/json", `{"jsonrpc":"2.0","result":{}}`),
		"wrong version":       body("application/json", `{"jsonrpc":"1.0","id":5,"result":{}}`),
		"result and error":    body("application/json", `{"jsonrpc":"2.0","id":5,"result":{},"error":{"code":1,"message":"m"}}`),
		"neither":             body("application/json", `{"jsonrpc":"2.0","id":5}`),
		"a request, no reply": body("application/json", `{"jsonrpc":"2.0","id":5,"method":"ping"}`),
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			f, g, r := callWith(t, h, nil)
			wantUnknown(t, r)
			if f.calls.Load() != 1 || f.inits.Load() != 1 {
				t.Errorf("calls=%d inits=%d: nothing here may be retried", f.calls.Load(), f.inits.Load())
			}
			if len(g.logs.errors()) != 0 {
				t.Errorf("unexpected ERROR logs %v", g.logs.errors())
			}
		})
	}
}

func TestUnauthorizedIsNotRunAndLoggedAsError(t *testing.T) {
	f, g, r := callWith(t, func(w http.ResponseWriter, _ *http.Request, _ envelope) {
		http.Error(w, "unauthorized", 401)
	}, nil)
	wantNotRun(t, r)
	if f.calls.Load() != 1 || f.inits.Load() != 1 {
		t.Errorf("calls=%d inits=%d, a 401 is not retried", f.calls.Load(), f.inits.Load())
	}
	if e := g.logs.errors(); len(e) == 0 || !strings.Contains(e[0], "401") {
		t.Errorf("want an authentication ERROR log, got %v", e)
	}
	if strings.Contains(string(r.body), "401") {
		t.Error("the model-facing text must not carry upstream details")
	}
}

func TestCutAfterSendAndTimeoutAreUnknownAndNeverRepeated(t *testing.T) {
	t.Run("cut", func(t *testing.T) {
		f, _, r := callWith(t, func(w http.ResponseWriter, _ *http.Request, _ envelope) {
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
		}, nil)
		wantUnknown(t, r)
		if f.calls.Load() != 1 || f.inits.Load() != 1 {
			t.Errorf("calls=%d inits=%d", f.calls.Load(), f.inits.Load())
		}
	})
	t.Run("timeout", func(t *testing.T) {
		f, _, r := callWith(t, func(w http.ResponseWriter, r *http.Request, _ envelope) {
			<-r.Context().Done()
		}, func(o *Options) { o.CallBudget = 200 * time.Millisecond })
		wantUnknown(t, r)
		if f.calls.Load() != 1 || f.inits.Load() != 1 {
			t.Errorf("calls=%d inits=%d", f.calls.Load(), f.inits.Load())
		}
	})
}

func TestTranscodeDownIsNotRun(t *testing.T) {
	g := newGateway(t, "http://"+closedAddr(t)+"/mcp", nil)
	wantNotRun(t, g.call("1", "memory_set", ""))
}

func TestInitializeFailureIsNotRun(t *testing.T) {
	for name, set := range map[string]func(f *fake){
		"initialize 500": func(f *fake) {
			f.onInit = func(w http.ResponseWriter, _ *http.Request) bool { http.Error(w, "x", 500); return true }
		},
		"initialize 401": func(f *fake) {
			f.onInit = func(w http.ResponseWriter, _ *http.Request) bool { http.Error(w, "x", 401); return true }
		},
		"initialize garbage": func(f *fake) {
			f.onInit = func(w http.ResponseWriter, _ *http.Request) bool {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, "not json")
				return true
			}
		},
		"initialized 500": func(f *fake) {
			f.onInitd = func(w http.ResponseWriter, _ *http.Request) bool { http.Error(w, "x", 500); return true }
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			set(f)
			g := newGateway(t, f.url(), nil)
			wantNotRun(t, g.call("1", "memory_set", ""))
			if f.calls.Load() != 0 {
				t.Error("the call must not be sent without a session")
			}
		})
	}
}

func TestFailedInitializedDoesNotPublishTheSession(t *testing.T) {
	f := newFake(t)
	var fail sync.Once
	f.onInitd = func(w http.ResponseWriter, _ *http.Request) bool {
		done := false
		fail.Do(func() { http.Error(w, "x", 500); done = true })
		return done
	}
	g := newGateway(t, f.url(), nil)
	wantNotRun(t, g.call("1", "memory_set", ""))
	if isErr, _ := toolResult(t, g.call("2", "memory_set", "")); isErr {
		t.Fatal("the next call should handshake again and succeed")
	}
	if f.inits.Load() != 2 {
		t.Errorf("inits %d, want 2: the first session must not have been kept", f.inits.Load())
	}
}

func TestSessionLostIsReinitializedAndRetriedOnce(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), nil)
	g.call("1", "memory_set", "")
	f.expire()
	if isErr, text := toolResult(t, g.call("2", "memory_set", "")); isErr || text != "done" {
		t.Fatalf("want the retried call to succeed, got %v %q", isErr, text)
	}
	if f.inits.Load() != 2 {
		t.Errorf("inits %d, want exactly one reinitialization", f.inits.Load())
	}
	if f.calls.Load() != 2 {
		t.Errorf("handler ran %d times for 2 calls: the 404 must not have run anything", f.calls.Load())
	}
}

func TestSessionLostAgainIsNotRun(t *testing.T) {
	f := newFake(t)
	// Every session is gone as soon as it is established.
	f.onInitd = func(w http.ResponseWriter, _ *http.Request) bool {
		f.expire()
		w.WriteHeader(http.StatusAccepted)
		return true
	}
	g := newGateway(t, f.url(), nil)
	wantNotRun(t, g.call("1", "memory_set", ""))
	if f.calls.Load() != 0 {
		t.Error("nothing may have run")
	}
	if f.inits.Load() != 2 {
		t.Errorf("inits %d, want the first plus a single retry", f.inits.Load())
	}
}

func TestConnectionRefusedAfterASessionIsNotRun(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), nil)
	g.call("1", "memory_set", "")
	f.srv.Close()
	wantNotRun(t, g.call("2", "memory_set", ""))
}

func TestConcurrentCallsReinitializeOnce(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), nil)
	g.call("0", "memory_set", "")
	f.expire()
	f.onInit = func(w http.ResponseWriter, _ *http.Request) bool {
		time.Sleep(100 * time.Millisecond)
		return false
	}
	const n = 6
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if isErr, text := toolResult(t, g.call(fmt.Sprint(i+1), "memory_set", "")); isErr {
				t.Errorf("call %d failed: %s", i, text)
			}
		}(i)
	}
	wg.Wait()
	if f.inits.Load() != 2 {
		t.Errorf("inits %d, want 2 (the first plus one shared reinitialization)", f.inits.Load())
	}
	if f.calls.Load() != n+1 {
		t.Errorf("calls %d, want each call to run exactly once (%d)", f.calls.Load(), n+1)
	}
}

func TestBudgetCoversReinitAndRetry(t *testing.T) {
	budget := 500 * time.Millisecond
	t.Run("reinit eats the budget: not run", func(t *testing.T) {
		f := newFake(t)
		g := newGateway(t, f.url(), func(o *Options) { o.CallBudget = budget })
		g.call("0", "memory_set", "")
		f.expire()
		f.onInit = func(w http.ResponseWriter, r *http.Request) bool {
			select {
			case <-time.After(800 * time.Millisecond):
			case <-r.Context().Done():
			}
			return false
		}
		start := time.Now()
		wantNotRun(t, g.call("1", "memory_set", ""))
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("took %s with a %s budget", d, budget)
		}
		if f.calls.Load() != 1 {
			t.Errorf("calls %d: the call must not have been sent", f.calls.Load())
		}
	})
	t.Run("retry hangs: unknown, still within budget", func(t *testing.T) {
		f := newFake(t)
		g := newGateway(t, f.url(), func(o *Options) { o.CallBudget = budget })
		g.call("0", "memory_set", "")
		f.expire()
		f.onInit = func(w http.ResponseWriter, _ *http.Request) bool {
			time.Sleep(150 * time.Millisecond)
			return false
		}
		f.onCall = func(w http.ResponseWriter, r *http.Request, _ envelope) bool {
			<-r.Context().Done()
			return true
		}
		start := time.Now()
		wantUnknown(t, g.call("1", "memory_set", ""))
		if d := time.Since(start); d > 2*time.Second || d < 300*time.Millisecond {
			t.Errorf("took %s, want about the %s budget in total", d, budget)
		}
		if f.calls.Load() != 2 {
			t.Errorf("calls %d, want the warm-up plus one retry", f.calls.Load())
		}
	})
}

func TestClientCancelAbortsTheUpstreamCall(t *testing.T) {
	f := newFake(t)
	started := make(chan struct{})
	aborted := make(chan struct{})
	f.onCall = func(w http.ResponseWriter, r *http.Request, _ envelope) bool {
		close(started)
		<-r.Context().Done()
		close(aborted)
		return true
	}
	g := newGateway(t, f.url(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, g.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_set"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Session-Id", goodSession)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	select {
	case <-aborted:
	case <-time.After(3 * time.Second):
		t.Fatal("the upstream call was not cancelled with the client")
	}
	time.Sleep(100 * time.Millisecond)
	if f.calls.Load() != 1 {
		t.Errorf("calls %d: a cancelled call must not be repeated", f.calls.Load())
	}
}

func TestCancelDuringReinitDoesNotWaitForIt(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), nil)
	g.call("0", "memory_set", "")
	f.expire()
	release := make(chan struct{})
	f.onInit = func(w http.ResponseWriter, _ *http.Request) bool {
		<-release
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *callError, 1)
	go func() {
		_, cerr := g.f.up.do(ctx, []byte("1"), "tools/call", []byte(`{"name":"memory_set"}`), true)
		done <- cerr
	}()
	for f.inits.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case cerr := <-done:
		if cerr == nil || cerr.kind != failNotDispatched {
			t.Fatalf("want a not-dispatched error, got %v", cerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the cancelled caller waited for the reinitialization")
	}
	// The handshake carries on and its session serves the next call.
	close(release)
	time.Sleep(100 * time.Millisecond)
	if isErr, _ := toolResult(t, g.call("2", "memory_set", "")); isErr {
		t.Fatal("the reinitialized session should be usable")
	}
	if f.inits.Load() != 2 {
		t.Errorf("inits %d, want 2", f.inits.Load())
	}
}
