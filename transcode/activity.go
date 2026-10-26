package transcode

import (
	"time"
)

type WorkerActivityJob struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	File           string    `json:"file"`
	Phase          string    `json:"phase"`
	StartedAt      time.Time `json:"started_at"`
	HeartbeatAt    time.Time `json:"heartbeat_at"`
	LastProgressAt time.Time `json:"last_progress_at"`
	Progress       *float64  `json:"progress,omitempty"`
	// Navigatorr fills these in from the job's action; a worker does not send
	// them. Encoding names the encode being run ("x265 CRF 22"), Speed is the
	// encoder's running speed against real time.
	// ActionID is the job to open for this slot, when its action is known.
	ActionID string   `json:"action_id,omitempty"`
	Encoding string   `json:"encoding,omitempty"`
	Speed    *float64 `json:"speed,omitempty"`
	FPS      *float64 `json:"fps,omitempty"`
}
type WorkerActivity struct {
	SlotsTotal       int                 `json:"worker_slots_total"`
	SlotsUsed        int                 `json:"worker_slots_used"`
	Jobs             []WorkerActivityJob `json:"jobs"`
	CheckedAt        time.Time           `json:"checked_at"`
	DetailsAvailable bool                `json:"details_available"`
	// Slots divides the totals above between video and podcast work. Absent
	// from workers that predate per-kind slots.
	Slots *WorkerSlots `json:"slots,omitempty"`
}
