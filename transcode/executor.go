package transcode

import "context"

// AvailabilityExecutor separates cheap availability checks from optional deep
// diagnostics. Existing executors remain compatible with Executor.
type AvailabilityExecutor interface {
	Health(context.Context) error
	Ready(context.Context) error
}

// Executor defines the generic interface for media transcoding execution.
// Navigatorr acts as the coordinator while Executor dispatches, inspects, and cancels work.
type Executor interface {
	Doctor(ctx context.Context) error
	Capabilities(ctx context.Context) (WorkerCapabilities, error)
	Submit(ctx context.Context, req Request) (Job, error)
	Status(ctx context.Context, jobID string) (JobStatus, error)
	Cancel(ctx context.Context, jobID string) error

	// Benchmark operations (Phase 4A)
	BenchmarkSubmit(ctx context.Context, req BenchmarkRequest) (BenchmarkJob, error)
	BenchmarkStatus(ctx context.Context, jobID string) (BenchmarkStatus, error)
	BenchmarkCancel(ctx context.Context, jobID string) error
}
