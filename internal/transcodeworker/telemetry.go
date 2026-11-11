package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

const (
	jobHeartbeatInterval = 2 * time.Second
	progressStaleAfter   = 15 * time.Second
)

// Both status reads and capacity sweeps may observe an exited process just
// after it saved completion. Re-read under the job lock before normalizing it.
// Callers that also need the capacity lock acquire capacity before job.
func (w *Worker) reconcileStoppedRunner(jobDir, jobFile string) (*JobRecord, error) {
	lock, err := acquireJobLock(jobDir)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	job, err := LoadJob(jobFile)
	if err != nil {
		return nil, err
	}
	if job.Status != "running" || job.PID <= 0 || w.jobAlive(job) {
		return job, nil
	}
	if IsPostEncodeFinished(job) {
		job.PID, job.ProcessStartTime, job.Phase = 0, "", "publishing"
		if !IsPostEncodeFinalizationPending(job) {
			job.Phase = "retained"
		}
	} else if podcastASRCheckpoint(jobDir, job) {
		job.Status, job.Phase, job.PID, job.ProcessStartTime = "queued", "transcribing", 0, ""
	} else {
		job.Status, job.Phase = "failed", "failed"
		job.FinishedAt = time.Now().UTC()
		job.Error = "process terminated unexpectedly"
		job.FailureClassification = "runner_killed"
		job.ProgressIsStale = false
	}
	return job, SaveJobAtomic(jobFile, job)
}

// startJobHeartbeat belongs to the detached runner, so progress is checkpointed
// even when neither the HTTP client nor Navigatorr is polling. It only mutates
// freshly loaded records under the job lock, never the runner's in-memory job.
func (w *Worker) startJobHeartbeat(ctx context.Context, jobDir, jobFile string, interval time.Duration) func() {
	if interval <= 0 {
		interval = jobHeartbeatInterval
	}
	stopCh, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	_ = w.persistJobHeartbeat(jobDir, jobFile, time.Now().UTC())
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case now := <-ticker.C:
				_ = w.persistJobHeartbeat(jobDir, jobFile, now.UTC())
			}
		}
	}()
	return func() { once.Do(func() { close(stopCh) }); <-done }
}

func (w *Worker) persistJobHeartbeat(jobDir, jobFile string, now time.Time) error {
	// A job whose directory moved (it was retained) must not get a shell
	// directory back from a late heartbeat.
	lock, err := acquireExistingJobLock(jobDir)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	job, err := LoadJob(jobFile)
	if err != nil {
		return err
	}
	// A cancelled/completed record or a finalization waiting for another runner
	// must never be revived by an old goroutine.
	if job.Status != "running" || job.PID != os.Getpid() {
		return nil
	}
	job.WorkerHeartbeatAt = now
	observeJobProgress(jobDir, job, now)
	return SaveJobAtomic(jobFile, job)
}

func observeJobProgress(jobDir string, job *JobRecord, now time.Time) ProgressMetrics {
	metrics := ParseProgress(filepath.Join(jobDir, "progress.txt"), job.DurationSec)
	if metrics.Valid && (job.LastKnownProgress == nil || !metrics.UpdatedAt.Before(job.LastProgressAt)) {
		job.LastProgressAt = metrics.UpdatedAt
		job.LastKnownProgress = &transcode.ProgressSnapshot{
			Progress: metrics.Progress, FPS: metrics.FPS, Speed: metrics.Speed, UpdatedAt: metrics.UpdatedAt,
		}
	}
	if job.LastKnownProgress != nil {
		metrics.Progress = job.LastKnownProgress.Progress
		metrics.FPS = job.LastKnownProgress.FPS
		metrics.Speed = job.LastKnownProgress.Speed
	}
	job.ProgressIsStale = job.Status == "running" && (job.LastProgressAt.IsZero() || now.Sub(job.LastProgressAt) > progressStaleAfter)
	if job.Status == "completed" {
		metrics.Progress = 100
		job.ProgressIsStale = false
	}
	return metrics
}

func durationMilliseconds(start, end time.Time) *int64 {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return nil
	}
	n := end.Sub(start).Milliseconds()
	return &n
}

