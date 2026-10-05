package action

import (
	"context"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

type WorkerAdmissionError struct {
	Connected bool
	Message   string
}

func (e *WorkerAdmissionError) Error() string { return e.Message }

func RequiresWorker(name string) bool {
	return name == "transcode_media" || name == "transcode_batch" || name == "benchmark_transcode"
}

// CheckWorkerAdmission is shared by external adapters. A lost connection after
// admission is still handled by durable reconciliation, not by duplicating jobs.
func (e *Engine) CheckWorkerAdmission(ctx context.Context) error {
	if e.deps.Transcode == nil {
		return &WorkerAdmissionError{Message: "video worker is not configured; no job was submitted"}
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var err error
	if worker, ok := e.deps.Transcode.(transcode.AvailabilityExecutor); ok {
		err = worker.Ready(ctx)
	} else {
		_, err = e.deps.Transcode.Capabilities(ctx)
	}
	if err != nil {
		return &WorkerAdmissionError{Message: "video worker is offline or not ready; no job was submitted"}
	}
	if err := e.deps.Transcode.Doctor(ctx); err != nil {
		message := "video worker is connected but its checks failed; no job was submitted"
		if text := strings.ToLower(err.Error()); strings.Contains(text, "root") || strings.Contains(text, "smb") || strings.Contains(text, "storage") {
			message = "video worker is connected but cannot access its media storage; no job was submitted"
		}
		return &WorkerAdmissionError{Connected: true, Message: message}
	}
	return nil
}
