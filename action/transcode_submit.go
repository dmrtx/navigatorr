package action

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

func (e *Engine) stepTranscodeSubmit(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if getBool(ec.State, "skip_transcode") {
		return StepResult{
			Status: StepSkipped,
			Outputs: map[string]any{
				"skipped":       true,
				"auto_decision": getString(ec.State, "auto_decision"),
				"auto_reasons":  ec.State["auto_reasons"],
			},
		}, nil
	}
	if getBool(ec.State, "transcode_submitted") && !getBool(ec.State, "transcode_reconcile") {
		jobID := getString(ec.State, "job_id")
		if jobID == "" {
			jobID = getString(ec.State, "external_reference")
		}
		cand := getString(ec.State, "candidate_path")
		return StepResult{Status: StepCompleted, Outputs: map[string]any{"job_id": jobID, "external_reference": jobID, "candidate_path": cand, "output_path": cand, "reused": true}}, nil
	}
	if e.deps.Transcode == nil {
		return StepResult{Status: StepFailed, Error: "transcode executor is disabled or not configured in navigatorr"}, nil
	}
	if wait := getString(ec.State, "retry_not_before"); wait != "" {
		if at, err := time.Parse(time.RFC3339Nano, wait); err == nil && time.Now().Before(at) {
			return StepResult{Status: StepWaitingExternal, WaitingCondition: "transcode_retry", WaitingReason: fmt.Sprintf("Retry is backed off until %s", at.UTC().Format(time.RFC3339))}, nil
		}
		delete(ec.State, "retry_not_before")
	}
	cleanPath := getString(ec.State, "resolved_path")
	if cleanPath == "" {
		cleanPath = getString(ec.Inputs, "path")
	}
	jobID := getString(ec.State, "job_id")
	if jobID == "" {
		jobID = fmt.Sprintf("job-%s", ec.InstanceID)
	}
	ext := getString(ec.State, "candidate_extension")
	if ext == "" {
		return StepResult{Status: StepFailed, Error: "candidate extension missing from resolved plan (fail closed)"}, nil
	}
	srcDir := filepath.Dir(cleanPath)
	base := filepath.Base(cleanPath)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	candidatePath := filepath.Join(srcDir, ".navigatorr-candidates", fmt.Sprintf("%s.%s%s", stem, jobID, ext))
	profile := getString(ec.State, "profile")
	if profile == "" {
		profile = "hevc-vt"
	}
	plan := getPlan(ec.State["plan"])
	if plan == nil {
		return StepResult{Status: StepFailed, Error: "resolved transcode plan is missing (fail closed)"}, nil
	}
	// Persist stable identity before first Submit so uncertainty can reconcile.
	ec.State["job_id"] = jobID
	ec.State["external_reference"] = jobID
	ec.State["candidate_path"] = candidatePath
	ec.State["output_path"] = candidatePath
	ec.State["idempotency_key"] = jobID
	req := transcode.Request{ID: jobID, SourcePath: cleanPath, CandidatePath: candidatePath, Profile: profile, Plan: plan, IdempotencyKey: jobID}
	// Transport-uncertainty reconciliation before reuse.
	if getBool(ec.State, "transcode_reconcile") {
		st, serr := e.deps.Transcode.Status(ctx, jobID)
		if serr != nil {
			if transcode.IsTransportUncertain(serr) {
				return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_unreachable", WaitingReason: fmt.Sprintf("Transcode reconcile status uncertain for job %s; awaiting worker", jobID), Outputs: map[string]any{"job_id": jobID, "external_reference": jobID, "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count")}}, nil
			}
			if httpErr, ok := transcodeHTTPError(serr); ok && httpErr.StatusCode == 404 {
				clearTranscodeReconcileFlags(ec)
			} else {
				return StepResult{Status: StepFailed, Error: fmt.Sprintf("transcode reconcile status failed (fail closed): %v", serr)}, nil
			}
		} else {
			switch st.Status {
			case transcode.StatusQueued, transcode.StatusRunning, transcode.StatusCompleted, transcode.StatusFailed, transcode.StatusCancelled:
				ec.State["transcode_submitted"] = true
				clearTranscodeReconcileFlags(ec)
				mirrorTranscodeWorkerMetadata(ec, st)
				if st.CandidatePath != "" {
					ec.State["candidate_path"] = st.CandidatePath
					ec.State["output_path"] = st.CandidatePath
				}
				return StepResult{Status: StepCompleted, Outputs: map[string]any{"job_id": jobID, "external_reference": jobID, "candidate_path": getString(ec.State, "candidate_path"), "output_path": getString(ec.State, "output_path"), "transcode_status": st.Status, "reconciled": true, "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count")}}, nil
			default:
				return StepResult{Status: StepFailed, Error: fmt.Sprintf("transcode reconcile unexpected status %q (fail closed)", st.Status)}, nil
			}
		}
	}
	job, err := e.deps.Transcode.Submit(ctx, req)
	if err != nil {
		if transcode.IsTransportUncertain(err) {
			ec.State["transcode_reconcile"] = true
			return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_reconciling", WaitingReason: fmt.Sprintf("Transcode submit uncertain for job %s; reconciling with worker", jobID), Outputs: map[string]any{"job_id": jobID, "external_reference": jobID, "candidate_path": candidatePath, "output_path": candidatePath, "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count"), "failure_classification": string(resilience.WorkerUnreachable)}}, nil
		}
		if isTranscodeIdempotencyConflict(err) {
			class := resilience.IdempotencyConflict
			ec.State["failure_classification"] = string(class)
			appendFailureHistory(ec, "submit", string(class), err.Error())
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("submit failed (%s): %v", class, err), Outputs: map[string]any{"attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count"), "failure_classification": string(class)}}, nil
		}
		return e.handleTransientFailure(ec, plan, err, "submit")
	}
	ec.State["transcode_submitted"] = true
	ec.State["job_id"] = job.ID
	ec.State["external_reference"] = job.ID
	ec.State["candidate_path"] = candidatePath
	ec.State["output_path"] = candidatePath
	clearTranscodeReconcileFlags(ec)
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"job_id": job.ID, "external_reference": job.ID, "candidate_path": candidatePath, "output_path": candidatePath, "profile": profile, "recipe_version": plan.RecipeVersion, "recipe_digest": plan.RecipeDigest, "plan_digest": plan.PlanDigest, "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count"), "fallback_count": getInt(ec.State, "fallback_count")}}, nil
}

