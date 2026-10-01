package store

import (
	"fmt"
	"strings"
	"time"
)

// ClaimActionExecution serializes action side effects across MCP clients and
// Navigatorr processes sharing the database. A crashed owner expires naturally.
func (s *Store) ClaimActionExecution(id, owner string, now time.Time, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`INSERT INTO action_execution_leases(action_id, owner, expires_at_ms)
		VALUES (?, ?, ?) ON CONFLICT(action_id) DO UPDATE SET owner=excluded.owner,
		expires_at_ms=excluded.expires_at_ms WHERE action_execution_leases.expires_at_ms <= ?`,
		id, owner, now.Add(ttl).UnixMilli(), now.UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) RenewActionExecution(id, owner string, now time.Time, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE action_execution_leases SET expires_at_ms=?
		WHERE action_id=? AND owner=? AND expires_at_ms>?`, now.Add(ttl).UnixMilli(), id, owner, now.UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) ReleaseActionExecution(id, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM action_execution_leases WHERE action_id=? AND owner=?`, id, owner)
	return err
}

// ListReconcilableActions uses stable keyset pagination: polling changes status
// and updated_at, so offset pagination would otherwise skip pending actions.
func (s *Store) ListReconcilableActions(afterID string, limit int) ([]ActionInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, action_name, status, current_step, inputs_json,
		outputs_json, state_json, waiting_reason, waiting_condition, waiting_options_json,
		error_json, idempotency_key, created_at, updated_at FROM action_instances
		WHERE status IN ('waiting_external', 'running') AND id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActionInstance
	for rows.Next() {
		var inst ActionInstance
		if err := rows.Scan(&inst.ID, &inst.ActionName, &inst.Status, &inst.CurrentStep,
			&inst.InputsJSON, &inst.OutputsJSON, &inst.StateJSON, &inst.WaitingReason,
			&inst.WaitingCondition, &inst.WaitingOptionsJSON, &inst.ErrorJSON,
			&inst.IdempotencyKey, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

// ClaimPromotionOriginal reserves the original file for one approved promotion.
// Ownership is durable across failures: another candidate cannot take over while
// an earlier import/delete outcome is still uncertain.
func (s *Store) ClaimPromotionOriginal(service string, episodeFileID int, actionID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	service = strings.ToLower(strings.TrimSpace(service))
	if service == "" || episodeFileID <= 0 || actionID == "" {
		return false, fmt.Errorf("service, positive episode_file_id, and action_id are required")
	}
	if _, err := s.db.Exec(`INSERT INTO promotion_original_claims(service, episode_file_id, action_id)
		VALUES(?, ?, ?) ON CONFLICT(service, episode_file_id) DO NOTHING`, service, episodeFileID, actionID); err != nil {
		return false, err
	}
	var owner string
	err := s.db.QueryRow(`SELECT action_id FROM promotion_original_claims WHERE service=? AND episode_file_id=?`, service, episodeFileID).Scan(&owner)
	return owner == actionID, err
}

// ClaimPromotionSeries coordinates the whole promotion, including external
// waits and the final series-wide rescan. Batch children share their parent's
// reservation. Failed or cancelled owners retain it when Sonarr side effects
// may need recovery. A standalone action stopped before its first import can
// safely release the series; its original-file reservation remains intact.
func (s *Store) ClaimPromotionSeries(service string, seriesID int, actionID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	service = strings.ToLower(strings.TrimSpace(service))
	if service == "" || seriesID <= 0 || actionID == "" {
		return false, fmt.Errorf("service, positive series_id, and action_id are required")
	}
	_, err := s.db.Exec(`INSERT INTO promotion_series_claims(service, series_id, owner_action_id)
		VALUES (?, ?, ?) ON CONFLICT(service, series_id) DO UPDATE SET owner_action_id=excluded.owner_action_id
		WHERE EXISTS (SELECT 1 FROM action_instances WHERE id=promotion_series_claims.owner_action_id AND (
			status='completed' OR (status IN ('failed','cancelled') AND action_name='promote_transcode_candidate'
			AND CASE WHEN json_valid(state_json) THEN
				json_extract(state_json, '$.promotion.approved')=1
				AND COALESCE(json_extract(state_json, '$.promotion.commands.import.sent_at'), '')=''
				AND COALESCE(json_extract(state_json, '$.promotion.new_episode_file_id'), 0)=0
				AND COALESCE(json_extract(state_json, '$.promotion.delete_sent_at'), '')=''
				AND COALESCE(json_extract(state_json, '$.promotion.old_removed'), 0)=0
				AND COALESCE(json_array_length(state_json, '$.promotion_reimport_history'), 0)=0
			ELSE 0 END)))`,
		service, seriesID, actionID)
	if err != nil {
		return false, err
	}
	var owner string
	err = s.db.QueryRow(`SELECT owner_action_id FROM promotion_series_claims WHERE service=? AND series_id=?`, service, seriesID).Scan(&owner)
	return owner == actionID, err
}
