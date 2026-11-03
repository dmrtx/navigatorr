package store

import (
	"database/sql"
	"errors"
)

// ReplaceWhenDoneDefaultKey is the preference (scope "maintenance") a job
// submitted without replace_when_done takes its value from.
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
// replace_when_done takes: false until it is set.
func (s *Store) ReplaceWhenDoneDefault() (bool, error) {
	p, err := s.GetPreference("maintenance", ReplaceWhenDoneDefaultKey)
	if err != nil {
		// No such preference: never set.
		return false, nil
	}
	return p.ValueJSON == "true", nil
}

// SetReplaceWhenDoneDefault stores the default.
func (s *Store) SetReplaceWhenDoneDefault(value bool) error {
	raw := "false"
	if value {
		raw = "true"
	}
	_, err := s.SetPreference("maintenance", ReplaceWhenDoneDefaultKey, raw, "user", 0)
	return err
}

// ReplacementApproval is the approval a file's replacement carries before
// its plan exists.
type ReplacementApproval struct {
	TranscodeActionID string
	Approval          string
	PromoteActionID   string // the promotion that used it; "" while unused
}

// GrantReplacementApproval records a fresh approval for a file, replacing
// any earlier one; no promotion uses it yet.
func (s *Store) GrantReplacementApproval(transcodeID, approval string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO replacement_approvals(transcode_action_id, approval, promote_action_id, created_at) VALUES(?, ?, '', ?)
		ON CONFLICT(transcode_action_id) DO UPDATE SET approval=excluded.approval, promote_action_id='', created_at=excluded.created_at`, transcodeID, approval, nowStr())
	return err
}

// RevokeReplacementApproval drops a file's approval that no promotion used.
func (s *Store) RevokeReplacementApproval(transcodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM replacement_approvals WHERE transcode_action_id=? AND promote_action_id=''`, transcodeID)
	return err
}

// GetReplacementApproval is a file's approval, nil when none.
func (s *Store) GetReplacementApproval(transcodeID string) (*ReplacementApproval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := ReplacementApproval{TranscodeActionID: transcodeID}
	err := s.db.QueryRow(`SELECT approval, promote_action_id FROM replacement_approvals WHERE transcode_action_id=?`, transcodeID).Scan(&a.Approval, &a.PromoteActionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// BindReplacementApproval marks a file's approval as used by one promotion.
// It is true when the approval is that promotion's: unused, or already its.
func (s *Store) BindReplacementApproval(transcodeID, approval, promoteID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE replacement_approvals SET promote_action_id=? WHERE transcode_action_id=? AND approval=? AND (promote_action_id='' OR promote_action_id=?)`, promoteID, transcodeID, approval, promoteID)
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
	rows, err := s.db.Query(`SELECT transcode_action_id, approval, promote_action_id FROM replacement_approvals`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ReplacementApproval{}
	for rows.Next() {
		var a ReplacementApproval
		if err := rows.Scan(&a.TranscodeActionID, &a.Approval, &a.PromoteActionID); err != nil {
			return nil, err
		}
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
