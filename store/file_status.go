package store

// FileStatusRow is one transcode batch item with the state of the actions that
// decide what the Files tab shows for its file: its batch, the attempt that
// converted it, and any replacement of that attempt's candidate.
type FileStatusRow struct {
	ItemID        int64
	FilePath      string
	ItemStatus    string
	Decision      string
	CandidatePath string
	Error         string
	BatchStatus   string
	// ChildStatus is empty until the item has an attempt.
	ChildStatus  string
	ChildSkipped bool
	// ChildQueuedOnWorker is true while the worker holds the attempt in its line
	// without encoding it yet.
	ChildQueuedOnWorker bool
	// ReplacementStatus is empty until the candidate has a replacement action.
	ReplacementStatus string
	Replaced          bool
}

// ListFileStatusRows returns every batch item of a real run, oldest first. A
// preview (dry run) only plans a batch, so its items never mark a file.
func (s *Store) ListFileStatusRows() ([]FileStatusRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT i.id, i.file_path, i.status, i.decision, i.candidate_path, i.error,
		b.status,
		COALESCE(c.status, ''),
		CASE WHEN json_valid(c.outputs_json) AND (COALESCE(json_extract(c.outputs_json, '$.skip_transcode'), 0) IN (1, 'true') OR COALESCE(json_extract(c.outputs_json, '$.skipped'), 0) IN (1, 'true')) THEN 1 ELSE 0 END,
		CASE WHEN c.status NOT IN ('completed', 'failed', 'cancelled') AND COALESCE(
			CASE WHEN json_valid(c.outputs_json) THEN json_extract(c.outputs_json, '$.transcode_phase') END,
			CASE WHEN json_valid(c.state_json) THEN json_extract(c.state_json, '$.transcode_phase') END, '') = 'queued' THEN 1 ELSE 0 END,
		COALESCE(r.status, ''),
		CASE WHEN json_valid(r.outputs_json) AND COALESCE(json_extract(r.outputs_json, '$.promoted'), 0) IN (1, 'true') THEN 1 ELSE 0 END
		FROM transcode_batch_items i
		JOIN action_instances b ON b.id = i.batch_id
		LEFT JOIN action_instances c ON i.child_action_id != '' AND c.id = i.child_action_id
		LEFT JOIN action_instances r ON i.child_action_id != '' AND r.action_name = 'promote_transcode_candidate'
			AND r.idempotency_key IN ('promote:sonarr:' || i.child_action_id, 'promote:radarr:' || i.child_action_id, 'promote:filesystem:' || i.child_action_id)
		WHERE NOT (json_valid(b.inputs_json) AND COALESCE(json_extract(b.inputs_json, '$.dry_run'), 0) IN (1, 'true'))
		ORDER BY i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileStatusRow
	for rows.Next() {
		var r FileStatusRow
		var skipped, queued, replaced int
		if err := rows.Scan(&r.ItemID, &r.FilePath, &r.ItemStatus, &r.Decision, &r.CandidatePath, &r.Error, &r.BatchStatus, &r.ChildStatus, &skipped, &queued, &r.ReplacementStatus, &replaced); err != nil {
			return nil, err
		}
		r.ChildSkipped, r.ChildQueuedOnWorker, r.Replaced = skipped == 1, queued == 1, replaced == 1
		out = append(out, r)
	}
	return out, rows.Err()
}
