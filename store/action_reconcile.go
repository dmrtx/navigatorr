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
		WHERE (status IN ('pending', 'waiting_external', 'running')
		OR status = 'waiting_decision' AND action_name = 'transcode_batch')
		AND id > ? ORDER BY id LIMIT ?`, afterID, limit)
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

// WorkerSlotWaiterRow is the projection of an action holding a worker slot
// ticket: only the fields the slot queue orders and filters on.
type WorkerSlotWaiterRow struct {
	ID               string
	Status           string
	WaitingCondition string
	UpdatedAt        string
	ParentID         string
	WaitSince        string // worker_slot_wait_since
	PolledAt         string // worker_slot_polled_at
	Paused           bool
	Worker           string // the assigned worker, '' before workers had names
}

// ListWorkerSlotWaiters returns the live transcode actions whose
// state holds a worker slot ticket (worker_slot_wait_since). The caller
// decides which of them are still contending; the query only narrows the scan
// to the few rows that can be and projects the fields it needs.
func (s *Store) ListWorkerSlotWaiters() ([]WorkerSlotWaiterRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id, status, waiting_condition, updated_at,
		COALESCE(json_extract(inputs_json, '$.parent_action_id'), json_extract(state_json, '$.parent_action_id'), ''),
		json_extract(state_json, '$.worker_slot_wait_since'),
		COALESCE(json_extract(state_json, '$.worker_slot_polled_at'), ''),
		CASE WHEN json_extract(state_json, '$.paused') IN (1, 'true') THEN 1 ELSE 0 END,
		COALESCE(json_extract(state_json, '$.worker'), '')
		FROM action_instances
		WHERE status IN ('pending', 'waiting_external', 'running')
		AND action_name = 'transcode_media'
		AND json_type(state_json, '$.worker_slot_wait_since') = 'text'
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkerSlotWaiterRow
	for rows.Next() {
		var w WorkerSlotWaiterRow
		if err := rows.Scan(&w.ID, &w.Status, &w.WaitingCondition, &w.UpdatedAt,
			&w.ParentID, &w.WaitSince, &w.PolledAt, &w.Paused, &w.Worker); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// CountActiveWorkerActions counts the unfinished actions of one kind by the
// worker they were assigned. Actions without a worker yet are not counted,
// nor a child held by its paused or decision-waiting batch before it sent
// anything to its worker: it uses no slot there.
func (s *Store) CountActiveWorkerActions(actionName string) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT json_extract(state_json, '$.worker'), COUNT(*)
		FROM action_instances
		WHERE status IN ('pending', 'waiting_external', 'running')
		AND action_name = ?
		AND json_valid(state_json)
		AND json_type(state_json, '$.worker') = 'text'
		AND NOT (COALESCE(waiting_condition, '') IN ('parent_paused', 'parent_waiting_decision')
			AND COALESCE(json_extract(state_json, '$.job_id'), '') = ''
			AND COALESCE(json_extract(state_json, '$.external_reference'), '') = ''
			AND COALESCE(json_extract(state_json, '$.transcode_submitted'), 0) NOT IN (1, 'true')
			AND COALESCE(json_extract(state_json, '$.transcode_reconcile'), 0) NOT IN (1, 'true'))
		GROUP BY 1`, actionName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// AssignLegacyWorker stamps worker on every worker action and batch item that
