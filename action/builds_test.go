package action

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
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

func buildEngine(t *testing.T, m1 transcode.Executor) (*Engine, *buildClock) {
	t.Helper()
	clock := &buildClock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: twoWorkers(t, m1, &mockTranscodeExecutor{}, 0), DrainSettle: -1, Now: clock.now})
	return e, clock
}

func startBuild(t *testing.T, e *Engine, target string, mode string, ttl int) store.Build {
	t.Helper()
	b, err := e.CreateBuild(context.Background(), BuildInput{Target: target, Source: "ci-tool", Holder: "runner1", Kind: "gradle", Title: "integration run", PreemptMode: mode, TTLSeconds: ttl, Via: BuildViaNodeLease})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func finishBuild(t *testing.T, e *Engine, id, status string) store.Build {
	t.Helper()
	b, _, err := e.FinishBuild(context.Background(), id, status, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tick is what the reconcile loop does for builds on each pass.
func tick(e *Engine) {
	e.reconcileBuilds(context.Background())
	e.syncWorkerHolds(context.Background())
}

func TestBuildLifecycleCreateHeartbeatFinish(t *testing.T) {
	e, clock := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	b := startBuild(t, e, "m1", "", 0)
	if b.ID == "" || b.Status != store.BuildRunning || b.PreemptMode != store.WorkerPauseSuspend || b.HoldsTranscodeNode {
		t.Fatalf("create: %+v", b)
	}
	if want := clock.now().Add(BuildDefaultTTL).Format(time.RFC3339); b.ExpiresAt != want {
		t.Fatalf("default ttl: expires %s want %s", b.ExpiresAt, want)
	}
	if e.workerPaused("m1") || e.workerPaused("m4") {
		t.Fatal("the server's build record holds no node")
	}

	clock.advance(5 * time.Minute)
	progress := "step 2 of 5"
	hb, err := e.HeartbeatBuild(context.Background(), b.ID, &progress, 0)
	if err != nil || hb.Progress != progress || hb.ExpiresAt != clock.now().Add(BuildDefaultTTL).Format(time.RFC3339) {
		t.Fatalf("heartbeat: %+v %v", hb, err)
	}
	// A heartbeat with a ttl of its own, capped at two hours.
	hb, err = e.HeartbeatBuild(context.Background(), b.ID, nil, 99999)
	if err != nil || hb.ExpiresAt != clock.now().Add(BuildMaxTTL).Format(time.RFC3339) || hb.Progress != progress {
		t.Fatalf("ttl cap / progress kept: %+v %v", hb, err)
	}
	// Without a ttl the window the build already has is kept.
	clock.advance(time.Minute)
	hb, _ = e.HeartbeatBuild(context.Background(), b.ID, nil, 0)
	if hb.ExpiresAt != clock.now().Add(BuildMaxTTL).Format(time.RFC3339) {
		t.Fatalf("a heartbeat without ttl keeps the window: %+v", hb)
	}

	exit := 3
	done, changed, err := e.FinishBuild(context.Background(), b.ID, store.BuildFailed, &exit)
	if err != nil || !changed || done.Status != store.BuildFailed || done.EndedAt == "" || done.ExitCode == nil || *done.ExitCode != 3 {
		t.Fatalf("finish: %+v %v %v", done, changed, err)
	}
	again, changed, err := e.FinishBuild(context.Background(), b.ID, store.BuildSucceeded, nil)
	if err != nil || changed || again.Status != store.BuildFailed {
		t.Fatalf("finishing an ended build changes nothing: %+v %v %v", again, changed, err)
	}
	if got, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0); !errors.Is(err, store.ErrBuildEnded) || got.Status != store.BuildFailed {
		t.Fatalf("a heartbeat after the end is refused: %+v %v", got, err)
	}
	if _, _, err := e.FinishBuild(context.Background(), "bld_nope", store.BuildSucceeded, nil); !errors.Is(err, store.ErrBuildNotFound) {
		t.Fatal(err)
	}
	if _, _, err := e.FinishBuild(context.Background(), b.ID, "running", nil); err == nil {
		t.Fatal("finish needs a final status")
	}
}

func TestBuildRequestsAreValidated(t *testing.T) {
	e, _ := buildEngine(t, &mockTranscodeExecutor{})
	cases := map[string]BuildInput{
		"no target":     {Source: "s", Title: "t"},
		"bad target":    {Target: "bad target!", Source: "s", Title: "t"},
		"long target":   {Target: strings.Repeat("a", 65), Source: "s", Title: "t"},
		"long source":   {Target: "m1", Source: strings.Repeat("a", 65), Title: "t"},
		"long holder":   {Target: "m1", Source: "s", Title: "t", Holder: strings.Repeat("a", 65)},
		"long kind":     {Target: "m1", Source: "s", Title: "t", Kind: strings.Repeat("a", 65)},
		"long progress": {Target: "m1", Source: "s", Title: "t", Progress: strings.Repeat("a", 201)},
		"no source":     {Target: "m1", Title: "t"},
		"no title":      {Target: "m1", Source: "s"},
		"bad mode":      {Target: "m1", Source: "s", Title: "t", PreemptMode: "sideways"},
		"negative ttl":  {Target: "m1", Source: "s", Title: "t", TTLSeconds: -1},
		"multiline":     {Target: "m1", Source: "s", Title: "a\nb"},
		"overlong text": {Target: "m1", Source: "s", Title: strings.Repeat("é", 201)},
	}
	for name, in := range cases {
		var invalid *BuildInvalidError
		if _, err := e.CreateBuild(context.Background(), in); !errors.As(err, &invalid) {
			t.Errorf("%s: want an invalid-request error, got %v", name, err)
		}
	}
	if list, _ := e.ListBuilds(store.BuildFilter{}); len(list) != 0 {
		t.Fatalf("a refused request records nothing: %+v", list)
	}
	// Exactly at the limits is accepted (the title and progress count characters).
	ok, err := e.CreateBuild(context.Background(), BuildInput{Target: strings.Repeat("a", 64), Source: strings.Repeat("s", 64), Holder: strings.Repeat("h", 64), Kind: strings.Repeat("k", 64), Title: strings.Repeat("é", 200), Progress: strings.Repeat("é", 200)})
	if err != nil || utf8Len(ok.Title) != 200 {
		t.Fatalf("the boundary values are valid: %+v %v", ok, err)
	}
	b := startBuild(t, e, "m1", "", 99999)
	if b.ExpiresAt != e.now().Add(BuildMaxTTL).Format(time.RFC3339) {
		t.Fatalf("ttl is capped at two hours: %+v", b)
	}
}

func utf8Len(s string) int { return len([]rune(s)) }

func TestBuildTTLCannotOverflowTheCap(t *testing.T) {
	for _, secs := range []int{9223372037, math.MaxInt, math.MaxInt32, 7201} {
		if d, err := buildTTL(secs); err != nil || d != BuildMaxTTL {
			t.Fatalf("%d: %v %v", secs, d, err)
		}
	}
	if d, _ := buildTTL(7200); d != BuildMaxTTL {
		t.Fatal(d)
	}
	e, _ := buildEngine(t, &mockTranscodeExecutor{})
	b := startBuild(t, e, "host-a", "", math.MaxInt)
	if b.ExpiresAt != e.now().Add(BuildMaxTTL).Format(time.RFC3339) {
		t.Fatalf("create: %+v", b)
	}
	hb, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 9223372037)
	if err != nil || hb.ExpiresAt != e.now().Add(BuildMaxTTL).Format(time.RFC3339) {
		t.Fatalf("heartbeat: %+v %v", hb, err)
	}
}

func TestBuildWithoutHeartbeatExpiresToLost(t *testing.T) {
	e, clock := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	b := startBuild(t, e, "m1", store.WorkerPauseSuspend, 60)

	clock.advance(59 * time.Second)
	tick(e)
	if got, _ := e.GetBuild(b.ID); got.Status != store.BuildRunning {
		t.Fatalf("before its expiry the build still runs: %+v", got)
	}
	clock.advance(2 * time.Second)
	tick(e)
	got, _ := e.GetBuild(b.ID)
	if got.Status != store.BuildLost || got.EndedAt == "" {
		t.Fatalf("past its expiry the build is lost: %+v", got)
	}
}

func TestBuildsSurviveAServerRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "builds.db")
	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	clock := &buildClock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	deps := func(st *store.Store) EngineDeps {
		return EngineDeps{Store: st, Config: &config.Config{}, Workers: twoWorkers(t, m1, &mockTranscodeExecutor{}, 0), DrainSettle: -1, Now: clock.now}
	}
	e1 := NewEngine(deps(st1))
	b := startBuild(t, e1, "m1", "", 600)
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st2.Close() })
	m1.suspend, m1.frozen = false, 0 // the worker lost its state too
	e2 := NewEngine(deps(st2))
	got, err := e2.GetBuild(b.ID)
	if err != nil || got.Status != store.BuildRunning || got.ExpiresAt != b.ExpiresAt || got.HoldsTranscodeNode {
		t.Fatalf("the build and its expiry survive: %+v %v", got, err)
	}
	if e2.workerPaused("m1") {
		t.Fatal("the server holds no node for a build record")
	}
	tick(e2)
	if m1.suspend {
		t.Fatal("the server freezes nothing for a build record")
	}
	clock.advance(11 * time.Minute)
	tick(e2)
	if got, _ := e2.GetBuild(b.ID); got.Status != store.BuildLost {
		t.Fatalf("expiry still applies after a restart: %+v", got)
	}
}

