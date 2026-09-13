package transcode

import "context"

// Executor defines the generic interface for media transcoding execution.
// Navigatorr acts as the coordinator while Executor dispatches, inspects, and cancels work.
type Executor interface {
	Doctor(ctx context.Context) error
	Capabilities(ctx context.Context) (WorkerCapabilities, error)
	Submit(ctx context.Context, req Request) (Job, error)
	Status(ctx context.Context, jobID string) (JobStatus, error)
	Cancel(ctx context.Context, jobID string) error
}
