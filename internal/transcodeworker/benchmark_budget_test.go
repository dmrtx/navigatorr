package transcodeworker

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode/optimization"

	"github.com/jakenesler/navigatorr/transcode"
)

func testSearchExtension() *transcode.SearchExtension {
	return &transcode.SearchExtension{MaxSeconds: 1800, SteadySeconds: 180}
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func (b *searchBudget) lastUnitTime() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastUnit
}

func TestSearchBudgetWithoutExtensionIsTheFixedBudget(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	b := newSearchBudget(start, 600, nil)
	if b.elastic() || !b.hardDeadline().Equal(start.Add(600*time.Second)) {
		t.Fatalf("fixed budget changed: elastic=%v deadline=%v", b.elastic(), b.hardDeadline())
	}
	if !b.continues(start.Add(599*time.Second)) || b.continues(start.Add(600*time.Second)) {
		t.Fatal("fixed budget must end exactly at the base")
	}
	// Finishing work buys nothing without a frozen extension.
	b.now = fixedClock(start.Add(590 * time.Second))
	b.noteUnit()
	if b.continues(start.Add(601 * time.Second)) {
		t.Fatal("progress extended a fixed budget")
	}
}

func TestSearchBudgetIgnoresAnInvalidExtension(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	for name, ext := range map[string]*transcode.SearchExtension{
		"cap not above base": {MaxSeconds: 600, SteadySeconds: 180},
		"cap below base":     {MaxSeconds: 300, SteadySeconds: 180},
		"steady too short":   {MaxSeconds: 1800, SteadySeconds: 5},
		"steady too long":    {MaxSeconds: 1800, SteadySeconds: 601},
		"cap over ceiling":   {MaxSeconds: transcode.MaxSearchBudgetSeconds + 1, SteadySeconds: 180},
	} {
		b := newSearchBudget(start, 600, ext)
		if b.elastic() || !b.hardDeadline().Equal(start.Add(600*time.Second)) {
			t.Errorf("%s: invalid extension granted time: elastic=%v deadline=%v", name, b.elastic(), b.hardDeadline())
		}
	}
}

func TestSearchBudgetExtendsOnlyWhileUnitsFinish(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	at := func(s int) time.Time { return start.Add(time.Duration(s) * time.Second) }
	b := newSearchBudget(start, 600, testSearchExtension())
	if !b.elastic() || !b.hardDeadline().Equal(at(1800)) {
		t.Fatalf("elastic budget not built: %v", b.hardDeadline())
	}
	if !b.continues(at(0)) || !b.continues(at(599)) {
		t.Fatal("the base budget is always granted")
	}
	if b.continues(at(600)) {
		t.Fatal("no unit ever finished, so the base is the end")
	}

	b.now = fixedClock(at(590))
	b.noteUnit()
	for _, c := range []struct {
		s    int
		want bool
	}{{700, true}, {770, true}, {771, false}} {
		if got := b.continues(at(c.s)); got != c.want {
			t.Errorf("t=%d: continues=%v want %v", c.s, got, c.want)
		}
	}

	b.now = fixedClock(at(770))
	b.noteUnit()
	if !b.continues(at(900)) {
		t.Fatal("a newer unit must restart the steady window")
	}

	b.now = fixedClock(at(1799))
	b.noteUnit()
	if !b.continues(at(1799)) {
		t.Fatal("search should run until the cap")
	}
	if b.continues(at(1800)) || b.continues(at(2400)) {
		t.Fatal("hard cap exceeded despite steady progress")
	}
}

