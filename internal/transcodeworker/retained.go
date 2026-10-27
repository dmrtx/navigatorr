package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Deferred candidate publication (docs/transcode/DEFERRED_CANDIDATE_PUBLICATION.md).
//
// A job submitted with publish_policy on_approval encodes and validates as
// usual, then stops at the encode checkpoint instead of publishing. It is then
// "retained": the validated candidate and the checkpoint stay on the worker, no
// runner and no slot are held, and the job waits for POST /v1/jobs/{id}/publish
// (or a release through Cancel, or expiry).
//
// A retained job lives in a dot-prefixed directory next to the regular job
// directories. Every scan of the state directory (startup reconciliation, slot
// accounting, the idempotency index, the activity listing, the post-encode
// sweep) already ignores dot-prefixed entries, so an older worker that is rolled
// back onto the same state directory neither fails on the unknown finalization
// state nor publishes the candidate: it answers "job not found".
//
// The directory move is the only crash-sensitive step, and both orders of
// "state" and "rename" are recoverable by the retained sweep:
//
//	retain:  [running, pending, deferred] --rename--> .retained-<id> --save--> [retained]
//	publish: [retained] --save--> [finalizing] --rename--> <id>
//
// pending+deferred is the "encoded, not yet retained" recovery state: nothing
// publishes it (IsPostEncodeFinalizationPending is false for it) and nothing
// marks it runner_killed (IsPostEncodeFinished is true), so only the sweep acts
// on it. Both moves happen under the job lock, whose flock follows the inode
// across the rename.

const (
	retainedDirPrefix = ".retained-"

	// defaultRetainedTTL is how long an unpublished candidate is kept.
	defaultRetainedTTL = 30 * 24 * time.Hour
	// expiredTombstoneTTL is how long an expired job keeps answering "expired"
	// before its record is removed and the job becomes unknown.
	expiredTombstoneTTL = 30 * 24 * time.Hour
	// retainedSweepInterval rate-limits the scheduler-driven sweep.
	retainedSweepInterval = time.Minute
	// shellDirGrace is how old an empty directory must be before the collector
	// removes it: a submit or a move may be creating it right now.
	shellDirGrace = time.Hour

	// StatusExpired is the status of a tombstone left by an expired candidate.
	StatusExpired = "expired"
)

var (
	// ErrNotRetained reports a publish for a job that is not a retained
	// candidate (unknown policy, still encoding, cancelled, failed or expired).
	ErrNotRetained = errors.New("job is not a retained candidate")
	// ErrPublishHeld reports a publish refused while the drain freeze is on.
	ErrPublishHeld = errors.New("publication is held while the worker drains")
	// ErrRetainedCandidateChanged reports a retained file that no longer matches
	// the checkpoint it was accepted with.
	ErrRetainedCandidateChanged = errors.New("retained candidate no longer matches its checkpoint")
	// ErrReleaseRefused reports a release (cancel) refused because a publication
	// may be running or may already have placed the file.
	ErrReleaseRefused = errors.New("release refused: a publication may be in progress")
)

func (w *Worker) retainedTTL() time.Duration {
	if w.cfg.RetainedTTLHours > 0 {
		return time.Duration(w.cfg.RetainedTTLHours) * time.Hour
	}
	return defaultRetainedTTL
}

func (w *Worker) regularDir(jobID string) string {
	return filepath.Join(filepath.Clean(w.cfg.StateDir), jobID)
}

func (w *Worker) retainedDir(jobID string) string {
	return filepath.Join(filepath.Clean(w.cfg.StateDir), retainedDirPrefix+jobID)
}

