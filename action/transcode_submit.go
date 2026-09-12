package action

import (
	"context"
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
	if getBool(ec.State, "transcode_submitted") || getString(ec.State, "job_id") != "" {
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
	req := transcode.Request{ID: jobID, SourcePath: cleanPath, CandidatePath: candidatePath, Profile: profile, Plan: plan}
	job, err := e.deps.Transcode.Submit(ctx, req)
	if err != nil {
		return e.handleTransientFailure(ec, plan, err, "submit")
	}
	ec.State["transcode_submitted"] = true
	ec.State["job_id"] = job.ID
	ec.State["external_reference"] = job.ID
	ec.State["candidate_path"] = candidatePath
	ec.State["output_path"] = candidatePath
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"job_id": job.ID, "external_reference": job.ID, "candidate_path": candidatePath, "output_path": candidatePath, "profile": profile, "recipe_version": plan.RecipeVersion, "recipe_digest": plan.RecipeDigest, "plan_digest": plan.PlanDigest, "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count"), "fallback_count": getInt(ec.State, "fallback_count")}}, nil
}

func (e *Engine) handleTransientFailure(ec *ExecutionContext, plan *transcode.Plan, err error, phase string) (StepResult, error) {
	class := resilience.Classify(err.Error())
	ec.State["failure_classification"] = string(class)
	appendFailureHistory(ec, phase, string(class), err.Error())
	if class == resilience.WorkerBusy && getBool(ec.Inputs, "surface_worker_busy") {
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
	plan := getPlan(ec.State["plan"])
	st, err := e.deps.Transcode.Status(ctx, jobID)
	if err != nil {
		if plan != nil {
			return e.handleTransientFailure(ec, plan, err, "status")
		}
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("status failed: %v", err)}, nil
	}
	switch st.Status {
	case transcode.StatusRunning, transcode.StatusQueued:
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "transcode_complete", WaitingReason: fmt.Sprintf("Transcoding media (%s, progress: %.1f%%, speed: %.1fx, fps: %.1f)", st.Status, st.Progress, st.Speed, st.FPS), Outputs: map[string]any{"transcode_status": st.Status, "progress": st.Progress, "speed": st.Speed, "fps": st.FPS, "job_id": jobID, "external_reference": jobID}}, nil
	case transcode.StatusFailed:
		msg := st.Error
		if msg == "" {
			msg = "transcode executor reported job failure"
		}
		class := resilience.Classify(msg)
		ec.State["failure_classification"] = string(class)
		appendFailureHistory(ec, "worker", string(class), msg)
		if class == resilience.WorkerBusy && getBool(ec.Inputs, "surface_worker_busy") {
			return StepResult{
				Status:           StepWaitingExternal,
				WaitingCondition: "worker_busy",
				WaitingReason:    "Worker is busy; waiting for transcode slot",
				Outputs: map[string]any{
					"failure_classification": string(class),
					"worker_busy":            true,
				},
			}, nil
		}
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("Transcode failed (%s): %s", class, msg), Outputs: map[string]any{"failure_classification": string(class)}}, nil
	case transcode.StatusCancelled:
		return StepResult{Status: StepFailed, Error: "transcode job was cancelled"}, nil
	case transcode.StatusCompleted:
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
		if len(st.Conversions) > 0 {
			out["conversions"] = st.Conversions
		}
		return StepResult{Status: StepCompleted, Outputs: out}, nil
	default:
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "transcode_complete", WaitingReason: fmt.Sprintf("Transcode in progress (%s)", st.Status)}, nil
	}
}
