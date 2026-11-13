package transcode

import (
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
	// Retention is the room the worker has left to retain candidates. Nil from
	// a worker that does not report it or has no retention limit.
	Retention *RetentionRoom `json:"retention,omitempty"`
}

// RetentionRoom is how many bytes of candidates a worker can still retain,
// measured the way it admits a job when its slot comes up: the candidates it
// holds, and the source size reserved for each job that has started and not
// written its candidate yet. The free-disk floor is not part of it.
type RetentionRoom struct {
	ObservedAt time.Time `json:"observed_at"`
	// CapacityBytes is the worker's retained_candidates_max_bytes.
	CapacityBytes int64 `json:"capacity_bytes"`
	// RoomBytes is what is left of it, never negative.
	RoomBytes int64 `json:"room_bytes"`
}

// RetentionRoomMaxAge is how long a worker's report of its retention room is
// trusted. The worker takes it once per scheduler pass.
const RetentionRoomMaxAge = 45 * time.Second

// RetentionFit says whether a candidate of need bytes can be retained by the
// worker that reported r, as of now. A report that is missing or old says
// nothing: the candidate is taken to fit, as before workers reported it.
type RetentionFit int

const (
	// RetentionFits: the room is enough now, or unknown.
	RetentionFits RetentionFit = iota
	// RetentionFull: the budget could hold the candidate, but not with what the
	// worker holds now; room comes back when a candidate is approved or
	// discarded.
	RetentionFull
	// RetentionTooLarge: the candidate is larger than the whole budget.
	RetentionTooLarge
)

func (r *RetentionRoom) Fit(need int64, now time.Time) RetentionFit {
	if r == nil || need <= 0 || r.CapacityBytes <= 0 {
		return RetentionFits
	}
	// A report dated further ahead than it may be old is as untrustworthy as an
	// old one: the worker's clock is off, and its age cannot be known.
	if age := now.Sub(r.ObservedAt); age > RetentionRoomMaxAge || age < -RetentionRoomMaxAge {
		return RetentionFits
	}
	switch {
	case need > r.CapacityBytes:
		return RetentionTooLarge
	case need > r.RoomBytes:
		return RetentionFull
	}
	return RetentionFits
}

type WorkerObservation struct {
	SchedulerObservation
	// Name is the worker observed; empty on the summary of several workers.
	Name       string    `json:"name,omitempty"`
	Podcast    bool      `json:"podcast,omitempty"`
	Configured bool      `json:"configured"`
	Reachable  bool      `json:"reachable"`
	Ready      bool      `json:"ready"`
	CheckedAt  time.Time `json:"checked_at"`
	Message    string    `json:"message"`
	// AdmissionNote states what the readiness result means for job
	// submission when the deep checks failed or did not finish.
	AdmissionNote string `json:"admission_note,omitempty"`
	// Builds are the builds running or waiting on this worker's node, oldest first.
	Builds []NodeBuild `json:"builds,omitempty"`
	// Workers is each configured worker's own observation, in config order.
	// The fields above summarise them: reachable and ready when any worker is.
	Workers []WorkerObservation `json:"workers,omitempty"`
	// Drain is the worker drain state, on the summary only.
	Drain *DrainStatus `json:"drain,omitempty"`
}

// Admission notes are shared by diagnostics and the worker observation, so
// the web UI and MCP clients explain one admission gate the same way.
const (
	AdmissionNoteReadinessOnly     = "can_accept_jobs reflects worker readiness only. Job submission also checks media storage and encoders; use worker_health or check_deep=true for that full gate."
	AdmissionNoteDeepChecksFailed  = "The worker is ready, but its deep checks failed, and job submission runs the same checks: transcode jobs will be refused until the doctor error is fixed."
	AdmissionNoteDeepChecksTimeout = "The deep checks timed out, so job admission is unknown; check worker health again before submitting."
)

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
