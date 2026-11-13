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
	b, err := e.CreateBuild(context.Background(), BuildInput{Target: target, Source: "ci-tool", Holder: "runner1", Kind: "gradle", Title: "integration run", PreemptMode: mode, TTLSeconds: ttl})
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
	if b.ID == "" || b.Status != store.BuildRunning || b.PreemptMode != store.WorkerPauseSuspend || !b.HoldsTranscodeNode {
		t.Fatalf("create: %+v", b)
	}
	if want := clock.now().Add(BuildDefaultTTL).Format(time.RFC3339); b.ExpiresAt != want {
		t.Fatalf("default ttl: expires %s want %s", b.ExpiresAt, want)
	}
	if !e.workerPaused("m1") || e.workerPaused("m4") {
		t.Fatal("a running build holds its node and only that node")
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
	if e.workerPaused("m1") {
		t.Fatal("the node resumes when its only build ends")
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

func TestBuildWithoutHeartbeatExpiresToLostAndFreesTheNode(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, clock := buildEngine(t, m1)
	seedEncoding(t, e, "enc", "m1", "job-enc")
	b := startBuild(t, e, "m1", store.WorkerPauseSuspend, 60)
	if !m1.suspend {
		t.Fatal("the build froze the encode")
	}

	clock.advance(59 * time.Second)
	tick(e)
	if got, _ := e.GetBuild(b.ID); got.Status != store.BuildRunning || !m1.suspend {
		t.Fatalf("before its expiry the build still runs: %+v", got)
	}
	clock.advance(2 * time.Second)
	tick(e)
	got, _ := e.GetBuild(b.ID)
	if got.Status != store.BuildLost || got.EndedAt == "" {
		t.Fatalf("past its expiry the build is lost: %+v", got)
	}
	if e.workerPaused("m1") || m1.suspend {
		t.Fatal("a lost build drops its hold and the frozen encode continues")
	}
	if m1.cancelCalls != 0 {
		t.Fatal("nothing was cancelled")
	}
}

func TestSuspendBuildFreezesAndResumesARunningEncode(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, _ := buildEngine(t, m1)
	seedEncoding(t, e, "enc", "m1", "job-enc")

	b := startBuild(t, e, "m1", "", 0)
	e.sweepPausedNodes(context.Background())
	if !m1.suspend || m1.cancelCalls != 0 {
		t.Fatalf("suspend=%v cancels=%d", m1.suspend, m1.cancelCalls)
	}
	state := parseExecutionContextMust(t, e, "enc").State
	if getString(state, "job_id") != "job-enc" || assignedWorker(state) != "m1" || getInt(state, requeueCountKey) != 0 {
		t.Fatalf("the frozen encode stays assigned: %+v", state)
	}
	st := e.NodePauseStatuses(context.Background())["m1"]
	if !st.Paused || st.HeldBy != "builds" || st.Mode != store.WorkerPauseSuspend || st.Suspended != 1 || !st.Idle || len(st.Builds) != 1 || st.Builds[0].ID != b.ID {
		t.Fatalf("node status while a build runs: %+v", st)
	}

	finishBuild(t, e, b.ID, store.BuildSucceeded)
	e.sweepPausedNodes(context.Background())
	if m1.suspend || m1.cancelCalls != 0 {
		t.Fatalf("the encode continues after the build: suspend=%v cancels=%d", m1.suspend, m1.cancelCalls)
	}
	state = parseExecutionContextMust(t, e, "enc").State
	if getString(state, "job_id") != "job-enc" || getInt(state, requeueCountKey) != 0 {
		t.Fatalf("the same job continues: %+v", state)
	}
	if st := e.NodePauseStatuses(context.Background())["m1"]; st.Paused || st.HeldBy != "" || len(st.Builds) != 0 {
		t.Fatalf("the node is free again: %+v", st)
	}
}

func TestImmediateBuildSendsEncodesBackToTheQueue(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	e, _ := buildEngine(t, m1)
	seedEncoding(t, e, "enc", "m1", "job-enc")
	startBuild(t, e, "m1", store.WorkerPauseImmediate, 0)
	e.sweepPausedNodes(context.Background())
	if m1.cancelCalls != 1 || assignedWorker(parseExecutionContextMust(t, e, "enc").State) != "" {
		t.Fatalf("an immediate build requeues the encode: cancels=%d", m1.cancelCalls)
	}
}

func TestNodeResumesOnlyAfterEveryBuildEnded(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, _ := buildEngine(t, m1)
	a := startBuild(t, e, "m1", store.WorkerPauseGraceful, 0)
	b := startBuild(t, e, "m1", store.WorkerPauseSuspend, 0)
	if !m1.suspend {
		t.Fatal("the strongest mode among the builds wins: suspend")
	}
	if st := e.NodePauseStatuses(context.Background())["m1"]; st.Mode != store.WorkerPauseSuspend || len(st.Builds) != 2 {
		t.Fatalf("%+v", st)
	}
	finishBuild(t, e, b.ID, store.BuildSucceeded)
	if !e.workerPaused("m1") {
		t.Fatal("one build is still running")
	}
	if m1.suspend {
		t.Fatal("with only the graceful build left the encode is no longer frozen")
	}
	if st := e.NodePauseStatuses(context.Background())["m1"]; st.Mode != store.WorkerPauseGraceful {
		t.Fatalf("%+v", st)
	}
	finishBuild(t, e, a.ID, store.BuildFailed)
	if e.workerPaused("m1") {
		t.Fatal("the node resumes after the last build")
	}
	// A stronger build keeps the freeze when the weaker one ends first.
	c := startBuild(t, e, "m1", store.WorkerPauseGraceful, 0)
	d := startBuild(t, e, "m1", store.WorkerPauseSuspend, 0)
	finishBuild(t, e, c.ID, store.BuildSucceeded)
	if !m1.suspend {
		t.Fatal("the suspend build still holds the freeze")
	}
	finishBuild(t, e, d.ID, store.BuildCancelled)
	if m1.suspend || e.workerPaused("m1") {
		t.Fatal("all ended")
	}
}

func TestBuildsNeverTouchTheOperatorPause(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, _ := buildEngine(t, m1)
	operator := func() map[string]store.WorkerPause {
		t.Helper()
		p, err := e.deps.Store.ListWorkerPauses()
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// A build alone writes no operator row.
	b := startBuild(t, e, "m1", "", 0)
	if len(operator()) != 0 {
		t.Fatalf("a build must not write the operator pause: %+v", operator())
	}
	// The operator pauses (graceful) while the build runs, then ends the build:
	// the operator pause stays and the node stays paused.
	pauseNode(t, e, "m1", store.WorkerPauseGraceful)
	if st := e.NodePauseStatuses(context.Background())["m1"]; st.HeldBy != "operator" || st.Mode != store.WorkerPauseGraceful {
		t.Fatalf("the operator pause wins while it is set: %+v", st)
	}
	finishBuild(t, e, b.ID, store.BuildSucceeded)
	if p := operator(); len(p) != 1 || p["m1"].Mode != store.WorkerPauseGraceful {
		t.Fatalf("builds ending must not clear the operator pause: %+v", p)
	}
	if !e.workerPaused("m1") {
		t.Fatal("the node is still paused by the operator")
	}
	if st := e.NodePauseStatuses(context.Background())["m1"]; st.HeldBy != "operator" {
		t.Fatalf("%+v", st)
	}
	pauseNode(t, e, "m1", "")

	// The operator resumes while a build runs: the build's hold, and the freeze
	// it put on the encode, stay.
	pauseNode(t, e, "m1", store.WorkerPauseSuspend)
	b = startBuild(t, e, "m1", "", 0)
	st := pauseNode(t, e, "m1", "")
	if len(operator()) != 0 {
		t.Fatal("the operator pause is gone")
	}
	if !e.workerPaused("m1") || !m1.suspend || st.HeldBy != "builds" || st.Mode != store.WorkerPauseSuspend {
		t.Fatalf("an operator resume must not unfreeze what a build holds: suspend=%v %+v", m1.suspend, st)
	}
	// An operator suspend on top of a build, then resume, keeps the freeze too.
	pauseNode(t, e, "m1", store.WorkerPauseSuspend)
	pauseNode(t, e, "m1", "")
	if !m1.suspend {
		t.Fatal("still frozen for the build")
	}
	finishBuild(t, e, b.ID, store.BuildSucceeded)
	if m1.suspend || e.workerPaused("m1") {
		t.Fatal("the build ended and nothing else holds the node")
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
	if err != nil || got.Status != store.BuildRunning || got.ExpiresAt != b.ExpiresAt || !got.HoldsTranscodeNode {
		t.Fatalf("the build and its expiry survive: %+v %v", got, err)
	}
	if !e2.workerPaused("m1") {
		t.Fatal("the node is still held after a restart")
	}
	tick(e2)
	if !m1.suspend {
		t.Fatal("the reconcile tick re-applies the freeze")
	}
	if _, err := e2.deps.Store.ListWorkerPauses(); err != nil {
		t.Fatal(err)
	}
	clock.advance(11 * time.Minute)
	tick(e2)
	if got, _ := e2.GetBuild(b.ID); got.Status != store.BuildLost || m1.suspend {
		t.Fatalf("expiry still applies after a restart: %+v suspend=%v", got, m1.suspend)
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
	if waiting.Status != store.BuildWaiting {
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
	if got[running.ID] != store.BuildRunning || got[waiting.ID] != store.BuildWaiting {
		t.Fatalf("running and waiting builds are never pruned: %+v", got)
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
	b, err := e.CreateBuild(context.Background(), BuildInput{Target: target, Source: "cache-warmer", Title: "warm", Priority: store.BuildPriorityLow, TTLSeconds: ttl})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLowPriorityBuildWaitsForAnIdleNodeAndNeverFreezes(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, _ := buildEngine(t, m1)
	seedEncoding(t, e, "enc", "m1", "job-enc")

	b := lowBuild(t, e, "m1", 0)
	if b.Status != store.BuildWaiting || b.StartedAt != "" || b.Priority != store.BuildPriorityLow || !b.HoldsTranscodeNode {
		t.Fatalf("a low priority build waits while an encode runs: %+v", b)
	}
	for i := 0; i < 3; i++ {
		tick(e)
		e.sweepPausedNodes(context.Background())
	}
	if got, _ := e.GetBuild(b.ID); got.Status != store.BuildWaiting {
		t.Fatalf("still waiting: %+v", got)
	}
	for _, on := range m1.calls {
		if on {
			t.Fatalf("a waiting build never freezes an encode: calls=%v", m1.calls)
		}
	}
	if e.workerPaused("m1") || m1.suspend || m1.cancelCalls != 0 {
		t.Fatalf("a waiting build holds nothing: paused=%v suspend=%v", e.workerPaused("m1"), m1.suspend)
	}
	st := e.NodePauseStatuses(context.Background())["m1"]
	if st.Paused || len(st.Builds) != 1 || st.Builds[0].Status != store.BuildWaiting {
		t.Fatalf("the node lists the waiting build and is not paused: %+v", st)
	}
	// The heartbeat shows the status the caller polls.
	if hb, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0); err != nil || hb.Status != store.BuildWaiting {
		t.Fatalf("%+v %v", hb, err)
	}

	endEncode(t, e, "enc")
	tick(e)
	got, _ := e.GetBuild(b.ID)
	if got.Status != store.BuildRunning || got.StartedAt == "" {
		t.Fatalf("the idle node starts the build: %+v", got)
	}
	if !e.workerPaused("m1") {
		t.Fatal("a running low priority build takes no new encodes")
	}
	if hb, _ := e.HeartbeatBuild(context.Background(), b.ID, nil, 0); hb.Status != store.BuildRunning {
		t.Fatalf("the heartbeat reports running: %+v", hb)
	}
	finishBuild(t, e, b.ID, store.BuildSucceeded)
	if e.workerPaused("m1") {
		t.Fatal("the node is free after the build")
	}
}

func TestLowPriorityBuildStartsAtOnceOnAnIdleNode(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	if b := lowBuild(t, e, "m1", 0); b.Status != store.BuildRunning || b.StartedAt != b.CreatedAt {
		t.Fatalf("%+v", b)
	}
}

func TestLowPriorityBuildIsNeverPromotedOverANormalBuild(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	e, clock := buildEngine(t, m1)
	normal := startBuild(t, e, "m1", "", 0)
	low := lowBuild(t, e, "m1", 60)
	if low.Status != store.BuildWaiting {
		t.Fatalf("%+v", low)
	}
	tick(e)
	if got, _ := e.GetBuild(low.ID); got.Status != store.BuildWaiting {
		t.Fatalf("a normal build keeps the node: %+v", got)
	}
	finishBuild(t, e, normal.ID, store.BuildSucceeded)
	tick(e)
	if got, _ := e.GetBuild(low.ID); got.Status != store.BuildRunning {
		t.Fatalf("it starts once the normal build ended and the node is idle: %+v", got)
	}
	// A second low build waits behind the running one, and expires like any other.
	second := lowBuild(t, e, "m1", 30)
	tick(e)
	if got, _ := e.GetBuild(second.ID); got.Status != store.BuildWaiting {
		t.Fatalf("%+v", got)
	}
	clock.advance(31 * time.Second)
	tick(e)
	if got, _ := e.GetBuild(second.ID); got.Status != store.BuildLost || got.EndedAt == "" {
		t.Fatalf("a waiting build that stops heartbeating is lost: %+v", got)
	}
}

func TestLowPriorityBuildWaitsWhileTheOperatorPausesTheNode(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	pauseNode(t, e, "m1", store.WorkerPauseGraceful)
	b := lowBuild(t, e, "m1", 0)
	tick(e)
	if got, _ := e.GetBuild(b.ID); got.Status != store.BuildWaiting {
		t.Fatalf("an operator pause is not an idle node: %+v", got)
	}
	pauseNode(t, e, "m1", "")
	tick(e)
	if got, _ := e.GetBuild(b.ID); got.Status != store.BuildRunning {
		t.Fatalf("%+v", got)
	}
}

func TestLowPriorityBuildOnAnotherTargetRunsAtOnce(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, _ := buildEngine(t, m1)
	seedEncoding(t, e, "enc", "m1", "job-enc") // a busy node does not matter
	b := lowBuild(t, e, "laptop.local", 0)
	if b.Status != store.BuildRunning || b.StartedAt == "" || b.StartedAt != b.CreatedAt || b.HoldsTranscodeNode {
		t.Fatalf("%+v", b)
	}
	if e.workerPaused("m1") || m1.suspend {
		t.Fatal("no effect on any node")
	}
	if _, err := e.CreateBuild(context.Background(), BuildInput{Target: "m1", Source: "s", Title: "t", Priority: "urgent"}); err == nil {
		t.Fatal("an unknown priority is refused")
	}
}

func TestWaitingBuildSurvivesARestartWithItsExpiry(t *testing.T) {
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
	seedEncoding(t, e1, "enc", "m1", "job-enc")
	b := lowBuild(t, e1, "m1", 600)
	st1.Close()

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st2.Close() })
	e2 := NewEngine(deps(st2))
	got, err := e2.GetBuild(b.ID)
	if err != nil || got.Status != store.BuildWaiting || got.ExpiresAt != b.ExpiresAt {
		t.Fatalf("%+v %v", got, err)
	}
	tick(e2)
	if got, _ := e2.GetBuild(b.ID); got.Status != store.BuildWaiting {
		t.Fatal("the encode still runs, so it still waits")
	}
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

func TestExpiredWaitingBuildIsNotPromotedWhenTheHolderFinishes(t *testing.T) {
	e, clock := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	normal := startBuild(t, e, "m1", "", 0)
	low := lowBuild(t, e, "m1", 60)
	if low.Status != store.BuildWaiting {
		t.Fatalf("%+v", low)
	}
	clock.advance(2 * time.Minute) // the low build's ttl passes before any tick
	finishBuild(t, e, normal.ID, store.BuildSucceeded)
	if got, _ := e.GetBuild(low.ID); got.Status != store.BuildWaiting {
		t.Fatalf("an expired build must not start: %+v", got)
	}
	if e.workerPaused("m1") {
		t.Fatal("nothing holds the node")
	}
	tick(e)
	if got, _ := e.GetBuild(low.ID); got.Status != store.BuildLost {
		t.Fatalf("the next tick marks it lost: %+v", got)
	}
	if e.workerPaused("m1") {
		t.Fatal("the node stays free")
	}
}

func TestLowBuildsStartInCreationOrder(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, _ := buildEngine(t, m1)
	seedEncoding(t, e, "enc", "m1", "job-enc")
	var ids []string
	for i := 0; i < 8; i++ { // same clock second for all
		ids = append(ids, lowBuild(t, e, "m1", 0).ID)
	}
	endEncode(t, e, "enc")
	m1.active = 0
	for i := range ids {
		tick(e)
		for j, id := range ids {
			got, _ := e.GetBuild(id)
			want := store.BuildWaiting
			if j == i {
				want = store.BuildRunning
			}
			if j < i {
				want = store.BuildSucceeded
			}
			if got.Status != want {
				t.Fatalf("round %d: build %d is %s, want %s", i, j, got.Status, want)
			}
		}
		finishBuild(t, e, ids[i], store.BuildSucceeded)
	}
}

func TestBuildHoldSinceIsWhenTheBuildStarted(t *testing.T) {
	e, clock := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	normal := startBuild(t, e, "m1", "", 0)
	low := lowBuild(t, e, "m1", 0)
	clock.advance(10 * time.Minute)
	finishBuild(t, e, normal.ID, store.BuildSucceeded)
	tick(e)
	got, _ := e.GetBuild(low.ID)
	if got.Status != store.BuildRunning || got.StartedAt == got.CreatedAt {
		t.Fatalf("%+v", got)
	}
	holds, err := e.buildHolds()
	if err != nil || holds["m1"].Since != got.StartedAt {
		t.Fatalf("the hold counts from the start, not the creation: %+v %v", holds, err)
	}
}

func TestLowBuildIsAlwaysGracefulAndNeverFreezesALateEncode(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	e, _ := buildEngine(t, m1)
	for _, mode := range []string{"", store.WorkerPauseSuspend, store.WorkerPauseImmediate} {
		b, err := e.CreateBuild(context.Background(), BuildInput{Target: "laptop.local", Source: "s", Title: "t", Priority: store.BuildPriorityLow, PreemptMode: mode})
		if err != nil || b.PreemptMode != store.WorkerPauseGraceful {
			t.Fatalf("mode %q: %+v %v", mode, b, err)
		}
	}
	// The idle node starts it; an encode that slips in afterwards (the
	// dispatcher takes no node lock) is neither frozen nor requeued.
	b, err := e.CreateBuild(context.Background(), BuildInput{Target: "m1", Source: "s", Title: "t", Priority: store.BuildPriorityLow, PreemptMode: store.WorkerPauseImmediate})
	if err != nil || b.Status != store.BuildRunning || b.PreemptMode != store.WorkerPauseGraceful {
		t.Fatalf("%+v %v", b, err)
	}
	m1.active = 1
	seedEncoding(t, e, "enc", "m1", "job-enc")
	tick(e)
	e.sweepPausedNodes(context.Background())
	if m1.suspend || m1.cancelCalls != 0 {
		t.Fatalf("a low build never freezes or requeues: suspend=%v cancels=%d", m1.suspend, m1.cancelCalls)
	}
	if assignedWorker(parseExecutionContextMust(t, e, "enc").State) != "m1" {
		t.Fatal("the encode stays on its node")
	}
	if !e.workerPaused("m1") {
		t.Fatal("the node still takes no new work")
	}
}

func TestNodeStaysHeldWhileAnotherBuildRunsAfterOneIsLost(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, clock := buildEngine(t, m1)
	seedEncoding(t, e, "enc", "m1", "job-enc")
	short := startBuild(t, e, "m1", store.WorkerPauseSuspend, 60)
	long := startBuild(t, e, "m1", store.WorkerPauseSuspend, 600)
	clock.advance(2 * time.Minute)
	tick(e)
	if got, _ := e.GetBuild(short.ID); got.Status != store.BuildLost {
		t.Fatalf("%+v", got)
	}
	if got, _ := e.GetBuild(long.ID); got.Status != store.BuildRunning {
		t.Fatalf("%+v", got)
	}
	if !e.workerPaused("m1") || !m1.suspend {
		t.Fatalf("the other build still holds and freezes: suspend=%v", m1.suspend)
	}
	finishBuild(t, e, long.ID, store.BuildSucceeded)
	if e.workerPaused("m1") || m1.suspend {
		t.Fatal("freed only after the second ended")
	}
}

func TestBuildHoldReturnsWhenTheOperatorResumes(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, _ := buildEngine(t, m1)
	seedEncoding(t, e, "enc", "m1", "job-enc")
	b := startBuild(t, e, "m1", store.WorkerPauseSuspend, 0)
	if !m1.suspend {
		t.Fatal("frozen by the build")
	}
	// The operator's graceful pause is weaker than the build's freeze, but it is
	// the effective state while it is set: the encode is thawed to finish.
	st := pauseNode(t, e, "m1", store.WorkerPauseGraceful)
	if st.HeldBy != "operator" || m1.suspend {
		t.Fatalf("operator graceful wins while set: suspend=%v %+v", m1.suspend, st)
	}
	st = pauseNode(t, e, "m1", "")
	if st.HeldBy != "builds" || st.Mode != store.WorkerPauseSuspend || !m1.suspend {
		t.Fatalf("after the resume the build freezes the encode again: suspend=%v %+v", m1.suspend, st)
	}
	finishBuild(t, e, b.ID, store.BuildSucceeded)
	if m1.suspend || e.workerPaused("m1") {
		t.Fatal("free after the build")
	}
}

func TestOperatorImmediateOverASuspendBuild(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, _ := buildEngine(t, m1)
	seedEncoding(t, e, "enc", "m1", "job-enc")
	b := startBuild(t, e, "m1", store.WorkerPauseSuspend, 0)
	if !m1.suspend {
		t.Fatal("frozen by the build")
	}
	st := pauseNode(t, e, "m1", store.WorkerPauseImmediate)
	e.sweepPausedNodes(context.Background())
	if st.HeldBy != "operator" || st.Mode != store.WorkerPauseImmediate {
		t.Fatalf("%+v", st)
	}
	if m1.cancelCalls != 1 || assignedWorker(parseExecutionContextMust(t, e, "enc").State) != "" {
		t.Fatalf("operator immediate sends the encode back to the queue as ever: cancels=%d", m1.cancelCalls)
	}
	// As for suspend -> immediate: the freeze stays until the node is empty,
	// then the worker is released.
	e.syncWorkerSuspend(context.Background(), mustWorker(t, e, "m1"))
	if m1.suspend {
		t.Fatal("with the node empty the worker is released under the operator's immediate")
	}
	st = pauseNode(t, e, "m1", "")
	if st.HeldBy != "builds" || st.Mode != store.WorkerPauseSuspend || !m1.suspend || !e.workerPaused("m1") {
		t.Fatalf("operator off: the build's suspend hold applies again: suspend=%v %+v", m1.suspend, st)
	}
	finishBuild(t, e, b.ID, store.BuildSucceeded)
	if e.workerPaused("m1") || m1.suspend {
		t.Fatal("free")
	}
}

func TestLateHeartbeatMarksTheBuildLostAndReleasesTheNode(t *testing.T) {
	for _, low := range []bool{false, true} {
		m1 := newSuspendingM1(transcode.StatusRunning)
		e, clock := buildEngine(t, m1)
		var b store.Build
		if low {
			m1.active = 1
			seedEncoding(t, e, "enc", "m1", "job-enc")
			b = lowBuild(t, e, "m1", 60)
			if b.Status != store.BuildWaiting {
				t.Fatalf("%+v", b)
			}
		} else {
			b = startBuild(t, e, "m1", "", 60)
		}
		created := b.ExpiresAt
		clock.advance(61 * time.Second) // past expires_at, before any tick
		got, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 600)
		if !errors.Is(err, store.ErrBuildEnded) || got.Status != store.BuildLost || got.EndedAt != created {
			t.Fatalf("low=%v: a late heartbeat does not revive the build: %+v %v", low, got, err)
		}
		if again, _ := e.GetBuild(b.ID); again.Status != store.BuildLost || again.ExpiresAt != created {
			t.Fatalf("low=%v: stored lost, expiry untouched: %+v", low, again)
		}
		if holds, _ := e.buildHolds(); len(holds) != 0 {
			t.Fatalf("low=%v: the node hold is released: %+v", low, holds)
		}
	}
}

func TestLateHeartbeatLetsAWaitingBuildTakeTheNode(t *testing.T) {
	e, clock := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	runner := lowBuild(t, e, "m1", 60)
	waiter := lowBuild(t, e, "m1", 3600)
	if runner.Status != store.BuildRunning || waiter.Status != store.BuildWaiting {
		t.Fatalf("%+v %+v", runner, waiter)
	}
	clock.advance(61 * time.Second)
	if _, err := e.HeartbeatBuild(context.Background(), runner.ID, nil, 0); !errors.Is(err, store.ErrBuildEnded) {
		t.Fatal(err)
	}
	if got, _ := e.GetBuild(waiter.ID); got.Status != store.BuildRunning {
		t.Fatalf("the waiting build takes the node: %+v", got)
	}
}

func TestNewLowBuildDoesNotJumpAheadOfAnOlderWaitingOne(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, _ := buildEngine(t, m1)
	seedEncoding(t, e, "enc", "m1", "job-enc")
	a := lowBuild(t, e, "m1", 0)
	if a.Status != store.BuildWaiting {
		t.Fatalf("%+v", a)
	}
	endEncode(t, e, "enc") // the node is idle now, the tick has not run
	m1.active = 0
	b := lowBuild(t, e, "m1", 0)
	ga, _ := e.GetBuild(a.ID)
	gb, _ := e.GetBuild(b.ID)
	if ga.Status != store.BuildRunning || gb.Status != store.BuildWaiting || b.Status != store.BuildWaiting {
		t.Fatalf("the older build starts, the new one waits: a=%+v b=%+v", ga, gb)
	}
}

// A conversion recorded on the node between a build's idle check and its
// promotion must never leave both running: the dispatcher defers the job.
func TestPromotionAndDispatchNeverBothTakeTheNode(t *testing.T) {
	m1 := newSuspendingM1(transcode.StatusRunning)
	m1.active = 1
	e, _ := buildEngine(t, m1)
	seedEncoding(t, e, "enc", "m1", "job-enc")
	low := lowBuild(t, e, "m1", 0)
	endEncode(t, e, "enc")
	m1.active = 0

	// A conversion assigned to the node that has not been sent yet.
	tmpl, _ := e.GetTemplate("transcode_media")
	seedActionInstance(t, e.deps.Store, "late", "transcode_media", StatusRunning, actionStepIndex(tmpl, "transcode_submit"), "",
		map[string]any{"path": "/Volumes/media/late.mkv"}, map[string]any{workerStateKey: "m1"})
	late := mustInstance(t, e.deps.Store, "late")
	ec := parseExecutionContext(late, e)
	ec.State["job_id"] = "job-late"
	var res StepResult
	var deferred bool
	var hookRan bool
	e.afterBuildIdleCheck = func(node string) {
		hookRan = true
		// The dispatcher reaches its recording step right now.
		var err error
		res, deferred, err = e.admitOnNode(ec, "transcode", func() error {
			ec.State["transcode_reconcile"] = true
			return e.persistExecutionState(context.Background(), ec)
		})
		if err != nil {
			t.Error(err)
		}
	}
	tick(e)
	if !hookRan {
		t.Fatal("the seam was not reached")
	}
	if !deferred || res.Status != StepWaitingExternal || res.WaitingCondition != pauseWaitCondition {
		t.Fatalf("the dispatch is deferred while the promotion holds the node: %+v %v", res, deferred)
	}
	if got, _ := e.GetBuild(low.ID); got.Status != store.BuildRunning {
		t.Fatalf("%+v", got)
	}
	inst := mustInstance(t, e.deps.Store, "late")
	if getBool(parseExecutionContext(inst, e).State, "transcode_reconcile") {
		t.Fatal("no job was recorded on the node")
	}
	// And the other order: the job is recorded first, the build then waits.
	e.afterBuildIdleCheck = nil
	finishBuild(t, e, low.ID, store.BuildSucceeded)
	next := lowBuild(t, e, "m1", 0)
	if next.Status != store.BuildRunning {
		t.Fatalf("idle node: %+v", next)
	}
	finishBuild(t, e, next.ID, store.BuildSucceeded)
	if _, d, err := e.admitOnNode(ec, "transcode", func() error {
		ec.State["transcode_reconcile"] = true
		return e.persistExecutionState(context.Background(), ec)
	}); err != nil || d {
		t.Fatalf("an idle node admits the job: %v %v", d, err)
	}
	third := lowBuild(t, e, "m1", 0)
	if third.Status != store.BuildWaiting {
		t.Fatalf("a recorded job keeps the build waiting: %+v", third)
	}
}
