package action

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
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
	// A worker that refused a submit as busy asked for a pause. Checking here
	// skips capability probes inside the window; reconciling an uncertain
	// submit is a read and never waits. A second check just before admission
	// covers the path where reconciliation falls through to a new submit.
	if !getBool(ec.State, "transcode_reconcile") {
		if res, waiting := e.workerBusyBackoff(ec, transcodeSlotWaitReason); waiting {
			return res, nil
		}
	}
	cleanPath := getString(ec.State, "resolved_path")
	if cleanPath == "" {
		cleanPath = getString(ec.Inputs, "path")
	}
	jobID := getString(ec.State, "job_id")
	if jobID == "" {
		jobID = fmt.Sprintf("job-%s", ec.InstanceID)
		for _, key := range []string{"phase", "progress", "speed", "fps", "last_progress_at", "last_known_progress", "worker_heartbeat_at", "progress_is_stale"} {
			delete(ec.State, key)
			delete(ec.Outputs, key)
		}
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
	if (plan.X265 != nil || plan.AudioMode == "he_aac") && !getBool(ec.State, "transcode_reconcile") {
		caps, err := e.deps.Transcode.Capabilities(ctx)
		if err != nil || (plan.X265 != nil && !caps.X265TwoPass) || (plan.AudioMode == "he_aac" && !caps.HEAACAudio) {
			return StepResult{Status: StepFailed, Error: "worker does not support the explicit x265/HE-AAC experiment; upgrade before encoding"}, nil
		}
	}
	if plan.AudioMode == transcode.AudioModeCompactLossless && !getBool(ec.State, "transcode_reconcile") {
		caps, err := e.deps.Transcode.Capabilities(ctx)
		if err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("checking lossless audio capability: %v", err)}, nil
		}
		if !caps.SizeModeV3 {
			return StepResult{Status: StepFailed, Error: "worker does not support lossless audio compaction; upgrade the worker before encoding"}, nil
		}
	}
	if transcode.IsSizeAudioOnlyPlan(plan) && !getBool(ec.State, "transcode_reconcile") {
		caps, err := e.deps.Transcode.Capabilities(ctx)
		if err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("checking audio-only capability: %v", err)}, nil
		}
		if !caps.SizeAudioOnly {
			return StepResult{Status: StepFailed, Error: "worker cannot prove a copied video stream; upgrade the worker before encoding"}, nil
		}
	}
	if plan.AudioMode == "compact" && !getBool(ec.State, "transcode_reconcile") {
		caps, err := e.deps.Transcode.Capabilities(ctx)
		if err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("checking compact audio capability: %v", err)}, nil
		}
		if !caps.CompactAudio {
			return StepResult{Status: StepFailed, Error: "worker does not support compact audio; upgrade the worker before encoding"}, nil
		}
	}
	if origMap, _ := ec.State["original"].(map[string]any); len(mediainspect.CoverArtStreams(getStreamsList(origMap, "video"))) > 0 && !getBool(ec.State, "transcode_reconcile") && !e.workerSupportsCoverArt(ctx) {
		return StepResult{Status: StepFailed, Error: errCoverArtWorker + " before encoding"}, nil
	}
	if plan.Compact != nil && !getBool(ec.State, "transcode_reconcile") {
		caps, err := e.deps.Transcode.Capabilities(ctx)
		if err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("checking compact capability: %v", err)}, nil
		}
		if !caps.SupportsCompactTranscode() {
			return StepResult{Status: StepFailed, Error: "worker does not support compact SDR conversion; upgrade the worker before encoding"}, nil
		}
	}
	req := transcode.Request{ID: jobID, SourcePath: cleanPath, CandidatePath: candidatePath, Profile: profile, Plan: plan, IdempotencyKey: jobID, SourceSHA256: getString(ec.State, "original_sha256")}

	// Transport-uncertainty reconciliation before reuse. This is a read-only
	// status query and deliberately does not take the parent admission lease.
	if getBool(ec.State, "transcode_reconcile") {
		e.recordWorkerPoll(ec)
		st, serr := e.deps.Transcode.Status(ctx, jobID)
		if serr != nil {
			if isRetryableWorkerPollError(serr) {
				return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_unreachable", WaitingReason: fmt.Sprintf("Transcode reconcile status uncertain for job %s; awaiting worker", jobID), Outputs: map[string]any{"job_id": jobID, "external_reference": jobID, "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count")}}, nil
			}
			if httpErr, ok := transcodeHTTPError(serr); ok && httpErr.StatusCode == 404 {
				// The worker has no such job. Any resubmit below re-reads the
				// parent policy under its lease; a decision cached before this
				// read must never authorize the resubmit.
				clearTranscodeReconcileFlags(ec)
			} else {
				return StepResult{Status: StepFailed, Error: fmt.Sprintf("transcode reconcile status failed (fail closed): %v", serr)}, nil
			}
		} else {
			switch st.Status {
			case transcode.StatusQueued, transcode.StatusRunning, transcode.StatusCompleted, transcode.StatusFailed, transcode.StatusCancelled:
				ec.State["job_id"] = jobID
				ec.State["external_reference"] = jobID
				ec.State["candidate_path"] = candidatePath
				ec.State["output_path"] = candidatePath
				ec.State["transcode_submitted"] = true
				delete(ec.State, transcodeRejectedKey)
				clearTranscodeReconcileFlags(ec)
				clearWorkerBusy(ec)
				e.observeTranscodeStatus(ec, st)
				if st.CandidatePath != "" {
					ec.State["candidate_path"] = st.CandidatePath
					ec.State["output_path"] = st.CandidatePath
				}
				// Persist acceptance/tracking before any admission lease release.
				if perr := e.persistExecutionState(ctx, ec); perr != nil {
					return StepResult{}, perr
				}
				return StepResult{Status: StepCompleted, Outputs: map[string]any{"job_id": jobID, "external_reference": jobID, "candidate_path": getString(ec.State, "candidate_path"), "output_path": getString(ec.State, "output_path"), "transcode_status": st.Status, "reconciled": true, "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count")}}, nil
			default:
				return StepResult{Status: StepFailed, Error: fmt.Sprintf("transcode reconcile unexpected status %q (fail closed)", st.Status)}, nil
			}
		}
	}

	// Only new writes wait for the busy window, as for benchmarks.
	if res, waiting := e.workerBusyBackoff(ec, transcodeSlotWaitReason); waiting {
		return res, nil
	}
	// Admission critical section: when the child belongs to a batch, hold the
	// parent's durable lease across the policy re-read, identity persistence and
	// the submit outcome so Engine.Cancel cannot interleave a confirmed cancel.
	lease, blockedRes, handled := e.beginAdmission(ctx, ec, "transcode")
	if handled {
		return blockedRes, nil
	}
	defer lease.Close()
	actx := lease.Context(ctx)
	// A current worker queues encodes itself and never refuses them as busy,
	// so only an encode already refused (an older worker) waits its turn in
	// the slot queue; it must not jump ahead of older slot waiters.
	if hasSlotTicket(ec.State) {
		if res, waiting := e.workerSlotTurn(ec, transcodeSlotWaitReason); waiting {
			return res, nil
		}
	}

	// Stable identity persisted before Submit so uncertainty can reconcile.
	ec.State["job_id"] = jobID
	ec.State["external_reference"] = jobID
	ec.State["candidate_path"] = candidatePath
	ec.State["output_path"] = candidatePath
	ec.State["idempotency_key"] = jobID
	ec.State["transcode_reconcile"] = true
	delete(ec.State, transcodeRejectedKey)
	if err := e.persistExecutionState(actx, ec); err != nil {
		return StepResult{}, err
	}
	// Ownership check after the identity checkpoint: if the parent lease was
	// lost (or is no longer renewed), do not create a new remote job. The
	// persisted uncertainty is preserved for a later safe reconciliation.
	if !e.parentLeaseHeld(lease) {
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "parent_busy", WaitingReason: fmt.Sprintf("Parent admission lease lost before submit for job %s; deferring without resubmit", jobID), Outputs: map[string]any{"job_id": jobID, "external_reference": jobID, "candidate_path": candidatePath, "output_path": candidatePath}}, nil
	}
	job, err := e.deps.Transcode.Submit(actx, req)
	if err != nil {
		if isRetryableWorkerPollError(err) {
			ec.State["transcode_reconcile"] = true
			clearWorkerBusy(ec) // This send may have been accepted; it was not refused as busy.
			res := StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_reconciling", WaitingReason: fmt.Sprintf("Transcode submit uncertain for job %s; reconciling with worker", jobID), Outputs: map[string]any{"job_id": jobID, "external_reference": jobID, "candidate_path": candidatePath, "output_path": candidatePath, "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count"), "failure_classification": string(resilience.WorkerUnreachable)}}
			if perr := e.persistExecutionState(actx, ec); perr != nil {
				return StepResult{}, perr
			}
			return res, nil
		}
		clearTranscodeReconcileFlags(ec) // A definitive rejection did not accept the job.
		ec.State[transcodeRejectedKey] = true
		var res StepResult
		if isTranscodeIdempotencyConflict(err) {
			clearWorkerBusy(ec)
			class := resilience.IdempotencyConflict
			ec.State["failure_classification"] = string(class)
			appendFailureHistory(ec, "submit", string(class), err.Error())
			res = StepResult{Status: StepFailed, Error: fmt.Sprintf("submit failed (%s): %v", class, err), Outputs: map[string]any{"attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count"), "failure_classification": string(class)}}
		} else {
			var herr error
			res, herr = e.handleTransientFailure(ec, plan, err, "submit")
			if herr != nil {
				return StepResult{}, herr
			}
		}
		// Persist the definitive outcome under the child owner before the parent
		// admission lease is released.
		if perr := e.persistExecutionState(actx, ec); perr != nil {
			return StepResult{}, perr
		}
		return res, nil
	}
	ec.State["transcode_submitted"] = true
	delete(ec.State, transcodeRejectedKey)
	clearWorkerBusy(ec)
	ec.State["job_id"] = job.ID
	ec.State["external_reference"] = job.ID
	ec.State["candidate_path"] = candidatePath
	ec.State["output_path"] = candidatePath
	clearTranscodeReconcileFlags(ec)
	// Persist acceptance before releasing the parent admission lease.
	if perr := e.persistExecutionState(actx, ec); perr != nil {
		return StepResult{}, perr
	}
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"job_id": job.ID, "external_reference": job.ID, "candidate_path": candidatePath, "output_path": candidatePath, "profile": profile, "recipe_version": plan.RecipeVersion, "recipe_digest": plan.RecipeDigest, "plan_digest": plan.PlanDigest, "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count"), "fallback_count": getInt(ec.State, "fallback_count")}}, nil
}

