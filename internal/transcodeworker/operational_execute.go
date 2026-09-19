package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/internal/smbdirect"
	"github.com/jakenesler/navigatorr/transcode/resilience"
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

// FailureStoragePublicationAmbiguous classifies a finalization failure where an
// exclusive-copy publication wrote and closed the destination but could not
// verify its post-close visibility (for example transient ENOENT on a macOS
// smbfs mount). The destination is preserved and the job may complete on a
// later resume only after full byte-for-byte content equality with the local
// candidate is established. It is distinct from FailureStorageFinalization so an
// ordinary (including destination-exists) failure is never silently upgraded to
// a successful publication.
const FailureStoragePublicationAmbiguous = "storage_publication_ambiguous"

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
		l.Phase = "encoding"
		l.EncodeStartedAt = time.Now().UTC()
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
	job.EncodeFinishedAt = time.Now().UTC()
	if ffmpegErr != nil {
		job.FailureClassification = string(resilience.ClassifyError(ffmpegErr))
		var classified interface{ FailureClass() string }
		if job.FailureClassification == string(resilience.FFmpegUnknown) && !errors.As(ffmpegErr, &classified) {
			job.FailureClassification = "worker_setup_failed"
		}
		return w.failJobTerminal(jobDir, jobFile, job, ffmpegErr)
	}
	if cancelled, err := w.persistOperationalProgress(jobDir, jobFile, job, func(l *JobRecord) {
		l.EncodeFinishedAt = job.EncodeFinishedAt
		l.Phase = "validating"
		l.ValidationStartedAt = time.Now().UTC()
	}); err != nil {
		return err
	} else if cancelled {
		return nil
	}

	if err := w.validateEncodedCandidateFull(ctx, r.localCandidate, execPlan, streams, job.DurationSec, 0); err != nil {
		// A bad local candidate must never be published. Remove only the
		// worker-owned local candidate; the semantic source and the shared
		// source cache are never touched. For local-only jobs the local
		// candidate IS the candidate path, and the bad file is still removed
		// because the job never completed (it is incomplete output, not a
		// finalized destination).
		if c := strings.TrimSpace(r.localCandidate); c != "" && !w.isCachePath(c) && c != job.Source {
			_ = os.Remove(c)
		}
		return w.failJobTerminal(jobDir, jobFile, job, err)
	}

	// Checkpoint: durably record that encoding finished BEFORE any
	// finalization. A crash after this point never reruns ffmpeg. Status stays
	// nonterminal.
	if cancelled, err := w.persistOperationalProgress(jobDir, jobFile, job, func(l *JobRecord) {
		l.EncodeComplete = true
		l.ValidationFinishedAt = time.Now().UTC()
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
//
// Transcode I/O optimization: when the shared source cache is enabled and holds
// a verified entry for the same immutable source identity, the per-job staged
// input is fulfilled with a purely local cache->staged copy (no NAS read).
// Otherwise the source is staged once from the NAS and the shared cache is
// populated best-effort from the staged copy, so a benchmark that ran first (or
// a later transcode of the same source) reuses it without a second full
// transfer. Cache misses, disabled caches, and local sources fall back to the
// legacy direct staging path exactly.
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
			l.Phase = "reading_source"
		}); err != nil {
			return false, err
		} else if cancelled {
			return true, nil
		}
		var err error
		if err := w.requireMediaBackend(job.Source); err != nil {
			return false, err
		}
		stagedFromCache := false
		if w.sourceCacheEnabled() {
			if fi, serr := w.statSourceForCache(ctx, job.Source); serr == nil {
				if hit, ok := lookupSourceCache(w.sourceCacheDir(), filepath.Clean(job.Source), fi); ok {
					if cerr := copyCacheToStaged(ctx, hit, r.stagedInput); cerr == nil {
						stagedFromCache = true
					} else if !IsDestinationExists(cerr) {
						// Fall through to direct staging on any non-trivial
						// cache-copy failure; a stale/corrupt cache entry is
						// never trusted over a fresh NAS read.
						stagedFromCache = false
					} else {
						stagedFromCache = true
					}
				}
			}
		}
		if !stagedFromCache {
			if w.mediaStore != nil && w.mediaStore.Maps(job.Source) {
				err = w.mediaStore.DownloadAtomic(ctx, job.Source, r.stagedInput)
			} else {
				err = StageInputAtomic(ctx, job.Source, r.stagedInput)
			}
			if err != nil {
				return false, err
			}
			// Best-effort: share this fresh NAS read with future jobs for the
			// same immutable source. Failures never fail the current job.
			if w.sourceCacheEnabled() {
				if fi, serr := w.statSourceForCache(ctx, job.Source); serr == nil {
					_, _ = populateSourceCacheFromLocal(ctx, w.sourceCacheDir(), filepath.Clean(job.Source), fi, r.stagedInput)
				}
			}
		}
	}

	cancelled, err := w.persistOperationalProgress(jobDir, jobFile, job, func(l *JobRecord) {
		l.StagingState = string(StagingStateReady)
		l.StagedInputPath = r.stagedInput
		l.EffectiveInputPath = r.effectiveInput
		l.Phase = "preparing"
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

	// Idempotent recovery of a prior ambiguous publication. It requires
	// FinalizationState == finalizing and EncodeComplete, plus durable proof that
	// the destination was created by THIS job's own O_EXCL publication:
	//
	//   - FailureStoragePublicationAmbiguous (new worker): the exclusive-copy
	//     path recorded the typed marker, so the destination is known to
	//     originate from this job's O_EXCL create; or
	//   - FailureStorageFinalization (old worker) whose job.Error is byte-for-byte
	//     the exact old ambiguous-publication error naming THIS job's partial and
	//     THIS destination, and whose exact own partial is still present and
	//     byte-for-byte identical to the local candidate (see
	//     proveLegacyAmbiguousPublication). The live E04 job has this shape.
	//
	// Without such proof the destination may be an unrelated file: completion is
	// still allowed only on full byte-for-byte equality, and a differing
	// destination is left untouched for the strict no-clobber finalizer to
	// report as ErrDestinationExists. Destructive recovery (removing a proven
	// own incomplete destination and republishing from the intact candidate) is
	// permitted only with proof.
	directSMB := w.mediaStore != nil && w.mediaStore.Maps(r.destination)
	if !directSMB && r.finalization == FinalizationStateFinalizing && job.EncodeComplete {
		eligible := job.FailureClassification == FailureStoragePublicationAmbiguous ||
			job.FailureClassification == FailureStorageFinalization ||
			finalizationClassRetryable(job.FailureClassification)
		if eligible {
			proven := job.FailureClassification == FailureStoragePublicationAmbiguous
			if !proven {
				p, perr := w.proveLegacyAmbiguousPublication(ctx, job, r)
				if perr != nil {
					return w.recordFinalizationFailure(jobDir, jobFile, job, perr)
				}
				proven = p
			}

			present, perr := pathExists(r.destination)
			if perr != nil {
				return w.recordFinalizationFailure(jobDir, jobFile, job, perr)
			}
			if present {
				equal, cerr := filesHaveEqualContent(ctx, r.localCandidate, r.destination)
				if cerr != nil {
					return w.recordFinalizationFailure(jobDir, jobFile, job, cerr)
				}
				if equal {
					w.removeOwnPartialIfSafe(job, r)
					cancelled, serr := w.persistOperationalProgress(jobDir, jobFile, job, func(l *JobRecord) {
						l.EncodeComplete = true
						l.FinalizationState = string(FinalizationStateCompleted)
					})
					if serr != nil {
						return serr
					}
					if cancelled {
						return nil
					}
					return w.completeOperationalJob(jobDir, jobFile, job, r)
				}
				if proven {
					// The destination is proven to be this job's own incomplete
					// publication: remove only it (collision-guarded) and fall
					// through to republish from the intact candidate.
					if _, rerr := w.removeAmbiguousDestinationIfSafe(job, r); rerr != nil {
						return w.recordFinalizationFailure(jobDir, jobFile, job, rerr)
					}
				}
			}
			// Destination absent (retry publication), removed (proven), or
			// differing without proof (conflict): drop any stale own partial and
			// fall through. The strict finalizer republishes or reports
			// ErrDestinationExists. Unrelated partials are never touched.
			w.removeOwnPartialIfSafe(job, r)
		}
	}

	cancelled, err := w.persistOperationalProgress(jobDir, jobFile, job, func(l *JobRecord) {
		l.EncodeComplete = true
		l.FinalizationState = string(FinalizationStateFinalizing)
		l.Phase = "publishing"
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

	var ferr error
	if directSMB {
		ferr = w.mediaStore.Publish(ctx, r.localCandidate, r.destination, job.ID)
	} else {
		finalize := w.finalizeOutput
		if finalize == nil {
			finalize = FinalizeOutputAtomic
		}
		ferr = finalize(ctx, r.localCandidate, r.destination, job.ID)
	}
	if ferr != nil {
		if ctx.Err() != nil {
			// Runner is shutting down mid-finalize: leave the job resumable
			// without recording a spurious finalization failure.
			return ctx.Err()
		}
		// The finalizer removes its own partial on ordinary failures, but an
		// early destination-exists precheck returns before its cleanup is set
		// up, so a stale own partial could otherwise survive and later be
		// mistaken for evidence of a publication attempt. Drop only this job's
		// exact partial (guarded); unrelated partials are never touched.
		if !directSMB {
			w.removeOwnPartialIfSafe(job, r)
		}
		return w.recordFinalizationFailure(jobDir, jobFile, job, ferr)
	}

	// Lightweight independent post-publish verification: the published object
	// must be the expected candidate (regular file, size-identical to the
	// accepted local candidate). This is stat-only and deliberately cheap after
	// the network copy; the expensive full structural/media validation already
	// ran locally before publish. Any mismatch fails closed and stays resumable
	// without marking the job complete.
	// Direct-SMB publication already performs its own verified publish
	// (hash-verified exclusive upload inside mediaStore.Publish), so no
	// additional filesystem stat applies to logical SMB destinations.
	if !directSMB {
		if verr := verifyPublishedCandidate(ctx, r.localCandidate, r.destination); verr != nil {
			w.removeOwnPartialIfSafe(job, r)
			return w.recordFinalizationFailure(jobDir, jobFile, job, verr)
		}
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
	job.NextFinalizationAt = time.Time{}
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
// finalization again. Only transient failures receive a bounded scheduled
// recovery. Permanent failures remain resumable without automatic respawns.
func (w *Worker) recordFinalizationFailure(jobDir, jobFile string, job *JobRecord, cause error) error {
	classification := FailureStorageFinalization
	if class := resilience.ClassifyError(cause); class != resilience.FFmpegUnknown && class != "" {
		classification = string(class)
	}
	switch {
	case errors.Is(cause, ErrDestinationExists), errors.Is(cause, smbdirect.ErrDestinationExists):
		classification = string(resilience.IdempotencyConflict)
	case errors.Is(cause, ErrAmbiguousPublication):
		classification = FailureStoragePublicationAmbiguous
	case classification == FailureStorageFinalization && errors.Is(cause, ErrStorageIO):
		classification = string(resilience.StorageIOError)
	}
	cancelled, err := w.persistOperationalProgress(jobDir, jobFile, job, func(l *JobRecord) {
		l.Status = "running"
		l.PID = 0
		l.ProcessStartTime = ""
		l.EncodeComplete = true
		if !IsPostEncodeFinalizationPending(l) {
			l.FinalizationState = string(FinalizationStatePending)
		}
		l.FailureClassification = classification
		l.Error = fmt.Sprintf("finalization failed: %v", cause)
		l.Phase = "publication_pending"
		l.NextFinalizationAt = time.Time{}
		if automaticFinalizationAllowed(l) {
			l.NextFinalizationAt = time.Now().UTC().Add(finalizationRetryDelay(l.FinalizationRetryCount))
		}
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
	if job.FailureClassification == "" {
		job.FailureClassification = string(resilience.ClassifyError(cause))
		if job.FailureClassification == string(resilience.FFmpegUnknown) {
			// The encoder path sets its own classification; an unrecognized
			// staging/probe/setup failure is never an FFmpeg execution failure.
			job.FailureClassification = "worker_setup_failed"
		}
	}
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
// touches the semantic source, the final destination, or shared source-cache
// entries (which are owned by the cache, not by any single job).
func (w *Worker) cleanupOperationalArtifacts(job *JobRecord, r *resolvedOperational) {
	if r == nil || job == nil {
		return
	}
	if s := strings.TrimSpace(r.stagedInput); s != "" && s != job.Source && s != job.Candidate && s != r.destination && !w.isCachePath(s) {
		_ = os.Remove(s)
	}
	if c := strings.TrimSpace(r.localCandidate); c != "" && c != job.Source && c != job.Candidate && c != r.destination && !w.isCachePath(c) {
		_ = os.Remove(c)
	}
}

// proveLegacyAmbiguousPublication reports whether job.Error is byte-for-byte the
// exact error an old worker persisted for THIS job's own partial and THIS
// destination, and whether this job's exact own partial is still present as a
// regular file byte-for-byte identical to the local candidate. Only that
// combination proves the existing destination was created by this job's prior
// O_EXCL publication and may therefore be removed for a safe republish.
//
// The match is exact (no substring heuristics): a generic storage error, a
// different partial, or a different destination never qualifies.
func (w *Worker) proveLegacyAmbiguousPublication(ctx context.Context, job *JobRecord, r *resolvedOperational) (bool, error) {
	if job == nil || r == nil {
		return false, nil
	}
	if job.FailureClassification != FailureStorageFinalization || !job.EncodeComplete {
		return false, nil
	}
	partial := strings.TrimSpace(r.partial)
	destination := strings.TrimSpace(r.destination)
	if partial == "" || destination == "" {
		return false, nil
	}
	if job.Error != legacyAmbiguousPublicationError(partial, destination) {
		return false, nil
	}

	info, err := os.Lstat(partial)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("%w: statting %s: %v", ErrStorageIO, partial, err)
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	equal, err := filesHaveEqualContent(ctx, r.localCandidate, partial)
	if err != nil {
		return false, err
	}
	return equal, nil
}

// legacyAmbiguousPublicationError reconstructs the exact error an old worker
// persisted when its exclusive-copy publication wrote and closed the
// destination but the post-close stat could not observe it. The old worker
// wrapped the failure as:
//
//	finalization failed: <storage io>: publishing <partial> to <destination>:
//	    <storage io>: statting <destination>: stat <destination>: <enoent>
//
// Matching this exact string (rather than a substring or a generic marker) is
// what proves the destination came from this job's own O_EXCL create.
func legacyAmbiguousPublicationError(partial, destination string) string {
	return fmt.Sprintf(
		"finalization failed: %s: publishing %s to %s: %s: statting %s: stat %s: %s",
		ErrStorageIO.Error(), partial, destination,
		ErrStorageIO.Error(), destination, destination,
		syscall.ENOENT.Error(),
	)
}

// removeAmbiguousDestinationIfSafe removes this job's proven own ambiguous
// destination so it can be republished. It refuses to touch a path that
// collides with the semantic source, the local candidate, or this job's exact
// partial, and refuses any non-regular destination. It never removes unrelated
// files.
//
// Note: for a finalized job the semantic Candidate normally equals the intended
// destination, so Candidate is only treated as a collision when it names a
// genuinely different path; otherwise no destination could ever be removed.
func (w *Worker) removeAmbiguousDestinationIfSafe(job *JobRecord, r *resolvedOperational) (bool, error) {
	if job == nil || r == nil {
		return false, nil
	}
	dest := strings.TrimSpace(r.destination)
	if dest == "" {
		return false, nil
	}
	clean := filepath.Clean(dest)
	protected := []string{job.Source, r.localCandidate, r.partial}
	if c := strings.TrimSpace(job.Candidate); c != "" && filepath.Clean(c) != clean {
		protected = append(protected, c)
	}
	for _, p := range protected {
		if pp := strings.TrimSpace(p); pp != "" && filepath.Clean(pp) == clean {
			return false, fmt.Errorf("%w: refusing to remove destination %s: collides with %s", ErrStorageIO, dest, pp)
		}
	}
	info, err := os.Lstat(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("%w: statting %s: %v", ErrStorageIO, dest, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%w: refusing to remove non-regular destination %s", ErrStorageIO, dest)
	}
	if err := os.Remove(dest); err != nil {
		return false, fmt.Errorf("%w: removing ambiguous destination %s: %v", ErrStorageIO, dest, err)
	}
	return true, nil
}

// removeOwnPartialIfSafe removes this job's exact finalization partial, if any,
// with the same path guards as cleanupOperationalArtifacts: it refuses to touch
// a path that collides with the semantic source, the semantic candidate, or the
// intended destination, so a partial can never delete original media or a
// finalized output. Unrelated partials are never targeted (the path is always
// PartialPathFor(destination, job.ID)). Removal is best effort.
func (w *Worker) removeOwnPartialIfSafe(job *JobRecord, r *resolvedOperational) {
	if job == nil || r == nil {
		return
	}
	p := strings.TrimSpace(r.partial)
	if p == "" {
		return
	}
	clean := filepath.Clean(p)
	for _, protected := range []string{job.Source, job.Candidate, r.destination} {
		if pp := strings.TrimSpace(protected); pp != "" && filepath.Clean(pp) == clean {
			return
		}
	}
	_ = os.Remove(p)
}

// ensureExternalHealthy drives the external storage root containing path to
// healthy through the existing lease primitive when one is configured. It is a
// no-op for local paths or when no lease manager is set ("where applicable").
func (w *Worker) ensureExternalHealthy(ctx context.Context, path string) error {
	if err := w.requireMediaBackend(path); err != nil {
		return err
	}
	if w.mediaStore != nil && w.mediaStore.Maps(path) {
		return nil
	}
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

// ResumePostEncode is the startup and periodic publication recovery sweep. It scans
// durable jobs for running PID-0 records whose encoding already completed and
// whose destination still awaits finalization, and spawns one detached runner
// per due transient job within its persisted retry budget. It NEVER schedules queued jobs (the queued scheduler is
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
	if err := ctx.Err(); err != nil {
		return 0, err
	}
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
	active, err := w.countActiveJobs("")
	if err != nil {
		return 0, fmt.Errorf("counting capacity for post-encode resume: %w", err)
	}
	maxParallel := w.cfg.MaxParallelJobs
	if maxParallel <= 0 {
		maxParallel = 1
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return started, errors.Join(append(errs, ctx.Err())...)
		}
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
			if active >= maxParallel || !automaticFinalizationAllowed(job) || time.Now().UTC().Before(job.NextFinalizationAt) {
				return
			}
			// Reserve an attempt before spawning so daemon crashes and fast
			// child failures cannot reset the budget or race an immediate retry.
			job.FinalizationRetryCount++
			job.NextFinalizationAt = time.Time{}
			if job.FinalizationRetryCount < MaxFinalizationRetries {
				job.NextFinalizationAt = time.Now().UTC().Add(finalizationRetryDelay(job.FinalizationRetryCount))
			}
			if serr := SaveJobAtomic(jobFile, job); serr != nil {
				errs = append(errs, fmt.Errorf("reserving post-encode retry for job %s: %w", job.ID, serr))
				return
			}
			pid, lstart, perr := w.spawnTranscodeProcess(selfExe, configPath, job.ID)
			if perr != nil {
				job.FailureClassification = failurePostEncodeRunnerUnavailable
				job.Error = fmt.Sprintf("post-encode resume runner unavailable: %v", perr)
				if job.FinalizationRetryCount >= MaxFinalizationRetries {
					job.NextFinalizationAt = time.Time{}
				}
				if serr := SaveJobAtomic(jobFile, job); serr != nil {
					errs = append(errs, fmt.Errorf("persisting failed post-encode spawn for job %s: %w", job.ID, serr))
				}
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
				active++
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
			active++
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
