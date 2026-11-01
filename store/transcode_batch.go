package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// TranscodeBatchItem represents one media item tracked within a transcode_batch workflow.
type TranscodeBatchItem struct {
	ID            int64    `json:"id"`
	BatchID       string   `json:"batch_id"`
	ItemKey       string   `json:"item_key"`
	FilePath      string   `json:"file_path"`
	DisplayLabel  string   `json:"display_label"`
	EpisodeInfo   string   `json:"episode_info"`
	Decision      string   `json:"decision"`
	Profile       string   `json:"profile"`
	Reasons       []string `json:"reasons"`
	Status        string   `json:"status"` // queued, skip, review, waiting_for_slot, waiting_decision, running, completed, failed
	ChildActionID string   `json:"child_action_id"`
	JobID         string   `json:"job_id"`
	// Worker is the worker the child action was assigned, once it has one.
	Worker        string `json:"worker,omitempty"`
	CandidatePath string `json:"candidate_path"`
	Error         string `json:"error"`
	Attempts      int    `json:"attempts"`
	// SourceBytes is the file's size when the batch planned it; 0 when unknown.
	SourceBytes int64  `json:"source_bytes,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// CreateTranscodeBatchItem inserts a new batch item record into the database.
func (s *Store) CreateTranscodeBatchItem(item TranscodeBatchItem) error {
	if item.BatchID == "" || item.ItemKey == "" {
		return fmt.Errorf("batch_id and item_key are required")
	}
	reasonsJSON, _ := json.Marshal(item.Reasons)
	if item.Reasons == nil {
		reasonsJSON = []byte("[]")
	}
	now := nowStr()
	if item.CreatedAt == "" {
		item.CreatedAt = now
	}
	item.UpdatedAt = now

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`INSERT INTO transcode_batch_items (
		batch_id, item_key, file_path, display_label, episode_info,
		decision, profile, reasons_json, status, child_action_id, job_id, worker,
		candidate_path, error, attempts, source_bytes, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.BatchID, item.ItemKey, item.FilePath, item.DisplayLabel, item.EpisodeInfo,
		item.Decision, item.Profile, string(reasonsJSON), item.Status, item.ChildActionID, item.JobID, item.Worker,
		item.CandidatePath, item.Error, item.Attempts, item.SourceBytes, item.CreatedAt, item.UpdatedAt)
	return err
}

// UpdateTranscodeBatchItem updates an existing batch item record.
func (s *Store) UpdateTranscodeBatchItem(item TranscodeBatchItem) error {
	if item.BatchID == "" || item.ItemKey == "" {
		return fmt.Errorf("batch_id and item_key are required")
	}
	reasonsJSON, _ := json.Marshal(item.Reasons)
	if item.Reasons == nil {
		reasonsJSON = []byte("[]")
	}
	now := nowStr()
	item.UpdatedAt = now

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`UPDATE transcode_batch_items SET
		file_path = ?, display_label = ?, episode_info = ?, decision = ?,
		profile = ?, reasons_json = ?, status = ?, child_action_id = ?, job_id = ?, worker = ?,
		candidate_path = ?, error = ?, attempts = ?, updated_at = ?
		WHERE batch_id = ? AND item_key = ?`,
		item.FilePath, item.DisplayLabel, item.EpisodeInfo, item.Decision,
		item.Profile, string(reasonsJSON), item.Status, item.ChildActionID, item.JobID, item.Worker,
		item.CandidatePath, item.Error, item.Attempts, item.UpdatedAt,
		item.BatchID, item.ItemKey)
	return err
}

// GetTranscodeBatchItem retrieves a batch item by batchID and itemKey.
func (s *Store) GetTranscodeBatchItem(batchID, itemKey string) (*TranscodeBatchItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var item TranscodeBatchItem
	var reasonsJSON string
	err := s.db.QueryRow(`SELECT id, batch_id, item_key, file_path, display_label,
		episode_info, decision, profile, reasons_json, status, child_action_id, job_id, worker,
		candidate_path, error, attempts, source_bytes, created_at, updated_at
		FROM transcode_batch_items WHERE batch_id = ? AND item_key = ?`,
		batchID, itemKey).Scan(
		&item.ID, &item.BatchID, &item.ItemKey, &item.FilePath, &item.DisplayLabel,
		&item.EpisodeInfo, &item.Decision, &item.Profile, &reasonsJSON, &item.Status,
		&item.ChildActionID, &item.JobID, &item.Worker, &item.CandidatePath, &item.Error, &item.Attempts, &item.SourceBytes,
		&item.CreatedAt, &item.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(reasonsJSON), &item.Reasons)
	return &item, nil
}

