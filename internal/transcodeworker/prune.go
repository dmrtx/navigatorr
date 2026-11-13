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
// progress, logs, scratch files) that nothing removed, so a long-lived worker
// kept hundreds of megabytes of them. For finished jobs older than the
// retention window everything but the record is removed.
//
// The record itself (job.json, with its terminal marker and lock file) stays:
// it is what lets a resubmit with the same idempotency key find the finished
// job, a different digest get its conflict, and GET /v1/jobs/<id> keep
// answering, and it is a few kilobytes.
//
// Only a job that is finished and that nothing can still act on is touched:
//
//   - status completed, cancelled, failed or expired, and a failed job only if
//     it has no finished encode with an unfinished publication (an operator can
//     still retry that);
//   - never a record whose finalization state is pending, finalizing or
//     retained, whatever its status; never a queued or running job, or one with
//     a live runner; never a hidden directory (retained candidates and their
//     tombstones are the retained sweep's).
//
// The age is the later of the job's own finish time and its record's file time,
// so a record someone touched recently is kept. The decision is repeated under
// the job's lock and the capacity lock before anything is removed.

const (
	// defaultJobRetentionDays is how long a finished job's directory is kept
	// when job_retention_days is not set.
	defaultJobRetentionDays = 30
	// maxJobRetentionDays keeps the window's Duration from overflowing (100 years).
	maxJobRetentionDays = 36500
	// jobPruneInterval is how often the scheduler looks for old jobs.
	jobPruneInterval = time.Hour
	// jobPruneBatch bounds how many directories one pass empties, so the pass
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
		return time.Duration(min(days, maxJobRetentionDays)) * 24 * time.Hour
	}
}

// prunable reports whether a record is a finished job nothing can act on any
// more. It looks at the record only; age and liveness are checked by the caller.
func prunable(job *JobRecord) bool {
	if job == nil {
		return false
	}
	switch FinalizationState(strings.TrimSpace(job.FinalizationState)) {
	case FinalizationStatePending, FinalizationStateFinalizing, FinalizationStateRetained:
		return false
	}
	switch job.Status {
	case "completed", "cancelled", StatusExpired:
		return true
	case "failed":
		// A finished encode whose publication did not finish can be retried.
		return !job.EncodeComplete || FinalizationState(strings.TrimSpace(job.FinalizationState)) == FinalizationStateCompleted
	}
	return false
}

// keptInPrunedJob are the files that stay in a pruned job directory.
func keptInPrunedJob(name string) bool {
	return name == "job.json" || name == "terminal.json" || name == ".lock"
}

// alreadyPruned reports a job directory that holds nothing but what stays.
func alreadyPruned(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true // unreadable: not ours to act on
	}
	for _, e := range entries {
		if !keptInPrunedJob(e.Name()) {
			return false
		}
	}
	return true
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

// PruneJobs empties the finished jobs older than the retention window as of
// now, up to a batch, and returns how many it emptied.
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
		if alreadyPruned(filepath.Join(stateDir, name)) {
			continue
		}
		candidates = append(candidates, name)
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
			if removed >= jobPruneBatch {
				break // busy and live ones above did not count against the batch
			}
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
	// Pins first: a crash between the two steps leaves a job to prune again,
	// not a cache entry nobody will release.
	if w.sourceCacheEnabled() {
		releaseSourceCachePins(w.sourceCacheDir(), name)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("listing finished job %s: %w", name, err)
	}
	var errs []error
	for _, e := range entries {
		if keptInPrunedJob(e.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, fmt.Errorf("removing %s of finished job %s: %w", e.Name(), name, err))
		}
	}
	return len(errs) == 0, errors.Join(errs...)
}
