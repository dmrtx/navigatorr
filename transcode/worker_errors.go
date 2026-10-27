package transcode

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