func telemetryFor(job *JobRecord, now time.Time) transcode.JobTelemetry {
	t := job.JobTelemetry
	t.PhaseCosts = make(map[string]transcode.PhaseCost, len(job.PhaseCosts)+3)
	for name, cost := range job.PhaseCosts {
		t.PhaseCosts[name] = cost
	}
	t.CreatedAt, t.StartedAt, t.FinishedAt = job.CreatedAt, job.StartedAt, job.FinishedAt
	if isTerminalStatus(job.Status) {
		t.Phase = job.Status
	} else if job.Status == "queued" {
		t.Phase = "queued"
		if job.PID > 0 {
			t.Phase = "accepted"
		}
	} else if t.Phase == "" {
		t.Phase = "preparing"
		if job.EncodeComplete {
			t.Phase = "publishing"
		}
	}
	t.WorkerResolvedPath = job.Source
	t.FinalizationRetryCount, t.NextFinalizationAt = job.FinalizationRetryCount, job.NextFinalizationAt
	t.ErrorClass = job.FailureClassification
	t.RecoveryRequired = isPostEncodeResume(job) && !automaticFinalizationAllowed(job)
	if t.RecoveryRequired {
		t.Phase = "publication_pending"
		t.NextFinalizationAt = time.Time{}
	}
	t.QueueDurationMs = durationMilliseconds(job.CreatedAt, job.StartedAt)
	t.EncodeDurationMs = durationMilliseconds(job.EncodeStartedAt, job.EncodeFinishedAt)
	t.ValidationDurationMs = durationMilliseconds(job.ValidationStartedAt, job.ValidationFinishedAt)
	end := job.FinishedAt
	if end.IsZero() && !isTerminalStatus(job.Status) {
		end = now
	}
	for name, duration := range map[string]*int64{"queue": t.QueueDurationMs, "encode": t.EncodeDurationMs, "validation": t.ValidationDurationMs} {
		if duration != nil {
			// Timestamps own the duration and make the phase one attempt;
			// byte counters, CPU time and steps a phase recorded (for example
			// an encode reading an unstaged NAS source, or final validation's
			// steps) survive. Its steps follow the same rule, one attempt
			// each, while their runs, wall and CPU time keep every execution
			// the phase's CPU time keeps.
			cost := t.PhaseCosts[name]
			t.PhaseCosts[name] = transcode.PhaseCost{DurationMs: *duration, Attempts: 1, NASReadBytes: cost.NASReadBytes, NASWrittenBytes: cost.NASWrittenBytes, ActiveComputeMs: cost.ActiveComputeMs, Steps: oneAttemptSteps(cost.Steps)}
		}
	}
	t.WallDurationMs = durationMilliseconds(job.CreatedAt, end)
	return t
}

// slotSnapshot is observational only. Runner reservations include staging and
// publishing; phase=encoding identifies work actually using the encoder.
//
// With jobID set and the ceiling split, total and used describe the pool that
// job draws from and position counts only queued jobs of its kind; otherwise
// they cover the whole ceiling.
func (w *Worker) slotSnapshot(jobID string) (total, used, position int) {
	limits := w.slotLimits()
	total = limits.Ceiling
	use, err := w.observeSlotUse(false)
	if err != nil {
		return total, 0, 0
	}
	used = use.Video + use.Podcast
	kind := use.kinds[jobID] // video unless the job is a podcast job
	splitView := limits.Split && jobID != ""
	if splitView {
		total, used = limits.Video, use.Video
		if kind == slotKindPodcast {
			total, used = limits.Podcast, use.Podcast
		}
	}
	sortQueuedJobs(use.Queued)
	n := 0
	for _, job := range use.Queued {
		if splitView && jobSlotKind(job) != kind {
			continue
		}
		n++
		if job.ID == jobID {
			position = n
			break
		}
	}
	return
}

func (w *Worker) requireMediaBackend(name string) error {
	if w.cfg.SMBDirect.Enabled && (w.mediaStore == nil || !w.mediaStore.Maps(name)) {
		return fmt.Errorf("storage_backend_mismatch: smb_direct path %q is not mapped; local filesystem fallback is disabled", name)
	}
	return nil
}

func (w *Worker) initializeStorageTelemetry(job *JobRecord) {
	job.StorageBackend = "local"
	job.WorkerResolvedPath = job.Source
	if w.mediaStore != nil && w.mediaStore.Maps(job.Source) {
		job.StorageBackend = "smb_direct"
		job.SMBShare = w.cfg.SMBDirect.Share
		if mapper, ok := w.mediaStore.(interface{ RemotePath(string) (string, bool) }); ok {
			job.SMBRelativePath, _ = mapper.RemotePath(job.Source)
		}
	}
}

