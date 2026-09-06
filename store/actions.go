package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Action execution statuses
const (
	ActionStatusPending         = "pending"
	ActionStatusRunning         = "running"
	ActionStatusWaitingExternal = "waiting_external"
	ActionStatusWaitingDecision = "waiting_decision"
	ActionStatusCompleted       = "completed"
	ActionStatusFailed          = "failed"
	ActionStatusCancelled       = "cancelled"
)

// ActionInstance represents a persistent multi-step action workflow.
type ActionInstance struct {
	ID                 string `json:"id"`
	ActionName         string `json:"action_name"`
	Status             string `json:"status"`
	CurrentStep        int    `json:"current_step"`
	InputsJSON         string `json:"inputs_json"`
	OutputsJSON        string `json:"outputs_json"`
	StateJSON          string `json:"state_json"`
	WaitingReason      string `json:"waiting_reason,omitempty"`
	WaitingCondition   string `json:"waiting_condition,omitempty"`
	WaitingOptionsJSON string `json:"waiting_options_json,omitempty"`
	ErrorJSON          string `json:"error_json,omitempty"`
	IdempotencyKey     string `json:"idempotency_key,omitempty"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

// ActionStepLog records an individual step execution in an action workflow.
type ActionStepLog struct {
	ID          int64  `json:"id"`
	InstanceID  string `json:"instance_id"`
	StepIndex   int    `json:"step_index"`
	StepName    string `json:"step_name"`
	Primitive   string `json:"primitive"`
	InputsJSON  string `json:"inputs_json"`
	OutputsJSON string `json:"outputs_json"`
	Status      string `json:"status"` // completed, failed, skipped
	Error       string `json:"error,omitempty"`
	DurationMs  int64  `json:"duration_ms"`
	CreatedAt   string `json:"created_at"`
}

// CreateActionInstance inserts a new action workflow instance.
func (s *Store) CreateActionInstance(inst ActionInstance) error {
	if inst.ID == "" || inst.ActionName == "" {
		return fmt.Errorf("id and action_name are required")
	}
	if inst.Status == "" {
		inst.Status = ActionStatusPending
	}
	if inst.InputsJSON == "" {
		inst.InputsJSON = "{}"
	}
	if inst.OutputsJSON == "" {
		inst.OutputsJSON = "{}"
	}
	if inst.StateJSON == "" {
		inst.StateJSON = "{}"
	}
	if inst.WaitingOptionsJSON == "" {
		inst.WaitingOptionsJSON = "[]"
	}
	now := nowStr()
	inst.CreatedAt = now
	inst.UpdatedAt = now

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`INSERT INTO action_instances (
		id, action_name, status, current_step, inputs_json, outputs_json,
		state_json, waiting_reason, waiting_condition, waiting_options_json,
		error_json, idempotency_key, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inst.ID, inst.ActionName, inst.Status, inst.CurrentStep, inst.InputsJSON, inst.OutputsJSON,
		inst.StateJSON, inst.WaitingReason, inst.WaitingCondition, inst.WaitingOptionsJSON,
		inst.ErrorJSON, inst.IdempotencyKey, inst.CreatedAt, inst.UpdatedAt)
	return err
}

// GetActionInstance retrieves an action instance by ID.
func (s *Store) GetActionInstance(id string) (*ActionInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var inst ActionInstance
	err := s.db.QueryRow(`SELECT id, action_name, status, current_step, inputs_json,
		outputs_json, state_json, waiting_reason, waiting_condition, waiting_options_json,
		error_json, idempotency_key, created_at, updated_at
		FROM action_instances WHERE id=?`, id).Scan(
		&inst.ID, &inst.ActionName, &inst.Status, &inst.CurrentStep, &inst.InputsJSON,
		&inst.OutputsJSON, &inst.StateJSON, &inst.WaitingReason, &inst.WaitingCondition,
		&inst.WaitingOptionsJSON, &inst.ErrorJSON, &inst.IdempotencyKey, &inst.CreatedAt, &inst.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("action instance %q not found", id)
	}
	if err != nil {
		return nil, err
	}
	return &inst, nil
}