func fileExists(path string) (bool, error) {
	// Any entry counts, even a directory: loading it then reports the read
	// failure instead of the job reading as unknown.
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// locateJob returns the directory holding a job's record: the regular one, or
// the hidden one of a retained candidate or tombstone. A job moves between the
// two under its job lock, so the regular directory is checked again after the
// hidden one before the job is declared unknown.
func (w *Worker) locateJob(jobID string) (string, error) {
	if err := validateJobID(jobID); err != nil || strings.HasPrefix(jobID, ".") {
		return "", fmt.Errorf("%w: invalid job id", ErrJobNotFound)
	}
	regular, hidden := w.regularDir(jobID), w.retainedDir(jobID)
	for _, dir := range []string{regular, hidden, regular} {
		ok, err := fileExists(filepath.Join(dir, "job.json"))
		if err != nil {
			return "", err
		}
		if ok {
			return dir, nil
		}
	}
	return "", fmt.Errorf("%w: %w", ErrJobNotFound, fs.ErrNotExist)
}

// lockLocatedJob locks the job wherever it currently lives and returns its
// freshly loaded record. A move between locating and locking is retried. The
// caller unlocks.
func (w *Worker) lockLocatedJob(jobID string) (string, *jobFileLock, *JobRecord, error) {
	for attempt := 0; attempt < 4; attempt++ {
		dir, err := w.locateJob(jobID)
		if err != nil {
			return "", nil, nil, err
		}
		lock, err := acquireExistingJobLock(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", nil, nil, err
		}
		job, err := loadJobForAPI(filepath.Join(dir, "job.json"))
		if err != nil {
			lock.Unlock()
			if errors.Is(err, ErrJobNotFound) {
				continue
			}
			return "", nil, nil, err
		}
		if job.ID != jobID {
			lock.Unlock()
			return "", nil, nil, fmt.Errorf("inconsistent durable job record in %s (fail closed): id mismatch", dir)
		}
		return dir, lock, job, nil
	}
	return "", nil, nil, fmt.Errorf("%w: job kept moving while locking it", ErrJobNotFound)
}

// loadLocatedJob reads a job's record without locking, wherever it lives.
func (w *Worker) loadLocatedJob(jobID string) (string, *JobRecord, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		dir, err := w.locateJob(jobID)
		if err != nil {
			return "", nil, err
		}
		job, err := loadJobForAPI(filepath.Join(dir, "job.json"))
		if err == nil {
			return dir, job, nil
		}
		if !errors.Is(err, ErrJobNotFound) {
			return "", nil, err
		}
		lastErr = err
	}
	return "", nil, lastErr
}

// removeShellDir removes an empty job directory (only a lock file) that a
// racing lock acquisition left behind, so a move can take its place. A
// directory holding a job record, or anything else, is a collision and fails.
func removeShellDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != ".lock" {
			return fmt.Errorf("directory %s is occupied (fail closed)", dir)
		}
	}
	_ = os.Remove(filepath.Join(dir, ".lock"))
	return os.Remove(dir)
}

// releaseRetainedInputs frees what a deferred job no longer needs once the
// candidate is accepted: the per-job staged source and its source-cache pins.
// The local candidate and the checkpoint stay. Idempotent.
func (w *Worker) releaseRetainedInputs(job *JobRecord) {
	w.removeOwnStagedInput(job)
	if w.sourceCacheEnabled() {
		releaseSourceCachePins(w.sourceCacheDir(), job.ID)
	}
}

// completeRetentionLocked finishes retaining a job whose record is in
// pending+deferred, from either side of the rename. The caller holds the job
// lock of jobDir. The directory is renamed first and the state saved inside the
// hidden directory, so no state is ever visible under the regular name that an
// older worker would not understand.
func (w *Worker) completeRetentionLocked(jobDir string, job *JobRecord) error {
	hidden := w.retainedDir(job.ID)
	if filepath.Clean(jobDir) != hidden {
		if err := removeShellDir(hidden); err != nil {
			return fmt.Errorf("preparing retained directory for job %q: %w", job.ID, err)
		}
		if err := os.Rename(jobDir, hidden); err != nil {
			return fmt.Errorf("moving job %q to its retained directory: %w", job.ID, err)
		}
	}
	w.releaseRetainedInputs(job)
	now := time.Now().UTC()
	job.FinalizationState = string(FinalizationStateRetained)
	job.Status = "running"
	job.PID, job.ProcessStartTime = 0, ""
	job.Phase = "retained"
	job.NextFinalizationAt = time.Time{}
	job.Error, job.FailureClassification = "", ""
	if job.RetainedAt.IsZero() {
		job.RetainedAt = now
	}
	job.JobTelemetry = telemetryFor(job, now)
	if err := SaveJobAtomic(filepath.Join(hidden, "job.json"), job); err != nil {
		return fmt.Errorf("saving retained job %q: %w", job.ID, err)
	}
	return nil
}