func clearTranscodeReconcileFlags(ec *ExecutionContext) {
	delete(ec.State, "transcode_reconcile")
}

func transcodeHTTPError(err error) (*transcode.HTTPError, bool) {
	var he *transcode.HTTPError
	if errors.As(err, &he) {
		return he, true
	}
	return nil, false
}

func isTranscodeIdempotencyConflict(err error) bool {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "idempotency_conflict") || strings.Contains(msg, "idempotency conflict") {
		return true
	}
	if strings.Contains(msg, "execution_spec_digest") && strings.Contains(msg, "mismatch") {
		return true
	}
	return false
}

func mirrorTranscodeWorkerMetadata(ec *ExecutionContext, st transcode.JobStatus) {
	if st.Attempt > 0 {
		ec.State["attempt"] = st.Attempt
	}
	ec.State["retry_count"] = st.RetryCount
	ec.State["fallback_count"] = st.FallbackCount
	if st.AppliedFallbacks != nil {
		ec.State["applied_fallbacks"] = st.AppliedFallbacks
	}
	if st.FailureClassification != "" {
		ec.State["failure_classification"] = st.FailureClassification
	}
}

func transcodeWorkerMetadataOutputs(ec *ExecutionContext, st transcode.JobStatus) map[string]any {
	out := map[string]any{
		"attempt":        getInt(ec.State, "attempt"),
		"retry_count":    getInt(ec.State, "retry_count"),
		"fallback_count": getInt(ec.State, "fallback_count"),
	}
	if v, ok := ec.State["applied_fallbacks"]; ok {
		out["applied_fallbacks"] = v
	} else if st.AppliedFallbacks != nil {
		out["applied_fallbacks"] = st.AppliedFallbacks
	}
	if fc := getString(ec.State, "failure_classification"); fc != "" {
		out["failure_classification"] = fc
	} else if st.FailureClassification != "" {
		out["failure_classification"] = st.FailureClassification
	}
	return out
}

