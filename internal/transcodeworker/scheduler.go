package transcodeworker

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// DefaultSchedulerInterval is the daemon queue-drain tick. It bounds how
// stale a freed slot can look to a queued job while keeping the sweep cheap
// (a directory scan + job.json reads under the capacity lock).
const DefaultSchedulerInterval = 2 * time.Second

const MaxFinalizationRetries = 3
const finalizationRetryBaseDelay = 5 * time.Second
const failurePostEncodeRunnerUnavailable = "post_encode_runner_unavailable"

func finalizationRetryDelay(retries int) time.Duration {
	if retries < 0 {
		retries = 0
	}
	if retries > MaxFinalizationRetries {
		retries = MaxFinalizationRetries
	}
	return finalizationRetryBaseDelay * time.Duration(1<<retries)
}

func finalizationClassRetryable(class string) bool {
	switch class {
	case string(resilience.SMBSigningRequired), string(resilience.SMBSessionInvalid),
		string(resilience.SMBTransportError), string(resilience.StorageIOError),
		string(resilience.StorageIOTransient), FailureStoragePublicationAmbiguous,
		failurePostEncodeRunnerUnavailable:
		return true
	default:
		return false
	}
}

func automaticFinalizationAllowed(job *JobRecord) bool {
	if job.FinalizationRetryCount >= MaxFinalizationRetries {
		return false
	}
	classification := strings.TrimSpace(job.FailureClassification)
	if classification == "" && strings.TrimSpace(job.Error) == "" {
		return true // interrupted after the durable encode checkpoint
	}
	if finalizationClassRetryable(classification) {
		return true
	}
	if classification != FailureStorageFinalization {
		return false
	}
	// Old jobs may carry only the generic finalization class. Preserve the
	// existing exact ambiguous-publication recovery, or require a recognizable
	// transient failure. Generic unknown/auth/permission/conflict errors stay
	// resumable for an operator but do not create an unattended retry loop.
	if job.PartialPath != "" && job.IntendedDestination != "" && job.Error == legacyAmbiguousPublicationError(job.PartialPath, job.IntendedDestination) {
		return true
	}
	return finalizationClassRetryable(string(resilience.Classify(job.Error)))
}

// RunScheduler drains queued jobs and resumes due post-encode publication
// recovery without a client request. Both sweeps honor capacity and persisted
// job ownership; publication recovery never runs the encoder again.
//
// Ownership: the persistent daemon calls this once with its serve lifetime
// context, so progress continues while Navigatorr is disconnected.
func (w *Worker) RunScheduler(ctx context.Context, selfExe, configPath string, interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = DefaultSchedulerInterval
	}
	// Finish a due publication before filling capacity with new encodes.
	_, _ = w.ResumePostEncode(ctx, selfExe, configPath)
	_, _ = w.ScheduleQueued(ctx, selfExe, configPath)

	ticker := time.NewTicker(interval)
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	var stopOnce sync.Once
	stop = func() {
		stopOnce.Do(func() { close(stopCh) })
		<-doneCh
	}
	go func() {
		defer ticker.Stop()
		defer close(doneCh)
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-ticker.C:
				_, _ = w.ResumePostEncode(ctx, selfExe, configPath)
				_, _ = w.ScheduleQueued(ctx, selfExe, configPath)
			}
		}
	}()
	return stop
}
