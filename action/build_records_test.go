package action

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/transcode"
)

// buildClock is a clock the tests move by hand.
type buildClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *buildClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *buildClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeRecords is the registry as the engine sees it (BuildRecords): in memory,
// with no rules of its own. The real one is package nodes, which this package
// never imports.
type fakeRecords struct {
	mu      sync.Mutex
	builds  map[string][]transcode.NodeBuild // by node
	pauses  map[string]string                // node -> since
	failing error                            // makes the pause reads fail
	seq     int
}

func newFakeRecords() *fakeRecords {
	return &fakeRecords{builds: map[string][]transcode.NodeBuild{}, pauses: map[string]string{}}
}

func (f *fakeRecords) NodeBuildRecords(node string) []transcode.NodeBuild {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transcode.NodeBuild(nil), f.builds[node]...)
}

func (f *fakeRecords) BuildsPauses() (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing != nil {
		return nil, f.failing
	}
	out := map[string]string{}
	for k, v := range f.pauses {
		out[k] = v
	}
	return out, nil
}

func (f *fakeRecords) SetBuildsPause(_ context.Context, node string, paused bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !paused {
		delete(f.pauses, node)
	} else if _, ok := f.pauses[node]; !ok {
		f.pauses[node] = "2026-11-13T10:00:00Z"
	}
	return nil
}

func (f *fakeRecords) BuildsPaused(node string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing != nil {
		return false, f.failing
	}
	_, ok := f.pauses[node]
	return ok, nil
}

// add registers a running record on a node.
func (f *fakeRecords) add(node, mode string) transcode.NodeBuild {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	b := transcode.NodeBuild{ID: "bld_fake" + string(rune('a'+f.seq)), Source: "ci-tool", Title: "integration run", PreemptMode: mode, Priority: "normal", Status: "running"}
	f.builds[node] = append(f.builds[node], b)
	return b
}

func records(e *Engine) *fakeRecords { return e.records.(*fakeRecords) }

func buildEngine(t *testing.T, m1 transcode.Executor) (*Engine, *buildClock) {
	t.Helper()
	clock := &buildClock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: twoWorkers(t, m1, &mockTranscodeExecutor{}, 0), DrainSettle: -1, Now: clock.now})
	e.SetBuildRecords(newFakeRecords())
	return e, clock
}

// startBuild puts a running record on the node. The engine only displays it.
func startBuild(t *testing.T, e *Engine, node string, mode string, _ int) transcode.NodeBuild {
	t.Helper()
	return records(e).add(node, mode)
}

func pauseBuilds(t *testing.T, e *Engine, node string, paused bool) transcode.NodePauseStatus {
	t.Helper()
	st, err := e.SetNodeBuildsPause(context.Background(), node, paused)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// tick is the engine's side of a reconcile pass.
func tick(e *Engine) { e.syncWorkerHolds(context.Background()) }

func TestBuildsPauseThroughTheEngineIsIndependentOfTheTranscodePause(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	flags := func() (bool, bool) {
		st := e.NodePauseStatuses(context.Background())["m1"]
		return st.Paused, st.BuildsPaused
	}
	for _, c := range []struct{ transcode, builds bool }{{false, false}, {true, false}, {true, true}, {false, true}, {false, false}} {
		if c.transcode {
			pauseNode(t, e, "m1", "graceful")
		} else {
			pauseNode(t, e, "m1", "")
		}
		pauseBuilds(t, e, "m1", c.builds)
		if tp, bp := flags(); tp != c.transcode || bp != c.builds {
			t.Fatalf("want transcode=%v builds=%v, got %v %v", c.transcode, c.builds, tp, bp)
		}
	}
	// The builds pause writes no transcode pause.
	pauseNode(t, e, "m1", "")
	pauseBuilds(t, e, "m1", true)
	if pauses, _ := e.workerPauses(); len(pauses) != 0 {
		t.Fatalf("a builds pause writes no transcode pause: %+v", pauses)
	}
}

func TestBuildsPauseUnknownNodeAndObservationFlag(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	var unknown *transcode.WorkerNotConfiguredError
	if _, err := e.SetNodeBuildsPause(context.Background(), "nowhere", true); !errors.As(err, &unknown) {
		t.Fatalf("an unknown node: %v", err)
	}
	pauseBuilds(t, e, "m4", true)
	byName := map[string]bool{}
	for _, w := range e.WorkerObservation(context.Background()).Workers {
		byName[w.Name] = w.BuildsPaused
	}
	if !byName["m4"] || byName["m1"] {
		t.Fatalf("worker_health carries the flag per node: %+v", byName)
	}
	if !e.NodeBuildsPaused("m4") || e.NodeBuildsPaused("m1") {
		t.Fatal("NodeBuildsPaused")
	}
}

// With the build-pause state unreadable, status and observation say paused, as
// the create path refuses.
func TestBuildsPauseStatusFailsClosedWhenTheStateCannotBeRead(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	if e.NodeBuildsPaused("m1") {
		t.Fatal("readable and unpaused")
	}
	records(e).failing = errors.New("registry unavailable")
	if !e.NodeBuildsPaused("m1") {
		t.Fatal("an unreadable state counts as paused")
	}
	st := e.NodePauseStatuses(context.Background())
	if !st["m1"].BuildsPaused || !st["m4"].BuildsPaused {
		t.Fatalf("status must fail closed: %+v", st)
	}
}

// Without a registry the engine reads no records and no builds pause.
func TestEngineWithoutARegistryShowsNothing(t *testing.T) {
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: twoWorkers(t, &mockTranscodeExecutor{}, &mockTranscodeExecutor{}, 0), DrainSettle: -1})
	if e.NodeBuildsPaused("m1") || len(e.NodeBuildRecords("m1")) != 0 {
		t.Fatal("no registry, no records, no pause")
	}
	if _, err := e.SetNodeBuildsPause(context.Background(), "m1", true); err == nil {
		t.Fatal("a pause needs the registry")
	}
}