func (e *Engine) handleTransientFailure(ec *ExecutionContext, plan *transcode.Plan, err error, phase string) (StepResult, error) {
	class := resilience.Classify(err.Error())
	ec.State["failure_classification"] = string(class)
	appendFailureHistory(ec, phase, string(class), err.Error())
	if class == resilience.WorkerBusy && (getBool(ec.Inputs, "surface_worker_busy") || getBool(ec.State, "surface_worker_busy")) {
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "worker_busy",
			WaitingReason:    "Worker is busy; waiting for transcode slot",
			Outputs: map[string]any{
				"attempt":                getInt(ec.State, "attempt"),
				"retry_count":            getInt(ec.State, "retry_count"),
				"failure_classification": string(class),
				"worker_busy":            true,
			},
		}, nil
	}
	retries := getInt(ec.State, "retry_count")
	attempt := getInt(ec.State, "attempt")
	if attempt <= 0 {
		attempt = 1
	}
	if resilience.Retryable(class, plan.Resilience.RetryOn) && retries < plan.Resilience.TransientRetries && attempt < plan.Resilience.MaxAttempts {
		retries++
		attempt++
		ec.State["retry_count"] = retries
		ec.State["attempt"] = attempt
		backoff := 0
		if retries-1 < len(plan.Resilience.RetryBackoffSeconds) {
			backoff = plan.Resilience.RetryBackoffSeconds[retries-1]
		}
		if backoff > 0 {
			ec.State["retry_not_before"] = time.Now().Add(time.Duration(backoff) * time.Second).UTC().Format(time.RFC3339Nano)
		}
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "transcode_retry", WaitingReason: fmt.Sprintf("%s classified as %s; bounded retry %d/%d after %ds", phase, class, retries, plan.Resilience.TransientRetries, backoff), Outputs: map[string]any{"attempt": attempt, "retry_count": retries, "failure_classification": string(class)}}, nil
	}
	return StepResult{Status: StepFailed, Error: fmt.Sprintf("%s failed (%s): %v", phase, class, err), Outputs: map[string]any{"attempt": attempt, "retry_count": retries, "failure_classification": string(class)}}, nil
}

func appendFailureHistory(ec *ExecutionContext, phase, class, msg string) {
	entry := map[string]any{"at": time.Now().UTC().Format(time.RFC3339), "phase": phase, "classification": class, "error": msg}
	switch v := ec.State["failure_history"].(type) {
	case []map[string]any:
		ec.State["failure_history"] = append(v, entry)
	case []any:
		ec.State["failure_history"] = append(v, entry)
	default:
		ec.State["failure_history"] = []any{entry}
	}
}

