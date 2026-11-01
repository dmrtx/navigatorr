package store

// QueuedBatchItemRow is a batch item still waiting to start, with the batch
// fields the transcode queue orders on.
type QueuedBatchItemRow struct {
	BatchID        string
	BatchCreatedAt string
	ItemID         int64
}

// ListQueuedBatchItems returns the queued items of the batches that can still
// start them: live (pending, running or waiting_external), not paused and not
// being cancelled. A batch waiting for a decision starts nothing, so its
// items do not hold anyone back.
func (s *Store) ListQueuedBatchItems() ([]QueuedBatchItemRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT i.batch_id, a.created_at, i.id
		FROM transcode_batch_items i JOIN action_instances a ON a.id = i.batch_id
		WHERE i.status = 'queued'
		AND a.action_name = 'transcode_batch'
		AND a.status IN ('pending', 'running', 'waiting_external')
		AND json_valid(a.state_json)
		AND COALESCE(json_extract(a.state_json, '$.paused'), 0) NOT IN (1, 'true')
		AND COALESCE(json_extract(a.state_json, '$.cancel_requested'), 0) NOT IN (1, 'true')
		ORDER BY i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedBatchItemRow
	for rows.Next() {
		var r QueuedBatchItemRow
		if err := rows.Scan(&r.BatchID, &r.BatchCreatedAt, &r.ItemID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TranscodeQueueWaiterRow is a conversion waiting in the transcode queue for
// a worker to be assigned.
type TranscodeQueueWaiterRow struct {
	ID         string
	QueueOrder string // transcode_queue_order
	UpdatedAt  string
}

// ListTranscodeQueueWaiters returns the live conversions that took a place in
// the transcode queue (transcode_queue_order) and have no worker yet. A
// paused conversion, or the child of a paused or finished batch, is left out:
// it cannot take a slot now.
func (s *Store) ListTranscodeQueueWaiters() ([]TranscodeQueueWaiterRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT a.id, json_extract(a.state_json, '$.transcode_queue_order'), a.updated_at
		FROM action_instances a
		LEFT JOIN action_instances p ON p.id = COALESCE(json_extract(a.inputs_json, '$.parent_action_id'), '')
		WHERE a.action_name = 'transcode_media'
		AND a.status IN ('pending', 'running', 'waiting_external')
		AND json_valid(a.state_json)
		AND json_type(a.state_json, '$.transcode_queue_order') = 'text'
		AND json_type(a.state_json, '$.worker') IS NULL
		AND COALESCE(json_extract(a.state_json, '$.paused'), 0) NOT IN (1, 'true')
		AND (p.id IS NULL OR (p.status IN ('pending', 'running', 'waiting_external')
			AND json_valid(p.state_json)
			AND COALESCE(json_extract(p.state_json, '$.paused'), 0) NOT IN (1, 'true')
			AND COALESCE(json_extract(p.state_json, '$.cancel_requested'), 0) NOT IN (1, 'true')))
		ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TranscodeQueueWaiterRow
	for rows.Next() {
		var r TranscodeQueueWaiterRow
		if err := rows.Scan(&r.ID, &r.QueueOrder, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
