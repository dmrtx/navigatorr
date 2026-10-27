package action

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// prepareRemoteRetry keeps transport recovery on the existing job identity.
// A new identity is allocated only after the worker confirms a terminal failure,
// otherwise an idempotent Submit would just return the same failed job forever.
func (e *Engine) prepareRemoteRetry(ctx context.Context, inst *store.ActionInstance, ec *ExecutionContext, tmpl ActionTemplate, resume int) (int, error) {
	if !e.workersConfigured() || inst.ActionName != "transcode_media" {
		return resume, nil
	}
	if jobID := getString(ec.State, "job_id"); jobID != "" && !getBool(ec.State, "transcode_done") {
		submit := actionStepIndex(tmpl, "submit_transcode")
		if submit < 0 {
			return resume, nil
		}
		e.recordWorkerPoll(ec)
		st, err := e.actionWorker(ec).Status(ctx, jobID)
		if err != nil {
			if isRetryableWorkerPollError(err) {
				ec.State["transcode_reconcile"] = true
				return submit, nil
			}
			if he, ok := transcodeHTTPError(err); ok && he.StatusCode == 404 {
				// A rejected admission leaves a coordinator identity but no worker
				// job. Reconcile/resubmit the same identity under the submit guard;
				// do not allocate another attempt or repeat a completed benchmark.
				ec.State["transcode_reconcile"] = true
				return submit, nil
			}
			return resume, fmt.Errorf("cannot retry before confirming remote job %s: %w", jobID, err)
		}
		e.observeTranscodeStatus(ec, st)
		if st.Status != transcode.StatusFailed && st.Status != transcode.StatusCancelled {
			// This includes completed: continue into validation of that candidate.
			ec.State["transcode_reconcile"] = true
			return submit, nil
		}
		class := st.FailureClassification
		if class == "" {
			class = string(resilience.Classify(st.Error))
		}
		if err := rejectUnchangedPermanentRetry(class); err != nil {
			return resume, err
		}
		// A manual retry that allocates a fresh identity is a new admission:
		// hold the parent guard across the identity mutation and its persistence,
		// then release it before execute so the submit step can re-admit without
		// deadlocking on a parent lease held here.
		lease, lerr := e.beginRetryAdmission(ctx, ec, "transcode retry")
		if lerr != nil {
			return resume, lerr
		}
		retry := getInt(ec.State, "manual_retry_count") + 1
		appendFailureHistory(ec, "manual_retry", class, fmt.Sprintf("confirmed terminal worker job %s; allocating attempt %d", jobID, retry))
		for _, key := range []string{"recovery_required", "worker_error", "finalization_retry_count", "next_finalization_at", "transcode_submitted", "transcode_reconcile", "transcode_done", "transcode_status", "transcode_phase", "candidate_path", "output_path", "idempotency_key", "worker_completed_at", "reconciled_at", "reconcile_lag_ms", "next_poll_at", "retry_not_before", workerBusyNotBeforeKey, "worker_busy", slotWaitLogKey, workerSlotWaitSinceKey, workerSlotPositionKey, workerSlotPolledAtKey, transcodeRejectedKey, "validation", "result", "original_intact", "original_integrity", "original_integrity_check", originalDigestConflictKey, "progress", "speed", "fps", "last_known_progress", "last_progress_at", "worker_heartbeat_at", "failure_classification", "queue_duration_ms", "encode_duration_ms", "validation_duration_ms", "worker_validation_duration_ms", "coordinator_validation_duration_ms", "worker_wall_duration_ms", candidateLocationKey, publishPolicyKey, publishPolicyFallbackKey, candidateRetainedAtKey, candidateRetainedUntilKey} {
			delete(ec.State, key)
			delete(ec.Outputs, key)
		}
		ec.State["previous_job_id"] = jobID
		ec.State["manual_retry_count"] = retry
		ec.State["job_id"] = fmt.Sprintf("job-%s-retry-%d", ec.InstanceID, retry)
		ec.State["external_reference"] = ec.State["job_id"]
		ec.State["attempt"], ec.State["retry_count"], ec.State["fallback_count"] = 1, 0, 0
		mergeMap(ec.Outputs, ec.State)
		if perr := e.persistExecutionState(lease.Context(ctx), ec); perr != nil {
			lease.Close()
			return resume, perr
		}
		lease.Close()
		return submit, nil
	}
	return resume, nil
}

// beginRetryAdmission acquires the narrow parent guard for a manual retry that
// is about to allocate a new remote identity. It must be closed before execute
// so the child's submit step can re-admit without a self parent_busy.
func (e *Engine) beginRetryAdmission(ctx context.Context, ec *ExecutionContext, phase string) (*admissionLease, error) {
	lease, blockedRes, handled := e.beginAdmission(ctx, ec, phase)
	if handled {
		return nil, blockedAdmissionError(phase, blockedRes)
	}
	return lease, nil
}

func blockedAdmissionError(phase string, res StepResult) error {
	if res.WaitingReason != "" {
		return fmt.Errorf("%s deferred: %s", phase, res.WaitingReason)
	}
	if res.Error != "" {
		return errors.New(res.Error)
	}
	return fmt.Errorf("%s blocked by parent policy", phase)
}

func actionStepIndex(tmpl ActionTemplate, name string) int {
	for i, step := range tmpl.Steps {
		if step.Name == name {
			return i
		}
	}
	return -1
}

func rejectUnchangedPermanentRetry(class string) error {
	switch strings.ToLower(class) {
	case "storage_permission_denied", "permission_denied", "smb_auth_failed", "source_changed", "ffmpeg_input_corrupt", "encoder_capability_unsupported", "codec_unsupported", "storage_full", "idempotency_conflict":
		return fmt.Errorf("retry blocked for %s: resolve the source, permissions, storage, or profile first, then create a new action", class)
	default:
		return nil
	}
}
