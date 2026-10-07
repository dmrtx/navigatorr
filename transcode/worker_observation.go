package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Scheduler evidence is independent from process liveness and admission checks.
// Missing/stale evidence means unknown, including older workers.
type WorkerDiagnostic struct {
	Class   string `json:"class"`
	Message string `json:"message"`
}
type SchedulerSweepObservation struct {
	ObservedAt        time.Time         `json:"observed_at,omitzero"`
	LastSuccessAt     time.Time         `json:"last_success_at,omitzero"`
	ConsecutiveErrors int               `json:"consecutive_errors"`
	LastError         *WorkerDiagnostic `json:"last_error,omitempty"`
	StartedJobs       int               `json:"started_jobs"`
	DurationMs        *int64            `json:"duration_ms,omitempty"`
}
type SchedulerObservation struct {
	FreshUntil            time.Time                            `json:"fresh_until,omitzero"`
	Health                string                               `json:"scheduler_health"`
	ObservedAt            time.Time                            `json:"observed_at,omitzero"`
	LastSuccessAt         time.Time                            `json:"last_success_at,omitzero"`
	ObservationAgeSeconds *float64                             `json:"observation_age_seconds,omitempty"`
	Stale                 bool                                 `json:"stale"`
	LastError             *WorkerDiagnostic                    `json:"last_error,omitempty"`
	Sweeps                map[string]SchedulerSweepObservation `json:"sweeps,omitempty"`
}
type WorkerObservation struct {
	SchedulerObservation
	Configured bool      `json:"configured"`
	Reachable  bool      `json:"reachable"`
	Ready      bool      `json:"ready"`
	CheckedAt  time.Time `json:"checked_at"`
	Message    string    `json:"message"`
}

// Scheduler probes the same bounded liveness response used by Health; it does
// not turn HTTP success into a claim that either scheduler sweep is healthy.
func (e *HTTPExecutor) Scheduler(ctx context.Context) (SchedulerObservation, error) {
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	req, err := e.newRequest(ctx, http.MethodGet, "/v1/health", nil)
	if err != nil {
		return SchedulerObservation{}, err
	}
	data, code, _, err := e.do(req, "health", "", false)
	if err != nil {
		return SchedulerObservation{}, err
	}
	if code != http.StatusOK {
		return SchedulerObservation{}, fmt.Errorf("worker health HTTP %d", code)
	}
	var body struct {
		OK        bool                 `json:"ok"`
		Scheduler SchedulerObservation `json:"scheduler"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return SchedulerObservation{}, err
	}
	if !body.OK {
		return SchedulerObservation{}, fmt.Errorf("worker liveness unconfirmed")
	}
	if body.Scheduler.Health != "ok" && body.Scheduler.Health != "degraded" {
		body.Scheduler.Health = "unknown"
	}
	if body.Scheduler.ObservedAt.IsZero() {
		body.Scheduler.Health = "unknown"
		body.Scheduler.Stale = true
	}
	return NormalizeSchedulerObservation(body.Scheduler, time.Now().UTC()), nil
}

// NormalizeSchedulerObservation derives freshness locally, so transport
// success and an old daemon response cannot conceal stale scheduler evidence.
func NormalizeSchedulerObservation(observation SchedulerObservation, now time.Time) SchedulerObservation {
	if observation.ObservedAt.IsZero() {
		observation.Health = "unknown"
		observation.Stale = true
		return observation
	}
	age := now.Sub(observation.ObservedAt).Seconds()
	if age < 0 {
		age = 0
	}
	observation.ObservationAgeSeconds = &age
	until := observation.FreshUntil
	if until.IsZero() {
		until = observation.ObservedAt.Add(15 * time.Second)
	}
	if observation.Stale || !now.Before(until) || observation.ObservedAt.After(now) {
		observation.Stale = true
		observation.Health = "unknown"
	}
	return observation
}
