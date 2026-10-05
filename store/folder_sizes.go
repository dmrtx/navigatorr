package store

import "database/sql"

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
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO folder_size_cache(path, snapshot_json, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET snapshot_json=excluded.snapshot_json, updated_at=excluded.updated_at`, path, snapshot, nowStr())
	if err == nil {
		_, err = s.db.Exec(`DELETE FROM folder_size_cache WHERE path IN
			(SELECT path FROM folder_size_cache ORDER BY updated_at DESC, path LIMIT -1 OFFSET 1000)`)
	}
	return err
}