func clearTranscodeReconcileFlags(ec *ExecutionContext) {
	delete(ec.State, "transcode_reconcile")
}

func transcodeHTTPError(err error) (*httpclient.HTTPError, bool) {
	var he *httpclient.HTTPError
	if errors.As(err, &he) {
		return he, true
	}
	return nil, false
}

// An unavailable status endpoint is not evidence that the encode failed. Keep
// the same identity and poll with backoff; only a remote terminal status can
// authorize creating a fresh attempt.
//
// A worker_busy refusal is retryable but definitive: the worker created
// nothing, so it goes to the transient-failure path (WorkerBusy) instead of
// reconciliation. Responses with an envelope code are classified by it;
// older workers and non-worker responses fall back to the status.
func isRetryableWorkerPollError(err error) bool {
	if httpclient.IsTransportUncertain(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if he, ok := transcodeHTTPError(err); ok {
		if he.IsWorkerBusy() {
			return false
		}
		if he.Code != "" {
			return he.Retryable
		}
		switch he.StatusCode {
		case 408, 425, 429, 500, 502, 503, 504:
			return true
		}
	}
	return false
}

func isTranscodeIdempotencyConflict(err error) bool {
	if he, ok := transcodeHTTPError(err); ok && he.Code != "" {
		return he.IsIdempotencyConflict()
	}
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
	out := transcodeTelemetryOutputs(ec)
	mergeMap(out, map[string]any{
		"attempt":        getInt(ec.State, "attempt"),
		"retry_count":    getInt(ec.State, "retry_count"),
		"fallback_count": getInt(ec.State, "fallback_count"),
	})
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
	if class, ok := out["failure_classification"]; ok {
		out["error_class"] = class
	}
	return out
}

// classifyWorkerFailure prefers the worker's envelope code for the two
// classes it names and falls back to message classification otherwise.
func classifyWorkerFailure(err error) resilience.FailureClass {
	if he, ok := transcodeHTTPError(err); ok && he.Code != "" {
		switch {
		case he.IsWorkerBusy():
			return resilience.WorkerBusy
		case he.IsIdempotencyConflict():
			return resilience.IdempotencyConflict
		}
	}
	return resilience.Classify(err.Error())
}

// workerBusyNotBeforeKey holds the earliest time a submit the worker refused
// as busy may be sent again, from the worker's Retry-After hint.
const workerBusyNotBeforeKey = "worker_busy_not_before"

// workerBusyRetryMargin is added to Retry-After so the next submit lands
// after the window the worker asked for, not on its edge.
const workerBusyRetryMargin = time.Second

// workerBusyWait turns a worker_busy refusal into a wait for a free slot. The
// worker created nothing, so the wait never consumes attempts or retry budget,
// and it lasts until a slot frees up or the action is cancelled, the same way
// batch children wait. The reconciler resumes it: next_poll_at honours
// Retry-After through workerBusyNotBeforeKey.
func (e *Engine) workerBusyWait(ec *ExecutionContext, err error, phase, reason string, outputs map[string]any) StepResult {
	recordWorkerBusyHistory(ec, phase, err.Error())
	e.ensureSlotTicket(ec)
	if e.deps.Store != nil && ec.InstanceID != "" {
		// Observational: the place shown while waiting. Order is enforced by
		// workerSlotTurn on the next submit, not here.
		if position, err := e.slotQueuePosition(ec); err == nil {
			outputs[workerSlotPositionKey] = position
		}
	}
	markWaitingForWorker(ec)
	if he, ok := transcodeHTTPError(err); ok && he.RetryAfter > 0 {
		ec.State[workerBusyNotBeforeKey] = e.now().Add(he.RetryAfter + workerBusyRetryMargin).Format(time.RFC3339Nano)
	} else {
		delete(ec.State, workerBusyNotBeforeKey)
	}
	outputs["failure_classification"] = string(resilience.WorkerBusy)
	outputs["worker_busy"] = true
	return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_busy", WaitingReason: reason, Outputs: outputs}
}

// workerBusyBackoff keeps a slot wait waiting until the worker's Retry-After
// has passed, whoever resumes the action (the reconciler or a batch parent).
func (e *Engine) workerBusyBackoff(ec *ExecutionContext, reason string) (StepResult, bool) {
	raw := getString(ec.State, workerBusyNotBeforeKey)
	if raw == "" {
		return StepResult{}, false
	}
	if at, err := time.Parse(time.RFC3339Nano, raw); err == nil && e.now().Before(at) {
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_busy", WaitingReason: reason, Outputs: map[string]any{"worker_busy": true}}, true
	}
	delete(ec.State, workerBusyNotBeforeKey)
	return StepResult{}, false
}

// clearWorkerBusy drops slot-wait markers once a submit had any outcome other
// than a busy refusal, so the refusal is not reported as the job's state or
// failure class afterwards. It also gives up the action's place in the worker
// slot queue and its waiting_for_worker phase. Whether the worker holds a job
// is tracked apart, by the rejected markers that noRemoteJob reads.
func clearWorkerBusy(ec *ExecutionContext) {
	for _, key := range []string{workerBusyNotBeforeKey, "worker_busy", slotWaitLogKey, workerSlotWaitSinceKey, workerSlotPositionKey, workerSlotPolledAtKey} {
		delete(ec.State, key)
		delete(ec.Outputs, key)
	}
	clearWaitingForWorker(ec)
	if getString(ec.State, "failure_classification") == string(resilience.WorkerBusy) {
		for _, key := range []string{"failure_classification", "error_class"} {
			delete(ec.State, key)
			delete(ec.Outputs, key)
		}
	}
}

// slotWaitLogKey remembers the worker_busy wait last written to the step log,
// so one slot wait is one row however many polls it takes.
const slotWaitLogKey = "slot_wait_logged"

// repeatedSlotWait reports whether a waiting result repeats the worker_busy
// wait already logged for this step with the same reason, and records a new
// one. A parent_busy deferral inside a slot wait (the batch happened to be
// executing at that poll) neither logs a row nor ends the run. Any other wait
// ends the run; clearWorkerBusy ends it too.
func repeatedSlotWait(ec *ExecutionContext, stepIdx int, res StepResult) bool {
	if res.WaitingCondition == "parent_busy" && getString(ec.State, slotWaitLogKey) != "" && hasSlotTicket(ec.State) {
		return true
	}
	if res.WaitingCondition != "worker_busy" {
		delete(ec.State, slotWaitLogKey)
		return false
	}
	key := fmt.Sprintf("%d|%s", stepIdx, res.WaitingReason)
	if getString(ec.State, slotWaitLogKey) == key {
		return true
	}
	ec.State[slotWaitLogKey] = key
	return false
}

// The rejected markers record that the last send of the current identity was
// refused outright (busy, conflict or another definite error). They are set
// on each such refusal and cleared at the identity checkpoint before a send.
const (
	transcodeRejectedKey = "transcode_submit_rejected"
	benchmarkRejectedKey = "benchmark_submit_rejected"
)

// noRemoteJob reports that the last send was refused outright and nothing was
// sent since: the worker holds no job under that identity and would answer a
// cancel with 404.
func noRemoteJob(ec *ExecutionContext, submittedKey, reconcileKey, rejectedKey string) bool {
	return getBool(ec.State, rejectedKey) && !getBool(ec.State, submittedKey) && !getBool(ec.State, reconcileKey)
}

// recordWorkerBusyHistory keeps one history entry per run of busy refusals in
// a phase, counting them; a long slot wait would otherwise add one per poll.
func recordWorkerBusyHistory(ec *ExecutionContext, phase, msg string) {
	var last map[string]any
	switch v := ec.State["failure_history"].(type) {
	case []map[string]any:
		if len(v) > 0 {
			last = v[len(v)-1]
		}
	case []any:
		if len(v) > 0 {
			last, _ = v[len(v)-1].(map[string]any)
		}
	}
	if last != nil && getString(last, "phase") == phase && getString(last, "classification") == string(resilience.WorkerBusy) {
		count := getInt(last, "count")
		if count < 1 {
			count = 1
		}
		last["count"] = count + 1
		last["last_at"] = time.Now().UTC().Format(time.RFC3339)
		return
	}
	appendFailureHistory(ec, phase, string(resilience.WorkerBusy), msg)
}

const transcodeSlotWaitReason = "Waiting for a free worker slot to start the encode"

func (e *Engine) handleTransientFailure(ec *ExecutionContext, plan *transcode.Plan, err error, phase string) (StepResult, error) {
	class := classifyWorkerFailure(err)
	ec.State["failure_classification"] = string(class)
	if class == resilience.WorkerBusy {
		return e.workerBusyWait(ec, err, phase, transcodeSlotWaitReason, map[string]any{
			"attempt":     getInt(ec.State, "attempt"),
			"retry_count": getInt(ec.State, "retry_count"),
		}), nil
	}
	// A different outcome ends the slot wait; the markers would otherwise
	// report a failed or retrying job as still waiting for a slot.
	clearWorkerBusy(ec)
	appendFailureHistory(ec, phase, string(class), err.Error())
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
	e.recordWorkerPoll(ec)
	st, err := e.deps.Transcode.Status(ctx, jobID)
	if err != nil {
		if isRetryableWorkerPollError(err) {
			ec.State["progress_is_stale"] = true
			return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_unreachable", WaitingReason: fmt.Sprintf("Transcode status uncertain for job %s; awaiting worker", jobID), Outputs: map[string]any{"job_id": jobID, "external_reference": jobID, "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count"), "failure_classification": string(resilience.WorkerUnreachable)}}, nil
		}
		if he, ok := transcodeHTTPError(err); ok && he.StatusCode == 404 {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("transcode job %s disappeared from worker (no resubmit)", jobID), Outputs: map[string]any{"job_id": jobID, "external_reference": jobID}}, nil
		}
		// Worker owns retry; coordinator must not consume budget on definitive
		// status errors. Fail closed with budgets unchanged.
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("transcode status failed (fail closed): %v", err), Outputs: map[string]any{"job_id": jobID, "external_reference": jobID, "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count")}}, nil
	}
	e.observeTranscodeStatus(ec, st)
	switch st.Status {
	case transcode.StatusCompleted, transcode.StatusFailed, transcode.StatusCancelled:
		e.noteWorkerSlotFreed()
	}
	if st.RecoveryRequired {
		out := transcodeWorkerMetadataOutputs(ec, st)
		out["job_id"], out["external_reference"] = jobID, jobID
		out["worker_error"] = st.Error
		return StepResult{Status: StepWaitingDecision, WaitingReason: fmt.Sprintf("Encoding finished for job %s; the encoded candidate is preserved but publication requires intervention (%s): %s", jobID, st.FailureClassification, st.Error), WaitingOptions: []WaitingOption{{Decision: "recheck", Description: "Check the same worker job after publication recovery; do not encode again"}}, Outputs: out}, nil
	}
	switch st.Status {
	case transcode.StatusRunning, transcode.StatusQueued:
		mirrorTranscodeWorkerMetadata(ec, st)
		meta := transcodeWorkerMetadataOutputs(ec, st)
		mergeMap(meta, transcodeTelemetryOutputs(ec))
		meta["job_id"] = jobID
		meta["external_reference"] = jobID
		reason := fmt.Sprintf("Transcoding media (%s, progress: %.1f%%, speed: %.1fx, fps: %.1f)", st.Status, st.Progress, st.Speed, st.FPS)
		if getBool(ec.State, "progress_is_stale") {
			reason = fmt.Sprintf("Transcoding media (%s; progress temporarily stale)", st.Status)
			if p := getFloat(ec.State, "progress"); p > 0 {
				reason += fmt.Sprintf("; last known: %.1f%%", p)
			}
		}
		if st.Phase != "" {
			reason += "; phase: " + st.Phase
		}
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "transcode_complete", WaitingReason: reason, Outputs: meta}, nil
	case transcode.StatusFailed:
		mirrorTranscodeWorkerMetadata(ec, st)
		if getString(ec.Inputs, "mode") != "" {
			switch st.ReasonCode {
			case "insufficient_savings", "size_growth", "size_growth_exceeded", "quality_not_met", "preservation_not_met", "already_target_codec":
				ec.State["skip_transcode"] = true
				ec.State["reason_code"] = st.ReasonCode
				if st.ReasonCode == "size_growth_exceeded" {
					ec.State["reason_code"] = "size_growth"
				}
				out := transcodeWorkerMetadataOutputs(ec, st)
				out["skipped"], out["reason_code"] = true, getString(ec.State, "reason_code")
				return StepResult{Status: StepCompleted, Outputs: out}, nil
			}
		}
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
