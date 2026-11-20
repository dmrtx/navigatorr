package mcpproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A redirect must never be followed: a 307 would replay the tools/call.
func TestRedirectIsNeverFollowedAndIsUnknown(t *testing.T) {
	target := closedAddr(t)
	var effects atomic.Int32
	f, g, r := callWith(t, func(w http.ResponseWriter, req *http.Request, env envelope) {
		effects.Add(1)
		http.Redirect(w, req, "http://"+target+"/mcp", http.StatusTemporaryRedirect)
	}, nil)
	wantUnknown(t, r)
	if effects.Load() != 1 || f.calls.Load() != 1 || f.inits.Load() != 1 {
		t.Errorf("effects=%d calls=%d inits=%d, want no retry", effects.Load(), f.calls.Load(), f.inits.Load())
	}
	if len(g.logs.errors()) != 0 {
		t.Errorf("unexpected ERROR logs %v", g.logs.errors())
	}
}

func TestInjectedClientIsCopiedAndDoesNotFollowRedirects(t *testing.T) {
	hc := &http.Client{Timeout: time.Minute}
	var effects atomic.Int32
	_, _, r := callWith(t, func(w http.ResponseWriter, req *http.Request, env envelope) {
		effects.Add(1)
		http.Redirect(w, req, "http://"+closedAddr(t)+"/mcp", http.StatusTemporaryRedirect)
	}, func(o *Options) { o.HTTPClient = hc })
	wantUnknown(t, r)
	if effects.Load() != 1 {
		t.Errorf("effects %d", effects.Load())
	}
	if hc.CheckRedirect != nil {
		t.Error("the caller's client was modified")
	}
}

func TestOnlyTheKeyMiddlewaresExactAnswerIsNotRun(t *testing.T) {
	cases := map[string]func(http.ResponseWriter){
		"json body": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			fmt.Fprint(w, "unauthorized\n")
		},
		"no newline": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(401)
			fmt.Fprint(w, "unauthorized")
		},
		"other text": func(w http.ResponseWriter) { http.Error(w, "Unauthorized access", 401) },
		"html": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(401)
			fmt.Fprint(w, "unauthorized\n")
		},
		"long": func(w http.ResponseWriter) { http.Error(w, "unauthorized\n"+strings.Repeat("x", 200), 401) },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			f, g, r := callWith(t, func(w http.ResponseWriter, _ *http.Request, _ envelope) { h(w) }, nil)
			wantUnknown(t, r)
			if f.calls.Load() != 1 {
				t.Errorf("calls %d", f.calls.Load())
			}
			if len(g.logs.errors()) != 0 {
				t.Errorf("an unknown 401 is not a key problem: %v", g.logs.errors())
			}
		})
	}
	t.Run("exact", func(t *testing.T) {
		_, g, r := callWith(t, func(w http.ResponseWriter, _ *http.Request, _ envelope) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(401)
			fmt.Fprint(w, "unauthorized\n")
		}, nil)
		wantNotRun(t, r)
		if len(g.logs.errors()) == 0 {
			t.Error("want an ERROR log")
		}
	})
}

func TestSessionGoneBodyMustMatchExactly(t *testing.T) {
	for _, b := range []string{" Invalid session ID \n", "Invalid session ID\n\n", "Invalid session ID", "invalid session id\n", "Invalid session ID\n" + strings.Repeat("x", 100)} {
		t.Run(fmt.Sprintf("%q", b[:min(len(b), 24)]), func(t *testing.T) {
			f, _, r := callWith(t, func(w http.ResponseWriter, _ *http.Request, _ envelope) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(404)
				fmt.Fprint(w, b)
			}, nil)
			wantUnknown(t, r)
			if f.calls.Load() != 1 || f.inits.Load() != 1 {
				t.Errorf("calls=%d inits=%d: no retry allowed", f.calls.Load(), f.inits.Load())
			}
		})
	}
}

func TestSessionTerminatedIsRetriedOnce(t *testing.T) {
	f := newFake(t)
	var first atomic.Bool
	f.onCall = func(w http.ResponseWriter, _ *http.Request, _ envelope) bool {
		if first.CompareAndSwap(false, true) {
			http.Error(w, "Session terminated", http.StatusNotFound)
			return true
		}
		return false
	}
	g := newGateway(t, f.url(), nil)
	if isErr, text := toolResult(t, g.call("1", "memory_set", "")); isErr || text != "done" {
		t.Fatalf("%v %q", isErr, text)
	}
	if f.inits.Load() != 2 || f.calls.Load() != 2 {
		t.Errorf("inits=%d calls=%d, want one retry on a new session", f.inits.Load(), f.calls.Load())
	}
}

