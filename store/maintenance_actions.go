package store

import "encoding/json"

// Memoize only JSON projections, never status/checkpoints or the ledger itself.
// Every snapshot rereads authoritative rows and compares the complete source
// JSON, so same-second and same-size writes also invalidate an entry.
const maintenanceMemoLimit = 64 * 1024 * 1024

type maintenanceMemo struct {
	source, projected [3]string
}

func (m maintenanceMemo) bytes() int {
	n := 0
	for i := range m.source {
		n += len(m.source[i]) + len(m.projected[i])
	}
	return n
}

var maintenanceInputKeys = []string{"path", "parent_action_id", "batch_promote_parent_id", "transcode_action_id", "library_context", "service", "series_id", "movie_id", "media_id", "paths", "episode_file_ids", "dry_run", "promote_candidates"}
var maintenanceOutputKeys = []string{"resolved_path", "original_sha256", "original", "result", "candidate_size_bytes", "promotion", "recovery_retained", "original_integrity", "promoted", "original_intact", "benchmark_decision", "expected_savings_percent", "transcode_phase", "progress_is_stale", "progress", "speed", "last_progress_at", "counts", "batch_promotion", "benchmark_comparison_available", "job_id", "transcode_job_id", "paused", "skip_transcode", "batch_item_generations", "batch_settings_revision", "batch_retry_pending", "post_batch_promotion"}

func maintenanceJSON(raw string, keys []string) string {
	var source map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &source) != nil {
		return "{}"
	}
	result := make(map[string]json.RawMessage, len(keys))
	for _, key := range keys {
		value, ok := source[key]
		if !ok {
			continue
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(value, &nested) == nil && nested != nil {
			switch key {
			case "original":
				value = json.RawMessage(maintenanceJSON(string(value), []string{"size_bytes", "duration_sec"}))
			case "result":
				value = json.RawMessage(maintenanceJSON(string(value), []string{"size_bytes"}))
			case "promotion":
				delete(nested, "quality")
				delete(nested, "languages")
				delete(nested, "commands")
				value, _ = json.Marshal(nested)
			case "benchmark_decision":
				if winner, exists := nested["winner"]; exists {
					nested = map[string]json.RawMessage{"winner": json.RawMessage(maintenanceJSON(string(winner), []string{"estimated_bytes"}))}
				} else {
					nested = map[string]json.RawMessage{}
				}
				value, _ = json.Marshal(nested)
			}
		}
		result[key] = value
	}
	data, _ := json.Marshal(result)
	return string(data)
}

// ListMaintenanceActionSnapshot reads the shared workflow ledger consistently.
// Filtering and accounting precede UI pagination. Full action history remains
// unchanged, and all metadata is fresh even when a payload projection is reused.
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
	if s.maintenanceMemo == nil {
		s.maintenanceMemo = map[string]maintenanceMemo{}
	}
	next := make(map[string]maintenanceMemo)
	nextBytes := 0
	var actions []ActionInstance
	for rows.Next() {
		var inst ActionInstance
		if err := rows.Scan(&inst.ID, &inst.ActionName, &inst.Status, &inst.CurrentStep,
			&inst.InputsJSON, &inst.OutputsJSON, &inst.StateJSON, &inst.WaitingReason,
			&inst.WaitingCondition, &inst.WaitingOptionsJSON, &inst.ErrorJSON,
			&inst.IdempotencyKey, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
			return nil, err
		}
		source := [3]string{inst.InputsJSON, inst.OutputsJSON, inst.StateJSON}
		memo, ok := s.maintenanceMemo[inst.ID]
		if !ok || memo.source != source {
			memo = maintenanceMemo{source: source, projected: [3]string{maintenanceJSON(source[0], maintenanceInputKeys), maintenanceJSON(source[1], maintenanceOutputKeys), maintenanceJSON(source[2], maintenanceOutputKeys)}}
		}
		if size := memo.bytes(); nextBytes+size <= maintenanceMemoLimit {
			next[inst.ID] = memo
			nextBytes += size
		}
		inst.InputsJSON, inst.OutputsJSON, inst.StateJSON = memo.projected[0], memo.projected[1], memo.projected[2]
		actions = append(actions, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Drop removed actions and enforce a bounded memory budget every snapshot.
	s.maintenanceMemo = next
	return actions, nil
}
