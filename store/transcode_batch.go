package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
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
	CandidatePath string   `json:"candidate_path"`
	Error         string   `json:"error"`
	Attempts      int      `json:"attempts"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
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
		decision, profile, reasons_json, status, child_action_id, job_id,
		candidate_path, error, attempts, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.BatchID, item.ItemKey, item.FilePath, item.DisplayLabel, item.EpisodeInfo,
		item.Decision, item.Profile, string(reasonsJSON), item.Status, item.ChildActionID, item.JobID,
		item.CandidatePath, item.Error, item.Attempts, item.CreatedAt, item.UpdatedAt)
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
		profile = ?, reasons_json = ?, status = ?, child_action_id = ?, job_id = ?,
		candidate_path = ?, error = ?, attempts = ?, updated_at = ?
		WHERE batch_id = ? AND item_key = ?`,
		item.FilePath, item.DisplayLabel, item.EpisodeInfo, item.Decision,
		item.Profile, string(reasonsJSON), item.Status, item.ChildActionID, item.JobID,
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
		episode_info, decision, profile, reasons_json, status, child_action_id, job_id,
		candidate_path, error, attempts, created_at, updated_at
		FROM transcode_batch_items WHERE batch_id = ? AND item_key = ?`,
		batchID, itemKey).Scan(
		&item.ID, &item.BatchID, &item.ItemKey, &item.FilePath, &item.DisplayLabel,
		&item.EpisodeInfo, &item.Decision, &item.Profile, &reasonsJSON, &item.Status,
		&item.ChildActionID, &item.JobID, &item.CandidatePath, &item.Error, &item.Attempts,
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
		episode_info, decision, profile, reasons_json, status, child_action_id, job_id,
		candidate_path, error, attempts, created_at, updated_at
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
			episode_info, decision, profile, reasons_json, status, child_action_id, job_id,
			candidate_path, error, attempts, created_at, updated_at
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
			episode_info, decision, profile, reasons_json, status, child_action_id, job_id,
			candidate_path, error, attempts, created_at, updated_at
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
			&item.ChildActionID, &item.JobID, &item.CandidatePath, &item.Error, &item.Attempts,
			&item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(reasonsJSON), &item.Reasons)
		items = append(items, item)
	}
	return items, rows.Err()
}
