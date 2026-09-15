package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file implements the Phase 6B2 operational execution state machine:
// staging (Source -> StagedInputPath), encode to the operational LocalCandidate,
// an EncodeComplete checkpoint, and output finalization (LocalCandidate ->
// IntendedDestination). Semantic Source/Candidate/PlanDigest and the
// execution-spec digest are never touched here.
//
// Invariants:
//   - Stored operational metadata is authoritative. Nothing is recomputed from
//     the current worker config on restart; blanks are legacy local->local.
//   - Unknown nonblank operational states fail closed before mutations.
//   - After EncodeComplete is persisted, ffmpeg is never invoked again, even
//     across crashes; only finalization may resume.
//   - Cancellation always wins between phases and before publish.

// Failure classification for an operational storage/finalization failure that
// happens after encoding completed. It is deliberately distinct from
// "source_corrupt" and "runner_killed" so a resumable finalization problem is
// never mistaken for a corrupt source or a killed encoder.
const FailureStorageFinalization = "storage_finalization_failed"

// resolvedOperational is the executable interpretation of a job's durable
// operational metadata. It is derived only from the persisted record.
type resolvedOperational struct {
	staging        StagingState
	finalization   FinalizationState
	effectiveInput string
	stagedInput    string
	localCandidate string
	destination    string
	partial        string
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// resolveOperationalForExecution interprets a persisted job's operational
// metadata into concrete paths/states. It never consults worker config, never
// mutates the record, and fails closed on unknown nonblank states or missing
// required paths. Blank/zero fields are treated as the legacy local->local
// layout: EffectiveInput=Source, LocalCandidate=Candidate,
// Destination=Candidate.
func resolveOperationalForExecution(job *JobRecord) (*resolvedOperational, error) {
	if job == nil {
		return nil, fmt.Errorf("nil job record (fail closed)")
	}
	if strings.TrimSpace(job.ID) == "" {
		return nil, fmt.Errorf("blank job id (fail closed)")
	}
	if err := ValidateStagingState(job.StagingState); err != nil {
		return nil, err
	}
	if err := ValidateFinalizationState(job.FinalizationState); err != nil {
		return nil, err
	}

	r := &resolvedOperational{}

	switch StagingState(strings.TrimSpace(job.StagingState)) {
	case "", StagingStateNotRequired:
		r.staging = StagingStateNotRequired
		r.effectiveInput = firstNonBlank(job.EffectiveInputPath, job.Source)
	case StagingStatePending, StagingStateStaging, StagingStateReady:
		r.staging = StagingState(strings.TrimSpace(job.StagingState))
		r.stagedInput = strings.TrimSpace(job.StagedInputPath)
		if r.stagedInput == "" {
			return nil, fmt.Errorf("%w: staged input path is required for staging state %q (fail closed)", ErrInvalidStagingState, job.StagingState)
		}
		r.effectiveInput = firstNonBlank(job.EffectiveInputPath, r.stagedInput)
	default:
		return nil, fmt.Errorf("%w: %q (fail closed)", ErrInvalidStagingState, job.StagingState)
	}
	if strings.TrimSpace(r.effectiveInput) == "" {
		return nil, fmt.Errorf("%w: effective input path is required (fail closed)", ErrInvalidStagingState)
	}

	switch FinalizationState(strings.TrimSpace(job.FinalizationState)) {
	case "", FinalizationStateNotRequired:
		r.finalization = FinalizationStateNotRequired
		r.localCandidate = firstNonBlank(job.LocalCandidatePath, job.Candidate)
	case FinalizationStatePending, FinalizationStateFinalizing, FinalizationStateCompleted:
		r.finalization = FinalizationState(strings.TrimSpace(job.FinalizationState))
		r.localCandidate = strings.TrimSpace(job.LocalCandidatePath)
		if r.localCandidate == "" {
			return nil, fmt.Errorf("%w: local candidate path is required for finalization state %q (fail closed)", ErrInvalidFinalizationState, job.FinalizationState)
		}
		r.destination = strings.TrimSpace(job.Destination())
		if r.destination == "" {
			return nil, fmt.Errorf("%w: intended destination is required for finalization state %q (fail closed)", ErrInvalidFinalizationState, job.FinalizationState)
		}
		wantPartial := PartialPathFor(r.destination, job.ID)
		if stored := strings.TrimSpace(job.PartialPath); stored != "" && stored != wantPartial {
			return nil, fmt.Errorf("%w: partial path %q does not match %q (fail closed)", ErrInvalidFinalizationState, stored, wantPartial)
		}
		r.partial = wantPartial
	default:
		return nil, fmt.Errorf("%w: %q (fail closed)", ErrInvalidFinalizationState, job.FinalizationState)
	}
	if strings.TrimSpace(r.localCandidate) == "" {
		return nil, fmt.Errorf("local candidate path is required (fail closed)")
	}
	return r, nil
}

// executeOperational drives a job from (unstaged) input through encode to the
// EncodeComplete checkpoint, then hands off to finalization.
func (w *Worker) executeOperational(ctx context.Context, jobDir, jobFile string, job *JobRecord, r *resolvedOperational) error {
	if err := w.ensureExternalHealthy(ctx, job.Source); err != nil {
		return w.failJobTerminal(jobDir, jobFile, job, fmt.Errorf("staging source storage unhealthy: %w", err))
	}

	if cancelled, err := w.ensureStaged(ctx, jobDir, jobFile, job, r); err != nil {
		return w.failJobTerminal(jobDir, jobFile, job, err)
	} else if cancelled {
		return nil
	}

	// Ensure plan is resolved
	if job.Plan == nil {
		plan, err := ResolveWorkerPlan(job.Profile, nil)
		if err != nil {
			return w.failJobTerminal(jobDir, jobFile, job, fmt.Errorf("resolving plan: %w", err))
		}
		job.Plan = plan
	}

	// Probe the operational input (staged when staging applies), never the
	// semantic source when a staged copy exists.
	streams, dur, probeErr := w.probeSourceForJob(ctx, r.effectiveInput)
	if dur > 0 {
		job.DurationSec = dur
	}
	if probeErr != nil {
		return w.failJobTerminal(jobDir, jobFile, job, fmt.Errorf("probing source streams: %w", probeErr))
	}

	execPlan, planErr := BuildExecutionPlan(job.Plan, streams, job.DurationSec)
	if planErr != nil {
		return w.failJobTerminal(jobDir, jobFile, job, fmt.Errorf("building execution plan: %w", planErr))
	}
	job.Conversions = execPlan.Conversions

	if cancelled, err := w.persistOperationalProgress(jobDir, jobFile, job, func(l *JobRecord) {
		l.DurationSec = job.DurationSec
		l.Conversions = job.Conversions
	}); err != nil {
		return err
	} else if cancelled {
		return nil
	}

	progressPath := filepath.Join(jobDir, "progress.txt")
	logPath := filepath.Join(jobDir, "ffmpeg.log")

	// Encode operational input -> local candidate. Semantic Source/Candidate
	// remain unchanged in the persisted record.
	ffmpegErr := w.runOperationalFFmpeg(ctx, execPlan, job, r.effectiveInput, r.localCandidate, progressPath, logPath)
	if ffmpegErr != nil {
		return w.failJobTerminal(jobDir, jobFile, job, ffmpegErr)
	}

	if err := validateEncodedCandidate(r.localCandidate); err != nil {
		return w.failJobTerminal(jobDir, jobFile, job, err)
	}

	// Checkpoint: durably record that encoding finished BEFORE any
	// finalization. A crash after this point never reruns ffmpeg. Status stays
	// nonterminal.
	if cancelled, err := w.persistOperationalProgress(jobDir, jobFile, job, func(l *JobRecord) {
		l.EncodeComplete = true
		l.LocalCandidatePath = r.localCandidate
		if strings.TrimSpace(r.destination) != "" {
			l.IntendedDestination = r.destination
		}
	}); err != nil {
		return err
	} else if cancelled {
		return nil
	}

	if w.afterEncodeCheckpoint != nil {
		w.afterEncodeCheckpoint(jobDir, job)
	}

	return w.finalizeOperational(ctx, jobDir, jobFile, job, r)
}

// ensureStaged materializes the staged input for staging states pending/staging
// and reuses a ready staged artifact without recopying. It persists each state
// transition and honors cancellation. A staged file already present at a
// pending/staging state is treated as already staged (idempotent recovery).
func (w *Worker) ensureStaged(ctx context.Context, jobDir, jobFile string, job *JobRecord, r *resolvedOperational) (bool, error) {
	switch r.staging {
	case StagingStateNotRequired:
		return false, nil
	case StagingStateReady:
		if _, err := stagedInputReady(r.stagedInput); err != nil {
			return false, err
		}
		return false, nil
	}

	// pending or staging
	if ready, err := stagedInputReady(r.stagedInput); err != nil {
		return false, err
	} else if !ready {
		if cancelled, err := w.persistOperationalProgress(jobDir, jobFile, job, func(l *JobRecord) {
			l.StagingState = string(StagingStateStaging)
		}); err != nil {
			return false, err
		} else if cancelled {
			return true, nil
		}
		if err := StageInputAtomic(ctx, job.Source, r.stagedInput); err != nil {
			return false, err
		}
	}

	cancelled, err := w.persistOperationalProgress(jobDir, jobFile, job, func(l *JobRecord) {
		l.StagingState = string(StagingStateReady)
		l.StagedInputPath = r.stagedInput
		l.EffectiveInputPath = r.effectiveInput
	})
	if err != nil {
		return false, err
	}
	return cancelled, nil
}

// finalizeOperational publishes the local candidate to the intended
// destination when required, then completes the job. A finalization or storage
// failure after EncodeComplete leaves the job nonterminal/resumable with a
// distinct classification and never reruns encode.
func (w *Worker) finalizeOperational(ctx context.Context, jobDir, jobFile string, job *JobRecord, r *resolvedOperational) error {
	switch r.finalization {
	case FinalizationStateNotRequired, FinalizationStateCompleted:
		return w.completeOperationalJob(jobDir, jobFile, job, r)
	}

	if err := w.ensureExternalHealthy(ctx, r.destination); err != nil {
		return w.recordFinalizationFailure(jobDir, jobFile, job, err)
	}

	cancelled, err := w.persistOperationalProgress(jobDir, jobFile, job, func(l *JobRecord) {
		l.EncodeComplete = true
		l.FinalizationState = string(FinalizationStateFinalizing)
	})
	if err != nil {
		return err
	} else if cancelled {
		return nil
	}

	// Cancellation wins before publish.
	if isCancelled, err := w.jobCancelled(jobDir, jobFile); err != nil {
		return err
	} else if isCancelled {
		return nil
	}

	finalize := w.finalizeOutput
	if finalize == nil {
		finalize = FinalizeOutputAtomic
	}
	if ferr := finalize(ctx, r.localCandidate, r.destination, job.ID); ferr != nil {
		if ctx.Err() != nil {
			// Runner is shutting down mid-finalize: leave the job resumable
			// without recording a spurious finalization failure.
			return ctx.Err()
		}
		return w.recordFinalizationFailure(jobDir, jobFile, job, ferr)
	}

	cancelled, err = w.persistOperationalProgress(jobDir, jobFile, job, func(l *JobRecord) {
		l.FinalizationState = string(FinalizationStateCompleted)
	})
	if err != nil {
		return err
	} else if cancelled {
		return nil
	}
	return w.completeOperationalJob(jobDir, jobFile, job, r)
}

// completeOperationalJob performs the normal terminal completion flow and then
// cleans up only worker-owned local artifacts.
func (w *Worker) completeOperationalJob(jobDir, jobFile string, job *JobRecord, r *resolvedOperational) error {
	job.Status = "completed"
	job.FinishedAt = time.Now().UTC()
	job.ExitCode = 0
	job.Error = ""
	job.FailureClassification = ""
	if perr := w.persistTerminalJob(jobDir, jobFile, job); perr != nil {
		return perr
	}
	w.cleanupOperationalArtifacts(job, r)
	return nil
}

// recordFinalizationFailure leaves the job nonterminal and resumable after a
// finalization/storage failure, preserving the local candidate and staged
// artifacts. The runner identity is cleared so a startup resume can spawn
// finalization again.
func (w *Worker) recordFinalizationFailure(jobDir, jobFile string, job *JobRecord, cause error) error {
	cancelled, err := w.persistOperationalProgress(jobDir, jobFile, job, func(l *JobRecord) {
		l.Status = "running"
		l.PID = 0
		l.ProcessStartTime = ""
		l.EncodeComplete = true
		if !IsPostEncodeFinalizationPending(l) {
			l.FinalizationState = string(FinalizationStatePending)
		}
		l.FailureClassification = FailureStorageFinalization
		l.Error = fmt.Sprintf("finalization failed: %v", cause)
		l.FinishedAt = time.Time{}
		l.ExitCode = 0
	})
	if err != nil {
		return err
	}
	if cancelled {
		return nil
	}
	return fmt.Errorf("finalization failed for job %s: %w", job.ID, cause)
}

// failJobTerminal persists a terminal failed transition (cancellation still
// wins inside persistTerminalJob) and returns the cause.
func (w *Worker) failJobTerminal(jobDir, jobFile string, job *JobRecord, cause error) error {
	job.Status = "failed"
	job.FinishedAt = time.Now().UTC()
	job.ExitCode = 1
	job.Error = cause.Error()
	if perr := w.persistTerminalJob(jobDir, jobFile, job); perr != nil {
		return perr
	}
	return cause
}

// persistOperationalProgress reloads the durable record under the per-job lock,
// applies mutate, and saves. Cancellation wins: a concurrently cancelled record
// is never overwritten and the caller receives cancelled=true. On success the
// latest durable record is copied back into job.
func (w *Worker) persistOperationalProgress(jobDir, jobFile string, job *JobRecord, mutate func(*JobRecord)) (cancelled bool, err error) {
	jobLock, lerr := acquireJobLock(jobDir)
	if lerr != nil {
		return false, fmt.Errorf("acquiring job lock for operational progress: %w", lerr)
	}
	defer jobLock.Unlock()

	latest, lerr := LoadJob(jobFile)
	if lerr != nil {
		return false, fmt.Errorf("reloading job for operational progress: %w", lerr)
	}
	if latest == nil {
		return false, fmt.Errorf("reloading job for operational progress: nil record")
	}
	if latest.Status == "cancelled" {
		return true, nil
	}
	mutate(latest)
	if serr := SaveJobAtomic(jobFile, latest); serr != nil {
		return false, fmt.Errorf("saving operational progress for job %q: %w", latest.ID, serr)
	}
	*job = *latest
	return false, nil
}

// jobCancelled reports whether the durable record is cancelled.
func (w *Worker) jobCancelled(jobDir, jobFile string) (bool, error) {
	jobLock, err := acquireJobLock(jobDir)
	if err != nil {
		return false, fmt.Errorf("acquiring job lock for cancellation check: %w", err)
	}
	defer jobLock.Unlock()
	latest, err := LoadJob(jobFile)
	if err != nil {
		return false, fmt.Errorf("reloading job for cancellation check: %w", err)
	}
	return latest != nil && latest.Status == "cancelled", nil
}

// cleanupOperationalArtifacts removes only worker-owned local artifacts after a
// fully successful completion: the staged input and a local candidate distinct
// from both the semantic candidate and the intended destination. It never
// touches the semantic source or the final destination.
func (w *Worker) cleanupOperationalArtifacts(job *JobRecord, r *resolvedOperational) {
	if r == nil || job == nil {
		return
	}
	if s := strings.TrimSpace(r.stagedInput); s != "" && s != job.Source && s != job.Candidate && s != r.destination {
		_ = os.Remove(s)
	}
	if c := strings.TrimSpace(r.localCandidate); c != "" && c != job.Source && c != job.Candidate && c != r.destination {
		_ = os.Remove(c)
	}
}

// ensureExternalHealthy drives the external storage root containing path to
// healthy through the existing lease primitive when one is configured. It is a
// no-op for local paths or when no lease manager is set ("where applicable").
func (w *Worker) ensureExternalHealthy(ctx context.Context, path string) error {
	if w.leaseManager == nil || strings.TrimSpace(path) == "" {
		return nil
	}
	if !IsExternalPath(path, w.cfg.ExternalRoots) {
		return nil
	}
	root, ok := externalRootFor(path, w.cfg.ExternalRoots)
	if !ok {
		return nil
	}
	_, err := w.leaseManager.EnsureHealthy(ctx, root)
	return err
}

// externalRootFor returns the configured external root containing path.
func externalRootFor(path string, roots []string) (string, bool) {
	clean := filepath.Clean(path)
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		cleanRoot := filepath.Clean(root)
		if clean == cleanRoot {
			return cleanRoot, true
		}
		rel, err := filepath.Rel(cleanRoot, clean)
		if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return cleanRoot, true
	}
	return "", false
}