func TestSearchBudgetVerificationAllowanceLiftsTheCapOnlyOnceVerifying(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	at := func(s int) time.Time { return start.Add(time.Duration(s) * time.Second) }
	b := newSearchBudget(start, 600, &transcode.SearchExtension{MaxSeconds: 1800, SteadySeconds: 180, VerifySeconds: 600})
	if !b.hardDeadline().Equal(at(2400)) {
		t.Fatalf("the hard deadline must include the verification allowance: %v", b.hardDeadline())
	}
	b.now = fixedClock(at(1790))
	b.noteUnit()
	if !b.continues(at(1799)) || b.continues(at(1800)) {
		t.Fatal("a search that never reached verification ends at the cap")
	}
	b.noteVerifying()
	if !b.continues(at(1800)) || !b.continues(at(1900)) {
		t.Fatal("a search verifying its winner must not be cut at the cap")
	}
	// Steady progress still counts: verification that stalls is stopped.
	if b.continues(at(1971)) {
		t.Fatal("a stalled verification ran on")
	}
	b.now = fixedClock(at(2399))
	b.noteUnit()
	if !b.continues(at(2399)) || b.continues(at(2400)) || b.continues(at(3000)) {
		t.Fatal("the verification allowance is bounded")
	}

	// Without an allowance (or without an extension) verification changes nothing.
	for name, ext := range map[string]*transcode.SearchExtension{"no allowance": {MaxSeconds: 1800, SteadySeconds: 180}, "no extension": nil} {
		fixed := newSearchBudget(start, 600, ext)
		fixed.now = fixedClock(at(590))
		fixed.noteUnit()
		fixed.noteVerifying()
		limit := 1800
		if ext == nil {
			limit = 600
		}
		if !fixed.hardDeadline().Equal(at(limit)) || fixed.continues(at(limit)) {
			t.Errorf("%s: verifying moved the limit: deadline=%v", name, fixed.hardDeadline())
		}
	}
}

func TestSearchBudgetNilIsSafe(t *testing.T) {
	var b *searchBudget
	b.noteUnit()
	b.noteVerifying()
	if b.wasStopped() || b.workerBusy() {
		t.Fatal("nil budget reported state")
	}
}

func TestSearchBudgetWorkerBusyNeedsHalfTheObservedTime(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	b := newSearchBudget(t0, 600, nil)
	if b.workerBusy() {
		t.Fatal("busy without any observation")
	}
	b.sample(t0, true) // the first sample only starts the clock
	if b.workerBusy() {
		t.Fatal("busy before any interval was observed")
	}
	b.sample(t0.Add(10*time.Second), true)
	if !b.workerBusy() {
		t.Fatal("shared interval not counted")
	}
	b.sample(t0.Add(30*time.Second), false)
	if b.workerBusy() {
		t.Fatalf("10 of 30 observed seconds shared is not busy: %+v", b)
	}
	b.sample(t0.Add(40*time.Second), true)
	if !b.workerBusy() {
		t.Fatal("exactly half shared counts as busy")
	}
	b.sample(t0.Add(40*time.Second), true) // an out-of-order sample adds nothing
	if b.observed != 40*time.Second {
		t.Fatalf("observed %v", b.observed)
	}
}

func TestSearchBudgetWatchCancelsAStalledSearchAtItsBase(t *testing.T) {
	// 200 ms of the 10 s base are left, so the watcher must wake for that
	// instant, not a 10 s poll later.
	b := newSearchBudget(time.Now().Add(-9800*time.Millisecond), 10, &transcode.SearchExtension{MaxSeconds: 60, SteadySeconds: 30})
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	stop := b.watch(ctx, cancel, func() bool { return true })
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("stalled search was not stopped at its base budget")
	}
	if !errors.Is(context.Cause(ctx), errSearchBudgetStopped) || !b.wasStopped() {
		t.Fatalf("cause=%v stopped=%v", context.Cause(ctx), b.wasStopped())
	}
	if !b.workerBusy() {
		t.Fatal("the shared interval before the stop was not recorded")
	}
}

