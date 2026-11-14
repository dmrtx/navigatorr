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

// RetentionRoom is what a worker can take now, measured the way it admits a
// job (see docs/transcode/DEFERRED_CANDIDATE_PUBLICATION.md, "Room"): the
// retention budget left, and the disk it could offer a job: free space plus the
// cached sources it would delete, less its free-disk floor and the reserves of
// encodes that have not written their candidates yet.
type RetentionRoom struct {
	ObservedAt time.Time `json:"observed_at"`
	// CapacityBytes is the worker's retained_candidates_max_bytes (0: none).
	CapacityBytes int64 `json:"capacity_bytes"`
	// RoomBytes is what is left of it, never negative.
	RoomBytes int64 `json:"room_bytes"`
	// HeldBytes is what finished candidates waiting for approval hold.
	HeldBytes int64 `json:"held_bytes,omitempty"`
	// DiskCapacityBytes is the size of the work disk; zero when not reported.
	DiskCapacityBytes int64 `json:"disk_capacity_bytes,omitempty"`
	// FloorBytes is the free space the worker keeps for itself.
	FloorBytes int64 `json:"floor_bytes,omitempty"`
	// DiskRoomBytes is what the disk can give a job now, never negative.
	DiskRoomBytes int64 `json:"disk_room_bytes,omitempty"`
	// Hold is "paused" while the node takes no new job (a pause or a
	// suspend) and "build" while a build holds it.
	Hold string `json:"hold,omitempty"`
}

// RetentionRoomMaxAge is how long a worker's report of its room is trusted.
// The worker takes it once per scheduler pass.
const RetentionRoomMaxAge = 45 * time.Second

// RoomNeed is what a conversion asks of a node: the source it copies in and the
// output it reserves. The node adds its own floor.
type RoomNeed struct {
	SourceBytes  int64
	ReserveBytes int64
	// Retained is true when the output waits on the node for approval, so it
	// also counts against the retention budget. A conversion that publishes on
	// encode (or may fall back to it) needs disk room only.
	Retained bool
}

// RoomVerdict says whether a node can take a conversion now.
type RoomVerdict int

const (
	// RoomFits: the node can take it, or says nothing (no report, an old one).
	RoomFits RoomVerdict = iota
	// RoomPaused: the node takes no new job.
	RoomPaused
	// RoomBuild: a build holds the node.
	RoomBuild
	// RoomApprovals: the retention budget is full; approving or discarding
	// finished candidates frees it.
	RoomApprovals
	// RoomNoSpace: the disk cannot give the job its room now.
	RoomNoSpace
	// RoomNever: not even an empty node could take it.
	RoomNever
)

// Fresh reports whether the report may be trusted at now. A report dated
// further ahead than it may be old is as untrustworthy as an old one: the
// worker's clock is off, and its age cannot be known.
func (r *RetentionRoom) Fresh(now time.Time) bool {
	if r == nil {
		return false
	}
	age := now.Sub(r.ObservedAt)
	return age <= RetentionRoomMaxAge && age >= -RetentionRoomMaxAge
}

// EverFits says whether an empty node of this report's size and budget could
// take the conversion. A report without sizes says yes.
func (r *RetentionRoom) EverFits(need RoomNeed) bool {
	if r == nil {
		return true
	}
	if r.DiskCapacityBytes > 0 && need.SourceBytes+need.ReserveBytes+r.FloorBytes > r.DiskCapacityBytes {
		return false
	}
	return !need.Retained || r.CapacityBytes <= 0 || need.ReserveBytes <= r.CapacityBytes
}

// Judge says whether the node that reported r can take need now, and for
// RoomNoSpace the bytes it can offer. A missing or old report says RoomFits: the
// node decides, and refuses at once when it must. RoomNever, too, is said only
// from a fresh report: an old one may describe a disk that has since changed.
func (r *RetentionRoom) Judge(need RoomNeed, now time.Time) (RoomVerdict, int64) {
	if r == nil {
		return RoomFits, 0
	}
	if !r.Fresh(now) {
		return RoomFits, 0
	}
	if !r.EverFits(need) {
		return RoomNever, 0
	}
	switch {
	case r.Hold == "paused":
		return RoomPaused, 0
	case r.Hold == "build":
		return RoomBuild, 0
	case need.Retained && r.CapacityBytes > 0 && need.ReserveBytes > r.RoomBytes:
		return RoomApprovals, r.RoomBytes
	case r.DiskCapacityBytes > 0 && need.SourceBytes+need.ReserveBytes > r.DiskRoomBytes:
		return RoomNoSpace, r.DiskRoomBytes
	}
	return RoomFits, 0
}

// OutputReserveBytes is the disk a conversion reserves for its output. A size
// goal reserves its byte budget; an audio-only goal (the video is copied) the
// most the size policy accepts, the source less its minimum saving; every
// other goal half the source. An output that grows past it is not compressing,
// and the job fails at that point. A plan that carries no size policy reserves
// half the source.
func OutputReserveBytes(plan *Plan, sourceBytes int64) int64 {
	if sourceBytes <= 0 {
		return 0
	}
	if plan != nil && plan.SizePolicy != nil {
		if IsSizeAudioOnlyPlan(plan) {
			if limit, ok := MaxAcceptedCandidateBytes(sourceBytes, plan.SizePolicy); ok && limit > 0 {
				return limit
			}
		} else if b := plan.SizePolicy.MaxCandidateBytes; b > 0 {
			return min(b, sourceBytes)
		}
	}
	return sourceBytes / 2
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
	// BuildsPaused is true while the server refuses new builds on this node.
	BuildsPaused bool `json:"builds_paused,omitempty"`
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
