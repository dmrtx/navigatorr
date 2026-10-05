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
