package store

import "time"

// StorageSample is one measurement of a volume or of a served folder.
type StorageSample struct {
	TakenAt time.Time `json:"taken_at"`
	// Scope is "volume" or "folder". A volume sample is keyed by one of the
	// served roots on it.
	Scope string `json:"scope"`
	Path  string `json:"path"`
	// Volume is the volume the sample belongs to, named by the first served
	// root on it. For a volume sample it equals Path.
	Volume     string `json:"volume"`
	UsedBytes  int64  `json:"used_bytes"`
	TotalBytes int64  `json:"total_bytes,omitempty"`
	FreeBytes  int64  `json:"free_bytes,omitempty"`
	// Source says where the number came from: "helper" (measured inside the NAS)
	// or "mount" (read through the network share).
	Source string `json:"source,omitempty"`
}

// AddStorageSamples records one measurement round, all or nothing.
func (s *Store) AddStorageSamples(samples []StorageSample) error {
	if len(samples) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sample := range samples {
		if _, err := tx.Exec(`INSERT INTO storage_samples(taken_at, scope, path, volume, used_bytes, total_bytes, free_bytes, source)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, sample.TakenAt.UTC().Format(time.RFC3339), sample.Scope, sample.Path, sample.Volume,
			sample.UsedBytes, sample.TotalBytes, sample.FreeBytes, sample.Source); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// StorageSamples returns the samples taken at or after since, oldest first.
func (s *Store) StorageSamples(since time.Time) ([]StorageSample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT taken_at, scope, path, volume, used_bytes, total_bytes, free_bytes, source
		FROM storage_samples WHERE taken_at >= ? ORDER BY taken_at, id`, since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StorageSample
	for rows.Next() {
		var sample StorageSample
		var taken string
		if err := rows.Scan(&taken, &sample.Scope, &sample.Path, &sample.Volume, &sample.UsedBytes, &sample.TotalBytes, &sample.FreeBytes, &sample.Source); err != nil {
			return nil, err
		}
		sample.TakenAt, _ = time.Parse(time.RFC3339, taken)
		out = append(out, sample)
	}
	return out, rows.Err()
}

// LastStorageSampleTime is when the newest sample was taken; zero if none.
func (s *Store) LastStorageSampleTime() (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var taken *string
	if err := s.db.QueryRow(`SELECT MAX(taken_at) FROM storage_samples`).Scan(&taken); err != nil || taken == nil {
		return time.Time{}, err
	}
	t, _ := time.Parse(time.RFC3339, *taken)
	return t, nil
}

// PruneStorageSamples drops samples older than before.
func (s *Store) PruneStorageSamples(before time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM storage_samples WHERE taken_at < ?`, before.UTC().Format(time.RFC3339))
	return err
}
