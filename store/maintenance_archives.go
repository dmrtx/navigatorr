package store

import (
	"errors"
	"fmt"
)

var ErrArchiveActiveWorkflow = errors.New("active workflows cannot be archived")

// Archives affect queue visibility only. Action history and media stay intact.
func (s *Store) MaintenanceArchives() (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT action_id, archived_at FROM maintenance_ui_archives`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var id, at string
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		result[id] = at
	}
	return result, rows.Err()
}

func (s *Store) SetMaintenanceArchived(root string, members []string, archived bool) error {
	return s.SetMaintenanceArchivedWith(root, members, archived, nil)
}

// ActionKey names an action by its type and idempotency key.
type ActionKey struct{ ActionName, IdempotencyKey string }

// SetMaintenanceArchivedWith also refuses to archive while an action named in keys is still
// active. A caller whose member list may be stale (a batch started from a preview an instant
// ago) names that action by key, and the check shares the archive's transaction.
func (s *Store) SetMaintenanceArchivedWith(root string, members []string, archived bool, keys []ActionKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if archived {
		containsRoot := false
		for _, id := range members {
			containsRoot = containsRoot || id == root
			var status string
			if err := tx.QueryRow(`SELECT status FROM action_instances WHERE id = ?`, id).Scan(&status); err != nil {
				return err
			}
			if status != ActionStatusCompleted && status != ActionStatusFailed && status != ActionStatusCancelled {
				return ErrArchiveActiveWorkflow
			}
		}
		for _, key := range keys {
			var active int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM action_instances WHERE action_name = ? AND idempotency_key = ? AND status NOT IN (?, ?, ?)`,
				key.ActionName, key.IdempotencyKey, ActionStatusCompleted, ActionStatusFailed, ActionStatusCancelled).Scan(&active); err != nil {
				return err
			}
			if active > 0 {
				return ErrArchiveActiveWorkflow
			}
		}
		if !containsRoot {
			return fmt.Errorf("archive root must belong to the workflow")
		}
		_, err = tx.Exec(`INSERT INTO maintenance_ui_archives(action_id, archived_at) VALUES(?, ?) ON CONFLICT(action_id) DO NOTHING`, root, nowStr())
	} else {
		_, err = tx.Exec(`DELETE FROM maintenance_ui_archives WHERE action_id = ?`, root)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
