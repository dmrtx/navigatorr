package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Job directory retention.
//
// Every submitted job leaves a directory in the state directory (record,
// progress, logs) that nothing removed, so a long-lived worker kept thousands
// of them and paid for the listing and the records on every scan. Finished jobs
// older than the retention window are removed here.
//
// Only a job that is finished and that nothing can still act on is removed:
//
//   - completed and cancelled jobs, and failed jobs that never reached a
//     finished encode. A failed job whose encode is complete but whose
//     publication did not finish stays: an operator can still retry it and its
//     record points at the candidate.
//   - never a queued, running, finalizing, held or retained job, or one with a
//     live runner, and never a hidden directory (retained candidates and their
//     tombstones are the retained sweep's), whatever their age.
//
// The age is the later of the job's own finish time and its record's file time,
// so a record someone touched recently is kept. The decision is repeated under
// the job's lock before anything is removed, and under the capacity lock so a
// submit that is matching an idempotency key never sees a half-removed job.

const (
	// defaultJobRetentionDays is how long a finished job's directory is kept
	// when job_retention_days is not set.
	defaultJobRetentionDays = 30
	// jobPruneInterval is how often the scheduler looks for old jobs.
	jobPruneInterval = time.Hour
	// jobPruneBatch bounds how many directories one pass removes, so the pass
	// never holds the capacity lock for long; the next pass takes the rest.
	jobPruneBatch = 200
)

// jobRetention is the age past which a finished job's directory is removed; zero
// means the worker keeps them all (job_retention_days below zero).
func (w *Worker) jobRetention() time.Duration {
	switch days := w.cfg.JobRetentionDays; {
	case days < 0:
		return 0
	case days == 0:
		return defaultJobRetentionDays * 24 * time.Hour
	default:
		return time.Duration(days) * 24 * time.Hour
	}
}

// prunable reports whether a record is a finished job nothing can act on any
// more. It looks at the record only; age and liveness are checked by the caller.
func prunable(job *JobRecord) bool {
	if job == nil {
		return false
	}
	switch job.Status {
	case "completed", "cancelled":
		return true
	case "failed":
		if job.EncodeComplete && FinalizationState(strings.TrimSpace(job.FinalizationState)) != FinalizationStateCompleted {
			return false // a publication an operator can still retry
		}
		return true
	}
	return false
}

// jobFinishedAt is when a record last meant anything: its finish time, or its
// creation, and never earlier than the file's own modification time.
func jobFinishedAt(job *JobRecord, fileTime time.Time) time.Time {
	t := job.FinishedAt
	if t.IsZero() {
		t = job.CreatedAt
	}
	if fileTime.After(t) {
		t = fileTime
	}
	return t
}

// MaybePruneJobs removes old finished jobs at most once an hour; it is cheap to
// call on every scheduler pass.
func (w *Worker) MaybePruneJobs(ctx context.Context) {
	if w.jobRetention() <= 0 {
		return
	}
	w.pruneMu.Lock()
	if !w.pruneAt.IsZero() && time.Since(w.pruneAt) < jobPruneInterval {
		w.pruneMu.Unlock()
		return
	}
	w.pruneAt = time.Now()
	w.pruneMu.Unlock()
	n, err := w.PruneJobs(ctx, time.Now())
	if n > 0 {
		log.Printf("worker pruned %d finished job directories older than %s", n, w.jobRetention())
	}
	if err != nil {
		log.Printf("worker job pruning: %v", err)
	}
}

// PruneJobs removes the finished jobs older than the retention window as of
// now, up to a batch, and returns how many it removed.
func (w *Worker) PruneJobs(ctx context.Context, now time.Time) (int, error) {
	retention := w.jobRetention()
	if retention <= 0 {
		return 0, nil
	}
	stateDir := filepath.Clean(w.cfg.StateDir)
	entries, err := os.ReadDir(stateDir) // fresh: a stale listing must never decide a removal
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading state dir for pruning: %w", err)
	}
	cutoff := now.Add(-retention)
	var candidates []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		jobFile := filepath.Join(stateDir, name, "job.json")
		fi, err := os.Stat(jobFile)
		if err != nil || fi.ModTime().After(cutoff) {
			continue
		}
		job, err := peekJob(jobFile)
		if err != nil || job.ID != name || !prunable(job) || jobFinishedAt(job, fi.ModTime()).After(cutoff) {
			continue
		}
		candidates = append(candidates, name)
		if len(candidates) >= jobPruneBatch {
			break
		}
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	capLock, err := acquireCapacityLock(stateDir)
	if err != nil {
		return 0, err
	}
	defer capLock.Unlock()
	removed := 0
	var errs []error
	for _, name := range candidates {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		ok, err := w.pruneOne(stateDir, name, cutoff)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			removed++
		}
	}
	return removed, errors.Join(errs...)
}

// pruneOne removes one job directory after deciding again under the job lock.
func (w *Worker) pruneOne(stateDir, name string, cutoff time.Time) (bool, error) {
	dir := filepath.Join(stateDir, name)
	lock, err := tryAcquireExistingJobLock(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errJobLockBusy) {
			return false, nil
		}
		return false, err
	}
	defer lock.Unlock()
	jobFile := filepath.Join(dir, "job.json")
	fi, err := os.Stat(jobFile)
	if err != nil {
		return false, nil
	}
	job, err := LoadJob(jobFile)
	if err != nil || job.ID != name || !prunable(job) || jobFinishedAt(job, fi.ModTime()).After(cutoff) {
		return false, nil
	}
	if job.PID > 0 && w.jobAlive(job) {
		return false, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("removing finished job %s: %w", name, err)
	}
	invalidateJobIndexDir(dir)
	if w.sourceCacheEnabled() {
		releaseSourceCachePins(w.sourceCacheDir(), name)
	}
	return true, nil
}
