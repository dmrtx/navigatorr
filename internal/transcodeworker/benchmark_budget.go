package transcodeworker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// errSearchBudgetStopped is the cancellation cause when a search runs out of
// budget before its hard deadline.
var errSearchBudgetStopped = errors.New("search budget exhausted")

// searchBudgetPollInterval is a variable so tests can watch a budget quickly.
var searchBudgetPollInterval = 10 * time.Second

// searchBudget decides how long one mode search may run. Without an extension
// it is the fixed wall-clock budget from the persisted search start. With one,
// the search may go on past that budget only while it keeps finishing units of
// work, and never past the hard cap, so a worker shared with a long encode
// does not turn a slow search into a false "no result".
//
// The cap and the steady window are frozen in the mode policy; the clock still
// starts at the persisted search start, so a restart cannot grant more time.
type searchBudget struct {
	start   time.Time
	base    time.Duration
	hardCap time.Duration
	steady  time.Duration
	now     func() time.Time

	mu        sync.Mutex
	lastUnit  time.Time
	sampledAt time.Time
	observed  time.Duration
	shared    time.Duration
	stopped   bool
}

func newSearchBudget(start time.Time, baseSeconds int, ext *transcode.SearchExtension) *searchBudget {
	b := &searchBudget{start: start, base: time.Duration(baseSeconds) * time.Second, now: time.Now}
	b.hardCap = b.base
	if ext != nil && ext.Validate(baseSeconds) == nil {
		b.hardCap = time.Duration(ext.MaxSeconds) * time.Second
		b.steady = time.Duration(ext.SteadySeconds) * time.Second
	}
	return b
}

func (b *searchBudget) elastic() bool { return b.steady > 0 }

// hardDeadline is the latest instant the search can run to.
func (b *searchBudget) hardDeadline() time.Time { return b.start.Add(b.hardCap) }

// noteUnit records that a unit of real work (not a skipped one) finished.
func (b *searchBudget) noteUnit() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.lastUnit = b.now()
	b.mu.Unlock()
}

// continues reports whether the search may keep running at now. Progress is
// remembered in memory only, so a search that re-enters past its base budget
// has shown none and stops at once; that is as conservative as the fixed budget.
func (b *searchBudget) continues(now time.Time) bool {
	elapsed := now.Sub(b.start)
	if elapsed >= b.hardCap {
		return false
	}
	if elapsed < b.base {
		return true
	}
	if !b.elastic() {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.lastUnit.IsZero() && now.Sub(b.lastUnit) <= b.steady
}

// sample accounts the time since the previous sample as shared when another
// job held a worker slot.
func (b *searchBudget) sample(now time.Time, shared bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.sampledAt.IsZero() && now.After(b.sampledAt) {
		d := now.Sub(b.sampledAt)
		b.observed += d
		if shared {
			b.shared += d
		}
	}
	b.sampledAt = now
}

// workerBusy reports whether another job held a slot for most of the search.
func (b *searchBudget) workerBusy() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.observed > 0 && b.shared*2 >= b.observed
}

// wasStopped reports whether the watcher ended the search at its budget (the
// hard deadline is reported by the context itself).
func (b *searchBudget) wasStopped() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopped
}

// watch samples whether the worker is shared and, for an elastic budget,
// cancels the search once it passes its base budget without steady progress.
// It returns a stop function that waits for the watcher to exit.
func (b *searchBudget) watch(ctx context.Context, cancel context.CancelCauseFunc, shared func() bool) func() {
	done := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-timer.C:
			}
			now := b.now()
			if shared != nil {
				b.sample(now, shared())
			}
			if b.elastic() && !b.continues(now) {
				b.mu.Lock()
				b.stopped = true
				b.mu.Unlock()
				cancel(errSearchBudgetStopped)
				return
			}
			wait := searchBudgetPollInterval
			// Check exactly when the base budget ends rather than a poll late.
			if untilBase := b.start.Add(b.base).Sub(now); untilBase > 0 && untilBase < wait {
				wait = untilBase
			}
			timer.Reset(wait)
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-done
	}
}

// otherWorkLive reports whether another job or benchmark has a live runner on
// this worker. A stale running record left by a crash or reboot is not work, so
// it must not make a slow search look contended. Unlike countActiveJobs it
// reads only: reconciling stale records stays with the capacity scan.
func (w *Worker) otherWorkLive(excludeID string) bool {
	entries, err := os.ReadDir(w.cfg.StateDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name == excludeID || strings.HasPrefix(name, ".") {
			continue
		}
		dir := filepath.Join(w.cfg.StateDir, name)
		if job, err := LoadJob(filepath.Join(dir, "job.json")); err == nil && job.ID == name &&
			(job.Status == "running" || job.Status == "queued") && job.PID > 0 && w.jobAlive(job) {
			return true
		}
		if bench, err := LoadBenchmark(filepath.Join(dir, "benchmark.json")); err == nil && bench.ID == name &&
			(bench.Status == "running" || bench.Status == "queued") && IsBenchmarkExecutionAlive(bench) {
			return true
		}
	}
	return false
}
