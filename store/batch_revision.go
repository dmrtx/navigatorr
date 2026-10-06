package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// CommitBatchRevision publishes the journal and item resets atomically, fenced
// by the coordinator lease. A crash cannot expose half of a new attempt.
func (s *Store) CommitBatchRevision(inst ActionInstance, owner string, items []TranscodeBatchItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var lease string
	if err := tx.QueryRow(`SELECT owner FROM action_execution_leases WHERE action_id=? AND expires_at_ms>?`, inst.ID, time.Now().UnixMilli()).Scan(&lease); err != nil || owner == "" || lease != owner {
		return fmt.Errorf("batch execution lease lost")
	}
	for _, item := range items {
		if item.BatchID != inst.ID {
			return fmt.Errorf("item belongs to another batch")
		}
		reasons, err := json.Marshal(item.Reasons)
		if err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE transcode_batch_items SET decision=?,profile=?,reasons_json=?,status=?,child_action_id=?,job_id=?,candidate_path=?,error=?,attempts=?,updated_at=? WHERE batch_id=? AND item_key=?`, item.Decision, item.Profile, string(reasons), item.Status, item.ChildActionID, item.JobID, item.CandidatePath, item.Error, item.Attempts, nowStr(), inst.ID, item.ItemKey)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("batch item disappeared")
		}
	}
	_, err = tx.Exec(`UPDATE action_instances SET status=?,current_step=?,state_json=?,outputs_json=?,waiting_reason=?,waiting_condition=?,waiting_options_json=?,error_json=?,updated_at=? WHERE id=?`, inst.Status, inst.CurrentStep, inst.StateJSON, inst.OutputsJSON, inst.WaitingReason, inst.WaitingCondition, inst.WaitingOptionsJSON, inst.ErrorJSON, nowStr(), inst.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
