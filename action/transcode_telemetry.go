package action

import (
	"encoding/json"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

func (e *Engine) observeTranscodeStatus(ec *ExecutionContext, st transcode.JobStatus) {
	mirrorTranscodeWorkerMetadata(ec, st)
	if st.QualityEvidence != nil {
		ec.State["quality_evidence"] = st.QualityEvidence
		ec.Outputs["quality_evidence"] = st.QualityEvidence
	} else {
		delete(ec.State, "quality_evidence")
		delete(ec.Outputs, "quality_evidence")
	}
	// Cheap independent identity metadata for lightweight post-publish
	// verification: the accepted local candidate's size and content digest,
	// attested by the worker before publish.
	if st.CandidateSizeBytes > 0 {
		ec.State["candidate_size_bytes"] = st.CandidateSizeBytes
	}
	if st.CandidateSHA256 != "" {
		ec.State["candidate_sha256"] = st.CandidateSHA256
	}
	if st.StorageBackend != "" {
		ec.State["storage_backend"] = st.StorageBackend
	}
	if st.FailureClassification == "" {
		delete(ec.State, "failure_classification")
		delete(ec.Outputs, "failure_classification")
		delete(ec.State, "error_class")
		delete(ec.Outputs, "error_class")
	}
	ec.State["last_worker_poll_at"] = e.now().Format(time.RFC3339Nano)
	ec.State["transcode_status"] = st.Status
	delete(ec.State, "worker_poll_failures")
	// Only copy known telemetry: an older worker must not fabricate zero phase
	// durations or replace a previously observed timestamp with the zero time.
	b, _ := json.Marshal(st.JobTelemetry)
	var fields map[string]any
	_ = json.Unmarshal(b, &fields)
	for _, key := range []string{"last_progress_at", "worker_heartbeat_at", "last_known_progress", "finalization_retry_count", "next_finalization_at", "recovery_required", "queue_duration_ms", "encode_duration_ms", "worker_slots_total", "worker_slots_used", "queue_position", "storage_backend", "navigatorr_path", "worker_resolved_path", "smb_share", "smb_relative_path"} {
		if v, ok := fields[key]; ok && v != "0001-01-01T00:00:00Z" {
			ec.State[key] = v
		}
	}
	if st.Phase != "" {
		ec.State["transcode_phase"] = st.Phase
	}
	if st.ValidationDurationMs != nil {
		ec.State["worker_validation_duration_ms"] = *st.ValidationDurationMs
		ec.State["validation_duration_ms"] = *st.ValidationDurationMs + getInt64(ec.State, "coordinator_validation_duration_ms")
	}
	if st.WallDurationMs != nil {
		ec.State["worker_wall_duration_ms"] = *st.WallDurationMs
	}
	stale := st.ProgressIsStale
	if st.Status == transcode.StatusRunning {
		stale = stale || st.LastProgressAt.IsZero() || e.now().Sub(st.LastProgressAt) > 30*time.Second
	}
	if st.Status == transcode.StatusCompleted || st.Status == transcode.StatusFailed || st.Status == transcode.StatusCancelled {
		stale = false
		if !st.FinishedAt.IsZero() {
			ec.State["worker_completed_at"] = st.FinishedAt.UTC().Format(time.RFC3339Nano)
			if getString(ec.State, "reconciled_at") == "" {
				now := e.now()
				ec.State["reconciled_at"] = now.Format(time.RFC3339Nano)
				lag := now.Sub(st.FinishedAt).Milliseconds()
				if lag < 0 {
					lag = 0
				}
				ec.State["reconcile_lag_ms"] = lag
			}
		} else if getString(ec.State, "reconciled_at") == "" {
			ec.State["reconciled_at"] = e.now().Format(time.RFC3339Nano)
		}
	}
	ec.State["progress_is_stale"] = stale
	ec.State["recovery_required"] = st.RecoveryRequired
	if st.NextFinalizationAt.IsZero() {
		delete(ec.State, "next_finalization_at")
		delete(ec.Outputs, "next_finalization_at")
	}
	if st.WorkerSlotsTotal > 0 {
		ec.State["queue_position"] = st.QueuePosition
	}
	if !stale || st.Progress > 0 {
		ec.State["progress"], ec.State["speed"], ec.State["fps"] = st.Progress, st.Speed, st.FPS
	}
	if st.Status == transcode.StatusCompleted {
		ec.State["progress"] = 100.0
	}
	if !stale && !st.LastProgressAt.IsZero() && st.LastKnownProgress == nil {
		ec.State["last_known_progress"] = map[string]any{"progress": st.Progress, "speed": st.Speed, "fps": st.FPS, "updated_at": st.LastProgressAt.UTC().Format(time.RFC3339Nano)}
	}
}

func transcodeTelemetryOutputs(ec *ExecutionContext) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"transcode_status", "transcode_phase", "quality_evidence", "finalization_retry_count", "next_finalization_at", "recovery_required", "progress", "speed", "fps", "progress_is_stale", "last_progress_at", "worker_heartbeat_at", "last_known_progress", "last_worker_poll_at", "worker_completed_at", "reconciled_at", "queue_duration_ms", "encode_duration_ms", "worker_validation_duration_ms", "validation_duration_ms", "worker_wall_duration_ms", "reconcile_lag_ms", "worker_slots_total", "worker_slots_used", "queue_position", "storage_backend", "navigatorr_path", "worker_resolved_path", "smb_share", "smb_relative_path"} {
		if v, ok := ec.State[key]; ok {
			out[key] = v
		}
	}
	return out
}

