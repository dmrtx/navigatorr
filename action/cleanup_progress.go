package action

import (
	"context"
	"time"
)

type promotionHashProgressKey struct{}
type promotionHashProgress func(string, int64, int64) error

// Cleanup observations are durable and read-only for clients. They never
// substitute for the finalizer's library, ownership or SHA-256 checks.
func (e *Engine) cleanupProgress(ctx context.Context, ec *ExecutionContext, phase, path string, read, total int64, errors ...string) error {
	progress := map[string]any{"phase": phase, "path": path, "bytes_read": read, "total_bytes": total, "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
	if len(errors) > 0 && errors[0] != "" {
		progress["error"] = errors[0]
	}
	ec.State["cleanup_progress"], ec.Outputs["cleanup_progress"] = progress, progress
	return e.persistExecutionState(ctx, ec)
}

func (e *Engine) observeCleanup(ctx context.Context, ec *ExecutionContext) context.Context {
	var last time.Time
	var previous string
	observer := promotionHashProgress(func(path string, read, total int64) error {
		if path == previous && read != 0 && read != total && time.Since(last) < time.Second {
			return nil
		}
		last, previous = time.Now(), path
		phase := "verifying_replacement"
		if p, err := loadPromotion(ec); err == nil && (path == p.BackupPath || path == p.BackupPath+".partial") {
			phase = "verifying_recovery"
		}
		return e.cleanupProgress(ctx, ec, phase, path, read, total)
	})
	return context.WithValue(ctx, promotionHashProgressKey{}, observer)
}