// Stored direct-SMB jobs cannot silently switch backend after a config change;
// nor may a legacy local layout bypass SSD staging when direct SMB is enabled.
func (w *Worker) validateJobStorageBackend(job *JobRecord, r *resolvedOperational) error {
	if r.staging != StagingStateNotRequired {
		if err := w.requireLocalScratch(r.stagedInput); err != nil {
			return err
		}
		if err := w.requireLocalScratch(r.effectiveInput); err != nil {
			return err
		}
	}
	if r.finalization != FinalizationStateNotRequired {
		if err := w.requireLocalScratch(r.localCandidate); err != nil {
			return err
		}
	}
	if job.StorageBackend == "smb_direct" && (w.mediaStore == nil || !w.mediaStore.Maps(job.Source) || !w.mediaStore.Maps(job.Candidate)) {
		return fmt.Errorf("storage_backend_mismatch: persisted smb_direct job requires its direct SMB mapping")
	}
	if err := w.requireMediaBackend(job.Source); err != nil {
		return err
	}
	if err := w.requireMediaBackend(job.Candidate); err != nil {
		return err
	}
	if w.cfg.SMBDirect.Enabled && (r.staging == StagingStateNotRequired || r.finalization == FinalizationStateNotRequired) {
		return fmt.Errorf("storage_backend_mismatch: smb_direct requires local staging and explicit SMB publication")
	}
	return nil
}

func knownCount(n int64) *int64 { return &n }

func (w *Worker) persistPhaseCost(jobDir, jobFile string, job *JobRecord, phase string, start time.Time, cost transcode.PhaseCost) {
	cost.DurationMs = time.Since(start).Milliseconds()
	if cost.Attempts <= 0 {
		cost.Attempts = 1 // one phase run; a transfer that reconnected reports more
	}
	shadow := *job
	_, _ = w.persistOperationalProgress(jobDir, jobFile, &shadow, func(latest *JobRecord) {
		if latest.PhaseCosts == nil {
			latest.PhaseCosts = map[string]transcode.PhaseCost{}
		}
		if previous, ok := latest.PhaseCosts[phase]; ok {
			cost = accumulatePhaseCost(previous, cost)
		}
		latest.PhaseCosts[phase] = cost
	})
	job.PhaseCosts = shadow.PhaseCosts
}

// accumulatePhaseCost adds a new execution of a phase (a retry, a resumed job)
// to what earlier executions recorded. A counter is known only when every
// execution measured it: unknown on either side makes it unknown, since the
// measured part alone would be a floor, not a total. When previous recorded no
// attempts, the new execution's counters pass through unchanged. Steps
// accumulate the same way, by name.
func accumulatePhaseCost(previous, cost transcode.PhaseCost) transcode.PhaseCost {
	cost.DurationMs += previous.DurationMs
	cost.Attempts += previous.Attempts
	cost.Runs += previous.Runs
	if previous.Attempts > 0 {
		cost.NASReadBytes = addKnown(previous.NASReadBytes, cost.NASReadBytes)
		cost.NASWrittenBytes = addKnown(previous.NASWrittenBytes, cost.NASWrittenBytes)
		cost.CacheHits = addKnown(previous.CacheHits, cost.CacheHits)
		cost.CacheMisses = addKnown(previous.CacheMisses, cost.CacheMisses)
		cost.ActiveComputeMs = addKnown(previous.ActiveComputeMs, cost.ActiveComputeMs)
	}
	if len(previous.Steps) > 0 {
		steps := make(map[string]transcode.PhaseCost, len(previous.Steps)+len(cost.Steps))
		for name, step := range previous.Steps {
			steps[name] = step
		}
		for name, step := range cost.Steps {
			if earlier, ok := steps[name]; ok {
				step = accumulatePhaseCost(earlier, step)
			}
			steps[name] = step
		}
		cost.Steps = steps
	}
	return cost
}

// oneAttemptSteps is a copy of steps (every field) counted as one attempt
// each, for a phase whose telemetry counts one attempt; the job's own record
// is not changed. A step that recorded no attempt keeps none.
func oneAttemptSteps(steps map[string]transcode.PhaseCost) map[string]transcode.PhaseCost {
	if len(steps) == 0 {
		return nil
	}
	out := make(map[string]transcode.PhaseCost, len(steps))
	for name, step := range steps {
		if step.Attempts > 0 {
			step.Attempts = 1
		}
		out[name] = step
	}
	return out
}

// addKnown sums two counters and is known only when both are: an unknown on
// either side makes the sum unknown (nil).
func addKnown(a, b *int64) *int64 {
	if a == nil || b == nil {
		return nil
	}
	return knownCount(*a + *b)
}
