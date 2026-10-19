package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// WorkerSlotSettings is the operator's split of one worker node's slots
// between video and podcast work. 0/0 is the cleared state: the node shares
// one pool. The row survives a clear so Revision keeps counting.
type WorkerSlotSettings struct {
	Node         string `json:"node"`
	VideoSlots   int    `json:"video_slots"`
	PodcastSlots int    `json:"podcast_slots"`
	Revision     int64  `json:"revision"`
	UpdatedAt    string `json:"updated_at"`
}

// Shared reports whether the node has no split.
func (w WorkerSlotSettings) Shared() bool { return w.VideoSlots == 0 && w.PodcastSlots == 0 }

// GetWorkerSlotSettings returns the node's settings, or nil when it has never
// been configured.
func (s *Store) GetWorkerSlotSettings(node string) (*WorkerSlotSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out WorkerSlotSettings
	err := s.db.QueryRow(`SELECT node, video_slots, podcast_slots, revision, updated_at FROM worker_slot_settings WHERE node=?`, node).
		Scan(&out.Node, &out.VideoSlots, &out.PodcastSlots, &out.Revision, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetWorkerSlotSettings stores the node's split and bumps its revision. Both
// zero clears the split. Range checks against the worker's ceiling belong to
// the caller; the store only refuses values that no caller could mean.
func (s *Store) SetWorkerSlotSettings(node string, video, podcast int) (WorkerSlotSettings, error) {
	node = strings.TrimSpace(node)
	if node == "" {
		return WorkerSlotSettings{}, fmt.Errorf("node is required")
	}
	if video < 0 || podcast < 0 || (video == 0) != (podcast == 0) {
		return WorkerSlotSettings{}, fmt.Errorf("video and podcast slots must both be positive, or both zero to clear the split")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`INSERT INTO worker_slot_settings(node, video_slots, podcast_slots, revision, updated_at)
		VALUES(?, ?, ?, 1, ?)
		ON CONFLICT(node) DO UPDATE SET video_slots=excluded.video_slots, podcast_slots=excluded.podcast_slots,
			revision=worker_slot_settings.revision+1, updated_at=excluded.updated_at`, node, video, podcast, nowStr()); err != nil {
		return WorkerSlotSettings{}, err
	}
	var out WorkerSlotSettings
	err := s.db.QueryRow(`SELECT node, video_slots, podcast_slots, revision, updated_at FROM worker_slot_settings WHERE node=?`, node).
		Scan(&out.Node, &out.VideoSlots, &out.PodcastSlots, &out.Revision, &out.UpdatedAt)
	return out, err
}

// DeleteWorkerSlotSettings forgets a node's settings, returning it to never
// having been configured. Deleting a node with no row is not an error.
func (s *Store) DeleteWorkerSlotSettings(node string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM worker_slot_settings WHERE node=?`, strings.TrimSpace(node))
	return err
}