// retainJob is the runner's last step for a deferred job: it moves the encoded
// job to its hidden directory. It must run after the runner's heartbeat has
// stopped, which would otherwise write into the old path.
func (w *Worker) retainJob(jobID string) error {
	jobDir := w.regularDir(jobID)
	lock, err := acquireExistingJobLock(jobDir)
	if err != nil {
		return fmt.Errorf("locking job %q to retain it: %w", jobID, err)
	}
	defer lock.Unlock()
	job, err := LoadJob(filepath.Join(jobDir, "job.json"))
	if err != nil {
		return fmt.Errorf("loading job %q to retain it: %w", jobID, err)
	}
	if job.Status == "cancelled" {
		return nil // cancellation wins
	}
	if !isEncodedAwaitingRetention(job) {
		return nil
	}
	return w.completeRetentionLocked(jobDir, job)
}

// isEncodedAwaitingRetention reports the recovery state of a deferred job whose
// encode checkpoint is durable but whose directory is not yet hidden.
func isEncodedAwaitingRetention(job *JobRecord) bool {
	return job != nil && job.deferredPublication() && job.Status == "running" && job.EncodeComplete &&
		FinalizationState(strings.TrimSpace(job.FinalizationState)) == FinalizationStatePending
}

// restoreRetainedLocked moves a hidden job directory back to its regular name.
// The caller holds the job lock.
func (w *Worker) restoreRetainedLocked(hiddenDir string, job *JobRecord) (string, error) {
	regular := w.regularDir(job.ID)
	if err := removeShellDir(regular); err != nil {
		return "", fmt.Errorf("preparing job directory for job %q: %w", job.ID, err)
	}
	if err := os.Rename(hiddenDir, regular); err != nil {
		return "", fmt.Errorf("moving retained job %q back: %w", job.ID, err)
	}
	return regular, nil
}

// PublishRetained is POST /v1/jobs/{id}/publish. It turns a retained candidate
// into an ordinary post-encode job awaiting finalization: it verifies the local
// file against the checkpoint, persists "finalizing" while the job is still
// hidden, moves the directory back and leaves the rest to the post-encode sweep,
// which publishes it like any other finished encode (under the video slot
// limit, with the retry budget reset). Idempotent: a job already finalizing or
// completed answers its status.
func (w *Worker) PublishRetained(ctx context.Context, jobID string) (JobStatusResponse, error) {
	if err := ctx.Err(); err != nil {
		return JobStatusResponse{ID: jobID, Status: "failed", Error: err.Error()}, err
	}
	dir, lock, job, err := w.lockLocatedJob(jobID)
	if err != nil {
		return JobStatusResponse{ID: jobID, Status: "failed", Error: jobLoadErrorMessage(err)}, err
	}
	locked := true
	defer func() {
		if locked {
			lock.Unlock()
		}
	}()
	refuse := func(err error) (JobStatusResponse, error) {
		return JobStatusResponse{ID: job.ID, Status: job.Status, CandidatePath: job.Candidate, Error: err.Error()}, err
	}
	if !job.deferredPublication() {
		return refuse(fmt.Errorf("%w: job %q was not submitted with publish_policy on_approval", ErrNotRetained, jobID))
	}
	switch job.Status {
	case "completed":
		return JobStatusResponse{ID: job.ID, Status: job.Status, CandidatePath: job.Candidate}, nil
	case "failed", "cancelled", StatusExpired:
		return refuse(fmt.Errorf("%w: job %q is %s", ErrNotRetained, jobID, job.Status))
	}
	switch FinalizationState(strings.TrimSpace(job.FinalizationState)) {
	case FinalizationStateFinalizing:
		// Already requested (or running): nothing to do.
		return JobStatusResponse{ID: job.ID, Status: job.Status, CandidatePath: job.Candidate}, nil
	case FinalizationStateRetained:
	default:
		return refuse(fmt.Errorf("%w: job %q has not finished encoding", ErrNotRetained, jobID))
	}
	if w.Held() {
		return refuse(ErrPublishHeld)
	}
	if err := verifyCheckpointCandidate(w.withJobDigestMemo(ctx), job, job.LocalCandidatePath); err != nil {
		return refuse(fmt.Errorf("%w: %v", ErrRetainedCandidateChanged, err))
	}

	now := time.Now().UTC()
	job.FinalizationState = string(FinalizationStateFinalizing)
	job.Phase = "publishing"
	job.FinalizationRetryCount = 0
	job.NextFinalizationAt = time.Time{}
	job.OperatorRetryRequested = false
	job.Error, job.FailureClassification = "", ""
	job.JobTelemetry = telemetryFor(job, now)
	// Persist first, still hidden: a crash after this line leaves a finalizing
	// job in a hidden directory, which the sweep moves back.
	if err := SaveJobAtomic(filepath.Join(dir, "job.json"), job); err != nil {
		return refuse(fmt.Errorf("persisting publication request for job %q: %w", jobID, err))
	}
	if _, err := w.restoreRetainedLocked(dir, job); err != nil {
		// The state is durable; the sweep retries the move.
		return refuse(err)
	}
	lock.Unlock()
	locked = false
	return w.Status(ctx, jobID)
}

