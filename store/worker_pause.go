package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Worker pause modes. A paused node admits no new job. Graceful lets the
// encodes already running finish; immediate sends them back to the transcode
// queue (see action/worker_pause.go).
const (
	WorkerPauseGraceful  = "graceful"
	WorkerPauseImmediate = "immediate"
)

// WorkerPause is the operator's switch that takes one node out of service so
// its CPU is free for other work. It is one durable row per paused node, so a
// restart of the server keeps the pause; only an explicit resume ends it.
type WorkerPause struct {
	Worker    string `json:"worker"`
	Mode      string `json:"mode"`
	Since     string `json:"since"`
	Revision  int64  `json:"revision"`
	UpdatedAt string `json:"updated_at"`
}

// ListWorkerPauses returns the paused nodes by worker name.
func (s *Store) ListWorkerPauses() (map[string]WorkerPause, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT worker, mode, since, revision, updated_at FROM worker_pauses`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WorkerPause{}
	for rows.Next() {
		var p WorkerPause
		if err := rows.Scan(&p.Worker, &p.Mode, &p.Since, &p.Revision, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out[p.Worker] = p
	}
	return out, rows.Err()
}

// SetWorkerPause pauses a node in a mode, or resumes it with an empty mode.
// Repeating the same request changes nothing. Changing the mode of a paused
// node keeps its start time; the revision moves on every real change.
func (s *Store) SetWorkerPause(worker, mode string) (WorkerPause, error) {
	if worker == "" {
		return WorkerPause{}, errors.New("worker name is required")
	}
	if mode != "" && mode != WorkerPauseGraceful && mode != WorkerPauseImmediate {
		return WorkerPause{}, fmt.Errorf("unknown pause mode %q", mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var current WorkerPause
	err := s.db.QueryRow(`SELECT worker, mode, since, revision, updated_at FROM worker_pauses WHERE worker=?`, worker).
		Scan(&current.Worker, &current.Mode, &current.Since, &current.Revision, &current.UpdatedAt)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return WorkerPause{}, err
	}
	if mode == "" {
		if found {
			if _, err := s.db.Exec(`DELETE FROM worker_pauses WHERE worker=?`, worker); err != nil {
				return WorkerPause{}, err
			}
		}
		return WorkerPause{}, nil
	}
	if found && current.Mode == mode {
		return current, nil
	}
	now := nowStr()
	since := now
	if found {
		since = current.Since
	}
	if _, err := s.db.Exec(`INSERT INTO worker_pauses(worker, mode, since, revision, updated_at) VALUES(?, ?, ?, 1, ?)
		ON CONFLICT(worker) DO UPDATE SET mode=excluded.mode, revision=worker_pauses.revision+1, updated_at=excluded.updated_at`, worker, mode, since, now); err != nil {
		return WorkerPause{}, err
	}
	var out WorkerPause
	err = s.db.QueryRow(`SELECT worker, mode, since, revision, updated_at FROM worker_pauses WHERE worker=?`, worker).
		Scan(&out.Worker, &out.Mode, &out.Since, &out.Revision, &out.UpdatedAt)
	return out, err
}

// ClearTranscodeBatchItemJob forgets the worker job and the worker of a batch
// item's child, for a child sent back to the transcode queue: the item is not
// on any worker until the child is assigned one again.
func (s *Store) ClearTranscodeBatchItemJob(childActionID string) error {
	if childActionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE transcode_batch_items SET job_id = '', worker = '', updated_at = ? WHERE child_action_id = ?`, nowStr(), childActionID)
	return err
}
