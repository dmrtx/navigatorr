package transcode

import "strings"

// Worker API error codes. The worker adds them to /v1 error bodies as the
// additive "code" field next to "error" and "retryable"; workers that predate
// the envelope send only "error", so clients must fall back to the status
// code and message for an empty code.
const (
	WorkerErrorBadRequest       = "bad_request"
	WorkerErrorUnauthorized     = "unauthorized"
	WorkerErrorNotFound         = "not_found"
	WorkerErrorMethodNotAllowed = "method_not_allowed"
	// WorkerErrorIdempotencyConflict (409) is definitive: the key already
	// names a job with a different execution spec. Never retry it.
	WorkerErrorIdempotencyConflict = "idempotency_conflict"
	// WorkerErrorBusy (429 with Retry-After) means the worker refused the
	// submit for lack of capacity. Nothing was created; retry later.
	WorkerErrorBusy = "worker_busy"
	// WorkerErrorAlreadyTerminal marks a 200 cancel acknowledgement for a job
	// that had already completed or failed: there is nothing left to cancel.
	WorkerErrorAlreadyTerminal = "already_terminal"
	// WorkerErrorNothingToRetry (409) answers a retry-publication request for
	// a job that is not waiting to publish. It is definitive.
	WorkerErrorNothingToRetry = "nothing_to_retry"
	// WorkerErrorHeld (503) answers a publish request while the drain freeze is
	// on. Nothing changed; repeat it after the freeze is lifted.
	WorkerErrorHeld = "worker_held"
	// WorkerErrorNotRetained (409) answers a publish request for a job that is
	// not a retained candidate (unknown policy, still encoding, released or
	// expired). It is definitive.
	WorkerErrorNotRetained = "not_retained"
	// WorkerErrorCandidateChanged (409) means the retained local file no longer
	// matches the size and SHA-256 the worker attested. Nothing was published and
	// the candidate must not be: release it and encode again.
	WorkerErrorCandidateChanged = "candidate_changed"
	// WorkerErrorReleaseRefused (409) answers a cancel of a retained job whose
	// publication has started and may already have changed the share.
	WorkerErrorReleaseRefused = "release_refused"
	WorkerErrorInternal       = "internal"
	WorkerErrorUnavailable    = "unavailable"
)

// RequestIDHeader carries a per-request correlation ID from Navigatorr to the
// worker, which echoes it and logs it with each request.
const RequestIDHeader = "X-Request-ID"

// JobIDHeader names the job a request concerns, so worker logs
// can be searched by job even for submits, whose ID travels in the body.
const JobIDHeader = "X-Navigatorr-Job-ID"

// Room refusals. A node never waits for room: a deferred submit it has no room
// for, or that arrives while it is paused or held by a build, is refused at once
// with the ordinary worker_busy answer (nothing was created) and this tag in the
// message. The coordinator reads the tag to tell "no room on this node" from "no
// free slot", and puts the conversion back in its own queue instead of waiting
// on that node. The tag is part of the message, not of the envelope, so workers
// and coordinators of any age keep treating the answer as busy.
const RoomRefusalTag = "no room here"

// The texts workers used for room refusals before the tag existed.
const (
	legacyRoomRefusalBudget = "retained candidates would use"
	legacyRoomRefusalDisk   = "free bytes would fall below"
)

// IsRoomRefusal reports a worker_busy refusal that came from the node's room
// limits, its pause or a build, not from a full set of slots.
func IsRoomRefusal(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "worker busy") {
		return false
	}
	// A worker from before the tag refuses with its retention texts; a mixed
	// fleet must requeue on those too instead of failing the conversion.
	return strings.Contains(msg, RoomRefusalTag) ||
		strings.Contains(msg, legacyRoomRefusalBudget) ||
		strings.Contains(msg, legacyRoomRefusalDisk)
}
