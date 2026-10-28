package action

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// Deferred candidate publication, coordinator side (see
// docs/transcode/DEFERRED_CANDIDATE_PUBLICATION.md).
//
// A worker whose publish_policy is on_approval keeps a validated candidate on
// its own disk and copies it to the share only when a promotion asks. The
// attempt records where the candidate is in candidate_location: "share" (the
// only place it has ever been) or "worker". An attempt without the key is on
// the share, so nothing changes for a worker that publishes on encode.
//
// While a candidate is on the worker nothing exists at its candidate path. The
// action therefore trusts the worker's attestation (size and SHA-256) instead
// of a stat, and every place that decides a candidate can no longer be
// promoted releases it through one hook, retireCandidate, which asks the worker
// to drop it.
const (
	candidateLocationKey = "candidate_location"

	candidateOnShare  = "share"
	candidateOnWorker = "worker"

	publishPolicyKey            = "publish_policy"
	publishPolicyFallbackKey    = "publish_policy_fallback"
	publishRetentionWaitKey     = "publish_retention_wait"
	candidateRetainedAtKey      = "candidate_retained_at"
	candidateRetainedUntilKey   = "candidate_retained_expires_at"
	candidateExpiredCountKey    = "candidate_expired_count"
	promotionReleaseRetainedKey = "release_retained_candidate"

	// fallbackRetentionFull is the publish_policy_fallback of a batch child the
	// worker had no room to retain.
	fallbackRetentionFull = transcode.PublishFallbackRetentionFull

	// maxCandidateExpiries bounds how often one attempt encodes again because
	// the worker dropped its candidate at the end of its retention.
	maxCandidateExpiries = 2
)

// More reasons a candidate is retired.
const (
	candidateRetiredSkipped          = "skipped"
	candidateRetiredPromotionRefused = "promotion_refused"
	candidateRetiredExpired          = "expired"
	candidateWorkerReleased          = "released"
	errRetainedCandidateGone         = "the worker no longer holds this candidate (its retention ended or it was released); encode the file again"
)

// retainedLeaseWait is how long a release or an upload waits for the lease of
// the attempt whose candidate it handles; a variable so tests can shorten it.
var retainedLeaseWait = 10 * time.Second