func TestSearchBudgetWatchLetsASteadySearchRunPastItsBase(t *testing.T) {
	b := newSearchBudget(time.Now().Add(-11*time.Second), 10, &transcode.SearchExtension{MaxSeconds: 60, SteadySeconds: 30})
	b.noteUnit()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	stop := b.watch(ctx, cancel, func() bool { return false })
	time.Sleep(300 * time.Millisecond)
	if ctx.Err() != nil || b.wasStopped() {
		t.Fatalf("steady search stopped: %v", context.Cause(ctx))
	}
	started := time.Now()
	stop()
	stop() // idempotent
	if time.Since(started) > time.Second {
		t.Fatal("stop waited for the next poll")
	}
	if b.workerBusy() {
		t.Fatal("an unshared worker was reported busy")
	}
}

func TestSearchBudgetWatchNeverCancelsAFixedBudget(t *testing.T) {
	// The context deadline ends a fixed budget; the watcher only measures.
	b := newSearchBudget(time.Now().Add(-11*time.Second), 10, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	stop := b.watch(ctx, cancel, func() bool { return true })
	time.Sleep(300 * time.Millisecond)
	stop()
	if ctx.Err() != nil || b.wasStopped() {
		t.Fatal("a fixed budget was cancelled by the watcher")
	}
}

func TestProgressReporterCountsOnlyRealWorkAsSteadyProgress(t *testing.T) {
	record := &BenchmarkRecord{}
	record.searchBudget = newSearchBudget(time.Now(), 600, testSearchExtension())
	p := newBenchmarkProgressReporter(nil, record, 10)
	p.SkipUnits(4)
	if !record.searchBudget.lastUnitTime().IsZero() {
		t.Fatal("skipped units must not look like progress")
	}
	p.ResolveUnits(2)
	if !record.searchBudget.lastUnitTime().IsZero() {
		t.Fatal("bulk resolution must not look like progress")
	}
	p.CompleteUnit()
	first := record.searchBudget.lastUnitTime()
	if first.IsZero() {
		t.Fatal("a completed unit is progress")
	}
	time.Sleep(2 * time.Millisecond)
	p.ResolveUnit()
	if !record.searchBudget.lastUnitTime().After(first) {
		t.Fatal("a resolved unit is progress")
	}
	// A record without a budget (older request, tests) must not panic.
	newBenchmarkProgressReporter(nil, &BenchmarkRecord{}, 1).CompleteUnit()
	newBenchmarkProgressReporter(nil, nil, 1).CompleteUnit()
}

type budgetProbeRunner struct {
	run func(ctx context.Context, record *BenchmarkRecord) error
}

func (r *budgetProbeRunner) RunBenchmark(ctx context.Context, w *Worker, record *BenchmarkRecord) error {
	return r.run(ctx, record)
}

// runBudgetBenchmark runs one benchmark whose search started so that
// remaining of its base budget is left, and returns the stored result.
func runBudgetBenchmark(t *testing.T, w *Worker, base int, remaining time.Duration, ext *transcode.SearchExtension, run func(ctx context.Context, record *BenchmarkRecord) error) (*BenchmarkRecord, error) {
	t.Helper()
	w.benchmarkRunner = &budgetProbeRunner{run: run}
	started := time.Now().UTC().Add(remaining - time.Duration(base)*time.Second)
	record := &BenchmarkRecord{ProtocolVersion: transcode.WorkerProtocolVersion, ID: "bench-budget", Status: "queued", RunToken: "budget-token", Source: filepath.Join(filepath.Dir(w.cfg.StateDir), "source.mkv"), Mode: "size", SearchStartedAt: started, SearchBudgetSeconds: base, SearchExtension: ext, CreatedAt: started}
	path := filepath.Join(w.cfg.StateDir, record.ID, "benchmark.json")
	if err := SaveBenchmarkAtomic(path, record); err != nil {
		t.Fatal(err)
	}
	runErr := w.InternalBenchmark(context.Background(), record.ID, record.RunToken)
	result, err := LoadBenchmark(path)
	if err != nil {
		t.Fatal(err)
	}
	return result, runErr
}

func newBudgetWorker(t *testing.T) *Worker {
	t.Helper()
	dir := t.TempDir()
	ioOptWriteFile(t, filepath.Join(dir, "source.mkv"), "synthetic source")
	return NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{dir}, DisableSourceCache: true, MaxParallelJobs: 2})
}

