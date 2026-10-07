package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	if IsPostEncodeFinalizationPending(job) {
		job.PID, job.ProcessStartTime, job.Phase = 0, "", "publishing"
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
	lock, err := acquireJobLock(jobDir)
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
			t.PhaseCosts[name] = transcode.PhaseCost{DurationMs: *duration, Attempts: 1}
		}
	}
	t.WallDurationMs = durationMilliseconds(job.CreatedAt, end)
	return t
}

// slotSnapshot is observational only. Runner reservations include staging and
// publishing; phase=encoding identifies work actually using the encoder.
func (w *Worker) slotSnapshot(jobID string) (total, used, position int) {
	total = w.cfg.MaxParallelJobs
	if total < 1 {
		total = 1
	}
	entries, err := os.ReadDir(w.cfg.StateDir)
	if err != nil {
		return total, 0, 0
	}
	var queued []*JobRecord
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dir := filepath.Join(w.cfg.StateDir, entry.Name())
		if job, err := LoadJob(filepath.Join(dir, "job.json")); err == nil && job.ID == entry.Name() {
			if (job.Status == "running" || job.Status == "queued") && job.PID > 0 {
				used++
			} else if job.Status == "queued" {
				queued = append(queued, job)
			}
		}
		if bench, err := LoadBenchmark(filepath.Join(dir, "benchmark.json")); err == nil && bench.ID == entry.Name() {
			if (bench.Status == "running" || bench.Status == "queued") && bench.PID > 0 {
				used++
			}
		}
	}
	sortQueuedJobs(queued)
	for i, job := range queued {
		if job.ID == jobID {
			position = i + 1
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
	cost.Attempts = 1
	shadow := *job
	_, _ = w.persistOperationalProgress(jobDir, jobFile, &shadow, func(latest *JobRecord) {
		if latest.PhaseCosts == nil {
			latest.PhaseCosts = map[string]transcode.PhaseCost{}
		}
		previous := latest.PhaseCosts[phase]
		cost.DurationMs += previous.DurationMs
		cost.Attempts += previous.Attempts
		add := func(a, b *int64) *int64 {
			if a == nil || b == nil {
				return nil
			}
			return knownCount(*a + *b)
		}
		if previous.Attempts > 0 {
			cost.NASReadBytes = add(previous.NASReadBytes, cost.NASReadBytes)
			cost.NASWrittenBytes = add(previous.NASWrittenBytes, cost.NASWrittenBytes)
			cost.CacheHits = add(previous.CacheHits, cost.CacheHits)
			cost.CacheMisses = add(previous.CacheMisses, cost.CacheMisses)
		}
		latest.PhaseCosts[phase] = cost
	})
	job.PhaseCosts = shadow.PhaseCosts
}

// A measurement collector lives on this one request context, never on Worker:
// concurrent jobs cannot steal each other's cache/transfer evidence.
type sourceCostKey struct{}

func sourceCost(ctx context.Context) *transcode.PhaseCost {
	cost, _ := ctx.Value(sourceCostKey{}).(*transcode.PhaseCost)
	return cost
}