func (e *Engine) stepTranscodeWait(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if getBool(ec.State, "skip_transcode") {
		return StepResult{
			Status:  StepSkipped,
			Outputs: map[string]any{"skipped": true},
		}, nil
	}
	jobID := getString(ec.State, "job_id")
	if jobID == "" {
		jobID = getString(ec.State, "external_reference")
	}
	if jobID == "" {
		return StepResult{Status: StepFailed, Error: "no active transcode job ID to monitor"}, nil
	}
	if e.deps.Transcode == nil {
		return StepResult{Status: StepFailed, Error: "transcode executor is not available to monitor job"}, nil
	}
	st, err := e.deps.Transcode.Status(ctx, jobID)
	if err != nil {
		if transcode.IsTransportUncertain(err) {
			return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_unreachable", WaitingReason: fmt.Sprintf("Transcode status uncertain for job %s; awaiting worker", jobID), Outputs: map[string]any{"job_id": jobID, "external_reference": jobID, "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count"), "failure_classification": string(resilience.WorkerUnreachable)}}, nil
		}
		if he, ok := transcodeHTTPError(err); ok && he.StatusCode == 404 {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("transcode job %s disappeared from worker (no resubmit)", jobID), Outputs: map[string]any{"job_id": jobID, "external_reference": jobID}}, nil
		}
		// Worker owns retry; coordinator must not consume budget on definitive
		// status errors. Fail closed with budgets unchanged.
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("transcode status failed (fail closed): %v", err), Outputs: map[string]any{"job_id": jobID, "external_reference": jobID, "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count")}}, nil
	}
	switch st.Status {
	case transcode.StatusRunning, transcode.StatusQueued:
		mirrorTranscodeWorkerMetadata(ec, st)
		meta := transcodeWorkerMetadataOutputs(ec, st)
		meta["transcode_status"] = st.Status
		meta["progress"] = st.Progress
		meta["speed"] = st.Speed
		meta["fps"] = st.FPS
		meta["job_id"] = jobID
		meta["external_reference"] = jobID
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "transcode_complete", WaitingReason: fmt.Sprintf("Transcoding media (%s, progress: %.1f%%, speed: %.1fx, fps: %.1f)", st.Status, st.Progress, st.Speed, st.FPS), Outputs: meta}, nil
	case transcode.StatusFailed:
		mirrorTranscodeWorkerMetadata(ec, st)
		msg := st.Error
		if msg == "" {
			msg = "transcode executor reported job failure"
		}
		class := st.FailureClassification
		if class == "" {
			class = string(resilience.Classify(msg))
		}
		ec.State["failure_classification"] = class
		appendFailureHistory(ec, "worker", class, msg)
		// Worker StatusFailed is always terminal: never coordinator retry/wait.
		meta := transcodeWorkerMetadataOutputs(ec, st)
		meta["failure_classification"] = class
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("Transcode failed (%s): %s", class, msg), Outputs: meta}, nil
	case transcode.StatusCancelled:
		mirrorTranscodeWorkerMetadata(ec, st)
		if st.FailureClassification != "" {
			ec.State["failure_classification"] = st.FailureClassification
		} else if getString(ec.State, "failure_classification") == "" {
			ec.State["failure_classification"] = string(resilience.Cancelled)
		}
		meta := transcodeWorkerMetadataOutputs(ec, st)
		return StepResult{Status: StepFailed, Error: "transcode job was cancelled", Outputs: meta}, nil
	case transcode.StatusCompleted:
		mirrorTranscodeWorkerMetadata(ec, st)
		cand := st.CandidatePath
		if cand == "" {
			cand = getString(ec.State, "candidate_path")
		}
		if cand == "" {
			cand = getString(ec.State, "output_path")
		}
		ec.State["candidate_path"] = cand
		ec.State["output_path"] = cand
		if len(st.Conversions) > 0 {
			ec.State["conversions"] = st.Conversions
		}
		out := map[string]any{"transcode_done": true, "candidate_path": cand, "output_path": cand, "job_id": jobID, "external_reference": jobID}
		meta := transcodeWorkerMetadataOutputs(ec, st)
		for k, v := range meta {
			out[k] = v
		}
		if len(st.Conversions) > 0 {
			out["conversions"] = st.Conversions
		}
		return StepResult{Status: StepCompleted, Outputs: out}, nil
	default:
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "transcode_complete", WaitingReason: fmt.Sprintf("Transcode in progress (%s)", st.Status)}, nil
	}
}