func waitDone(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errors.New("search was never stopped")
	}
}

func TestInternalBenchmarkLetsASteadySearchOutliveItsBase(t *testing.T) {
	w := newBudgetWorker(t)
	var deadline, started time.Time
	result, err := runBudgetBenchmark(t, w, 10, 300*time.Millisecond, &transcode.SearchExtension{MaxSeconds: 60, SteadySeconds: 30}, func(ctx context.Context, record *BenchmarkRecord) error {
		deadline, _ = ctx.Deadline()
		started = record.SearchStartedAt
		record.searchBudget.noteUnit()
		// Past the base the search is still alive because a unit finished.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(900 * time.Millisecond):
			return nil
		}
	})
	if err != nil || result.Status != "completed" || result.ReasonCode != "" {
		t.Fatalf("steady search was stopped at its base: err=%v result=%+v", err, result)
	}
	if !deadline.Equal(started.Add(60 * time.Second)) {
		t.Fatalf("the hard cap must count from the persisted search start: deadline=%v started=%v", deadline, started)
	}
}

func TestInternalBenchmarkStopsAStalledSearchAtItsBaseAndSaysSo(t *testing.T) {
	w := newBudgetWorker(t)
	began := time.Now()
	result, err := runBudgetBenchmark(t, w, 10, 300*time.Millisecond, &transcode.SearchExtension{MaxSeconds: 60, SteadySeconds: 30}, func(ctx context.Context, record *BenchmarkRecord) error {
		return waitDone(ctx)
	})
	if err == nil || result.Status != "failed" || result.ReasonCode != "search_budget_exhausted" || !strings.HasPrefix(result.Error, "search_budget_exhausted:") {
		t.Fatalf("stalled search not reported as exhausted: err=%v result=%+v", err, result)
	}
	if strings.Contains(err.Error(), "never stopped") || time.Since(began) > 3*time.Second {
		t.Fatalf("the stall was not stopped at the base budget: %v after %s", err, time.Since(began))
	}
	if result.SearchWorkerBusy {
		t.Fatal("a worker running only this search was reported busy")
	}
}

func TestInternalBenchmarkKeepsARealFailureThatTheWatcherOutlived(t *testing.T) {
	w := newBudgetWorker(t)
	// The run was already failing for its own reason when the watcher stopped
	// the stalled search: the failure must not be relabelled as a budget stop.
	result, err := runBudgetBenchmark(t, w, 10, 300*time.Millisecond, &transcode.SearchExtension{MaxSeconds: 60, SteadySeconds: 30}, func(ctx context.Context, record *BenchmarkRecord) error {
		if err := waitDone(ctx); err != nil && strings.Contains(err.Error(), "never stopped") {
			return err
		}
		return errors.New("source media modified during benchmark (integrity violation, fail closed)")
	})
	if err == nil || result.Status != "failed" || result.ReasonCode == "search_budget_exhausted" || !strings.Contains(result.Error, "integrity violation") {
		t.Fatalf("a real failure was reported as budget exhaustion: err=%v result=%+v", err, result)
	}
	if result.SearchWorkerBusy {
		t.Fatal("busy hint set on a failure that is not an exhaustion")
	}
}