// refuser fails the first n tools/call requests with a refused connection.
type refuser struct {
	rt       http.RoundTripper
	n        int32
	attempts atomic.Int32
}

func (r *refuser) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.GetBody != nil {
		rc, _ := req.GetBody()
		b, _ := io.ReadAll(rc)
		if strings.Contains(string(b), `"tools/call"`) && r.attempts.Add(1) <= r.n {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}
	}
	return r.rt.RoundTrip(req)
}

func TestConnectionRefusedThenBackIsRetriedOnce(t *testing.T) {
	f := newFake(t)
	rf := &refuser{rt: http.DefaultTransport, n: 1}
	g := newGateway(t, f.url(), func(o *Options) { o.HTTPClient = &http.Client{Transport: rf} })
	if isErr, text := toolResult(t, g.call("1", "memory_set", "")); isErr || text != "done" {
		t.Fatalf("%v %q", isErr, text)
	}
	if rf.attempts.Load() != 2 || f.calls.Load() != 1 || f.inits.Load() != 2 {
		t.Errorf("attempts=%d calls=%d inits=%d", rf.attempts.Load(), f.calls.Load(), f.inits.Load())
	}
}

func TestTwoRefusedConnectionsAreNotRunWithoutAThirdTry(t *testing.T) {
	f := newFake(t)
	rf := &refuser{rt: http.DefaultTransport, n: 5}
	g := newGateway(t, f.url(), func(o *Options) { o.HTTPClient = &http.Client{Transport: rf} })
	wantNotRun(t, g.call("1", "memory_set", ""))
	if rf.attempts.Load() != 2 || f.calls.Load() != 0 {
		t.Errorf("attempts=%d calls=%d, want exactly two attempts", rf.attempts.Load(), f.calls.Load())
	}
}

// A caller that checked the session just before another flight replaced it
// must reuse the replacement instead of handshaking again.
func TestEnsureDoesNotReinitializeWhenAFlightJustFinished(t *testing.T) {
	f := newFake(t)
	u := newUpstream(f.url(), "", nil, 5*time.Second, func(string, ...any) {}, func(string, ...any) {})
	ctx := context.Background()
	s0, err := u.ensure(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	reached, release := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	armed.Store(true)
	u.afterCheck = func() {
		if armed.CompareAndSwap(true, false) { // only the first caller waits
			close(reached)
			<-release
		}
	}
	type res struct {
		s   *session
		err error
	}
	b := make(chan res, 1)
	go func() { s, err := u.ensure(ctx, s0); b <- res{s, err} }()
	<-reached // B has seen s0 as the failed current session

	sa, err := u.ensure(ctx, s0) // A replaces it
	if err != nil || sa == s0 {
		t.Fatalf("A: %v %v", sa, err)
	}
	close(release)
	rb := <-b
	if rb.err != nil || rb.s != sa {
		t.Fatalf("B got %v %v, want A's session", rb.s, rb.err)
	}
	if f.inits.Load() != 2 {
		t.Errorf("initialize sent %d times, want 2 (the first plus one replacement)", f.inits.Load())
	}
}

func TestSSEDispatchRules(t *testing.T) {
	const resp = `{"jsonrpc":"2.0","id":5,"result":{"content":[{"type":"text","text":"sse-ok"}]}}`
	cases := map[string]string{
		"priming event":     "id: 1\ndata:\n\nevent: message\ndata: " + resp + "\n\n",
		"priming no space":  "id: 1\nretry: 100\ndata\n\ndata: " + resp + "\n\n",
		"ping event":        "event: ping\ndata: ok\n\ndata: " + resp + "\n\n",
		"typed non-message": "event: endpoint\ndata: " + resp + "\n\nevent: message\ndata: " + resp + "\n\n",
		"multi-line data":   "data: {\"jsonrpc\":\"2.0\",\"id\":5,\ndata: \"result\":{\"content\":[{\"type\":\"text\",\"text\":\"sse-ok\"}]}}\n\n",
		"comments":          ": keepalive\n\n: more\ndata: " + resp + "\n: tail\n\n",
		"id and retry":      "id: 7\nretry: 5000\nevent: message\ndata: " + resp + "\n\n",
	}
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, r := callWith(t, func(w http.ResponseWriter, _ *http.Request, _ envelope) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, stream)
			}, nil)
			if isErr, text := toolResult(t, r); isErr || text != "sse-ok" {
				t.Errorf("%v %q (%s)", isErr, text, r.body)
			}
		})
	}
}
