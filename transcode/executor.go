package transcode

import (
	"context"
	"errors"
)

// AvailabilityExecutor separates cheap availability checks from optional deep
// diagnostics. Existing executors remain compatible with Executor.
type AvailabilityExecutor interface {
	Health(context.Context) error
	Ready(context.Context) error
}

// PublicationRetrier is implemented by workers that can publish a finished
// encode again after they stopped retrying on their own. It never encodes
// again. ErrPublicationRetryUnsupported means the worker predates it.
type PublicationRetrier interface {
	RetryPublication(ctx context.Context, jobID string) error
}

// ErrPublicationRetryUnsupported is a worker that has no retry-publication
// endpoint, such as one that has not been updated yet.
var ErrPublicationRetryUnsupported = errors.New("worker cannot retry a publication")

// ErrNothingToRetry is a worker that has no publication waiting for this job.
var ErrNothingToRetry = errors.New("worker has no publication to retry for this job")

// RetainedCandidateHandler is implemented by workers that keep a validated
// candidate on their own disk until it is wanted (publish_policy on_approval).
type RetainedCandidateHandler interface {
	// PublishRetained asks the worker to copy the retained candidate to the
	// share. It is idempotent: a job already publishing or published answers its
	// status. It returns immediately; the caller polls Status until the job is
	// completed.
	PublishRetained(ctx context.Context, jobID string) (JobStatus, error)
	// ReleaseRetained asks the worker to drop a retained candidate. A nil error
	// means the worker answered definitively, and the outcome says what happened.
	ReleaseRetained(ctx context.Context, jobID string) (ReleaseOutcome, error)
}

// ReleaseOutcome is what a release of a retained candidate came to.
type ReleaseOutcome string

const (
	// ReleaseDropped means nothing of the candidate is left on the worker: it was
	// released, had expired, had failed or was never known.
	ReleaseDropped ReleaseOutcome = "dropped"
	// ReleasePublished means the candidate is already on the share (the worker
	// finished a publication), so the file must be removed from the share.
	ReleasePublished ReleaseOutcome = "published"
	// ReleaseRefused means a publication has started and may already have changed
	// the share. Nothing was released; ask again once the job has completed.
	ReleaseRefused ReleaseOutcome = "refused"
)

// Errors a retained-candidate publish can answer. All are definitive.
var (
	// ErrDeferredPublicationUnsupported is a worker that predates deferred
	// publication. It must never be read as "the candidate is lost".
	ErrDeferredPublicationUnsupported = errors.New("worker does not support deferred publication")
	// ErrPublishHeld is a worker that refuses publication while it drains.
	// Nothing changed; ask again later.
	ErrPublishHeld = errors.New("worker is holding publication while it drains")
	// ErrNotRetained is a job that is not a retained candidate any more.
	ErrNotRetained = errors.New("job is not a retained candidate")
	// ErrRetainedCandidateChanged is a retained file that no longer matches its
	// attestation. It must be released and encoded again, never published.
	ErrRetainedCandidateChanged = errors.New("retained candidate no longer matches its attestation")
)

// Executor defines the generic interface for media transcoding execution.
// Navigatorr acts as the coordinator while Executor dispatches, inspects, and cancels work.
type Executor interface {
	Doctor(ctx context.Context) error
	Capabilities(ctx context.Context) (WorkerCapabilities, error)
	Submit(ctx context.Context, req Request) (Job, error)
	Status(ctx context.Context, jobID string) (JobStatus, error)
	Cancel(ctx context.Context, jobID string) error
}
