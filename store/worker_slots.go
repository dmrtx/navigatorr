package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// WorkerSlotSettings is the operator's choice for one worker node: how many
// jobs it runs at once (Ceiling), how those slots are split between video
// and podcast work, and how many x265 encodes may run at once (X265Slots).
// 0/0 is the cleared split: the node shares one pool. Ceiling 0 follows the
// worker's own max_parallel_jobs; X265Slots 0 is no cap. The row survives a
// clear so Revision keeps counting.
type WorkerSlotSettings struct {
	Node         string `json:"node"`
	VideoSlots   int    `json:"video_slots"`
	PodcastSlots int    `json:"podcast_slots"`
	Ceiling      int    `json:"ceiling"`
	X265Slots    int    `json:"x265_slots"`
	Revision     int64  `json:"revision"`
	UpdatedAt    string `json:"updated_at"`
}

// Shared reports whether the node has no video/podcast split.
func (w WorkerSlotSettings) Shared() bool { return w.VideoSlots == 0 && w.PodcastSlots == 0 }

const workerSlotSettingsColumns = `node, video_slots, podcast_slots, ceiling, x265_slots, revision, updated_at`

func scanWorkerSlotSettings(row *sql.Row) (WorkerSlotSettings, error) {
	var out WorkerSlotSettings
	err := row.Scan(&out.Node, &out.VideoSlots, &out.PodcastSlots, &out.Ceiling, &out.X265Slots, &out.Revision, &out.UpdatedAt)
	return out, err
}

// GetWorkerSlotSettings returns the node's settings, or nil when it has never
// been configured.
func (s *Store) GetWorkerSlotSettings(node string) (*WorkerSlotSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, err := scanWorkerSlotSettings(s.db.QueryRow(`SELECT `+workerSlotSettingsColumns+` FROM worker_slot_settings WHERE node=?`, node))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetWorkerSlotSettings stores the node's split and ceiling and bumps its
// revision. Both split values zero clears the split; ceiling zero follows the
// worker's config. Range checks against the worker's limits belong to the
// caller; the store only refuses values that no caller could mean. x265 zero
// is no cap on x265 encodes.
func (s *Store) SetWorkerSlotSettings(node string, video, podcast, ceiling, x265 int) (WorkerSlotSettings, error) {
	node = strings.TrimSpace(node)
	if node == "" {
		return WorkerSlotSettings{}, fmt.Errorf("node is required")
	}
	if video < 0 || podcast < 0 || (video == 0) != (podcast == 0) {
		return WorkerSlotSettings{}, fmt.Errorf("video and podcast slots must both be positive, or both zero to clear the split")
	}
	if ceiling < 0 {
		return WorkerSlotSettings{}, fmt.Errorf("parallel jobs must be positive, or zero to follow the worker's config")
	}
	if x265 < 0 {
		return WorkerSlotSettings{}, fmt.Errorf("x265 slots must be positive, or zero for no limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`INSERT INTO worker_slot_settings(node, video_slots, podcast_slots, ceiling, x265_slots, revision, updated_at)
		VALUES(?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT(node) DO UPDATE SET video_slots=excluded.video_slots, podcast_slots=excluded.podcast_slots,
			ceiling=excluded.ceiling, x265_slots=excluded.x265_slots, revision=worker_slot_settings.revision+1, updated_at=excluded.updated_at`, node, video, podcast, ceiling, x265, nowStr()); err != nil {
		return WorkerSlotSettings{}, err
	}
	return scanWorkerSlotSettings(s.db.QueryRow(`SELECT `+workerSlotSettingsColumns+` FROM worker_slot_settings WHERE node=?`, node))
}

// DeleteWorkerSlotSettings forgets a node's settings, returning it to never
// having been configured. Deleting a node with no row is not an error.
func (s *Store) DeleteWorkerSlotSettings(node string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM worker_slot_settings WHERE node=?`, strings.TrimSpace(node))
	return err
}
