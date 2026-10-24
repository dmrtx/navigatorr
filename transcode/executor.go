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

// Executor defines the generic interface for media transcoding execution.
// Navigatorr acts as the coordinator while Executor dispatches, inspects, and cancels work.
type Executor interface {
	Doctor(ctx context.Context) error
	Capabilities(ctx context.Context) (WorkerCapabilities, error)
	Submit(ctx context.Context, req Request) (Job, error)
	Status(ctx context.Context, jobID string) (JobStatus, error)
	Cancel(ctx context.Context, jobID string) error
}