// releaseRefusedLocked reports whether Cancel must refuse a deferred job: a
// publication has been requested, so a runner may be uploading or the file may
// already be on the share. Releasing then would delete the only local copy
// while the outcome is unknown.
func (w *Worker) releaseRefusedLocked(job *JobRecord) bool {
	if !job.deferredPublication() {
		return false
	}
	switch FinalizationState(strings.TrimSpace(job.FinalizationState)) {
	case FinalizationStateFinalizing:
		return job.Status == "running" || job.Status == "queued"
	}
	return false
}

// cancelRetainedLocked cancels a retained candidate (a release): the local file
// is removed, the record becomes an ordinary cancelled job and the directory
// moves back to its regular name. The caller holds the job lock of dir.
func (w *Worker) cancelRetainedLocked(dir string, job *JobRecord) (string, error) {
	job.Status = "cancelled"
	job.FinishedAt = time.Now().UTC()
	job.Phase = "cancelled"
	job.ProgressIsStale = false
	job.NextFinalizationAt = time.Time{}
	// Not a state older workers know under the regular name.
	job.FinalizationState = string(FinalizationStatePending)
	if err := SaveJobAtomic(filepath.Join(dir, "job.json"), job); err != nil {
		return dir, err
	}
	if filepath.Clean(dir) == w.retainedDir(job.ID) {
		regular, err := w.restoreRetainedLocked(dir, job)
		if err != nil {
			return dir, err
		}
		dir = regular
	}
	return dir, nil
}

// SweepRetained runs the retained-candidate maintenance at most once a minute:
// it finishes interrupted retains and publish requests, expires candidates past
// retained_ttl_hours, drops old tombstones and collects stale empty directories. It
// never touches a job whose publication is running: such a job is not in a
// hidden directory.
func (w *Worker) SweepRetained(ctx context.Context) error {
	w.retainedSweepMu.Lock()
	if !w.retainedSweepAt.IsZero() && time.Since(w.retainedSweepAt) < retainedSweepInterval {
		w.retainedSweepMu.Unlock()
		return nil
	}
	w.retainedSweepAt = time.Now()
	w.retainedSweepMu.Unlock()
	return w.sweepRetained(ctx)
}

