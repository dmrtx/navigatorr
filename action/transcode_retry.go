package action

import (
	"context"
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
	if e.deps.Transcode == nil || (inst.ActionName != "transcode_media" && inst.ActionName != "benchmark_transcode") {
		return resume, nil
	}
	if jobID := getString(ec.State, "job_id"); jobID != "" && !getBool(ec.State, "transcode_done") {
		submit := actionStepIndex(tmpl, "submit_transcode")
		if submit < 0 {
			return resume, nil
		}
		e.recordWorkerPoll(ec)
		st, err := e.deps.Transcode.Status(ctx, jobID)
		if err != nil {
			if isRetryableWorkerPollError(err) {
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
		retry := getInt(ec.State, "manual_retry_count") + 1
		appendFailureHistory(ec, "manual_retry", class, fmt.Sprintf("confirmed terminal worker job %s; allocating attempt %d", jobID, retry))
		for _, key := range []string{"recovery_required", "worker_error", "finalization_retry_count", "next_finalization_at", "transcode_submitted", "transcode_reconcile", "transcode_done", "transcode_status", "transcode_phase", "candidate_path", "output_path", "idempotency_key", "worker_completed_at", "reconciled_at", "reconcile_lag_ms", "next_poll_at", "retry_not_before", "validation", "result", "original_intact", "original_integrity", "progress", "speed", "fps", "last_known_progress", "last_progress_at", "worker_heartbeat_at", "failure_classification", "queue_duration_ms", "encode_duration_ms", "validation_duration_ms", "worker_validation_duration_ms", "coordinator_validation_duration_ms", "worker_wall_duration_ms"} {
			delete(ec.State, key)
			delete(ec.Outputs, key)
		}
		ec.State["previous_job_id"] = jobID
		ec.State["manual_retry_count"] = retry
		ec.State["job_id"] = fmt.Sprintf("job-%s-retry-%d", ec.InstanceID, retry)
		ec.State["external_reference"] = ec.State["job_id"]
		ec.State["attempt"], ec.State["retry_count"], ec.State["fallback_count"] = 1, 0, 0
		mergeMap(ec.Outputs, ec.State)
		return submit, nil
	}
	if jobID := getString(ec.State, "benchmark_job_id"); jobID != "" && !getBool(ec.State, "benchmark_done") {
		submit := actionStepIndex(tmpl, "submit_benchmark")
		if submit < 0 {
			return resume, nil
		}
		e.recordWorkerPoll(ec)
		st, err := e.deps.Transcode.BenchmarkStatus(ctx, jobID)
		if err != nil {
			if isRetryableWorkerPollError(err) {
				ec.State["benchmark_reconcile"] = true
				return submit, nil
			}
			return resume, fmt.Errorf("cannot retry before confirming remote benchmark %s: %w", jobID, err)
		}
		if st.Status != transcode.StatusFailed && st.Status != transcode.StatusCancelled {
			ec.State["benchmark_reconcile"] = true
			return submit, nil
		}
		class := string(resilience.Classify(st.Error))
		if err := rejectUnchangedPermanentRetry(class); err != nil {
			return resume, err
		}
		retry := getInt(ec.State, "benchmark_manual_retry_count") + 1
		for _, key := range []string{"benchmark_submitted", "benchmark_reconcile", "benchmark_done", "benchmark_status", "benchmark_decision", "benchmark_request", "benchmark_request_digest", "benchmark_retry_not_before", "next_poll_at", "failure_classification"} {
			delete(ec.State, key)
			delete(ec.Outputs, key)
		}
		ec.State["previous_benchmark_job_id"] = jobID
		ec.State["benchmark_manual_retry_count"] = retry
		ec.State["benchmark_job_id"] = fmt.Sprintf("bench-%s-retry-%d", ec.InstanceID, retry)
		ec.State["benchmark_attempt"], ec.State["benchmark_retry_count"] = 1, 0
		mergeMap(ec.Outputs, ec.State)
		return submit, nil
	}
	return resume, nil
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
