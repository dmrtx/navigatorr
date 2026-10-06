package store

import "fmt"

// Project only the ledger fields needed for grouping and accounting. JSON is
// filtered in SQLite before allocating/decoding large media and audit reports.
// Original action payloads and the full-history APIs remain untouched.
func maintenanceProjection(column, keys string) string {
	return fmt.Sprintf(`(SELECT COALESCE('{' || group_concat(json_quote(key) || ':' ||
		CASE WHEN type='object' AND key='original' THEN json_object('size_bytes',json_extract(value,'$.size_bytes'),'duration_sec',json_extract(value,'$.duration_sec'))
		WHEN type='object' AND key='result' THEN json_object('size_bytes',json_extract(value,'$.size_bytes'))
		WHEN type='object' AND key='promotion' THEN json_remove(value,'$.quality','$.languages','$.commands')
		WHEN type='object' AND key='benchmark_decision' THEN json_object('winner',json_object('estimated_bytes',json_extract(value,'$.winner.estimated_bytes')))
		ELSE CASE type WHEN 'object' THEN value WHEN 'array' THEN value
		WHEN 'true' THEN 'true' WHEN 'false' THEN 'false' WHEN 'null' THEN 'null'
		ELSE json_quote(value) END END) || '}', '{}')
		FROM json_each(CASE WHEN json_valid(%s) THEN %s ELSE '{}' END)
		WHERE key IN (%s))`, column, column, keys)
}

// ListMaintenanceActionSnapshot reads the shared workflow ledger consistently.
// Filtering and accounting must precede UI pagination, and offset scans ordered
// by updated_at could otherwise miss or duplicate actions while reconciliation
// updates them. No separate browser queue or savings ledger is maintained.
func (s *Store) ListMaintenanceActionSnapshot() ([]ActionInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inputs := maintenanceProjection("inputs_json", `'path','parent_action_id','transcode_action_id','library_context','service','series_id','movie_id','media_id','paths','episode_file_ids','dry_run','promote_candidates'`)
	keys := `'resolved_path','original_sha256','original','result','candidate_size_bytes','promotion','recovery_retained','original_integrity','promoted','original_intact','benchmark_decision','expected_savings_percent','transcode_phase','progress_is_stale','progress','speed','last_progress_at','counts','batch_promotion','benchmark_comparison_available','job_id','transcode_job_id','paused','skip_transcode'`
	rows, err := s.db.Query(`SELECT id, action_name, status, current_step, ` +
		inputs + `, ` + maintenanceProjection("outputs_json", keys) + `, ` + maintenanceProjection("state_json", keys) + `, waiting_reason, waiting_condition,
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
