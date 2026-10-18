package store

// UnsuccessfulBenchmarks returns at most two completed, no-winner rounds for
// the same source bytes, across action names, paths and recipe changes. Worker
// errors and unfinished jobs are not quality-search attempts. No new ledger is
// needed: the action checkpoints already contain the authoritative outcome.
func (s *Store) UnsuccessfulBenchmarks(sourceSHA, excludeID string) ([]string, error) {
	return s.UnsuccessfulBenchmarkHistory(sourceSHA, excludeID, 2)
}

// UnsuccessfulBenchmarkHistory returns up to limit such rounds, newest first,
// so a caller can keep only the rounds comparable with a new proposal.
func (s *Store) UnsuccessfulBenchmarkHistory(sourceSHA, excludeID string, limit int) ([]string, error) {
	if sourceSHA == "" || limit <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id FROM action_instances
		WHERE action_name IN ('benchmark_transcode', 'transcode_media') AND id != ?
		AND json_extract(state_json, '$.original_sha256') = ?
		AND json_extract(state_json, '$.benchmark_done') = 1
		AND json_extract(state_json, '$.has_winner') = 0
		ORDER BY created_at DESC, id DESC LIMIT ?`, excludeID, sourceSHA, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