func TestBuildOnAnUnknownTargetIsOnlyTracked(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, clock := buildEngine(t, m1)
	seedEncoding(t, e, "enc", "m1", "job-enc")

	b := startBuild(t, e, "laptop.local", store.WorkerPauseImmediate, 120)
	if b.HoldsTranscodeNode || b.Status != store.BuildRunning {
		t.Fatalf("%+v", b)
	}
	e.sweepPausedNodes(context.Background())
	for _, node := range []string{"m1", "m4"} {
		if e.workerPaused(node) {
			t.Fatalf("a build on another machine holds no node: %s", node)
		}
	}
	if m1.suspend || m1.cancelCalls != 0 {
		t.Fatal("no node was touched")
	}
	if pauses, _ := e.workerPauses(); len(pauses) != 0 {
		t.Fatalf("no pause of any kind: %+v", pauses)
	}
	for node, st := range e.NodePauseStatuses(context.Background()) {
		if st.Paused || len(st.Builds) != 0 {
			t.Fatalf("%s: %+v", node, st)
		}
	}
	if _, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0); err != nil {
		t.Fatal(err)
	}
	list, _ := e.ListBuilds(store.BuildFilter{Target: "laptop.local", Status: store.BuildRunning})
	if len(list) != 1 || list[0].HoldsTranscodeNode {
		t.Fatalf("%+v", list)
	}

	clock.advance(3 * time.Minute)
	tick(e)
	if got, _ := e.GetBuild(b.ID); got.Status != store.BuildLost || got.EndedAt == "" || got.HoldsTranscodeNode {
		t.Fatalf("an untracked target still expires to lost: %+v", got)
	}
	if m1.suspend || e.workerPaused("m1") {
		t.Fatal("no effect on any node")
	}
}