func TestInternalBenchmarkNeverRunsPastTheHardCapDespiteProgress(t *testing.T) {
	w := newBudgetWorker(t)
	var cause error
	result, err := runBudgetBenchmark(t, w, 10, 400*time.Millisecond, &transcode.SearchExtension{MaxSeconds: 11, SteadySeconds: 30}, func(ctx context.Context, record *BenchmarkRecord) error {
		for ctx.Err() == nil {
			record.searchBudget.noteUnit()
			select {
			case <-ctx.Done():
			case <-time.After(20 * time.Millisecond):
			}
		}
		cause = context.Cause(ctx)
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(cause, context.DeadlineExceeded) {
		t.Fatalf("hard cap must be the context deadline: err=%v cause=%v", err, cause)
	}
	if result.Status != "failed" || result.ReasonCode != "search_budget_exhausted" {
		t.Fatalf("hard cap not reported as exhausted: %+v", result)
	}
}

type pastCapRun struct {
	result  *BenchmarkRecord
	err     error
	cause   error
	pastCap time.Duration // how long the runner lived past the cap
}

// runVerifyingBenchmark runs a search that keeps finishing units past its cap
// and, when verifying is set, reports the verifying_samples stage first.
func runVerifyingBenchmark(t *testing.T, verifying bool) pastCapRun {
	t.Helper()
	w := newBudgetWorker(t)
	var cause error
	var pastCap time.Duration
	result, err := runBudgetBenchmark(t, w, 10, 400*time.Millisecond, &transcode.SearchExtension{MaxSeconds: 11, SteadySeconds: 30, VerifySeconds: 2}, func(ctx context.Context, record *BenchmarkRecord) error {
		if verifying {
			newBenchmarkProgressReporter(nil, record, 1).SetPhase(phaseVerifyingSamples)
		}
		for ctx.Err() == nil {
			record.searchBudget.noteUnit()
			select {
			case <-ctx.Done():
			case <-time.After(20 * time.Millisecond):
			}
		}
		cause = context.Cause(ctx)
		pastCap = time.Since(record.SearchStartedAt.Add(11 * time.Second))
		return ctx.Err()
	})
	return pastCapRun{result: result, err: err, cause: cause, pastCap: pastCap}
}

func TestInternalBenchmarkLetsAVerifyingSearchPastTheCapButNotPastTheAllowance(t *testing.T) {
	run := runVerifyingBenchmark(t, true)
	if !errors.Is(run.err, context.DeadlineExceeded) || !errors.Is(run.cause, context.DeadlineExceeded) {
		t.Fatalf("the allowance must end at the context deadline: err=%v cause=%v", run.err, run.cause)
	}
	// The cap is 11 s from the start and the allowance 2 s more.
	if run.pastCap < 1500*time.Millisecond || run.pastCap > 3*time.Second {
		t.Fatalf("a verifying search ended %s past the cap, want about 2 s", run.pastCap)
	}
	if run.result.ReasonCode != "search_budget_exhausted" {
		t.Fatalf("the allowance running out is still an exhaustion: %+v", run.result)
	}
}

func TestInternalBenchmarkStopsASearchWithoutAWinnerAtTheCapDespiteTheAllowance(t *testing.T) {
	run := runVerifyingBenchmark(t, false)
	if !errors.Is(run.cause, errSearchBudgetStopped) || !errors.Is(run.err, context.Canceled) {
		t.Fatalf("a search that never verified must be stopped by the watcher at the cap: err=%v cause=%v", run.err, run.cause)
	}
	if run.pastCap > 1500*time.Millisecond {
		t.Fatalf("the watcher stopped the search %s past the cap", run.pastCap)
	}
	if run.result.ReasonCode != "search_budget_exhausted" {
		t.Fatalf("cap stop not reported as exhausted: %+v", run.result)
	}
}

func TestInternalBenchmarkWithoutExtensionKeepsTheFixedDeadline(t *testing.T) {
	w := newBudgetWorker(t)
	var deadline, started time.Time
	result, err := runBudgetBenchmark(t, w, 10, 300*time.Millisecond, nil, func(ctx context.Context, record *BenchmarkRecord) error {
		deadline, _ = ctx.Deadline()
		started = record.SearchStartedAt
		record.searchBudget.noteUnit() // progress is irrelevant without an extension
		return waitDone(ctx)
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fixed budget must end on the context deadline: %v", err)
	}
	if !deadline.Equal(started.Add(10*time.Second)) || result.ReasonCode != "search_budget_exhausted" || result.SearchWorkerBusy {
		t.Fatalf("fixed budget changed: deadline=%v started=%v result=%+v", deadline, started, result)
	}
}

func TestInternalBenchmarkReportsABusyWorkerOnlyForLivePeers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alive bool
		busy  bool
	}{
		{"another job is running", true, true},
		// A running record left behind by a crash is not work.
		{"stale record of a dead runner", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newBudgetWorker(t)
			w.SetAliveFunc(func(*JobRecord) bool { return tc.alive })
			other := &JobRecord{ID: "other-job", Status: "running", PID: 4242}
			if err := SaveJobAtomic(filepath.Join(w.cfg.StateDir, other.ID, "job.json"), other); err != nil {
				t.Fatal(err)
			}
			// The margin leaves the watcher a few samples before the base, even
			// when setup is slow: a first tick already past the base would
			// observe nothing and never call the worker busy.
			result, _ := runBudgetBenchmark(t, w, 10, 1500*time.Millisecond, &transcode.SearchExtension{MaxSeconds: 60, SteadySeconds: 30}, func(ctx context.Context, record *BenchmarkRecord) error {
				return waitDone(ctx)
			})
			if result.ReasonCode != "search_budget_exhausted" || result.SearchWorkerBusy != tc.busy {
				t.Fatalf("busy=%v want %v: %+v", result.SearchWorkerBusy, tc.busy, result)
			}
			st, err := w.BenchmarkStatus(context.Background(), result.ID)
			if err != nil {
				t.Fatal(err)
			}
			if st.SearchWorkerBusy != tc.busy || st.SearchExtension == nil || st.SearchExtension.MaxSeconds != 60 || st.ReasonCode != "search_budget_exhausted" {
				t.Fatalf("status does not carry the busy hint and the extension: %+v", st)
			}
			st.SearchExtension.MaxSeconds = 1
			if again, _ := w.BenchmarkStatus(context.Background(), result.ID); again.SearchExtension.MaxSeconds != 60 {
				t.Fatal("status exposed the stored extension for mutation")
			}
		})
	}
}