// isPostEncodeResume reports whether a persisted job must resume ONLY output
// finalization: encoding completed, the runner is gone, and the intended
// destination still awaits finalization.
func isPostEncodeResume(job *JobRecord) bool {
	if job == nil || job.Status != "running" || job.PID != 0 || !job.EncodeComplete {
		return false
	}
	return IsPostEncodeFinalizationPending(job)
}

// ResumePostEncode is the explicit Phase 6B2 restart resume mechanism. It scans
// durable jobs for running PID-0 records whose encoding already completed and
// whose destination still awaits finalization, and spawns one detached runner
// per such job. It NEVER schedules queued jobs (the queued scheduler is
// unaffected) and never runs ffmpeg: the spawned runner resumes finalization
// only. Returns the number of runners started.
//
// Meaningful failures for candidate job records (lock/load/ID mismatch/spawn/
// save) are surfaced as a contextual joined error while the scan continues so
// one bad record cannot strand the others. Directories without job.json
// (benchmark-only/empty) and jobs that are not post-encode candidates are
// skipped safely.
//
// A spawned runner's identity is stamped only when the record is still
// unclaimed: the child may update job.json first (claiming the job or even
// completing finalization), and the parent must never overwrite that with its
// stale pre-spawn snapshot. A runner whose identity cannot be persisted is
// killed and the persistence failure is surfaced.
func (w *Worker) ResumePostEncode(ctx context.Context, selfExe, configPath string) (int, error) {
	_ = ctx
	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	capLock, err := acquireCapacityLock(cleanStateDir)
	if err != nil {
		return 0, fmt.Errorf("acquiring capacity lock for post-encode resume: %w", err)
	}
	defer capLock.Unlock()

	entries, err := os.ReadDir(w.cfg.StateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading state dir for post-encode resume: %w", err)
	}

	started := 0
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		jobDir := filepath.Join(w.cfg.StateDir, name)
		jobFile := filepath.Join(jobDir, "job.json")
		if _, serr := os.Stat(jobFile); serr != nil {
			if os.IsNotExist(serr) {
				continue // benchmark-only / empty / non-job dir
			}
			errs = append(errs, fmt.Errorf("statting job file %s for post-encode resume: %w", jobFile, serr))
			continue
		}
		jobLock, lerr := acquireJobLock(jobDir)
		if lerr != nil {
			errs = append(errs, fmt.Errorf("acquiring job lock for post-encode resume of %s: %w", name, lerr))
			continue
		}
		func() {
			defer jobLock.Unlock()
			job, lerr := LoadJob(jobFile)
			if lerr != nil {
				errs = append(errs, fmt.Errorf("loading job %s for post-encode resume: %w", name, lerr))
				return
			}
			if job == nil {
				errs = append(errs, fmt.Errorf("loading job %s for post-encode resume: nil record (fail closed)", name))
				return
			}
			if job.ID != name {
				errs = append(errs, fmt.Errorf("post-encode resume job %s: id %q does not match directory (fail closed)", name, job.ID))
				return
			}
			if !isPostEncodeResume(job) {
				return // not a post-encode candidate: safely skipped
			}
			pid, lstart, perr := w.spawnTranscodeProcess(selfExe, configPath, job.ID)
			if perr != nil {
				errs = append(errs, fmt.Errorf("spawning post-encode resume runner for job %s: %w", job.ID, perr))
				return
			}
			// Re-read after spawn: the child may already have claimed the job
			// (set PID/running) or progressed further. Never overwrite child
			// state with the parent's stale pre-spawn snapshot.
			latest, lerr := LoadJob(jobFile)
			if lerr != nil {
				killTranscodeProcess(pid)
				errs = append(errs, fmt.Errorf("reloading job %s after spawn for post-encode resume: %w", job.ID, lerr))
				return
			}
			if latest == nil || latest.ID != job.ID {
				killTranscodeProcess(pid)
				errs = append(errs, fmt.Errorf("post-encode resume job %s: inconsistent record after spawn (fail closed)", job.ID))
				return
			}
			if latest.PID != 0 || !isPostEncodeResume(latest) {
				// The child already owns/advanced the record; leave it alone.
				started++
				return
			}
			latest.PID = pid
			latest.ProcessStartTime = lstart
			if strings.TrimSpace(latest.Status) == "" {
				latest.Status = "running"
			}
			if serr := SaveJobAtomic(jobFile, latest); serr != nil {
				killTranscodeProcess(pid)
				errs = append(errs, fmt.Errorf("persisting post-encode resume runner identity for job %s: %w", job.ID, serr))
				return
			}
			started++
		}()
	}
	return started, errors.Join(errs...)
}

