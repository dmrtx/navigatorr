package store

import "database/sql"

// folderSizeRows is how many folder totals are kept, the newest first. A walk of
// a library reports a folder and the ones inside it, so a few thousand fit in one.
const folderSizeRows = 20000

// Folder totals are advisory metadata, never media integrity evidence.
func (s *Store) FolderSizeSnapshot(path string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var snapshot string
	err := s.db.QueryRow(`SELECT snapshot_json FROM folder_size_cache WHERE path = ?`, path).Scan(&snapshot)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return snapshot, err
}

func (s *Store) SaveFolderSizeSnapshot(path, snapshot string) error {
	return s.SaveFolderSizeSnapshots(map[string]string{path: snapshot})
}

// SaveFolderSizeSnapshots saves several totals at once, in one transaction.
func (s *Store) SaveFolderSizeSnapshots(snapshots map[string]string) error {
	if len(snapshots) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowStr()
	for path, snapshot := range snapshots {
		if _, err := tx.Exec(`INSERT INTO folder_size_cache(path, snapshot_json, updated_at) VALUES(?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET snapshot_json=excluded.snapshot_json, updated_at=excluded.updated_at`, path, snapshot, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM folder_size_cache WHERE path IN
		(SELECT path FROM folder_size_cache ORDER BY updated_at DESC, path LIMIT -1 OFFSET ?)`, folderSizeRows); err != nil {
		return err
	}
	return tx.Commit()
}
