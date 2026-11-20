package store

import (
	"database/sql"
	"fmt"
)

// Queue marks: what an operator decided about the place of a waiting
// conversion in the transcode queue, kept apart from the action's own state
// (the steps rewrite that whole, which would drop a hold made while one ran).

// Mark kinds.
const (
	QueueMarkAction = "action"
	QueueMarkBatch  = "batch"
)

// QueueMark is the hold and the order key an operator gave to one conversion
// (kind action) or the key prefix of a whole batch (kind batch).
type QueueMark struct {
	Held  bool
	Order string // "" when none
}

// QueueRevision is the counter every change of a hold or an order moves.
func (s *Store) QueueRevision() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rev int64
	err := s.db.QueryRow(`SELECT revision FROM transcode_queue_revision WHERE id = 1`).Scan(&rev)
	return rev, err
}

// GetQueueMark returns the mark of a conversion or a batch (zero when none).
func (s *Store) GetQueueMark(kind, id string) (QueueMark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var held int
	var m QueueMark
	err := s.db.QueryRow(`SELECT held, queue_order FROM transcode_queue_marks WHERE kind = ? AND id = ?`, kind, id).Scan(&held, &m.Order)
	if err == sql.ErrNoRows {
		return QueueMark{}, nil
	}
	m.Held = held != 0
	return m, err
}

// QueueItemMark is the hold and the order key of one batch item.
type QueueItemMark struct {
	Held  bool
	Order string
}

// GetQueueItemMark returns the hold and the order of a batch item.
func (s *Store) GetQueueItemMark(itemID int64) (QueueItemMark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var held int
	var m QueueItemMark
	err := s.db.QueryRow(`SELECT queue_held, COALESCE(queue_order, '') FROM transcode_batch_items WHERE id = ?`, itemID).Scan(&held, &m.Order)
	if err == sql.ErrNoRows {
		return QueueItemMark{}, nil
	}
	m.Held = held != 0
	return m, err
}

// GetTranscodeBatchItemByID returns a batch item by its id (nil when none).
func (s *Store) GetTranscodeBatchItemByID(id int64) (*TranscodeBatchItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findTranscodeBatchItemLocked(
		`SELECT id, batch_id, item_key, file_path, display_label,
			episode_info, decision, profile, reasons_json, status, child_action_id, job_id, worker,
			candidate_path, error, attempts, source_bytes, created_at, updated_at
			FROM transcode_batch_items WHERE id = ? LIMIT 2`, id)
}

// QueueWrite is a set of changes to holds and orders, applied together with
// one move of the revision. An order of "" clears it.
type QueueWrite struct {
	ActionHeld  map[string]bool
	ActionOrder map[string]string
	BatchOrder  map[string]string
	ItemHeld    map[int64]bool
	ItemOrder   map[int64]string
	// ClearItemOrdersOf clears the order of every item of these batches.
	ClearItemOrdersOf []string
	// ExpectRevision, when set, is the revision the change was decided on: the
	// write is refused with a StaleQueueRevisionError if the queue moved since.
	ExpectRevision *int64
}

// StaleQueueRevisionError is a write refused because the queue's revision is
// not the one the change was decided on.
type StaleQueueRevisionError struct {
	Want, Have int64
}

func (e *StaleQueueRevisionError) Error() string {
	return fmt.Sprintf("the queue changed since it was read (queue_revision %d, now %d); read it again", e.Want, e.Have)
}

// ApplyQueueWrite applies w in one transaction and returns the new revision.
func (s *Store) ApplyQueueWrite(w QueueWrite) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if w.ExpectRevision != nil {
		var have int64
		if err := tx.QueryRow(`SELECT revision FROM transcode_queue_revision WHERE id = 1`).Scan(&have); err != nil {
			return 0, err
		}
		if have != *w.ExpectRevision {
			return 0, &StaleQueueRevisionError{Want: *w.ExpectRevision, Have: have}
		}
	}
	now := nowStr()
	for id, held := range w.ActionHeld {
		if _, err := tx.Exec(`INSERT INTO transcode_queue_marks (kind, id, held, queue_order, updated_at) VALUES ('action', ?, ?, '', ?)
			ON CONFLICT(kind, id) DO UPDATE SET held = excluded.held, updated_at = excluded.updated_at`, id, b2i(held), now); err != nil {
			return 0, err
		}
	}
	for kind, orders := range map[string]map[string]string{"action": w.ActionOrder, "batch": w.BatchOrder} {
		for id, order := range orders {
			if _, err := tx.Exec(`INSERT INTO transcode_queue_marks (kind, id, held, queue_order, updated_at) VALUES (?, ?, 0, ?, ?)
				ON CONFLICT(kind, id) DO UPDATE SET queue_order = excluded.queue_order, updated_at = excluded.updated_at`, kind, id, order, now); err != nil {
				return 0, err
			}
		}
	}
	for id, held := range w.ItemHeld {
		if _, err := tx.Exec(`UPDATE transcode_batch_items SET queue_held = ? WHERE id = ?`, b2i(held), id); err != nil {
			return 0, err
		}
	}
	for id, order := range w.ItemOrder {
		var v any
		if order != "" {
			v = order
		}
		if _, err := tx.Exec(`UPDATE transcode_batch_items SET queue_order = ? WHERE id = ?`, v, id); err != nil {
			return 0, err
		}
	}
	for _, batch := range w.ClearItemOrdersOf {
		if _, err := tx.Exec(`UPDATE transcode_batch_items SET queue_order = NULL WHERE batch_id = ?`, batch); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(`UPDATE transcode_queue_revision SET revision = revision + 1 WHERE id = 1`); err != nil {
		return 0, err
	}
	var rev int64
	if err := tx.QueryRow(`SELECT revision FROM transcode_queue_revision WHERE id = 1`).Scan(&rev); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("saving the queue change: %w", err)
	}
	return rev, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// HeldBatchItemIDs are the items of a batch the operator holds: a queued item
// held itself, or an item whose child conversion is held. A hold moves from
// the item to its child in one write (the item's flag is cleared as the
// child's mark is set), so an item flag still set is a hold not yet carried.
func (s *Store) HeldBatchItemIDs(batchID string) (map[int64]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT i.id FROM transcode_batch_items i
		WHERE i.batch_id = ? AND i.status IN ('queued', 'waiting_for_slot')
		AND (i.queue_held = 1 OR EXISTS (SELECT 1 FROM transcode_queue_marks m
			WHERE m.kind = 'action' AND m.id = i.child_action_id AND m.held = 1))`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// WindowWaitingBatchItemIDs are the items of a batch whose conversion waits
// only for a node's availability window (waiting condition node_window): the
// batch neither counts them as contending for a slot nor lets them hold its
// other items back.
func (s *Store) WindowWaitingBatchItemIDs(batchID string) (map[int64]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT i.id FROM transcode_batch_items i
		JOIN action_instances a ON a.id = i.child_action_id
		WHERE i.batch_id = ? AND i.status IN ('queued', 'waiting_for_slot')
		AND a.status = 'waiting_external' AND a.waiting_condition = 'node_window'`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