func (w *Worker) sweepRetained(ctx context.Context) error {
	stateDir := filepath.Clean(w.cfg.StateDir)
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading state dir for retained sweep: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		switch {
		case strings.HasPrefix(name, retainedDirPrefix):
			if err := w.sweepHiddenDir(ctx, name); err != nil {
				errs = append(errs, err)
			}
		case !strings.HasPrefix(name, "."):
			if err := w.sweepRegularDir(name); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// sweepRegularDir recovers a deferred job whose runner stopped after the encode
// checkpoint but before its directory was hidden, and collects empty shells.
func (w *Worker) sweepRegularDir(name string) error {
	dir := filepath.Join(filepath.Clean(w.cfg.StateDir), name)
	jobFile := filepath.Join(dir, "job.json")
	ok, err := fileExists(jobFile)
	if err != nil {
		return err
	}
	if !ok {
		return w.collectShellDir(dir)
	}
	job, err := LoadJob(jobFile)
	if err != nil || job == nil || job.ID != name || !isEncodedAwaitingRetention(job) {
		return nil // not ours; other sweeps report corrupt records
	}
	lock, err := acquireExistingJobLock(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer lock.Unlock()
	job, err = LoadJob(jobFile)
	if err != nil || !isEncodedAwaitingRetention(job) {
		return nil
	}
	if job.PID > 0 && w.jobAlive(job) {
		return nil // the runner is still on its way to retain it
	}
	return w.completeRetentionLocked(dir, job)
}

// sweepHiddenDir acts on one hidden job directory under its job lock.
func (w *Worker) sweepHiddenDir(ctx context.Context, name string) error {
	id := strings.TrimPrefix(name, retainedDirPrefix)
	dir := filepath.Join(filepath.Clean(w.cfg.StateDir), name)
	jobFile := filepath.Join(dir, "job.json")
	if id == "" || validateJobID(id) != nil {
		return nil
	}
	ok, err := fileExists(jobFile)
	if err != nil {
		return err
	}
	if !ok {
		return w.collectShellDir(dir)
	}
	lock, err := acquireExistingJobLock(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer lock.Unlock()
	job, err := LoadJob(jobFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("loading retained job %s: %w", name, err)
	}
	if job == nil || job.ID != id {
		return fmt.Errorf("retained job %s: id does not match directory (fail closed)", name)
	}
	now := time.Now().UTC()
	switch {
	case job.Status == StatusExpired:
		if !job.ExpiredAt.IsZero() && now.Sub(job.ExpiredAt) > expiredTombstoneTTL {
			// The candidate is long gone; only the record is removed.
			return os.RemoveAll(dir)
		}
	case job.Status == "cancelled", job.Status == "completed", job.Status == "failed":
		// A release or publication interrupted between saving and moving back.
		_, err := w.restoreRetainedLocked(dir, job)
		return err
	case !job.deferredPublication():
		return nil
	case job.Status != "running":
		return nil
	}
	if job.Status != "running" {
		return nil
	}
	switch FinalizationState(strings.TrimSpace(job.FinalizationState)) {
	case FinalizationStateFinalizing:
		// A publish request that was saved but not yet moved back.
		_, err := w.restoreRetainedLocked(dir, job)
		return err
	case FinalizationStatePending:
		if job.EncodeComplete {
			return w.completeRetentionLocked(dir, job)
		}
	case FinalizationStateRetained:
		if !job.RetainedAt.IsZero() && now.Sub(job.RetainedAt) > w.retainedTTL() {
			return w.expireRetainedLocked(dir, job, now)
		}
	}
	return nil
}

// expireRetainedLocked discards an unpublished candidate and leaves a tombstone
// so the job answers "expired" instead of 404. The caller holds the job lock.
func (w *Worker) expireRetainedLocked(dir string, job *JobRecord, now time.Time) error {
	w.cleanupCancelledArtifacts(job)
	job.Status = StatusExpired
	job.FinalizationState = string(FinalizationStateExpired)
	job.Phase = StatusExpired
	job.PID, job.ProcessStartTime = 0, ""
	job.ExpiredAt = now
	job.FinishedAt = now
	job.ReasonCode = "retained_expired"
	job.Error = fmt.Sprintf("retained candidate expired after %s without being published", w.retainedTTL())
	job.NextFinalizationAt = time.Time{}
	job.ProgressIsStale = false
	job.JobTelemetry = telemetryFor(job, now)
	return SaveJobAtomic(filepath.Join(dir, "job.json"), job)
}

// collectShellDir removes an old directory that holds nothing but a lock file:
// the leftover of a job directory created by a lock acquisition that raced a
// move, or of a submit that failed before saving. Anything else is left alone.
func (w *Worker) collectShellDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if time.Since(fi.ModTime()) < shellDirGrace {
		return nil
	}
	lock, err := acquireExistingJobLock(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	// A submit that wins the race for the lock saves its record before it
	// unlocks, so a record found now means the directory is in use.
	if ok, _ := fileExists(filepath.Join(dir, "job.json")); ok {
		lock.Unlock()
		return nil
	}
	err = removeShellDir(dir)
	lock.Unlock()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil // occupied by something else: not a shell
	}
	return nil
}

// findRetainedLocked finds a live retained candidate by job id or idempotency
// key so a repeated submit answers it instead of encoding again. Tombstones are
// ignored: an expired job is encoded again. Callers hold the capacity lock.
func (w *Worker) findRetainedLocked(jobID, key string) (*JobRecord, error) {
	entries, err := os.ReadDir(w.cfg.StateDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !strings.HasPrefix(name, retainedDirPrefix) {
			continue
		}
		id := strings.TrimPrefix(name, retainedDirPrefix)
		jobFile := filepath.Join(w.cfg.StateDir, name, "job.json")
		ok, err := fileExists(jobFile)
		if err != nil {
			return nil, fmt.Errorf("statting retained job record %s (fail closed): %w", jobFile, err)
		}
		if !ok {
			continue
		}
		job, err := LoadJob(jobFile)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("loading retained job record %s (fail closed): %w", jobFile, err)
		}
		if job == nil || job.ID != id {
			return nil, fmt.Errorf("inconsistent retained job record %s (fail closed): id mismatch", jobFile)
		}
		if job.Status != "running" {
			continue
		}
		stored := strings.TrimSpace(job.IdempotencyKey)
		if id != jobID && stored != key && !(stored == "" && id == key) {
			continue
		}
		cp := *job
		return &cp, nil
	}
	return nil, nil
}

// retainedBytesLocked sums the disk held or reserved by deferred jobs that have
// not finished: encoding ones by their admission reservation, finished ones by
// their attested size. Callers hold the capacity lock.
func (w *Worker) retainedBytesLocked() (int64, error) {
	entries, err := os.ReadDir(w.cfg.StateDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			continue
		}
		var id string
		switch {
		case strings.HasPrefix(name, retainedDirPrefix):
			id = strings.TrimPrefix(name, retainedDirPrefix)
		case strings.HasPrefix(name, "."):
			continue
		default:
			id = name
		}
		job, err := LoadJob(filepath.Join(w.cfg.StateDir, name, "job.json"))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return 0, fmt.Errorf("loading %s to measure retained candidates (fail closed): %w", name, err)
		}
		if job == nil || job.ID != id || !job.deferredPublication() || (job.Status != "queued" && job.Status != "running") {
			continue
		}
		if job.CandidateSizeBytes > 0 {
			total += job.CandidateSizeBytes
		} else {
			total += job.RetainReserveBytes
		}
	}
	return total, nil
}