// UpdateActionInstance updates status, state, outputs and progress of an action instance.
func (s *Store) UpdateActionInstance(inst ActionInstance) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := nowStr()
	_, err := s.db.Exec(`UPDATE action_instances SET
		status=?, current_step=?, outputs_json=?, state_json=?,
		waiting_reason=?, waiting_condition=?, waiting_options_json=?,
		error_json=?, idempotency_key=?, updated_at=?
		WHERE id=?`,
		inst.Status, inst.CurrentStep, inst.OutputsJSON, inst.StateJSON,
		inst.WaitingReason, inst.WaitingCondition, inst.WaitingOptionsJSON,
		inst.ErrorJSON, inst.IdempotencyKey, now, inst.ID)
	return err
}

// ListActionInstances returns action instances optionally filtered by status.
func (s *Store) ListActionInstances(status string, limit int) ([]ActionInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := `SELECT id, action_name, status, current_step, inputs_json, outputs_json,
		state_json, waiting_reason, waiting_condition, waiting_options_json,
		error_json, idempotency_key, created_at, updated_at
		FROM action_instances WHERE 1=1`
	var args []any
	if status != "" && !strings.EqualFold(status, "all") {
		q += ` AND status=?`
		args = append(args, status)
	}
	q += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ActionInstance
	for rows.Next() {
		var inst ActionInstance
		if err := rows.Scan(
			&inst.ID, &inst.ActionName, &inst.Status, &inst.CurrentStep, &inst.InputsJSON,
			&inst.OutputsJSON, &inst.StateJSON, &inst.WaitingReason, &inst.WaitingCondition,
			&inst.WaitingOptionsJSON, &inst.ErrorJSON, &inst.IdempotencyKey, &inst.CreatedAt, &inst.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

// FindActiveActionByIdempotencyKey finds an active (non-terminal) action by name and idempotency key.
func (s *Store) FindActiveActionByIdempotencyKey(actionName, idempotencyKey string) (*ActionInstance, error) {
	if idempotencyKey == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var inst ActionInstance
	err := s.db.QueryRow(`SELECT id, action_name, status, current_step, inputs_json,
		outputs_json, state_json, waiting_reason, waiting_condition, waiting_options_json,
		error_json, idempotency_key, created_at, updated_at
		FROM action_instances
		WHERE action_name=? AND idempotency_key=? AND status NOT IN ('completed', 'failed', 'cancelled')
		ORDER BY created_at DESC LIMIT 1`, actionName, idempotencyKey).Scan(
		&inst.ID, &inst.ActionName, &inst.Status, &inst.CurrentStep, &inst.InputsJSON,
		&inst.OutputsJSON, &inst.StateJSON, &inst.WaitingReason, &inst.WaitingCondition,
		&inst.WaitingOptionsJSON, &inst.ErrorJSON, &inst.IdempotencyKey, &inst.CreatedAt, &inst.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &inst, nil
}

// LogActionStep appends an execution record for one step of an action.
func (s *Store) LogActionStep(step ActionStepLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := nowStr()
	_, err := s.db.Exec(`INSERT INTO action_steps_log (
		instance_id, step_index, step_name, primitive, inputs_json,
		outputs_json, status, error, duration_ms, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		step.InstanceID, step.StepIndex, step.StepName, step.Primitive,
		step.InputsJSON, step.OutputsJSON, step.Status, step.Error,
		step.DurationMs, now)
	return err
}

// GetActionSteps retrieves the step execution logs for an action instance.
func (s *Store) GetActionSteps(instanceID string) ([]ActionStepLog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT id, instance_id, step_index, step_name, primitive,
		inputs_json, outputs_json, status, error, duration_ms, created_at
		FROM action_steps_log WHERE instance_id=? ORDER BY step_index ASC, id ASC`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ActionStepLog
	for rows.Next() {
		var step ActionStepLog
		if err := rows.Scan(
			&step.ID, &step.InstanceID, &step.StepIndex, &step.StepName, &step.Primitive,
			&step.InputsJSON, &step.OutputsJSON, &step.Status, &step.Error,
			&step.DurationMs, &step.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, step)
	}
	return out, rows.Err()
}

// AutoResolveStale marks an existing active maintenance item as done if its issue has been resolved.
func (s *Store) AutoResolveStale(service, mediaType, mediaID, issueType, notes string) (*MaintenanceItem, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var it MaintenanceItem
	err := s.db.QueryRow(`SELECT `+maintColumns+` FROM maintenance_items
		WHERE service=? AND media_type=? AND media_id=? AND issue_type=?
		AND status NOT IN ('done', 'failed')`,
		service, mediaType, mediaID, issueType).Scan(maintScan(&it)...)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	now := nowStr()
	resolvedNotes := it.Notes
	if notes != "" {
		if resolvedNotes != "" {
			resolvedNotes += "; " + notes
		} else {
			resolvedNotes = notes
		}
	}

	_, err = s.db.Exec(`UPDATE maintenance_items SET status='done', notes=?, updated_at=? WHERE id=?`,
		resolvedNotes, now, it.ID)
	if err != nil {
		return nil, false, err
	}
	it.Status = MaintDone
	it.Notes = resolvedNotes
	it.UpdatedAt = now
	return &it, true, nil
}

// AutoResolveByMedia marks any active maintenance items for the media as done.
func (s *Store) AutoResolveByMedia(service, mediaID, notes string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := nowStr()
	res, err := s.db.Exec(`UPDATE maintenance_items SET status='done',
		notes = CASE WHEN notes != '' THEN notes || '; ' || ? ELSE ? END,
		updated_at=?
		WHERE service=? AND media_id=? AND status NOT IN ('done', 'failed')`,
		notes, notes, now, service, mediaID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// GetLatestMediaCheck returns the most recent check for a given file_id to avoid redundant ffprobe runs.
// If fileID changed, no record will match, invalidating the previous inspection.
func (s *Store) GetLatestMediaCheck(mediaType, mediaID, fileID string) (*MediaCheck, error) {
	if fileID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var c MediaCheck
	var audioJSON, subJSON, dangerJSON string
	err := s.db.QueryRow(`SELECT id, media_type, media_id, file_id, path, container, video_codec,
		resolution, bit_depth, audio_languages, subtitle_languages, dangerous_files,
		duration_sec, size_bytes, checked_at
		FROM media_checks WHERE media_type=? AND media_id=? AND file_id=?
		ORDER BY id DESC LIMIT 1`, mediaType, mediaID, fileID).
		Scan(&c.ID, &c.MediaType, &c.MediaID, &c.FileID, &c.Path, &c.Container, &c.VideoCodec,
			&c.Resolution, &c.BitDepth, &audioJSON, &subJSON, &dangerJSON,
			&c.DurationSec, &c.SizeBytes, &c.CheckedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(audioJSON), &c.AudioLanguages)
	_ = json.Unmarshal([]byte(subJSON), &c.SubtitleLanguages)
	_ = json.Unmarshal([]byte(dangerJSON), &c.DangerousFiles)
	return &c, nil
}

// LogActionEnriched records an action with execution duration, errors, and relevant identifiers.
func (s *Store) LogActionEnriched(action, service, media, argsJSON, result, errStr, identifiers string, durationMs int64) error {
	if action == "" {
		return fmt.Errorf("action is required")
	}
	if len(argsJSON) > 4096 {
		argsJSON = argsJSON[:4096] + "…(truncated)"
	}
	if len(result) > 4096 {
		result = result[:4096] + "…(truncated)"
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`INSERT INTO action_log (
		action, service, media, args_json, result, error, identifiers, duration_ms, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		action, service, media, RedactSecrets(argsJSON), result, errStr, identifiers, durationMs, nowStr())
	return err
}

// QueryActionLog searches action logs by media name, service, or action.
func (s *Store) QueryActionLog(media, service, action string, limit int) ([]map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if limit <= 0 || limit > 100 {
		limit = 25
	}
	q := `SELECT id, action, service, media, args_json, result, error, identifiers, duration_ms, created_at
		FROM action_log WHERE 1=1`
	var args []any
	if media != "" {
		like := "%" + strings.ToLower(media) + "%"
		q += ` AND (LOWER(media) LIKE ? OR LOWER(identifiers) LIKE ? OR LOWER(args_json) LIKE ?)`
		args = append(args, like, like, like)
	}
	if service != "" {
		q += ` AND LOWER(service)=LOWER(?)`
		args = append(args, service)
	}
	if action != "" {
		q += ` AND LOWER(action) LIKE LOWER(?)`
		args = append(args, "%"+action+"%")
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var id int64
		var act, svc, med, argStr, res, errVal, idents, at string
		var dur int64
		if err := rows.Scan(&id, &act, &svc, &med, &argStr, &res, &errVal, &idents, &dur, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id":          id,
			"action":      act,
			"service":     svc,
			"media":       med,
			"args":        argStr,
			"result":      res,
			"error":       errVal,
			"identifiers": idents,
			"duration_ms": dur,
			"created_at":  at,
		})
	}
	return out, rows.Err()
}