func TestBuildHistoryIsOrderedFilteredAndPruned(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, clock := buildEngine(t, m1)
	seedEncoding(t, e, "enc", "m1", "job-enc") // keeps the low build waiting
	old := startBuild(t, e, "host-a", "", 0)
	finishBuild(t, e, old.ID, store.BuildSucceeded)
	clock.advance(time.Hour)
	mid := startBuild(t, e, "host-b", "", 0)
	finishBuild(t, e, mid.ID, store.BuildFailed)
	clock.advance(time.Hour)
	running := startBuild(t, e, "host-a", "", 7200)

	list, err := e.ListBuilds(store.BuildFilter{})
	if err != nil || len(list) != 3 || list[0].ID != running.ID || list[1].ID != mid.ID || list[2].ID != old.ID {
		t.Fatalf("running first, then history newest first: %+v %v", list, err)
	}
	if l, _ := e.ListBuilds(store.BuildFilter{Target: "host-a"}); len(l) != 2 {
		t.Fatalf("target filter: %+v", l)
	}
	if l, _ := e.ListBuilds(store.BuildFilter{Status: store.BuildFailed}); len(l) != 1 || l[0].ID != mid.ID {
		t.Fatalf("status filter: %+v", l)
	}
	if l, _ := e.ListBuilds(store.BuildFilter{Source: "nobody"}); len(l) != 0 {
		t.Fatalf("source filter: %+v", l)
	}
	if l, _ := e.ListBuilds(store.BuildFilter{Limit: 2}); len(l) != 3 {
		t.Fatalf("limit cuts only the history: the running build plus two ended: %+v", l)
	}
	waiting := lowBuild(t, e, "m1", 7200)
	if waiting.Status != store.BuildRunning {
		t.Fatalf("%+v", waiting)
	}

	// Retention is 30 days. Both active builds are heartbeated inside their
	// 7200 s window (every 90 minutes), so they stay active for 32 days; they
	// are never pruned while the ended builds past the retention go.
	for i := 0; i < 32*16; i++ {
		clock.advance(90 * time.Minute)
		for _, id := range []string{running.ID, waiting.ID} {
			if _, err := e.HeartbeatBuild(context.Background(), id, nil, 7200); err != nil {
				t.Fatal(err)
			}
		}
		e.reconcileBuilds(context.Background())
	}
	l, _ := e.ListBuilds(store.BuildFilter{Limit: -1})
	if len(l) != 2 {
		t.Fatalf("only the two active builds are left: %+v", l)
	}
	got := map[string]string{}
	for _, b := range l {
		got[b.ID] = b.Status
	}
	if got[running.ID] != store.BuildRunning || got[waiting.ID] != store.BuildRunning {
		t.Fatalf("running builds are never pruned: %+v", got)
	}
}