// probeSourceForJob uses the injected probe when present.
func (w *Worker) probeSourceForJob(ctx context.Context, path string) ([]SourceStream, float64, error) {
	if w.probeSource != nil {
		return w.probeSource(ctx, path)
	}
	return ProbeSourceStreams(ctx, w.ffprobePath, path)
}

// runOperationalFFmpeg uses the injected encoder when present, else the
// production path with explicit operational input/output.
func (w *Worker) runOperationalFFmpeg(ctx context.Context, execPlan *ExecutionPlan, job *JobRecord, inputPath, outputPath, progressPath, logPath string) error {
	if w.runFFmpeg != nil {
		return w.runFFmpeg(ctx, execPlan, job, inputPath, outputPath, progressPath, logPath)
	}
	return RunFFmpegPaths(ctx, w.ffmpegPath, execPlan, inputPath, outputPath, progressPath, logPath)
}

// stagedInputReady reports whether the staged input already exists as a
// non-empty regular file. Other failures fail closed.
func stagedInputReady(path string) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, fmt.Errorf("%w: empty staged input path", ErrDestinationInvalid)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("%w: statting staged input %s: %v", ErrStorageIO, path, err)
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("%w: staged input %s is not a regular file", ErrStorageIO, path)
	}
	if fi.Size() == 0 {
		return false, fmt.Errorf("%w: staged input %s is empty", ErrStorageIO, path)
	}
	return true, nil
}

// validateEncodedCandidate requires a non-empty regular file at the encoder
// output path before the EncodeComplete checkpoint is persisted.
func validateEncodedCandidate(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: empty encoder output path", ErrSourceInvalid)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: encoder output %s is unavailable: %v", ErrStorageIO, path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: encoder output %s is not a regular file", ErrSourceInvalid, path)
	}
	if fi.Size() == 0 {
		return fmt.Errorf("%w: encoder output %s is empty", ErrSourceInvalid, path)
	}
	return nil
}
