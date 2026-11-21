package action

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
	mu         sync.Mutex
	builds     map[string][]transcode.NodeBuild // by node
	pauses     map[string]string                // node -> since
	failing    error                            // makes the pause reads fail
	failOnList error                            // set as failing by the first list read (the registry dies mid-observation)
	listFail   error                            // fails the next list read only (the registry is back for the pause read)
	seq        int
}

func newFakeRecords() *fakeRecords {
	return &fakeRecords{builds: map[string][]transcode.NodeBuild{}, pauses: map[string]string{}}
}

func (f *fakeRecords) NodeBuildRecords(node string) []transcode.NodeBuild {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnList != nil {
		f.failing, f.failOnList = f.failOnList, nil
		return nil
	}
	return append([]transcode.NodeBuild(nil), f.builds[node]...)
}

// ListNodeBuildRecords makes the fake a BuildRecordsLister, like the remote
// registry.
func (f *fakeRecords) ListNodeBuildRecords(node string) ([]transcode.NodeBuild, error) {
	f.mu.Lock()
	if err := f.listFail; err != nil {
		f.listFail = nil
		f.mu.Unlock()
		return nil, err
	}
	f.mu.Unlock()
	return f.NodeBuildRecords(node), nil
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

func records(e *Engine) *fakeRecords { return e.buildRecords().(*fakeRecords) }

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

// The registry may be wired while engine goroutines already read it: the field
// is atomic, so both orders are safe (checked by go test -race where cgo is on).
func TestSetBuildRecordsWhileReading(t *testing.T) {
	e, _ := buildEngine(t, &mockTranscodeExecutor{})
	e.SetBuildRecords(nil)
	const readers = 4
	var wg sync.WaitGroup
	var started sync.WaitGroup // every reader has read at least once
	var reads atomic.Int64
	stop := make(chan struct{})
	started.Add(readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			first := true
			for {
				_ = e.NodeBuildRecords("m1")
				_ = e.NodeBuildsPaused("m1")
				reads.Add(1)
				if first {
					first = false
					started.Done()
				}
				select {
				case <-stop:
					return
				default:
				}
			}
		}()
	}
	started.Wait()
	// The writer keeps going until the readers have read again during it, so
	// reads and writes certainly overlap.
	seen := reads.Load()
	for i := 0; i < 100 || reads.Load() < seen+readers*10; i++ {
		e.SetBuildRecords(newFakeRecords())
		e.SetBuildRecords(nil)
		if i > 1_000_000 {
			t.Fatal("the readers made no progress")
		}
	}
	close(stop)
	wg.Wait()
	e.SetBuildRecords(newFakeRecords())
	if e.buildRecords() == nil {
		t.Fatal("wired")
	}
}

// A registry that cannot be read at all (it runs in another process) is
// neither paused nor not paused: the observation, the pause statuses and the
// per-node state say "unavailable", and a read error of any other kind still
// reads as paused.
func TestUnavailableRegistryIsNeitherPausedNorNot(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	records(e).failing = fmt.Errorf("%w: %w", ErrBuildRecordsUnavailable, errors.New("connection refused"))

	if paused, unavailable := e.NodeBuildsState("m1"); paused || !unavailable {
		t.Fatalf("NodeBuildsState: paused=%v unavailable=%v", paused, unavailable)
	}
	if e.NodeBuildsPaused("m1") {
		t.Fatal("an unavailable registry must not read as paused")
	}
	st := e.NodePauseStatuses(context.Background())["m1"]
	if st.BuildsPaused || !st.BuildsUnavailable {
		t.Fatalf("pause status: %+v", st)
	}
	obs := e.WorkerObservation(context.Background())
	if !obs.BuildsUnavailable || obs.BuildsPaused || len(obs.Workers) == 0 {
		t.Fatalf("observation: %+v", obs)
	}
	for _, w := range obs.Workers {
		if w.BuildsPaused || !w.BuildsUnavailable {
			t.Fatalf("worker %s: %+v", w.Name, w)
		}
	}

	// Any other error keeps failing closed.
	records(e).failing = errors.New("disk I/O error")
	if paused, unavailable := e.NodeBuildsState("m1"); !paused || unavailable {
		t.Fatalf("other error: paused=%v unavailable=%v", paused, unavailable)
	}
	st = e.NodePauseStatuses(context.Background())["m1"]
	if !st.BuildsPaused || st.BuildsUnavailable {
		t.Fatalf("other error, status: %+v", st)
	}

	// Healthy again: neither flag.
	records(e).failing = nil
	st = e.NodePauseStatuses(context.Background())["m1"]
	if st.BuildsPaused || st.BuildsUnavailable {
		t.Fatalf("healthy: %+v", st)
	}
}

