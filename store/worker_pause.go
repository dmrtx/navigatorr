package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Worker pause modes. A paused node admits no new job. Graceful lets the
// encodes already running finish; immediate sends them back to the transcode
// queue; suspend freezes them in place (see action/worker_pause.go and
// action/worker_suspend.go).
const (
	WorkerPauseGraceful  = "graceful"
	WorkerPauseImmediate = "immediate"
	// WorkerPauseSuspend freezes the encodes that are running on the node and
	// continues them where they were on resume.
	WorkerPauseSuspend = "suspend"
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
	if mode != "" && mode != WorkerPauseGraceful && mode != WorkerPauseImmediate && mode != WorkerPauseSuspend {
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

// BuildPause is the operator's switch that makes the server refuse new builds
// on one node. It is one durable row per node, so a restart keeps it, and it
// is independent of the transcode pause (WorkerPause).
type BuildPause struct {
	Worker    string `json:"worker"`
	Since     string `json:"since"`
	UpdatedAt string `json:"updated_at"`
}

// ListBuildPauses returns the nodes whose builds are paused, by worker name.
func (s *Store) ListBuildPauses() (map[string]BuildPause, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT worker, since, updated_at FROM build_pauses`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]BuildPause{}
	for rows.Next() {
		var p BuildPause
		if err := rows.Scan(&p.Worker, &p.Since, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out[p.Worker] = p
	}
	return out, rows.Err()
}

// BuildsPaused reports whether new builds on the node are refused.
func (s *Store) BuildsPaused(worker string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM build_pauses WHERE worker=?`, worker).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// SetBuildPause pauses (true) or resumes (false) new builds on a node.
// Repeating the same request changes nothing and keeps the start time.
func (s *Store) SetBuildPause(worker string, paused bool) error {
	if worker == "" {
		return errors.New("worker name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !paused {
		_, err := s.db.Exec(`DELETE FROM build_pauses WHERE worker=?`, worker)
		return err
	}
	now := nowStr()
	_, err := s.db.Exec(`INSERT INTO build_pauses(worker, since, updated_at) VALUES(?, ?, ?) ON CONFLICT(worker) DO NOTHING`, worker, now, now)
	return err
}
