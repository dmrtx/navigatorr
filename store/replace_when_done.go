package store

import (
	"database/sql"
	"errors"
)

// ReplaceWhenDoneDefaultKey names the setting a job submitted without
// replace_when_done takes its value from.
const ReplaceWhenDoneDefaultKey = "replace_when_done_default"

// SetReplaceWhenDone records that a job or batch replaces each finished file
// by itself (or no longer does), overriding the choice it was submitted with.
func (s *Store) SetReplaceWhenDone(actionID string, value bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := 0
	if value {
		v = 1
	}
	_, err := s.db.Exec(`INSERT INTO action_replace_when_done(action_id, value, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(action_id) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, actionID, v, nowStr())
	return err
}

// GetReplaceWhenDone is the changed value of one job, ok false when it was
// never changed after submission.
func (s *Store) GetReplaceWhenDone(actionID string) (value, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var v int
	err = s.db.QueryRow(`SELECT value FROM action_replace_when_done WHERE action_id=?`, actionID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return v == 1, true, nil
}

// ListReplaceWhenDone is every changed value, by action id.
func (s *Store) ListReplaceWhenDone() (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT action_id, value FROM action_replace_when_done`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		var v int
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v == 1
	}
	return out, rows.Err()
}

// ReplaceWhenDoneDefault is the value a job submitted without
// replace_when_done takes: false until it is set. It is kept apart from the
// preferences, which agents can write through memory_set; only
// PUT /api/maintenance/preferences changes it.
func (s *Store) ReplaceWhenDoneDefault() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var v string
	err := s.db.QueryRow(`SELECT value FROM replace_settings WHERE key=?`, ReplaceWhenDoneDefaultKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return v == "true", err
}

// SetReplaceWhenDoneDefault stores the default.
func (s *Store) SetReplaceWhenDoneDefault(value bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw := "false"
	if value {
		raw = "true"
	}
	_, err := s.db.Exec(`INSERT INTO replace_settings(key, value, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, ReplaceWhenDoneDefaultKey, raw, nowStr())
	return err
}

// ReplacementApproval is the approval a file's replacement carries before
// its plan exists.
type ReplacementApproval struct {
	TranscodeActionID string
	Approval          string
	PromoteActionID   string // the one promotion that can use it
	Used              bool   // that promotion went ahead on it
}

// GrantReplacementApproval records a file's approval for one promotion,
// replacing any earlier one. Only that promotion can use it.
func (s *Store) GrantReplacementApproval(transcodeID, approval, promoteID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO replacement_approvals(transcode_action_id, approval, promote_action_id, used, created_at) VALUES(?, ?, ?, 0, ?)
		ON CONFLICT(transcode_action_id) DO UPDATE SET approval=excluded.approval, promote_action_id=excluded.promote_action_id, used=0, created_at=excluded.created_at`, transcodeID, approval, promoteID, nowStr())
	return err
}

// RevokeReplacementApproval drops a file's approval held by one promotion:
// its plan was rejected, it failed before its approval, or it never started.
func (s *Store) RevokeReplacementApproval(transcodeID, promoteID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM replacement_approvals WHERE transcode_action_id=? AND promote_action_id=?`, transcodeID, promoteID)
	return err
}

// GetReplacementApproval is a file's approval, nil when none.
func (s *Store) GetReplacementApproval(transcodeID string) (*ReplacementApproval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := ReplacementApproval{TranscodeActionID: transcodeID}
	var used int
	err := s.db.QueryRow(`SELECT approval, promote_action_id, used FROM replacement_approvals WHERE transcode_action_id=?`, transcodeID).Scan(&a.Approval, &a.PromoteActionID, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.Used = used == 1
	return &a, nil
}

// UseReplacementApproval marks the approval its promotion went ahead on,
// true when it was that promotion's.
func (s *Store) UseReplacementApproval(transcodeID, approval, promoteID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE replacement_approvals SET used=1 WHERE transcode_action_id=? AND approval=? AND promote_action_id=?`, transcodeID, approval, promoteID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ListReplacementApprovals is every approval, by transcode id.
func (s *Store) ListReplacementApprovals() (map[string]ReplacementApproval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT transcode_action_id, approval, promote_action_id, used FROM replacement_approvals`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ReplacementApproval{}
	for rows.Next() {
		var a ReplacementApproval
		var used int
		if err := rows.Scan(&a.TranscodeActionID, &a.Approval, &a.PromoteActionID, &used); err != nil {
			return nil, err
		}
		a.Used = used == 1
		out[a.TranscodeActionID] = a
	}
	return out, rows.Err()
}

// AddAutoReplacePending records a finished file whose automatic replacement
// is still to be submitted.
func (s *Store) AddAutoReplacePending(transcodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO auto_replace_pending(transcode_action_id, created_at) VALUES(?, ?) ON CONFLICT(transcode_action_id) DO NOTHING`, transcodeID, nowStr())
	return err
}

// AutoReplacePending says whether a file's automatic replacement is still
// to be submitted, and how often submitting it failed.
func (s *Store) AutoReplacePending(transcodeID string) (pending bool, attempts int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.db.QueryRow(`SELECT attempts FROM auto_replace_pending WHERE transcode_action_id=?`, transcodeID).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	return err == nil, attempts, err
}

// ListAutoReplacePending is every file still to be submitted.
func (s *Store) ListAutoReplacePending() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT transcode_action_id FROM auto_replace_pending ORDER BY created_at, transcode_action_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FailAutoReplacePending counts a failed submission.
func (s *Store) FailAutoReplacePending(transcodeID, why string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE auto_replace_pending SET attempts=attempts+1, last_error=? WHERE transcode_action_id=?`, why, transcodeID)
	return err
}

// DoneAutoReplacePending drops a file from the list.
func (s *Store) DoneAutoReplacePending(transcodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM auto_replace_pending WHERE transcode_action_id=?`, transcodeID)
	return err
}

// ReplaceReceipt is the stored answer to one replace request.
type ReplaceReceipt struct {
	Request  string
	Status   int
	Response string
}

// GetReplaceReceipt is the answer stored under a key, nil when none.
func (s *Store) GetReplaceReceipt(key string) (*ReplaceReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var r ReplaceReceipt
	err := s.db.QueryRow(`SELECT request, status, response FROM replace_receipts WHERE key=?`, key).Scan(&r.Request, &r.Status, &r.Response)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// PutReplaceReceipt stores the answer under its key; an existing one stays.
func (s *Store) PutReplaceReceipt(key string, r ReplaceReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO replace_receipts(key, request, status, response, created_at) VALUES(?, ?, ?, ?, ?) ON CONFLICT(key) DO NOTHING`, key, r.Request, r.Status, r.Response, nowStr())
	return err
}