func (e *Engine) recordWorkerPoll(ec *ExecutionContext) {
	ec.State["last_worker_poll_at"] = e.now().Format(time.RFC3339Nano)
	ec.Outputs["last_worker_poll_at"] = ec.State["last_worker_poll_at"]
}

func (e *Engine) observeBenchmarkStatus(ec *ExecutionContext, st transcode.BenchmarkStatus) {
	ec.State["benchmark_status"] = st.Status
	if len(st.Quality) > 0 {
		ec.State["benchmark_quality"] = st.Quality
		ec.Outputs["benchmark_quality"] = st.Quality
	} else {
		delete(ec.State, "benchmark_quality")
		delete(ec.Outputs, "benchmark_quality")
	}
	if st.Phase != "" {
		ec.State["phase"] = st.Phase
	}
	ec.State["progress"] = st.Progress
	ec.State["progress_is_stale"] = st.ProgressIsStale
	if !st.LastProgressAt.IsZero() {
		ec.State["last_progress_at"] = st.LastProgressAt.UTC().Format(time.RFC3339Nano)
	}
	if !st.HeartbeatAt.IsZero() {
		ec.State["worker_heartbeat_at"] = st.HeartbeatAt.UTC().Format(time.RFC3339Nano)
	}
	if st.ProgressDetails != nil {
		ec.State["progress_details"] = st.ProgressDetails
	}
	if st.SamplesPlanned > 0 {
		ec.State["samples_planned"] = st.SamplesPlanned
	}
	if st.CandidatesCount > 0 {
		ec.State["candidates_count"] = st.CandidatesCount
	}
	if !st.FinishedAt.IsZero() {
		ec.State["benchmark_worker_completed_at"] = st.FinishedAt.UTC().Format(time.RFC3339Nano)
		if getString(ec.State, "benchmark_reconciled_at") == "" {
			ec.State["benchmark_reconciled_at"] = e.now().Format(time.RFC3339Nano)
			lag := e.now().Sub(st.FinishedAt).Milliseconds()
			if lag < 0 {
				lag = 0
			}
			ec.State["benchmark_reconcile_lag_ms"] = lag
		}
		if ec.ActionName == "benchmark_transcode" {
			ec.State["worker_completed_at"] = ec.State["benchmark_worker_completed_at"]
			ec.State["reconciled_at"] = ec.State["benchmark_reconciled_at"]
			ec.State["reconcile_lag_ms"] = ec.State["benchmark_reconcile_lag_ms"]
		}
	}
	for _, key := range []string{"benchmark_status", "phase", "progress", "progress_is_stale", "last_progress_at", "worker_heartbeat_at", "progress_details", "samples_planned", "candidates_count", "benchmark_worker_completed_at", "benchmark_reconciled_at", "benchmark_reconcile_lag_ms", "worker_completed_at", "reconciled_at", "reconcile_lag_ms"} {
		if v, ok := ec.State[key]; ok {
			ec.Outputs[key] = v
		}
	}
}