// holds a worker job and names no worker, or names legacyNode while worker is
// another name; legacyNode's slot settings then become worker's unless worker
// already has its own. With worker == legacyNode it only names the unnamed.
// It reports how many actions and items it stamped, and is idempotent.
func (s *Store) AssignLegacyWorker(worker, legacyNode string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE action_instances SET state_json = json_set(state_json, '$.worker', ?)
		WHERE action_name IN ('transcode_media', 'clean_podcast_ads')
		AND json_valid(state_json)
		AND (COALESCE(json_extract(state_json, '$.worker'), '') = ''
				AND (COALESCE(json_extract(state_json, '$.job_id'), '') != ''
					OR COALESCE(json_extract(state_json, '$.external_reference'), '') != ''
					OR json_extract(state_json, '$.transcode_submitted') IN (1, 'true')
					OR json_extract(state_json, '$.transcode_reconcile') IN (1, 'true'))
			OR (json_extract(state_json, '$.worker') = ? AND ? != ?))`, worker, legacyNode, worker, legacyNode)
	if err != nil {
		return 0, err
	}
	actions, _ := res.RowsAffected()
	res, err = tx.Exec(`UPDATE transcode_batch_items SET worker = ?
		WHERE (worker = '' AND job_id != '') OR (worker = ? AND ? != ?)`, worker, legacyNode, worker, legacyNode)
	if err != nil {
		return 0, err
	}
	items, _ := res.RowsAffected()
	if _, err := tx.Exec(`UPDATE worker_slot_settings SET node = ?
		WHERE node = ? AND NOT EXISTS (SELECT 1 FROM worker_slot_settings WHERE node = ?)`, worker, legacyNode, worker); err != nil {
		return 0, err
	}
	return actions + items, tx.Commit()
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

// promotionSideEffectFreeSQL is true for a promotion whose state proves that no
// Sonarr or filesystem side effect was ever sent. The placeholder is the column
// prefix ("" or an alias such as "c.").
const promotionSideEffectFreeSQL = `COALESCE(json_extract(%[1]sstate_json, '$.promotion.commands.import.sent_at'), '')=''
	AND COALESCE(json_extract(%[1]sstate_json, '$.promotion.new_episode_file_id'), 0)=0
	AND COALESCE(json_extract(%[1]sstate_json, '$.promotion.delete_sent_at'), '')=''
	AND COALESCE(json_extract(%[1]sstate_json, '$.promotion.old_removed'), 0)=0
	AND COALESCE(json_extract(%[1]sstate_json, '$.filesystem_publish_started'), 0)=0
	AND COALESCE(json_extract(%[1]sstate_json, '$.filesystem_published'), 0)=0
	AND COALESCE(json_array_length(%[1]sstate_json, '$.promotion_reimport_history'), 0)=0`

// promotionSeriesTakeoverSQL decides, against the current owner row in
// action_instances, whether a different action may take over the reservation.
//
//   - A completed owner never needs the reservation again.
//   - A failed or cancelled standalone promotion releases it only if it was
//     approved and never sent a side effect.
//   - A failed or cancelled transcode_batch releases it once every promotion it
//     spawned is finished: completed, or failed/cancelled with no side effect
//     (a child that never reached approval has none either). Any child still
//     active, or stopped after a side effect that may need recovery, keeps it.
//     So does the batch's own series-wide RescanSeries while it was sent but is
//     neither done nor confirmed failed, or when the batch state is unreadable.
var promotionSeriesTakeoverSQL = fmt.Sprintf(`EXISTS (SELECT 1 FROM action_instances WHERE id=promotion_series_claims.owner_action_id AND (
	status='completed'
	OR (status IN ('failed','cancelled') AND action_name='promote_transcode_candidate'
		AND CASE WHEN json_valid(state_json) THEN
			json_extract(state_json, '$.promotion.approved')=1 AND `+promotionSideEffectFreeSQL+`
		ELSE 0 END)
	OR (status IN ('failed','cancelled') AND action_name='transcode_batch'
		AND CASE WHEN json_valid(state_json) THEN
			COALESCE(json_extract(state_json, '$.batch_promotion_commands.rescan.sent_at'), '')=''
			OR COALESCE(json_extract(state_json, '$.batch_promotion_commands.rescan.done'), 0)=1
			OR COALESCE(json_extract(state_json, '$.batch_promotion_commands.rescan.failed'), 0)=1
		ELSE 0 END
		AND NOT EXISTS (SELECT 1 FROM action_instances c
			WHERE c.action_name='promote_transcode_candidate'
			AND CASE WHEN json_valid(c.inputs_json) THEN json_extract(c.inputs_json, '$.batch_promote_parent_id')=action_instances.id ELSE 0 END
			AND NOT (c.status='completed' OR (c.status IN ('failed','cancelled')
				AND CASE WHEN json_valid(c.state_json) THEN `+promotionSideEffectFreeSQL+` ELSE 0 END)))))
)`, "", "c.")

// PromotionSeriesOwnerInfo describes the action currently holding a series
// reservation.
type PromotionSeriesOwnerInfo struct {
	ActionID   string
	ActionName string
	Status     string
}

// PromotionSeriesOwner reports who holds the reservation, so a waiting
// promotion can name its blocker.
func (s *Store) PromotionSeriesOwner(service string, seriesID int) (PromotionSeriesOwnerInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.promotionSeriesOwner(strings.ToLower(strings.TrimSpace(service)), seriesID)
}

func (s *Store) promotionSeriesOwner(service string, seriesID int) (PromotionSeriesOwnerInfo, error) {
	var o PromotionSeriesOwnerInfo
	err := s.db.QueryRow(`SELECT c.owner_action_id, COALESCE(a.action_name, ''), COALESCE(a.status, '')
		FROM promotion_series_claims c LEFT JOIN action_instances a ON a.id=c.owner_action_id
		WHERE c.service=? AND c.series_id=?`, service, seriesID).Scan(&o.ActionID, &o.ActionName, &o.Status)
	return o, err
}

// ClaimPromotionSeries coordinates the whole promotion, including external
// waits and the final series-wide rescan. Batch children share their parent's
// reservation. Failed or cancelled owners retain it when Sonarr side effects
// may need recovery; see promotionSeriesTakeoverSQL for when it is released.
func (s *Store) ClaimPromotionSeries(service string, seriesID int, actionID string) (bool, error) {
	owner, err := s.ClaimPromotionSeriesOwner(service, seriesID, actionID)
	return owner.ActionID == actionID, err
}

// ClaimPromotionSeriesOwner is ClaimPromotionSeries returning the resulting
// owner, which is actionID on success and the blocking action otherwise.
func (s *Store) ClaimPromotionSeriesOwner(service string, seriesID int, actionID string) (PromotionSeriesOwnerInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	service = strings.ToLower(strings.TrimSpace(service))
	if service == "" || seriesID <= 0 || actionID == "" {
		return PromotionSeriesOwnerInfo{}, fmt.Errorf("service, positive series_id, and action_id are required")
	}
	_, err := s.db.Exec(`INSERT INTO promotion_series_claims(service, series_id, owner_action_id)
		VALUES (?, ?, ?) ON CONFLICT(service, series_id) DO UPDATE SET owner_action_id=excluded.owner_action_id
		WHERE `+promotionSeriesTakeoverSQL,
		service, seriesID, actionID)
	if err != nil {
		return PromotionSeriesOwnerInfo{}, err
	}
	return s.promotionSeriesOwner(service, seriesID)
}