// ListTranscodeBatchItems lists all items associated with a batch.
func (s *Store) ListTranscodeBatchItems(batchID string) ([]TranscodeBatchItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT id, batch_id, item_key, file_path, display_label,
		episode_info, decision, profile, reasons_json, status, child_action_id, job_id, worker,
		candidate_path, error, attempts, source_bytes, created_at, updated_at
		FROM transcode_batch_items WHERE batch_id = ? ORDER BY id ASC`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTranscodeBatchItems(rows)
}

// FindTranscodeBatchItemByChildActionID returns the single batch item whose
// child_action_id references the given child action. A non-unique association
// is rejected so a manipulated link cannot mask the real parent.
func (s *Store) FindTranscodeBatchItemByChildActionID(childActionID string) (*TranscodeBatchItem, error) {
	if childActionID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findTranscodeBatchItemLocked(
		`SELECT id, batch_id, item_key, file_path, display_label,
			episode_info, decision, profile, reasons_json, status, child_action_id, job_id, worker,
			candidate_path, error, attempts, source_bytes, created_at, updated_at
			FROM transcode_batch_items WHERE child_action_id = ? LIMIT 2`, childActionID)
}

// FindTranscodeBatchItemByChildKey returns the single batch item whose exact
// constructed child idempotency key ('batch-' || batch_id || '-' || item_key)
// matches the supplied key. The key is compared as a whole; it is never split
// on separators, so ids containing '-' are handled unambiguously.
func (s *Store) FindTranscodeBatchItemByChildKey(childKey string) (*TranscodeBatchItem, error) {
	if childKey == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findTranscodeBatchItemLocked(
		`SELECT id, batch_id, item_key, file_path, display_label,
			episode_info, decision, profile, reasons_json, status, child_action_id, job_id, worker,
			candidate_path, error, attempts, source_bytes, created_at, updated_at
			FROM transcode_batch_items
			WHERE ? = 'batch-' || batch_id || '-' || item_key LIMIT 2`, childKey)
}

func (s *Store) findTranscodeBatchItemLocked(query string, args ...any) (*TranscodeBatchItem, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := scanTranscodeBatchItems(rows)
	if err != nil {
		return nil, err
	}
	if len(items) > 1 {
		return nil, fmt.Errorf("ambiguous transcode_batch item association: %d matches", len(items))
	}
	if len(items) == 0 {
		return nil, nil
	}
	return &items[0], nil
}

func scanTranscodeBatchItems(rows *sql.Rows) ([]TranscodeBatchItem, error) {
	var items []TranscodeBatchItem
	for rows.Next() {
		var item TranscodeBatchItem
		var reasonsJSON string
		if err := rows.Scan(
			&item.ID, &item.BatchID, &item.ItemKey, &item.FilePath, &item.DisplayLabel,
			&item.EpisodeInfo, &item.Decision, &item.Profile, &reasonsJSON, &item.Status,
			&item.ChildActionID, &item.JobID, &item.Worker, &item.CandidatePath, &item.Error, &item.Attempts, &item.SourceBytes,
			&item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(reasonsJSON), &item.Reasons)
		items = append(items, item)
	}
	return items, rows.Err()
}

// BatchSourceTotal is the size of the files a batch planned: the files it
// listed (whatever was decided about each), how many of them have a size on
// record, and the sum of those sizes. A size of 0 is read as unknown: a video
// file is never empty, and an older row cannot say which it was.
type BatchSourceTotal struct {
	Items int
	Sized int
	Bytes int64
}

// BatchSourceTotals returns the totals of the given batches, by batch id. A
// batch with no items has no entry. Items planned before the size was recorded
// count as unsized, and a file listed under two item keys counts once.
func (s *Store) BatchSourceTotals(batchIDs []string) (map[string]BatchSourceTotal, error) {
	out := map[string]BatchSourceTotal{}
	const chunk = 400
	s.mu.Lock()
	defer s.mu.Unlock()
	for start := 0; start < len(batchIDs); start += chunk {
		ids := batchIDs[start:min(start+chunk, len(batchIDs))]
		marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		rows, err := s.db.Query(`SELECT batch_id, COUNT(*), SUM(CASE WHEN size > 0 THEN 1 ELSE 0 END), COALESCE(SUM(size), 0)
			FROM (SELECT batch_id, file_path, MAX(source_bytes) AS size FROM transcode_batch_items
				WHERE batch_id IN (`+marks+`) GROUP BY batch_id, file_path)
			GROUP BY batch_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var t BatchSourceTotal
			if err := rows.Scan(&id, &t.Items, &t.Sized, &t.Bytes); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = t
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
