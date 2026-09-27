package store

// GetActionStepSummaries reads the latest observation per logical step without
// loading the large JSON payloads. Full poll history remains in GetActionSteps.
func (s *Store) GetActionStepSummaries(instanceID string, limit int) ([]ActionStepLog, int, error) {
	if limit <= 0 || limit > 25 {
		limit = 25
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM action_steps_log WHERE instance_id=?`, instanceID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT step_index, step_name, status, duration_ms, substr(error,1,512), created_at
		FROM action_steps_log WHERE id IN (
			SELECT MAX(id) FROM action_steps_log WHERE instance_id=? GROUP BY step_index
		) ORDER BY step_index DESC LIMIT ?`, instanceID, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	steps := []ActionStepLog{}
	for rows.Next() {
		var step ActionStepLog
		if err := rows.Scan(&step.StepIndex, &step.StepName, &step.Status, &step.DurationMs, &step.Error, &step.CreatedAt); err != nil {
			return nil, 0, err
		}
		steps = append(steps, step)
	}
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}
	return steps, total, rows.Err()
}

// ActionBriefing points to actionable work; action_status supplies its details.
type ActionBriefing struct {
	ID               string `json:"id"`
	ActionName       string `json:"action_name"`
	Status           string `json:"status"`
	WaitingReason    string `json:"waiting_reason,omitempty"`
	WaitingCondition string `json:"waiting_condition,omitempty"`
	UpdatedAt        string `json:"updated_at"`
}

func (s *Store) activeActionBriefingsLocked(limit int) ([]ActionBriefing, error) {
	rows, err := s.db.Query(`SELECT id, action_name, status, substr(waiting_reason,1,512), waiting_condition, updated_at
		FROM action_instances WHERE status IN ('pending','running','waiting_external','waiting_decision')
		ORDER BY CASE WHEN status='waiting_decision' THEN 0 ELSE 1 END, updated_at ASC, id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ActionBriefing{}
	for rows.Next() {
		var item ActionBriefing
		if err := rows.Scan(&item.ID, &item.ActionName, &item.Status, &item.WaitingReason, &item.WaitingCondition, &item.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
