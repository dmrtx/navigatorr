package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// WorkerSlotSettings is the operator's choice for one worker node: how many
// jobs it runs at once (Ceiling), how those slots are split between video
// and podcast work, how many x265 encodes may run at once (X265Slots) and how
// many of its CPUs encodes may use together (CoreBudget). 0/0 is the cleared
// split: the node shares one pool. Ceiling 0 follows the worker's own
// max_parallel_jobs; X265Slots 0 is no cap; CoreBudget 0 is every CPU. The row
// survives a clear so Revision keeps counting.
type WorkerSlotSettings struct {
	Node         string `json:"node"`
	VideoSlots   int    `json:"video_slots"`
	PodcastSlots int    `json:"podcast_slots"`
	Ceiling      int    `json:"ceiling"`
	X265Slots    int    `json:"x265_slots"`
	CoreBudget   int    `json:"core_budget"`
	Revision     int64  `json:"revision"`
	UpdatedAt    string `json:"updated_at"`
}

// Shared reports whether the node has no video/podcast split.
func (w WorkerSlotSettings) Shared() bool { return w.VideoSlots == 0 && w.PodcastSlots == 0 }

const workerSlotSettingsColumns = `node, video_slots, podcast_slots, ceiling, x265_slots, core_budget, revision, updated_at`

var ErrWorkerSlotSettingsChanged = errors.New("worker slot settings changed")

func scanWorkerSlotSettings(row *sql.Row) (WorkerSlotSettings, error) {
	var out WorkerSlotSettings
	err := row.Scan(&out.Node, &out.VideoSlots, &out.PodcastSlots, &out.Ceiling, &out.X265Slots, &out.CoreBudget, &out.Revision, &out.UpdatedAt)
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
// is no cap on x265 encodes; cores zero is no limit on the CPUs they use.
func (s *Store) SetWorkerSlotSettings(node string, video, podcast, ceiling, x265, cores int) (WorkerSlotSettings, error) {
	return s.setWorkerSlotSettings(node, video, podcast, ceiling, x265, cores, nil)
}

// CompareAndSwapWorkerSlotSettings stores settings only if their revision is
// still current; revision zero expects a node that has never been configured.
func (s *Store) CompareAndSwapWorkerSlotSettings(row WorkerSlotSettings) (WorkerSlotSettings, error) {
	return s.setWorkerSlotSettings(row.Node, row.VideoSlots, row.PodcastSlots, row.Ceiling, row.X265Slots, row.CoreBudget, &row.Revision)
}

func (s *Store) setWorkerSlotSettings(node string, video, podcast, ceiling, x265, cores int, revision *int64) (WorkerSlotSettings, error) {
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
	if cores < 0 {
		return WorkerSlotSettings{}, fmt.Errorf("cores must be positive, or zero to use them all")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision != nil {
		var current int64
		err := s.db.QueryRow(`SELECT revision FROM worker_slot_settings WHERE node=?`, node).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return WorkerSlotSettings{}, err
		}
		if current != *revision {
			return WorkerSlotSettings{}, ErrWorkerSlotSettingsChanged
		}
	}
	if _, err := s.db.Exec(`INSERT INTO worker_slot_settings(node, video_slots, podcast_slots, ceiling, x265_slots, core_budget, revision, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT(node) DO UPDATE SET video_slots=excluded.video_slots, podcast_slots=excluded.podcast_slots,
			ceiling=excluded.ceiling, x265_slots=excluded.x265_slots, core_budget=excluded.core_budget, revision=worker_slot_settings.revision+1, updated_at=excluded.updated_at`, node, video, podcast, ceiling, x265, cores, nowStr()); err != nil {
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

// RestoreWorkerSlotSettings undoes a refused save only while its revision is
// still current. A later save must survive the earlier worker's refusal.
func (s *Store) RestoreWorkerSlotSettings(node string, revision int64, previous *WorkerSlotSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous == nil {
		_, err := s.db.Exec(`DELETE FROM worker_slot_settings WHERE node=? AND revision=?`, node, revision)
		return err
	}
	_, err := s.db.Exec(`UPDATE worker_slot_settings SET video_slots=?, podcast_slots=?, ceiling=?, x265_slots=?, core_budget=?,
		revision=revision+1, updated_at=? WHERE node=? AND revision=?`, previous.VideoSlots, previous.PodcastSlots, previous.Ceiling, previous.X265Slots, previous.CoreBudget, nowStr(), node, revision)
	return err
}
