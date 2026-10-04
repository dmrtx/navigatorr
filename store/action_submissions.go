package store

import "database/sql"

// QueueAction atomically records an action and its durable submission receipt.
// Receipts survive terminal states, so a browser retry never duplicates work.
func (s *Store) QueueAction(inst ActionInstance, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRow(`SELECT action_id FROM action_submissions WHERE submission_key=?`, key).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	now := nowStr()
	_, err = tx.Exec(`INSERT INTO action_instances
		(id,action_name,status,current_step,inputs_json,outputs_json,state_json,
		waiting_reason,waiting_condition,waiting_options_json,error_json,idempotency_key,created_at,updated_at)
		VALUES (?,?,?,0,?,?,?,'','','[]','',?,?,?)`,
		inst.ID, inst.ActionName, ActionStatusPending, inst.InputsJSON, "{}", inst.StateJSON, inst.IdempotencyKey, now, now)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(`INSERT INTO action_submissions VALUES (?,?)`, key, inst.ID); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return inst.ID, nil
}
