package transcodeworker

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

func schedulerDiagnostic(err error) *transcode.WorkerDiagnostic {
	// Public messages are allowlisted; raw errors can contain NAS paths, tokens
	// and URLs. Classification carries the useful diagnostic without the dump.
	class := string(resilience.ClassifyError(err))
	message := "Scheduler sweep failed; inspect worker storage and readiness."
	switch {
	case errors.Is(err, context.Canceled):
		class = "cancelled"
		message = "Scheduler sweep cancelled."
	case errors.Is(err, context.DeadlineExceeded):
		class = "timeout"
		message = "Scheduler sweep timed out."
	case errors.Is(err, os.ErrPermission):
		class = "storage_permission"
		message = "Scheduler cannot access its state storage."
	case errors.Is(err, os.ErrNotExist):
		class = "storage_missing"
		message = "Scheduler state storage is missing."
	}
	if class == "" || class == string(resilience.FFmpegUnknown) {
		class = "scheduler_sweep_failed"
	}
	return &transcode.WorkerDiagnostic{Class: class, Message: message}
}

func (w *Worker) runSchedulerSweeps(ctx context.Context, selfExe, configPath string) {
	for _, name := range []string{"post_encode", "queue"} {
		start := time.Now().UTC()
		var count int
		var err error
		if w.schedulerSweepHook != nil {
			err = w.schedulerSweepHook(name)
		} else if name == "post_encode" {
			count, err = w.ResumePostEncode(ctx, selfExe, configPath)
		} else {
			count, err = w.ScheduleQueued(ctx, selfExe, configPath)
		}
		w.recordSchedulerSweep(name, start, time.Now().UTC(), count, err)
	}
}
func (w *Worker) recordSchedulerSweep(name string, start, now time.Time, count int, err error) {
	w.schedulerMu.Lock()
	defer w.schedulerMu.Unlock()
	if w.schedulerHealth.Sweeps == nil {
		w.schedulerHealth.Sweeps = make(map[string]transcode.SchedulerSweepObservation)
	}
	sweep := w.schedulerHealth.Sweeps[name]
	previous := sweep.LastError
	sweep.ObservedAt = now
	sweep.StartedJobs = count
	sweep.DurationMs = durationMilliseconds(start, now)
	if err != nil {
		sweep.ConsecutiveErrors++
		sweep.LastError = schedulerDiagnostic(err)
		if previous == nil || previous.Class != sweep.LastError.Class {
			log.Printf("worker scheduler sweep=%s class=%s message=%s", name, sweep.LastError.Class, sweep.LastError.Message)
		}
	} else {
		sweep.LastSuccessAt = now
		sweep.ConsecutiveErrors = 0
		sweep.LastError = nil
		if previous != nil {
			log.Printf("worker scheduler sweep=%s recovered", name)
		}
	}
	w.schedulerHealth.Sweeps[name] = sweep
}
func (w *Worker) SchedulerObservation(now time.Time) transcode.SchedulerObservation {
	result := transcode.SchedulerObservation{Health: "unknown", Stale: true}
	if w == nil {
		return result
	}
	w.schedulerMu.Lock()
	defer w.schedulerMu.Unlock()
	result.Sweeps = make(map[string]transcode.SchedulerSweepObservation, len(w.schedulerHealth.Sweeps))
	staleAfter := 3 * w.schedulerInterval
	if staleAfter < 15*time.Second {
		staleAfter = 15 * time.Second
	}
	for _, name := range []string{"post_encode", "queue"} {
		sweep, exists := w.schedulerHealth.Sweeps[name]
		if !exists {
			continue
		}
		result.Sweeps[name] = sweep
		if result.ObservedAt.IsZero() || sweep.ObservedAt.Before(result.ObservedAt) {
			result.ObservedAt = sweep.ObservedAt
		}
		if sweep.LastError != nil {
			result.LastError = sweep.LastError
		}
		if result.LastSuccessAt.IsZero() || sweep.LastSuccessAt.Before(result.LastSuccessAt) {
			result.LastSuccessAt = sweep.LastSuccessAt
		}
	}
	if len(result.Sweeps) != 2 {
		return result
	}
	// Both sweeps must be observed and successful; one zero timestamp cannot
	// be concealed by the other's success.
	for _, sweep := range result.Sweeps {
		if sweep.LastSuccessAt.IsZero() {
			result.LastSuccessAt = time.Time{}
			break
		}
	}
	result.FreshUntil = result.ObservedAt.Add(staleAfter)
	age := now.Sub(result.ObservedAt).Seconds()
	if age < 0 {
		age = 0
	}
	result.ObservationAgeSeconds = &age
	if now.Sub(result.ObservedAt) > staleAfter || result.ObservedAt.After(now) {
		return result
	}
	result.Stale = false
	result.Health = "ok"
	if result.LastError != nil {
		result.Health = "degraded"
	}
	return result
}
