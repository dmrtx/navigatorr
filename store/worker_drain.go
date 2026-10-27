package store

import (
	"database/sql"
	"errors"
)

// WorkerDrain is the operator's switch that stops the server from admitting
// new worker jobs so a worker can be deployed while idle. It is one durable
// row, so a restart of the server keeps draining and only an explicit resume
// ends it. Revision counts changes, like the other settings. A missing row
// means drain was never used, which reads as off.
type WorkerDrain struct {
	Enabled   bool   `json:"enabled"`
	Since     string `json:"since,omitempty"`
	Revision  int64  `json:"revision"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// GetWorkerDrain returns the drain switch; off when it has never been set.
func (s *Store) GetWorkerDrain() (WorkerDrain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out WorkerDrain
	err := s.db.QueryRow(`SELECT enabled, since, revision, updated_at FROM worker_drain WHERE id=1`).Scan(&out.Enabled, &out.Since, &out.Revision, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerDrain{}, nil
	}
	if err != nil {
		return WorkerDrain{}, err
	}
	if !out.Enabled {
		out.Since = ""
	}
	return out, nil
}

// SetWorkerDrain turns drain on or off and bumps the revision. Turning it on
// while it is already on keeps the original start time, so repeating the
// request is harmless; the revision only moves on a real change.
func (s *Store) SetWorkerDrain(enabled bool) (WorkerDrain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var current WorkerDrain
	err := s.db.QueryRow(`SELECT enabled, since, revision, updated_at FROM worker_drain WHERE id=1`).Scan(&current.Enabled, &current.Since, &current.Revision, &current.UpdatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return WorkerDrain{}, err
	}
	if err == nil && current.Enabled == enabled {
		if !enabled {
			current.Since = ""
		}
		return current, nil
	}
	now := nowStr()
	since := ""
	if enabled {
		since = now
	}
	if _, err := s.db.Exec(`INSERT INTO worker_drain(id, enabled, since, revision, updated_at) VALUES(1, ?, ?, 1, ?)
		ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled, since=excluded.since, revision=worker_drain.revision+1, updated_at=excluded.updated_at`, enabled, since, now); err != nil {
		return WorkerDrain{}, err
	}
	var revision int64
	if err := s.db.QueryRow(`SELECT revision FROM worker_drain WHERE id=1`).Scan(&revision); err != nil {
		return WorkerDrain{}, err
	}
	return WorkerDrain{Enabled: enabled, Since: since, Revision: revision, UpdatedAt: now}, nil
}

// DrainActionRow is the projection of an unfinished worker action that the
// drain status classifies: only the fields it needs, read from the state
// without loading it whole.
type DrainActionRow struct {
	ID               string
	ActionName       string
	Status           string
	WaitingCondition string
	ParentID         string
	Worker           string // the assigned worker, '' before workers had names
	JobID            string
	ExternalRef      string
	Path             string
	Submitted        bool // transcode_submitted
	Reconcile        bool // transcode_reconcile: a submit may have been accepted
	Rejected         bool // transcode_submit_rejected
	Done             bool // transcode_done: the worker job ended
	RecoveryRequired bool // the encode finished and only saving it failed
	WorkerQueued     bool // transcode_status is queued: accepted by the worker, not encoding yet
	Uploading        bool // a promotion is having the worker copy a retained candidate to the share
}

// ListDrainActions returns the unfinished actions that run worker jobs, and the
// promotions that are having a worker copy a retained candidate to the share
// (every other promotion is listed too; the drain classifies it as nothing).
func (s *Store) ListDrainActions() ([]DrainActionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	flag := func(key string) string {
		return `COALESCE(json_extract(state_json, '$.` + key + `'), 0) IN (1, 'true')`
	}
	rows, err := s.db.Query(`SELECT id, action_name, status, waiting_condition,
		COALESCE(json_extract(inputs_json, '$.parent_action_id'), json_extract(state_json, '$.parent_action_id'), ''),
		COALESCE(json_extract(state_json, '$.worker'), json_extract(state_json, '$.upload_worker'), ''),
		COALESCE(json_extract(state_json, '$.job_id'), ''),
		COALESCE(json_extract(state_json, '$.external_reference'), ''),
		COALESCE(json_extract(state_json, '$.resolved_path'), json_extract(inputs_json, '$.path'), ''),
		` + flag("transcode_submitted") + `, ` + flag("transcode_reconcile") + `, ` + flag("transcode_submit_rejected") + `, ` + flag("transcode_done") + `, ` + flag("recovery_required") + `,
		COALESCE(json_extract(state_json, '$.transcode_status'), '') = 'queued',
		COALESCE(json_extract(state_json, '$.work_progress.phase'), '') = 'uploading'
		FROM action_instances
		WHERE status IN ('pending', 'running', 'waiting_external', 'waiting_decision')
		AND action_name IN ('transcode_media', 'clean_podcast_ads', 'promote_transcode_candidate')
		AND json_valid(state_json)
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DrainActionRow
	for rows.Next() {
		var r DrainActionRow
		if err := rows.Scan(&r.ID, &r.ActionName, &r.Status, &r.WaitingCondition, &r.ParentID, &r.Worker, &r.JobID, &r.ExternalRef, &r.Path,
			&r.Submitted, &r.Reconcile, &r.Rejected, &r.Done, &r.RecoveryRequired, &r.WorkerQueued, &r.Uploading); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountUnstartedBatchItems counts the files of unfinished batches that have
// no child action yet: work not admitted to any worker.
func (s *Store) CountUnstartedBatchItems() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM transcode_batch_items i
		JOIN action_instances a ON a.id = i.batch_id
		WHERE i.status = 'queued' AND i.child_action_id = ''
		AND a.status IN ('pending', 'running', 'waiting_external', 'waiting_decision')`).Scan(&n)
	return n, err
}
