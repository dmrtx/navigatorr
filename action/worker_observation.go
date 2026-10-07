package action

import (
	"context"
	"errors"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// WorkerObservation is shared by UI and MCP. A short cache collapses concurrent
// screen/tool reads into one observation; admission remains independently live.
func (e *Engine) WorkerObservation(ctx context.Context) transcode.WorkerObservation {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	for !e.workerObservationMu.TryLock() {
		select {
		case <-ctx.Done():
			return transcode.WorkerObservation{SchedulerObservation: transcode.SchedulerObservation{Health: "unknown", Stale: true}, CheckedAt: time.Now().UTC(), Message: "Worker observation unavailable."}
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer e.workerObservationMu.Unlock()
	now := time.Now().UTC()
	if !e.workerObservation.CheckedAt.IsZero() && now.Sub(e.workerObservation.CheckedAt) < 5*time.Second {
		out := e.workerObservation
		out.SchedulerObservation = transcode.NormalizeSchedulerObservation(out.SchedulerObservation, now)
		return out
	}
	out := transcode.WorkerObservation{SchedulerObservation: transcode.SchedulerObservation{Health: "unknown", Stale: true}, Configured: e.deps.Transcode != nil, CheckedAt: now, Message: "Configure a video worker before submitting jobs."}
	if !out.Configured {
		e.workerObservation = out
		return out
	}

	if worker, ok := e.deps.Transcode.(interface {
		Scheduler(context.Context) (transcode.SchedulerObservation, error)
	}); ok {
		observation, err := worker.Scheduler(ctx)
		if err == nil {
			out.Reachable = true
			out.SchedulerObservation = transcode.NormalizeSchedulerObservation(observation, time.Now().UTC())
		}
	} else if worker, ok := e.deps.Transcode.(transcode.AvailabilityExecutor); ok {
		out.Reachable = worker.Health(ctx) == nil
	}
	if err := e.CheckWorkerAdmission(ctx); err == nil {
		out.Ready = true
		out.Reachable = true
		out.Message = "Connected. Media storage and encoder checks passed."
	} else {
		out.Message = "Worker offline or not ready. New jobs are blocked."
		var admission *WorkerAdmissionError
		if errors.As(err, &admission) && admission.Connected {
			out.Reachable = true
			out.Message = "Connected, but media storage or encoder checks failed. New jobs are blocked."
		}
	}
	if !out.Reachable {
		if !e.workerObservation.ObservedAt.IsZero() {
			out.SchedulerObservation = e.workerObservation.SchedulerObservation
		}
		out.Health = "unknown"
		out.Stale = true
	}
	if out.Health == "degraded" {
		out.Message = "Worker reachable; scheduler is degraded. See sweep diagnostics."
	}
	out.CheckedAt = time.Now().UTC()
	e.workerObservation = out
	return out
}
