package action

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

type observationFakeWorker struct {
	transcode.Executor
	calls       int
	observation transcode.SchedulerObservation
	offline     bool
	doctorErr   error
}

func (w *observationFakeWorker) Scheduler(context.Context) (transcode.SchedulerObservation, error) {
	w.calls++
	if w.offline {
		return transcode.SchedulerObservation{}, errors.New("transport unavailable")
	}
	return w.observation, nil
}
func (w *observationFakeWorker) Ready(context.Context) error {
	if w.offline {
		return errors.New("offline")
	}
	return nil
}
func (w *observationFakeWorker) Doctor(context.Context) error { return w.doctorErr }
func TestSharedWorkerObservationFreshnessAndDependencyRecovery(t *testing.T) {
	now := time.Now().UTC()
	worker := &observationFakeWorker{observation: transcode.SchedulerObservation{Health: "ok", ObservedAt: now.Add(-time.Minute)}}
	engine := NewEngine(EngineDeps{Transcode: worker})
	out := engine.WorkerObservation(context.Background())
	if out.Health != "unknown" || !out.Stale || !out.Reachable || !out.Ready {
		t.Fatal("old scheduler claim inferred progress", out)
	}
	worker.observation = transcode.SchedulerObservation{Health: "degraded", ObservedAt: now, FreshUntil: now.Add(time.Minute), LastError: &transcode.WorkerDiagnostic{Class: "storage_missing", Message: "Missing scheduler state."}}
	engine.workerObservation.CheckedAt = now.Add(-time.Minute)
	out = engine.WorkerObservation(context.Background())
	if out.Health != "degraded" || out.Stale || !out.Reachable {
		t.Fatal(out)
	}
	before := worker.calls
	cached := engine.WorkerObservation(context.Background())
	if worker.calls != before || !cached.CheckedAt.Equal(out.CheckedAt) {
		t.Fatal("shared read duplicated a probe", cached)
	}
	worker.offline = true
	engine.workerObservation.CheckedAt = now.Add(-time.Minute)
	out = engine.WorkerObservation(context.Background())
	if out.Reachable || out.Health != "unknown" || !out.Stale || !out.ObservedAt.Equal(now) {
		t.Fatal("outage discarded evidence or claimed scheduler stopped", out)
	}
	worker.offline = false
	worker.observation.LastError = nil
	worker.observation.Health = "ok"
	engine.workerObservation.CheckedAt = now.Add(-time.Minute)
	out = engine.WorkerObservation(context.Background())
	if out.Health != "ok" || out.LastError != nil {
		t.Fatal("recovery preserved false active alert", out)
	}
}
func TestWorkerObservationCancelledWaitIsBounded(t *testing.T) {
	engine := NewEngine(EngineDeps{})
	engine.workerObservationMu.Lock()
	defer engine.workerObservationMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	out := engine.WorkerObservation(ctx)
	if time.Since(start) > 100*time.Millisecond || out.Health != "unknown" || !out.Stale {
		t.Fatal("cancelled cache waiter blocked", out)
	}
}

type liveTimestampWorker struct{ transcode.Executor }

func (w liveTimestampWorker) Scheduler(context.Context) (transcode.SchedulerObservation, error) {
	now := time.Now().UTC()
	return transcode.SchedulerObservation{Health: "ok", ObservedAt: now, FreshUntil: now.Add(time.Minute)}, nil
}
func (w liveTimestampWorker) Ready(context.Context) error  { return nil }
func (w liveTimestampWorker) Doctor(context.Context) error { return nil }
func TestWorkerObservationUsesResponseTimeForFreshness(t *testing.T) {
	engine := NewEngine(EngineDeps{Transcode: liveTimestampWorker{}})
	out := engine.WorkerObservation(context.Background())
	if out.Health != "ok" || out.Stale || out.ObservedAt.After(out.CheckedAt) {
		t.Fatal("fresh worker sweep treated as future evidence", out)
	}
}

func (w *observationFakeWorker) Health(context.Context) error {
	if w.offline {
		return errors.New("offline")
	}
	return nil
}
func (w liveTimestampWorker) Health(context.Context) error { return nil }