func TestOtherWorkLiveIgnoresItselfAndIdleRecords(t *testing.T) {
	w := newBudgetWorker(t)
	alive := true
	w.SetAliveFunc(func(*JobRecord) bool { return alive })
	save := func(id, status string, pid int) {
		t.Helper()
		if err := SaveJobAtomic(filepath.Join(w.cfg.StateDir, id, "job.json"), &JobRecord{ID: id, Status: status, PID: pid}); err != nil {
			t.Fatal(err)
		}
	}
	if w.otherWorkLive("self") {
		t.Fatal("an empty worker is not contended")
	}
	save("self", "running", 4242)
	save("finished", "completed", 4243)
	save("queued-unspawned", "queued", 0)
	if w.otherWorkLive("self") {
		t.Fatal("itself, finished and unspawned jobs are not contention")
	}
	save("peer", "running", 4244)
	if !w.otherWorkLive("self") {
		t.Fatal("a live running peer is contention")
	}
	alive = false
	if w.otherWorkLive("self") {
		t.Fatal("a dead runner's record is not contention")
	}
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestSearchBudgetWatchStopsASearchOnceItsProgressStalls(t *testing.T) {
	start := time.Unix(2_000_000, 0)
	clock := &testClock{t: start.Add(11 * time.Second)} // past the 10 s base
	b := newSearchBudget(start, 10, &transcode.SearchExtension{MaxSeconds: 3600, SteadySeconds: 30})
	b.now = clock.Now
	b.poll = 5 * time.Millisecond
	b.noteUnit()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	stop := b.watch(ctx, cancel, nil)
	defer stop()

	settle := func() { time.Sleep(60 * time.Millisecond) }
	settle()
	if ctx.Err() != nil {
		t.Fatal("stopped while progress was fresh")
	}
	clock.Advance(20 * time.Second) // 20 s since the last unit
	settle()
	if ctx.Err() != nil {
		t.Fatal("stopped inside the steady window")
	}
	b.noteUnit() // new work restarts the window
	clock.Advance(20 * time.Second)
	settle()
	if ctx.Err() != nil {
		t.Fatal("a newer unit did not extend the search")
	}
	clock.Advance(20 * time.Second) // 40 s since the last unit
	deadline := time.After(3 * time.Second)
	for ctx.Err() == nil {
		select {
		case <-deadline:
			t.Fatal("a stalled search was not stopped")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if !errors.Is(context.Cause(ctx), errSearchBudgetStopped) || !b.wasStopped() {
		t.Fatalf("cause=%v", context.Cause(ctx))
	}
	if b.now().Sub(start) >= b.hardCap {
		t.Fatal("test stopped at the cap, not on the stall")
	}
}

// budgetWiringRunner runs the production runner and keeps what the budget saw.
type budgetWiringRunner struct {
	inner    BenchmarkRunner
	lastUnit time.Time
	elastic  bool
}

func (r *budgetWiringRunner) RunBenchmark(ctx context.Context, w *Worker, record *BenchmarkRecord) error {
	err := r.inner.RunBenchmark(ctx, w, record)
	r.lastUnit, r.elastic = record.searchBudget.lastUnitTime(), record.searchBudget.elastic()
	return err
}

// The production runner reports each finished unit through its own progress
// reporter; the budget must hear about it from there.
func TestProductionRunnerFinishedUnitsReachTheSearchBudget(t *testing.T) {
	dir := t.TempDir()
	source := ioOptWriteFile(t, filepath.Join(dir, "source.mkv"), string(make([]byte, 100)))
	probe := strings.ReplaceAll(sdr8BitProbeJSON, `"50000000"`, `"100"`)
	mockFFmpeg, mockProbe, _ := setupMockToolsWithQualitySizes(t, dir, probe)
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{dir}, FFmpeg: mockFFmpeg, FFprobe: mockProbe, MaxParallelJobs: 2})
	production := &ProductionBenchmarkRunner{}
	production.SetMetricsHook(func(ctx context.Context, w *Worker, record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence) error {
		for i, c := range record.Candidates {
			evidence.CandidateMetrics = append(evidence.CandidateMetrics, BenchmarkCandidateMetricAggregate{CandidateID: c.ID, CandidateIndex: i, MetricType: optimization.MetricTypeVMAF, Aggregate: optimization.MetricAggregate{MetricType: optimization.MetricTypeVMAF, Valid: true, MeanScore: 96.5, MinScore: 96.5, MaxScore: 96.5, SampleScores: []optimization.SampleScore{{SampleIndex: 0, Score: 96.5, Valid: true}}}})
		}
		return nil
	})
	runner := &budgetWiringRunner{inner: production}
	w.benchmarkRunner = runner
	record := &BenchmarkRecord{ProtocolVersion: transcode.WorkerProtocolVersion, ID: "bench-wiring", Status: "queued", Source: source, Metric: "vmaf", Mode: "size", SearchBudgetSeconds: 2, SearchExtension: &transcode.SearchExtension{MaxSeconds: 60, SteadySeconds: 30}, RunToken: "wiring-token", CreatedAt: time.Now().UTC(), Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 5, DurationSeconds: 10}}, Candidates: []transcode.BenchmarkCandidate{{ID: "large", Quality: 70}}}
	if err := SaveBenchmarkAtomic(filepath.Join(w.cfg.StateDir, record.ID, "benchmark.json"), record); err != nil {
		t.Fatal(err)
	}
	if err := w.InternalBenchmark(context.Background(), record.ID, record.RunToken); err != nil {
		t.Fatal(err)
	}
	if !runner.elastic {
		t.Fatal("the stored extension did not reach the live budget")
	}
	if runner.lastUnit.IsZero() {
		t.Fatal("finished units never reached the search budget")
	}
	result, err := LoadBenchmark(filepath.Join(w.cfg.StateDir, record.ID, "benchmark.json"))
	if err != nil || result.Status != "completed" || result.SearchWorkerBusy {
		t.Fatalf("search did not complete cleanly: %+v %v", result, err)
	}
}
