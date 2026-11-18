package store

import "fmt"

// RenameWorker rewrites what the store kept under a worker's former name (an
// alias in the worker config) to its current name, one transaction per alias: the
// worker in the state of every action, in batch items, in builds and the slot
// settings, transcode pause and builds pause of the node. A node that already
// has its own row under the new name keeps it and the old one is dropped, so the
// current name wins. It reports how many rows it changed and is idempotent: with
// nothing left under the old name it changes nothing.
func (s *Store) RenameWorker(old, current string) (int64, error) {
	if old == "" || current == "" || old == current {
		return 0, fmt.Errorf("rename worker: two different names are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var total int64
	run := func(query string, args ...any) error {
		res, err := tx.Exec(query, args...)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		total += n
		return nil
	}
	for _, key := range []string{"$.worker", "$.upload_worker"} {
		if err := run(`UPDATE action_instances SET state_json = json_set(state_json, '`+key+`', ?)
			WHERE json_valid(state_json) AND json_extract(state_json, '`+key+`') = ?`, current, old); err != nil {
			return 0, err
		}
	}
	if err := run(`UPDATE transcode_batch_items SET worker = ? WHERE worker = ?`, current, old); err != nil {
		return 0, err
	}
	if err := run(`UPDATE builds SET target = ? WHERE target = ?`, current, old); err != nil {
		return 0, err
	}
	for _, t := range []struct{ table, key string }{
		{"worker_slot_settings", "node"}, {"worker_pauses", "worker"}, {"build_pauses", "worker"},
	} {
		if err := run(`UPDATE `+t.table+` SET `+t.key+` = ? WHERE `+t.key+` = ?
			AND NOT EXISTS (SELECT 1 FROM `+t.table+` WHERE `+t.key+` = ?)`, current, old, current); err != nil {
			return 0, err
		}
		if err := run(`DELETE FROM `+t.table+` WHERE `+t.key+` = ?`, old); err != nil {
			return 0, err
		}
	}
	return total, tx.Commit()
}