// claimExecutionWithin is claimExecution that waits up to d for a busy lease.
// The lease itself is not bound to d.
func (e *Engine) claimExecutionWithin(ctx context.Context, id string, d time.Duration) (context.Context, func(), error) {
	deadline := time.Now().Add(d)
	for {
		lease, release, err := e.claimExecution(ctx, id, false)
		if !errors.Is(err, errActionBusy) || !time.Now().Before(deadline) {
			return lease, release, err
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// candidateIsOnWorker reports an attempt whose candidate is still on its
// worker.
func candidateIsOnWorker(state map[string]any) bool {
	return getString(state, candidateLocationKey) == candidateOnWorker
}

// retainedHandler is the retained-candidate interface of an executor.
func retainedHandler(ex transcode.Executor) (transcode.RetainedCandidateHandler, bool) {
	h, ok := ex.(transcode.RetainedCandidateHandler)
	return h, ok
}

// isBatchChild reports an attempt that belongs to a batch.
func isBatchChild(ec *ExecutionContext) bool {
	return getString(ec.Inputs, "parent_action_id") != ""
}

// publishPolicyFor decides, once, whether this attempt's candidate is kept on
// the worker. Deferral needs both: the assigned worker is configured
// publish_policy on_approval and it advertises deferred_publication. A batch
// child is an attempt like any other: the batch's replacement is approved as
// one plan, but each member is promoted by its own promotion, which uploads the
// candidate its worker still holds (see DEFERRED_CANDIDATE_PUBLICATION.md,
// "Batches"). The decision is stored with the attempt, so a resubmit after an
// uncertain submit sends what the first submit sent.
//
// An attempt that was refused for lack of room to retain (see
// spillRetentionRefusal) stays on publish-on-encode.
//
// A worker that advertises start_retention_admission applies its retention
// budget when the job's slot comes up, not at submit. A standalone attempt then
// asks it to wait in the queue for room (publish_retention_wait); a batch child
// does not, and the worker starts it to publish on encode if the budget is
// full (see recordPublishFallback).
//
// The result is "" (publish on encode, the default and the only value a worker
// that was not configured for deferral ever sees) or on_approval. A non-nil
// result must be returned by the caller.
func (e *Engine) publishPolicyFor(ctx context.Context, ec *ExecutionContext) (string, *StepResult) {
	if v := getString(ec.State, publishPolicyKey); v != "" {
		return v, nil
	}
	if getString(ec.State, publishPolicyFallbackKey) == fallbackRetentionFull {
		return "", nil
	}
	w, ok := e.deps.Workers.Get(e.resolveWorker(assignedWorker(ec.State)))
	if !ok || w.PublishPolicy != transcode.PublishPolicyOnApproval {
		return "", nil
	}
	if _, ok := retainedHandler(w.Executor); !ok {
		ec.State[publishPolicyFallbackKey] = "executor_cannot_release"
		return "", nil
	}
	caps, err := w.Executor.Capabilities(ctx)
	if err != nil {
		res := workerCapabilityLookupResult(err, "deferred publication")
		return "", &res
	}
	if !caps.DeferredPublication {
		// The worker has not been updated or enabled for it: publish on
		// encode, as before, and say so.
		ec.State[publishPolicyFallbackKey] = "worker_lacks_deferred_publication"
		log.Printf("action %s: worker %s is configured publish_policy on_approval but does not advertise deferred publication; publishing on encode", ec.InstanceID, w.Name)
		return "", nil
	}
	ec.State[publishPolicyKey] = transcode.PublishPolicyOnApproval
	if caps.StartRetentionAdmission && !isBatchChild(ec) {
		ec.State[publishRetentionWaitKey] = true
	}
	return transcode.PublishPolicyOnApproval, nil
}

// recordPublishFallback records that the worker started an on_approval job to
// publish on encode because its retention budget was full when the job's slot
// came up. publish_policy stays as it was sent, so a resubmit of the same job
// still matches its execution-spec digest; the candidate's location follows the
// job's status as for any job.
func recordPublishFallback(ec *ExecutionContext, st transcode.JobStatus) {
	if st.PublishFallback == "" || getString(ec.State, publishPolicyFallbackKey) == st.PublishFallback {
		return
	}
	ec.State[publishPolicyFallbackKey] = st.PublishFallback
	log.Printf("action %s: the worker had no room to retain another candidate when job %s started; it publishes on encode", ec.InstanceID, st.ID)
}

// spillRetentionRefusal handles a worker that refused a deferred submit because
// it has no room left to retain another candidate. A batch child must not wait
// for that room: nothing in a batch can be approved until every child has
// encoded, so the retained candidates that fill the worker would never leave,
// the remaining children would wait forever and the first candidates would
// expire and be encoded again. The child publishes on encode instead, as it did
// before deferral existed, and records why. A standalone attempt keeps waiting
// (the caller's busy handling): its owner approves or discards the candidates
// that fill the worker one by one.
//
// The worker's disk floor is not a retention refusal: a job that publishes on
// encode needs that disk too, so that answer stays an ordinary wait.
//
// It reports whether the submit should be sent again with publish on encode.
// The refused submit created no job (the worker may have left an empty job
// directory with no state in it), so changing the policy cannot collide with a
// job the worker already holds.
func spillRetentionRefusal(ec *ExecutionContext, req *transcode.Request, err error) bool {
	if !canSpillRetentionRefusal(ec, req, err) {
		return false
	}
	req.PublishPolicy = ""
	delete(ec.State, publishPolicyKey)
	ec.State[publishPolicyFallbackKey] = fallbackRetentionFull
	log.Printf("action %s: the worker has no room to retain another candidate (%v); this batch file publishes on encode", ec.InstanceID, err)
	return true
}

// canSpillRetentionRefusal says whether err is a refusal spillRetentionRefusal
// would act on, without acting on it.
func canSpillRetentionRefusal(ec *ExecutionContext, req *transcode.Request, err error) bool {
	return req.PublishPolicy == transcode.PublishPolicyOnApproval && isBatchChild(ec) && transcode.IsRetentionRefusal(err)
}

// retainedWaitResult finishes wait_transcode for a job whose encode is done and
// whose candidate stays on the worker. The job holds no process, so its slot is
// free; the action carries on with the worker's attestation in place of a file.
func (e *Engine) retainedWaitResult(ec *ExecutionContext, st transcode.JobStatus, jobID string) StepResult {
	mirrorTranscodeWorkerMetadata(ec, st)
	cand := st.CandidatePath
	if cand == "" {
		cand = getString(ec.State, "candidate_path")
	}
	if cand == "" {
		cand = getString(ec.State, "output_path")
	}
	if st.CandidateSizeBytes <= 0 || st.CandidateSHA256 == "" {
		return StepResult{Status: StepFailed, Error: "the worker retained a candidate without attesting its size and SHA-256 (fail closed)", Outputs: map[string]any{"job_id": jobID, "external_reference": jobID}}
	}
	ec.State["candidate_path"] = cand
	ec.State["output_path"] = cand
	ec.State[candidateLocationKey] = candidateOnWorker
	if len(st.Conversions) > 0 {
		ec.State["conversions"] = st.Conversions
	}
	out := map[string]any{"transcode_done": true, "candidate_path": cand, "output_path": cand, "job_id": jobID, "external_reference": jobID, candidateLocationKey: candidateOnWorker, publishPolicyKey: transcode.PublishPolicyOnApproval}
	if !st.RetainedAt.IsZero() {
		out[candidateRetainedAtKey] = st.RetainedAt.UTC().Format(time.RFC3339Nano)
	}
	if !st.RetainedExpiresAt.IsZero() {
		out[candidateRetainedUntilKey] = st.RetainedExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	for k, v := range transcodeWorkerMetadataOutputs(ec, st) {
		out[k] = v
	}
	if len(st.Conversions) > 0 {
		out["conversions"] = st.Conversions
	}
	return StepResult{Status: StepCompleted, Outputs: out}
}

// retainedExpiredResult sends an attempt whose retained candidate the worker
// dropped back to encode the same job again. It is bounded: a worker that
// keeps dropping candidates is a configuration problem, not a transient one.
func (e *Engine) retainedExpiredResult(ec *ExecutionContext, jobID string) StepResult {
	count := getInt(ec.State, candidateExpiredCountKey) + 1
	if count > maxCandidateExpiries {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("the worker dropped the retained candidate of job %s %d times; check its retention settings before encoding again", jobID, count), Outputs: map[string]any{"job_id": jobID, "external_reference": jobID}}
	}
	for _, key := range transcodeAttemptKeys {
		delete(ec.State, key)
		delete(ec.Outputs, key)
	}
	ec.State[candidateExpiredCountKey] = count
	ec.State["job_id"], ec.State["external_reference"] = jobID, jobID
	appendFailureHistory(ec, "retained_candidate", "expired", fmt.Sprintf("the worker dropped the retained candidate of job %s; encoding it again (%d of %d)", jobID, count, maxCandidateExpiries))
	mergeMap(ec.Outputs, map[string]any{"job_id": jobID, "external_reference": jobID, candidateExpiredCountKey: count})
	return StepResult{Status: StepCompleted, RewindTo: "submit_transcode", Outputs: map[string]any{candidateExpiredCountKey: count, "plan_note": "The worker's retention of the candidate ended; encoding it again."}}
}

// releaseRetainedOn asks the attempt's worker to drop its retained candidate.
func (e *Engine) releaseRetainedOn(ctx context.Context, ec *ExecutionContext) (transcode.ReleaseOutcome, error) {
	jobID := getString(ec.State, "job_id")
	if jobID == "" {
		return "", fmt.Errorf("the attempt has no recorded job to release")
	}
	if !e.workersConfigured() {
		return "", fmt.Errorf("no transcode worker is configured to release job %s", jobID)
	}
	h, ok := retainedHandler(e.actionWorker(ec))
	if !ok {
		return "", fmt.Errorf("the worker that holds job %s is not configured or cannot release a retained candidate", jobID)
	}
	return h.ReleaseRetained(ctx, jobID)
}

type promotionReleaseCtxKey struct{}

// removeRetainedCandidate is removeOwnCandidate for a candidate that is on its
// worker: the worker drops it and nothing on the share is touched. A worker
// that already published it (the candidate is then on the share) gets the
// regular removal, unless a promotion released it: that file is the
// promotion's, and stays.
func (e *Engine) removeRetainedCandidate(ctx context.Context, inst *store.ActionInstance, ec *ExecutionContext, pruneDir bool) (candidateRemoval, error) {
	var removal candidateRemoval
	candidate, _, err := ownCandidateFiles(ec.State)
	if err != nil {
		return removal, err
	}
	byPromotion, _ := ctx.Value(promotionReleaseCtxKey{}).(bool)
	if !byPromotion {
		if err := e.candidatePromotionStarted(inst, ec, candidate); err != nil {
			return removal, err
		}
	}
	outcome, err := e.releaseRetainedOn(ctx, ec)
	if err != nil {
		return removal, fmt.Errorf("the retained candidate could not be released on its worker: %v", err)
	}
	switch outcome {
	case transcode.ReleaseDropped:
		// A copy to the share that was asked for and failed can leave the
		// attempt's own files there. They are removed through the same guards,
		// and a clean share costs two stats.
		swept, err := e.removeOwnCandidateFromShare(ctx, inst, ec, pruneDir)
		swept.Worker = candidateWorkerReleased
		return swept, err
	case transcode.ReleasePublished:
		// The worker finished a publication, so the file is on the share now.
		// Whoever released it has no use for it, and the removal runs while the
		// attempt still says "worker": a promotion that ended is not a reference
		// to keep it for, an active one is.
		removal, err := e.removeOwnCandidateFromShare(ctx, inst, ec, pruneDir)
		ec.State[candidateLocationKey] = candidateOnShare
		ec.Outputs[candidateLocationKey] = candidateOnShare
		return removal, err
	}
	return removal, fmt.Errorf("the worker is copying the candidate to the share; release it again once that finishes")
}

// retainedGoneError is a retained candidate that can never be uploaded: the
// plan that finds it releases what is left of it.
type retainedGoneError struct{ msg string }

func (e *retainedGoneError) Error() string { return e.msg }

// planRetainedCandidate asks the worker, at plan time, whether the candidate a
// promotion would replace the original with can still be uploaded. It reports
// retained=true when the file is not on the share yet; a candidate the worker
// already published is promoted like any other.
func (e *Engine) planRetainedCandidate(ctx context.Context, source *ExecutionContext, p *promotionState) (retained bool, err error) {
	sha, size := getString(source.State, "candidate_sha256"), getInt64(source.State, "candidate_size_bytes")
	if sha == "" || size <= 0 {
		return false, &retainedGoneError{"the retained candidate has no worker attestation (size and SHA-256); encode the file again"}
	}
	jobID := getString(source.State, "job_id")
	st, serr := e.actionWorker(source).Status(ctx, jobID)
	switch {
	case serr != nil && isRetryableWorkerPollError(serr):
		return false, fmt.Errorf("the worker that holds the candidate cannot be reached: %v", serr)
	case serr != nil && workerJobUnknown(serr):
		return false, &retainedGoneError{errRetainedCandidateGone}
	case serr != nil:
		return false, fmt.Errorf("the candidate's worker could not be asked: %v", serr)
	case st.Status == transcode.StatusExpired:
		return false, &retainedGoneError{errRetainedCandidateGone}
	case st.Status == transcode.StatusCompleted:
		return false, nil
	case st.Status == transcode.StatusRunning && (st.Retained || st.FinalizationState == "finalizing"):
		p.CandidateRetained = true
		p.CandidateSHA, p.CandidateBytes = sha, size
		return true, nil
	case st.Status == transcode.StatusFailed || st.RecoveryRequired:
		// A copy that was asked for ended badly. That says nothing about
		// whether the candidate can still be had, and the worker may have left
		// bytes on the share: the owner decides, so nothing is released here.
		return false, fmt.Errorf("the worker reports the candidate's job as %q (%s); try the promotion again, or discard the candidate and encode the file again", st.Status, strings.TrimSpace(st.Error))
	}
	return false, &retainedGoneError{fmt.Sprintf("the worker reports the candidate's job as %q, so it cannot be uploaded; encode the file again", st.Status)}
}

// uploadRetainedCandidate has the worker copy the retained candidate to the
// share. It runs inside preserve_original, before anything reads or moves the
// original, and it is idempotent: every run asks for the publication again and
// the worker answers the same job. A copy that is still going is a wait, not a
// loop: the scheduler asks again, and neither this executor nor the promotion's
// lease is held for as long as the copy sits behind encodes on the worker.
//
// The wait keeps the promotion's progress at "uploading", which is what keeps a
// worker deploy from starting while the copy runs (retained candidates that
// are not being uploaded never block one).
func (e *Engine) uploadRetainedCandidate(ctx context.Context, ec *ExecutionContext, p *promotionState) (StepResult, bool, error) {
	source, err := e.deps.Store.GetActionInstance(p.SourceActionID)
	if err != nil || source == nil {
		return promoteFailedResult(fmt.Errorf("the transcode action could not be read: %v", err)), true, nil
	}
	sec := parseExecutionContext(source, e)
	if !candidateIsOnWorker(sec.State) {
		// An earlier run saw the copy finish and recorded it on the attempt,
		// then stopped before recording it here. The worker may have forgotten
		// the job since, so it is not asked again.
		return StepResult{}, false, nil
	}
	jobID := getString(sec.State, "job_id")
	worker := e.resolveWorker(assignedWorker(sec.State))
	h, ok := retainedHandler(e.actionWorker(sec))
	if !ok {
		return promoteFailedResult(fmt.Errorf("the worker that holds the candidate is not configured or cannot publish a retained candidate")), true, nil
	}
	ec.State["upload_worker"] = worker
	ec.State["upload_job_id"] = jobID
	ec.State["work_progress"] = uploadingProgress(p)
	if err := e.persistExecutionState(ctx, ec); err != nil {
		return promoteFailedResult(err), true, nil
	}
	st, err := h.PublishRetained(ctx, jobID)
	if err != nil {
		return e.uploadFailure(err, p), true, nil
	}
	switch {
	case st.Status == transcode.StatusCompleted:
		if err := e.finishRetainedUpload(ctx, ec, p, source); err != nil {
			if errors.Is(err, errActionBusy) {
				return uploadingWait(p, "The transcode action is busy; the upload is recorded when it is free"), true, nil
			}
			return promoteFailedResult(err), true, nil
		}
		return StepResult{}, false, nil
	case st.Status == transcode.StatusExpired:
		return promoteFailedFinal(errors.New(errRetainedCandidateGone)), true, nil
	case st.Status == transcode.StatusFailed || st.Status == transcode.StatusCancelled:
		msg := st.Error
		if msg == "" {
			msg = "the worker ended the job"
		}
		return promoteFailedResult(fmt.Errorf("the worker could not copy the candidate to the share (%s); try the promotion again, or encode the file again", msg)), true, nil
	case st.RecoveryRequired:
		return promoteFailedResult(fmt.Errorf("the worker could not copy the candidate to the share and needs intervention: %s", st.Error)), true, nil
	}
	return uploadingWait(p, "The worker is copying the candidate to the share; the replacement continues when it is there"), true, nil
}

// uploadingProgress is the promotion's progress while its candidate is copied.
func uploadingProgress(p *promotionState) map[string]any {
	return map[string]any{"phase": "uploading", "total_bytes": p.CandidateBytes, "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
}

// uploadingWait waits for a copy that may be under way. Its result carries the
// progress marker again: the step drops its own progress when it returns, and
// a deploy must keep seeing the upload for as long as the wait lasts.
func uploadingWait(p *promotionState, reason string) StepResult {
	res := promoteWaitResult(reason)
	res.Outputs = map[string]any{"work_progress": uploadingProgress(p)}
	return res
}

// uploadFailure maps a worker's answer to a publish request to the promotion's
// result. Nothing here releases the candidate unless the worker says it is no
// longer what was attested.
func (e *Engine) uploadFailure(err error, p *promotionState) StepResult {
	switch {
	case errors.Is(err, transcode.ErrPublishHeld):
		return promoteWaitResult("The worker is draining and holds publication; the replacement continues when it accepts work again")
	case errors.Is(err, transcode.ErrDeferredPublicationUnsupported):
		return promoteFailedResult(fmt.Errorf("the worker was not updated for deferred publication, so it cannot upload this candidate; update the worker and promote again"))
	case errors.Is(err, transcode.ErrNotRetained), workerJobUnknown(err):
		return promoteFailedFinal(errors.New(errRetainedCandidateGone))
	case errors.Is(err, transcode.ErrRetainedCandidateChanged):
		return promoteFailedFinal(errors.New("the retained candidate no longer matches what the worker attested; it was not published. Encode the file again"))
	case httpclient.IsTransportUncertain(err), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The request may have arrived and the copy may be under way.
		return uploadingWait(p, "The worker holding the candidate cannot be reached; the upload is retried automatically")
	}
	return promoteFailedResult(fmt.Errorf("the worker refused to upload the candidate: %v", err))
}

// finishRetainedUpload records, once the file is on the share, that the
// candidate is there. The attempt's location is written before the original is
// touched, so a failure from here on is handled as the failure of a candidate
// on the share.
func (e *Engine) finishRetainedUpload(ctx context.Context, ec *ExecutionContext, p *promotionState, source *store.ActionInstance) error {
	lease, release, err := e.claimExecutionWithin(ctx, source.ID, retainedLeaseWait)
	if err != nil {
		return fmt.Errorf("the transcode action could not be updated after the upload: %w", err)
	}
	defer release()
	fresh, err := e.deps.Store.GetActionInstance(source.ID)
	if err != nil || fresh == nil {
		return fmt.Errorf("the transcode action could not be read after the upload: %v", err)
	}
	sec := parseExecutionContext(fresh, e)
	sec.State[candidateLocationKey], sec.Outputs[candidateLocationKey] = candidateOnShare, candidateOnShare
	fresh.StateJSON, fresh.OutputsJSON = toJSON(sec.State), toJSON(sec.Outputs)
	if err := e.updateInstance(lease, fresh); err != nil {
		return fmt.Errorf("recording the uploaded candidate: %w", err)
	}
	return nil
}

// inspectUploadedCandidate holds the file that arrived on the share to what the
// worker attested, and runs the full inspection a promotion of a published
// candidate runs at plan time. Only then is the upload recorded as done.
func (e *Engine) inspectUploadedCandidate(ctx context.Context, ec *ExecutionContext, p *promotionState) error {
	progress := &promotionPreserveProgress{e: e, ec: ec, p: p}
	sha, n, err := e.promotionHash(context.WithValue(ctx, promotionHashProgressKey{}, progress.hashes(ctx)), p.CandidatePath)
	if err != nil {
		return err
	}
	if sha != p.CandidateSHA || n != p.CandidateBytes {
		return fmt.Errorf("the uploaded candidate does not match what its worker attested; promotion blocked")
	}
	source, err := e.deps.Store.GetActionInstance(p.SourceActionID)
	if err != nil || source == nil {
		return fmt.Errorf("the transcode action could not be read: %v", err)
	}
	validation, err := e.validateCandidateDetailed(ctx, parseExecutionContext(source, e))
	if err != nil {
		return err
	}
	if validation.Status != StepCompleted {
		return fmt.Errorf("candidate failed promotion validation: %s %s", validation.Error, validation.WaitingReason)
	}
	p.CandidateUploaded = true
	return e.savePromotion(ctx, ec, p)
}

// releaseRetainedForPromotion releases the retained candidate behind a
// promotion that ended for good: it was rejected, or its original changed. A
// candidate the promotion already had uploaded is on the share and stays.
func (e *Engine) releaseRetainedForPromotion(ctx context.Context, ec *ExecutionContext, why string) {
	sourceID := getString(ec.Inputs, "transcode_action_id")
	if sourceID == "" {
		return
	}
	lease, release, err := e.claimExecutionWithin(ctx, sourceID, retainedLeaseWait)
	if err != nil {
		log.Printf("action %s: retained candidate of %s kept: %v", ec.InstanceID, sourceID, err)
		return
	}
	defer release()
	source, err := e.deps.Store.GetActionInstanceIfExists(sourceID)
	if err != nil || source == nil || source.ActionName != "transcode_media" {
		return
	}
	sec := parseExecutionContext(source, e)
	if !candidateIsOnWorker(sec.State) || getBool(sec.State, candidateRemovedKey) {
		return
	}
	e.retireCandidate(context.WithValue(lease, promotionReleaseCtxKey{}, true), source, sec, why)
}

// releaseRetainedChild releases the retained candidate of a finished batch
// child. Engine.cancel refuses a completed action, so it cannot reach one. A
// candidate it could not release stays on its worker and is listed as a
// leftover to discard; the error says why, for the caller to report. Nothing
// else depends on it: the decision that led here is already durable.
func (e *Engine) releaseRetainedChild(ctx context.Context, id, why string) error {
	lease, release, err := e.claimExecutionWithin(ctx, id, retainedLeaseWait)
	if err != nil {
		log.Printf("action %s: retained candidate kept: %v", id, err)
		return fmt.Errorf("action %s: retained candidate kept: %v", id, err)
	}
	defer release()
	child, err := e.deps.Store.GetActionInstanceIfExists(id)
	if err != nil || child == nil || child.ActionName != "transcode_media" {
		return nil
	}
	ec := parseExecutionContext(child, e)
	if !candidateIsOnWorker(ec.State) || getBool(ec.State, candidateRemovedKey) {
		return nil
	}
	e.retireCandidate(lease, child, ec, why)
	if cleanup, _ := ec.State[candidateCleanupKey].(map[string]any); getString(cleanup, "outcome") == "kept" {
		return fmt.Errorf("action %s: retained candidate kept: %s", id, getString(cleanup, "detail"))
	}
	return nil
}

func promoteFailedResult(err error) StepResult {
	res, _ := promoteFailed(err)
	return res
}

func promoteWaitResult(reason string) StepResult {
	res, _ := promoteWait(reason)
	return res
}

// promoteFailedFinal is a failure nothing will retry: the candidate can no
// longer be promoted, so the engine releases it once the failure is durable.
func promoteFailedFinal(err error) StepResult {
	res := promoteFailedResult(err)
	res.Outputs = map[string]any{promotionReleaseRetainedKey: true}
	return res
}
