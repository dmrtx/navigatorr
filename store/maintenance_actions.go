package store

// ListMaintenanceActionSnapshot reads the shared workflow ledger consistently.
// Filtering and accounting must precede UI pagination, and offset scans ordered
// by updated_at could otherwise miss or duplicate actions while reconciliation
// updates them. No separate browser queue or savings ledger is maintained.
func (s *Store) ListMaintenanceActionSnapshot() ([]ActionInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id, action_name, status, current_step,
		inputs_json, outputs_json, state_json, waiting_reason, waiting_condition,
		waiting_options_json, error_json, idempotency_key, created_at, updated_at
		FROM action_instances WHERE action_name IN
		('transcode_media','transcode_batch','benchmark_transcode','promote_transcode_candidate')
		ORDER BY updated_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var actions []ActionInstance
	for rows.Next() {
		var inst ActionInstance
		if err := rows.Scan(&inst.ID, &inst.ActionName, &inst.Status, &inst.CurrentStep,
			&inst.InputsJSON, &inst.OutputsJSON, &inst.StateJSON, &inst.WaitingReason,
			&inst.WaitingCondition, &inst.WaitingOptionsJSON, &inst.ErrorJSON,
			&inst.IdempotencyKey, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
			return nil, err
		}
		actions = append(actions, inst)
	}
	return actions, rows.Err()
}
