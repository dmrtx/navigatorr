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