// A registry that stops answering while the observation reads the build lists
// shows as unavailable, never as nodes with no builds.
func TestRegistryLostMidObservationReadsAsUnavailable(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	records(e).failOnList = fmt.Errorf("%w: %w", ErrBuildRecordsUnavailable, errors.New("connection refused"))
	obs := e.WorkerObservation(context.Background())
	if !obs.BuildsUnavailable || len(obs.Workers) == 0 {
		t.Fatalf("observation: %+v", obs)
	}
	for _, w := range obs.Workers {
		if !w.BuildsUnavailable {
			t.Fatalf("worker %s: %+v", w.Name, w)
		}
	}
}

// A list read that failed shows as unavailable even when the registry answers
// the pause read that follows (a write in between cleared the client's
// backoff): the failed list is never "no builds" beside a known pause.
func TestFailedListReadsAsUnavailableWhenThePausesAnswer(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	records(e).add("m1", "graceful")
	records(e).pauses["m1"] = "2026-11-13T10:00:00Z"
	unavail := fmt.Errorf("%w: %w", ErrBuildRecordsUnavailable, errors.New("connection refused"))

	records(e).listFail = unavail
	obs := e.WorkerObservation(context.Background())
	if !obs.BuildsUnavailable {
		t.Fatalf("observation: %+v", obs)
	}
	for _, w := range obs.Workers {
		if !w.BuildsUnavailable || w.BuildsPaused {
			t.Fatalf("worker %s: %+v", w.Name, w)
		}
	}

	records(e).listFail = unavail
	st := e.AttachNodeBuilds(e.NodePauseStatuses(context.Background()))["m1"]
	if !st.BuildsUnavailable || st.BuildsPaused || len(st.Builds) != 0 {
		t.Fatalf("attached: %+v", st)
	}

	obs = e.WorkerObservation(context.Background())
	for _, w := range obs.Workers {
		if w.Name == "m1" && (w.BuildsUnavailable || !w.BuildsPaused || len(w.Builds) != 1) {
			t.Fatalf("recovered m1: %+v", w)
		}
	}
}

// When one node's list read fails, every node's builds side reads as unknown:
// no list, no pause and no pause time, whatever the pause read said.
func TestAFailedListMakesEveryNodeUnknown(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	records(e).add("m2", "graceful")
	records(e).pauses["m1"] = "2026-11-13T10:00:00Z"
	records(e).pauses["m2"] = "2026-11-13T10:00:00Z"
	statuses := e.NodePauseStatuses(context.Background())
	if !statuses["m1"].BuildsPaused || statuses["m1"].BuildsPausedSince == "" {
		t.Fatalf("setup: %+v", statuses["m1"])
	}
	records(e).listFail = fmt.Errorf("%w: %w", ErrBuildRecordsUnavailable, errors.New("connection refused"))
	for name, st := range e.AttachNodeBuilds(statuses) {
		if !st.BuildsUnavailable || st.BuildsPaused || st.BuildsPausedSince != "" || len(st.Builds) != 0 {
			t.Fatalf("%s: %+v", name, st)
		}
	}
}