// freeDiskBytes measures the free space of the filesystem holding the local
// work directory (or its nearest existing parent).
func (w *Worker) freeDiskBytes() (int64, error) {
	dir := w.localWorkDir()
	if w.freeDisk != nil {
		return w.freeDisk(dir)
	}
	probe := dir
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	avail, _, ok := diskSpace(probe)
	if !ok {
		return 0, fmt.Errorf("free disk space of %s is unknown", probe)
	}
	return int64(avail), nil
}

// admitDeferredLocked applies the retention limits to a new on_approval job
// whose output is expected to take at most expected bytes. Past a limit it
// answers the existing "worker busy" error, which the API maps to 429
// worker_busy; a measurement that fails refuses the job rather than guessing.
// Callers hold the capacity lock.
func (w *Worker) admitDeferredLocked(expected int64) error {
	if limit := w.cfg.RetainedCandidatesMaxBytes; limit > 0 {
		held, err := w.retainedBytesLocked()
		if err != nil {
			return fmt.Errorf("measuring retained candidates: %w", err)
		}
		if held+expected > limit {
			return fmt.Errorf("worker busy: retained candidates would use %d of %d allowed bytes", held+expected, limit)
		}
	}
	if floor := w.cfg.MinFreeDiskBytes; floor > 0 {
		free, err := w.freeDiskBytes()
		if err != nil {
			return fmt.Errorf("measuring free disk: %w", err)
		}
		if free-expected < floor {
			return fmt.Errorf("worker busy: %d free bytes would fall below the %d byte minimum", free-expected, floor)
		}
	}
	return nil
}