func TestBuildRetentionIsConfigurable(t *testing.T) {
	e, clock := buildEngine(t, &mockTranscodeExecutor{})
	e.deps.Config.Builds.RetentionDays = 2
	b := startBuild(t, e, "host-a", "", 0)
	finishBuild(t, e, b.ID, store.BuildSucceeded)
	clock.advance(36 * time.Hour)
	e.reconcileBuilds(context.Background())
	if l, _ := e.ListBuilds(store.BuildFilter{}); len(l) != 1 {
		t.Fatal("still inside the retention")
	}
	clock.advance(36 * time.Hour)
	e.reconcileBuilds(context.Background())
	if l, _ := e.ListBuilds(store.BuildFilter{}); len(l) != 0 {
		t.Fatalf("pruned after two days: %+v", l)
	}
}

func endEncode(t *testing.T, e *Engine, id string) {
	t.Helper()
	inst := mustInstance(t, e.deps.Store, id)
	inst.Status = StatusCompleted
	if err := e.updateInstance(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
}

func lowBuild(t *testing.T, e *Engine, target string, ttl int) store.Build {
	t.Helper()
	b, err := e.CreateBuild(context.Background(), BuildInput{Target: target, Source: "cache-warmer", Title: "warm", Priority: store.BuildPriorityLow, TTLSeconds: ttl, Via: BuildViaNodeLease})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHeartbeatWithoutTTLKeepsAShortWindow(t *testing.T) {
	e, clock := buildEngine(t, &mockTranscodeExecutor{})
	b := startBuild(t, e, "host-a", "", 30)
	clock.advance(10 * time.Second)
	hb, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0)
	if err != nil || hb.ExpiresAt != clock.now().Add(30*time.Second).Format(time.RFC3339) {
		t.Fatalf("a build created with ttl 30 keeps 30 s, not 60: %+v %v", hb, err)
	}
}

func TestLateHeartbeatMarksTheBuildLost(t *testing.T) {
	e, clock := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	b := startBuild(t, e, "m1", "", 60)
	created := b.ExpiresAt
	clock.advance(61 * time.Second) // past expires_at, before any tick
	got, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 600)
	if !errors.Is(err, store.ErrBuildEnded) || got.Status != store.BuildLost || got.EndedAt != created {
		t.Fatalf("a late heartbeat does not revive the build: %+v %v", got, err)
	}
	if again, _ := e.GetBuild(b.ID); again.Status != store.BuildLost || again.ExpiresAt != created {
		t.Fatalf("stored lost, expiry untouched: %+v", again)
	}
}
