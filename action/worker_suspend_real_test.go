package action

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/internal/transcodeworker"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// recordedWorker is the real HTTP executor with the order of its hold and
// suspend changes written down.
type recordedWorker struct {
	*httpclient.HTTPExecutor
	log *[]string
}

func (r recordedWorker) SetHold(ctx context.Context, hold bool) (httpclient.HoldStatus, error) {
	*r.log = append(*r.log, map[bool]string{true: "hold on", false: "hold off"}[hold])
	return r.HTTPExecutor.SetHold(ctx, hold)
}

func (r recordedWorker) SetSuspend(ctx context.Context, suspend bool) (httpclient.SuspendStatus, error) {
	*r.log = append(*r.log, map[bool]string{true: "suspend on", false: "suspend off"}[suspend])
	return r.HTTPExecutor.SetSuspend(ctx, suspend)
}

// The coordinator against a real worker over HTTP: a suspended node's queue is
// closed by the suspend alone, so the worker's hold, which also refuses
// publishing a finished encode, is not set; the drain still sets it; and the
// two never leave the queue open between them.
func TestSuspendedNodeKeepsItsQueueClosedWithoutTheWorkerHold(t *testing.T) {
	dir := t.TempDir()
	cfg := transcodeworker.DefaultWorkerConfig()
	cfg.StateDir = filepath.Join(dir, "jobs")
	cfg.LocalWorkDir = filepath.Join(dir, "work")
	cfg.AllowedRoots = []string{dir}
	w := transcodeworker.NewWorker(cfg)
	srv := httptest.NewServer(transcodeworker.NewServer(w, "unused", "unused", "").Handler())
	defer srv.Close()
	tc, err := httpclient.NewHTTPExecutor(httpclient.HTTPConfig{BaseURL: srv.URL, PathMappings: []transcode.PathMapping{{Local: dir, Remote: dir}}})
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	st, err := store.Open(filepath.Join(dir, "action.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}, Workers: twoWorkers(t, recordedWorker{tc, &steps}, &mockTranscodeExecutor{}, 0), DrainSettle: -1})
	ctx := context.Background()
	state := func() string {
		switch {
		case w.Held() && w.Suspended():
			return "hold+suspend"
		case w.Held():
			return "hold"
		case w.Suspended():
			return "suspend"
		}
		return "open"
	}
	step := func(mode, want string) {
		t.Helper()
		if _, err := e.SetNodePause(ctx, "m1", mode); err != nil {
			t.Fatalf("%q: %v", mode, err)
		}
		if got := state(); got != want {
			t.Fatalf("after %q the worker is %s, want %s (steps %v)", mode, got, want, steps)
		}
	}

	// Graceful holds the queue; taking the suspend turns the suspend on first and
	// only then drops the hold, so the queue is never open.
	step(store.WorkerPauseGraceful, "hold")
	steps = nil
	step(store.WorkerPauseSuspend, "suspend")
	if len(steps) != 2 || steps[0] != "suspend on" || steps[1] != "hold off" {
		t.Fatalf("suspend must be confirmed before the hold is released: %v", steps)
	}
	// Leaving the suspend puts the hold back before the freeze is lifted.
	steps = nil
	step(store.WorkerPauseGraceful, "hold")
	if len(steps) != 2 || steps[0] != "hold on" || steps[1] != "suspend off" {
		t.Fatalf("the hold must be in place before the suspend is lifted: %v", steps)
	}
	step(store.WorkerPauseSuspend, "suspend")
	// The drain holds a suspended node too, so publishing stays refused there.
	if _, err := e.SetWorkerDrain(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "hold+suspend" {
		t.Fatalf("drain on a suspended node: %s", got)
	}
	if _, err := e.SetWorkerDrain(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "suspend" {
		t.Fatalf("drain off: %s", got)
	}
	// An immediate pause of a node with nothing running holds and lets go of the
	// freeze (the keep-while-running case is covered with seeded encodes).
	step(store.WorkerPauseImmediate, "hold")
	step("", "open")
}
