package store

// StatusCounts returns exact counts, independent of list pagination limits.
func (s *Store) StatusCounts() (map[string]int, map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT 'action', status, COUNT(*) FROM action_instances GROUP BY status
		UNION ALL SELECT 'maintenance', status, COUNT(*) FROM maintenance_items GROUP BY status`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	actions, maintenance := map[string]int{}, map[string]int{}
	for rows.Next() {
		var kind, status string
		var count int
		if err := rows.Scan(&kind, &status, &count); err != nil {
			return nil, nil, err
		}
		if kind == "action" {
			actions[status] = count
		} else {
			maintenance[status] = count
		}
	}
	return actions, maintenance, rows.Err()
}
