package maintenanceui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/internal/nodeswire"
	"github.com/jakenesler/navigatorr/internal/transcodeview"
	"github.com/jakenesler/navigatorr/nodes"
	"github.com/jakenesler/navigatorr/transcode"
)

// viewAdapter is the few lines the nodes binary uses to give the HTTP client
// the shape of nodes.TranscodeView (transcodeview cannot import nodes).
type viewAdapter struct{ c *transcodeview.Client }

func (a viewAdapter) TranscodeStates(ctx context.Context) map[string]nodes.TranscodeState {
	out := map[string]nodes.TranscodeState{}
	states, _ := a.c.States(ctx)
	for name, s := range states {
		out[name] = nodes.TranscodeState{Paused: s.Paused, Mode: s.Mode, Since: s.Since, HeldBy: s.HeldBy, Message: s.Message}
	}
	return out
}

var _ nodes.TranscodeView = viewAdapter{}

// httpView serves the real /transcode/v1 handler over real HTTP and returns a
// fresh client for it (no cache shared between calls).
func httpView(t *testing.T, h http.Handler) viewAdapter {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := transcodeview.New(transcodeview.Options{BaseURL: srv.URL, APIKey: testTranscodeKey})
	if err != nil {
		t.Fatal(err)
	}
	return viewAdapter{c}
}

// The endpoint tells a separate Nodes process what the in-process view tells
// the registry today, field for field, through real HTTP and JSON.
func TestNodeStatesEqualTheInProcessView(t *testing.T) {
	node := transcode.DefaultWorkerName
	lease := transcode.WorkerLease{ID: "lease1", Status: "running", Mode: "suspend", StartedAt: "2026-11-13T10:00:00Z"}
	cases := []struct {
		name   string
		leases []transcode.WorkerLease
		pause  string
		check  func(t *testing.T, st nodes.TranscodeState)
	}{
		{"idle", nil, "", func(t *testing.T, st nodes.TranscodeState) {
			if st.Paused || st.HeldBy != "" || st.Message == "" {
				t.Fatalf("%+v", st)
			}
		}},
		{"running lease", []transcode.WorkerLease{lease}, "", func(t *testing.T, st nodes.TranscodeState) {
			if !st.Paused || st.HeldBy != "builds" || st.Mode != "suspend" || st.Since != lease.StartedAt || st.Message == "" {
				t.Fatalf("%+v", st)
			}
		}},
		{"operator pause", nil, "graceful", func(t *testing.T, st nodes.TranscodeState) {
			if !st.Paused || st.HeldBy != "operator" || st.Mode != "graceful" || st.Since == "" {
				t.Fatalf("%+v", st)
			}
		}},
		{"operator pause and a running lease", []transcode.WorkerLease{lease}, "graceful", func(t *testing.T, st nodes.TranscodeState) {
			if st.HeldBy != "operator" || st.Mode != "graceful" || st.Since == lease.StartedAt {
				t.Fatalf("the operator pause wins over the lease: %+v", st)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, h := transcodeUI(t)
			if len(c.leases) > 0 {
				reportLeases(t, s, c.leases...)
			}
			if c.pause != "" {
				if _, err := s.engine.SetNodePause(t.Context(), node, c.pause); err != nil {
					t.Fatal(err)
				}
			}
			want := nodeswire.TranscodeView(s.engine).TranscodeStates(t.Context())
			got := httpView(t, h).TranscodeStates(t.Context())
			if len(want) == 0 || !reflect.DeepEqual(want, got) {
				t.Fatalf("over HTTP:\n got %+v\nwant %+v", got, want)
			}
			c.check(t, got[node])
		})
	}
}

// countingRecords wraps the registry the engine is wired to and counts every
// call into it.
type countingRecords struct {
	action.BuildRecords
	calls atomic.Int64
}

func (c *countingRecords) NodeBuildRecords(n string) []transcode.NodeBuild {
	c.calls.Add(1)
	return c.BuildRecords.NodeBuildRecords(n)
}
func (c *countingRecords) BuildsPauses() (map[string]string, error) {
	c.calls.Add(1)
	return c.BuildRecords.BuildsPauses()
}
func (c *countingRecords) SetBuildsPause(ctx context.Context, n string, p bool) error {
	c.calls.Add(1)
	return c.BuildRecords.SetBuildsPause(ctx, n, p)
}
func (c *countingRecords) BuildsPaused(n string) (bool, error) {
	c.calls.Add(1)
	return c.BuildRecords.BuildsPaused(n)
}

// Reading the node states never calls the registry: no build record and no
// builds pause, so a Nodes process asking the engine cannot make the engine
// ask it back.
func TestNodeStatesNeverCallTheBuildRecords(t *testing.T) {
	s, h := transcodeUI(t)
	rec := &countingRecords{BuildRecords: nodeswire.Records(s.nodes)}
	s.engine.SetBuildRecords(rec)
	reportLeases(t, s, transcode.WorkerLease{ID: "lease1", Status: "running", StartedAt: "2026-11-13T10:00:00Z"})
	if _, err := s.engine.SetNodePause(t.Context(), transcode.DefaultWorkerName, "graceful"); err != nil {
		t.Fatal(err)
	}
	before := rec.calls.Load()
	got := httpView(t, h).TranscodeStates(t.Context())
	if len(got) == 0 {
		t.Fatal("no states")
	}
	if n := rec.calls.Load() - before; n != 0 {
		t.Fatalf("the node states endpoint called the build records %d times", n)
	}
	// The legacy view does read them, so the counter is not blind.
	s.engine.AttachNodeBuilds(s.engine.NodePauseStatesStored(t.Context()))
	if rec.calls.Load() == before {
		t.Fatal("the counter does not see calls")
	}
}

// The endpoint takes the same key as the other /transcode/v1 routes.
func TestNodeStatesNeedTheAPIKey(t *testing.T) {
	_, h := transcodeUI(t)
	if code, e := get(t, h, "/transcode/v1/node-states", nil); code != 401 || e["code"] != "unauthorized" {
		t.Fatal(code, e)
	}
	if code, e := get(t, h, "/transcode/v1/node-states", keyHdr()); code != 200 || e["nodes"] == nil {
		t.Fatal(code, e)
	}
}

// blockingWriter holds the response in Write until released.
type blockingWriter struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingWriter) Header() http.Header { return w.header }
func (w *blockingWriter) WriteHeader(int)     {}
func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

// While a client is slow to read the answer, the engine stays usable: the
// handler holds no engine lock across the write.
func TestNodeStatesHoldNoEngineLockWhileResponding(t *testing.T) {
	s, _ := transcodeUI(t)
	node := transcode.DefaultWorkerName
	w := &blockingWriter{header: http.Header{}, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.v1NodeStates(w, httptest.NewRequest("GET", "/transcode/v1/node-states", nil))
	}()
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never wrote")
	}
	ops := make(chan error, 1)
	go func() {
		ctx := context.Background()
		s.engine.NodePauseStatesStored(ctx)
		s.engine.NodeTranscodeStates(ctx)
		s.engine.RunningLeases(node)
		_, err := s.engine.SetNodePause(ctx, node, "graceful")
		ops <- err
	}()
	select {
	case err := <-ops:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an engine operation waited behind a response being written")
	}
	close(w.release)
	<-done
}
