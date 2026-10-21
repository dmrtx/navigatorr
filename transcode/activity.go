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
