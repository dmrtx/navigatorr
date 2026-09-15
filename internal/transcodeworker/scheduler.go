package transcodeworker

import (
	"context"
	"sync"
	"time"
)

// DefaultSchedulerInterval is the daemon queue-drain tick. It bounds how
// stale a freed slot can look to a queued job while keeping the sweep cheap
// (a directory scan + job.json reads under the capacity lock).
const DefaultSchedulerInterval = 2 * time.Second

// RunScheduler starts the daemon-owned queue drain: an initial ScheduleQueued
// sweep (so jobs persisted before a restart become schedulable immediately),
// then a bounded ticker sweep until ctx is done or the returned stop func is
// called. It only ever starts persisted queued jobs as global slots open; it
// deliberately performs no running-job reconciliation itself, because the
// serve startup path guarantees ReconcileStartup has completed successfully
// before this scheduler is started.
//
// Ownership: the persistent daemon calls this once with its serve lifetime
// context, so queued jobs start while Navigatorr is disconnected. Callers
// must not broaden this into retry/state-machine behavior.
func (w *Worker) RunScheduler(ctx context.Context, selfExe, configPath string, interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = DefaultSchedulerInterval
	}
	// Initial sweep: previously persisted queued jobs are discoverable and
	// schedulable on daemon/worker startup without another submit.
	_, _ = w.ScheduleQueued(ctx, selfExe, configPath)

	ticker := time.NewTicker(interval)
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	stop = func() {
		stopOnce.Do(func() { close(stopCh) })
	}
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-ticker.C:
				_, _ = w.ScheduleQueued(ctx, selfExe, configPath)
			}
		}
	}()
	return stop
}
