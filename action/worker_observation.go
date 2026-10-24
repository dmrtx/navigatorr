package action

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// WorkerObservation is shared by UI and MCP. Every worker is observed at once,
// each with its own short cache, so an offline worker never delays the others
// and concurrent screen/tool reads collapse into one observation per worker;
// admission remains independently live. The result summarises the workers and
// lists each one.
func (e *Engine) WorkerObservation(ctx context.Context) transcode.WorkerObservation {
	out := e.observeWorkers(ctx)
	if len(out.Workers) > 0 {
		drain := e.drainStatus(ctx, out)
		out.Drain = &drain
	}
	return out
}

func (e *Engine) observeWorkers(ctx context.Context) transcode.WorkerObservation {
	workers := e.deps.Workers.All()
	if len(workers) == 0 {
		return transcode.WorkerObservation{SchedulerObservation: transcode.SchedulerObservation{Health: "unknown", Stale: true}, CheckedAt: time.Now().UTC(), Message: "Configure a video worker before submitting jobs."}
	}
	list := make([]transcode.WorkerObservation, len(workers))
	var wg sync.WaitGroup
	for i, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			list[i] = e.workerObservation(ctx, w)
		}()
	}
	wg.Wait()
	if len(list) == 1 {
		out := list[0]
		out.Workers = list
		return out
	}
	return summariseWorkerObservations(list)
}

// summariseWorkerObservations is the summary of several workers: reachable and
// ready when any worker is, healthy when every worker is.
func summariseWorkerObservations(list []transcode.WorkerObservation) transcode.WorkerObservation {
	out := transcode.WorkerObservation{Configured: true, CheckedAt: time.Now().UTC(), Workers: list}
	health := "ok"
	var ready, notReady []string
	for _, w := range list {
		out.Reachable = out.Reachable || w.Reachable
		if w.Ready {
			ready = append(ready, w.Name)
		} else {
			notReady = append(notReady, w.Name)
		}
		switch {
		case w.Health == "degraded":
			health = "degraded"
		case w.Health != "ok" && health == "ok":
			health = "unknown"
		}
	}
	out.Ready = len(ready) > 0
	out.Health = health
	out.Stale = health == "unknown"
	switch {
	case len(notReady) == 0:
		out.Message = fmt.Sprintf("All %d workers are connected and ready.", len(list))
	case out.Ready:
		out.Message = fmt.Sprintf("%d of %d workers ready; not ready: %s. Their jobs wait for them.", len(ready), len(list), strings.Join(notReady, ", "))
	default:
		out.Message = "No worker is ready. New jobs are blocked."
		for _, w := range list {
			if w.AdmissionNote != "" {
				out.AdmissionNote = w.AdmissionNote
				break
			}
		}
	}
	return out
}

// workerObservation observes one worker, at most every five seconds.
func (e *Engine) workerObservation(ctx context.Context, w transcode.NamedWorker) transcode.WorkerObservation {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	st := e.workerStateFor(w.Name)
	for !st.obsMu.TryLock() {
		select {
		case <-ctx.Done():
			return transcode.WorkerObservation{SchedulerObservation: transcode.SchedulerObservation{Health: "unknown", Stale: true}, Name: w.Name, Podcast: w.Podcast, Configured: true, CheckedAt: time.Now().UTC(), Message: "Worker observation unavailable."}
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer st.obsMu.Unlock()
	now := time.Now().UTC()
	if !st.obs.CheckedAt.IsZero() && now.Sub(st.obs.CheckedAt) < 5*time.Second {
		out := st.obs
		out.SchedulerObservation = transcode.NormalizeSchedulerObservation(out.SchedulerObservation, now)
		return out
	}
	out := transcode.WorkerObservation{SchedulerObservation: transcode.SchedulerObservation{Health: "unknown", Stale: true}, Name: w.Name, Podcast: w.Podcast, Configured: true, CheckedAt: now}

	if worker, ok := w.Executor.(interface {
		Scheduler(context.Context) (transcode.SchedulerObservation, error)
	}); ok {
		observation, err := worker.Scheduler(ctx)
		if err == nil {
			out.Reachable = true
			out.SchedulerObservation = transcode.NormalizeSchedulerObservation(observation, time.Now().UTC())
		}
	} else if worker, ok := w.Executor.(transcode.AvailabilityExecutor); ok {
		out.Reachable = worker.Health(ctx) == nil
	}
	if err := checkWorkerAdmission(ctx, w.Executor); err == nil {
		out.Ready = true
		out.Reachable = true
		out.Message = "Connected. Media storage and encoder checks passed."
	} else {
		out.Message = "Worker offline or not ready. New jobs are blocked."
		var admission *WorkerAdmissionError
		if errors.As(err, &admission) && admission.Connected {
			out.Reachable = true
			out.Message = "Connected, but media storage or encoder checks failed. New jobs are blocked."
			out.AdmissionNote = transcode.AdmissionNoteDeepChecksFailed
			if admission.TimedOut {
				out.AdmissionNote = transcode.AdmissionNoteDeepChecksTimeout
			}
		}
	}
	if !out.Reachable {
		if !st.obs.ObservedAt.IsZero() {
			out.SchedulerObservation = st.obs.SchedulerObservation
		}
		out.Health = "unknown"
		out.Stale = true
	}
	if out.Health == "degraded" {
		out.Message = "Worker reachable; scheduler is degraded. See sweep diagnostics."
	}
	out.CheckedAt = time.Now().UTC()
	st.obs = out
	return out
}
