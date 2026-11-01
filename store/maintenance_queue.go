package store

import (
	"fmt"
	"strings"
)

// The console closes candidates by hand. Its two verdicts are recorded in the
// item's notes so they can be told from a close made by the safe-replacement
// workflow, which alone proves a replacement was verified and imported.
const (
	// ConsoleDismissedMark is on an item the owner dropped (status failed).
	ConsoleDismissedMark = "[consola: descartada]"
	// ConsoleProcessedMark is on an item the owner says is already handled (status done).
	ConsoleProcessedMark = "[consola: procesada]"
)

// MaintenanceCounts says how many items are open, processed and dismissed.
type MaintenanceCounts struct {
	Open      int `json:"open"`
	Processed int `json:"processed"`
	Dismissed int `json:"dismissed"`
}

// ConsoleClosed reports how the owner closed the item from the console, "" if not.
func (it MaintenanceItem) ConsoleClosed() string {
	switch {
	case it.Status == MaintFailed && strings.Contains(it.Notes, ConsoleDismissedMark):
		return "dismissed"
	case it.Status == MaintDone && strings.Contains(it.Notes, ConsoleProcessedMark):
		return "processed"
	}
	return ""
}

// ListItemsForConsole returns the items in the given statuses (all when empty),
// highest priority first, then newest. Unlike ListItems it is not capped at 100:
// the console shows the whole queue.
func (s *Store) ListItemsForConsole(statuses []string, issueType string, limit int) ([]MaintenanceItem, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q := `SELECT ` + maintColumns + ` FROM maintenance_items WHERE 1=1`
	var args []any
	if len(statuses) > 0 {
		q += ` AND status IN (` + strings.Repeat("?,", len(statuses)-1) + `?)`
		for _, st := range statuses {
			if !ValidMaintStatus(st) {
				return nil, fmt.Errorf("unknown status %q", st)
			}
			args = append(args, st)
		}
	}
	if issueType != "" {
		q += ` AND issue_type=?`
		args = append(args, issueType)
	}
	q += ` ORDER BY priority DESC, updated_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MaintenanceItem{}
	for rows.Next() {
		var it MaintenanceItem
		if err := rows.Scan(maintScan(&it)...); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CountItemsForConsole counts open, processed and dismissed items. Items the
// workflow closed (done or failed without a console mark) count as processed
// and dismissed respectively, so the three always add up to the queue.
func (s *Store) CountItemsForConsole(issueType string) (MaintenanceCounts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := `SELECT status, COUNT(*) FROM maintenance_items`
	var args []any
	if issueType != "" {
		q += ` WHERE issue_type=?`
		args = append(args, issueType)
	}
	q += ` GROUP BY status`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return MaintenanceCounts{}, err
	}
	defer rows.Close()
	var c MaintenanceCounts
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return MaintenanceCounts{}, err
		}
		switch status {
		case MaintDone:
			c.Processed += n
		case MaintFailed:
			c.Dismissed += n
		default:
			c.Open += n
		}
	}
	return c, rows.Err()
}

func appendNote(notes, add string) string {
	if notes == "" {
		return add
	}
	return notes + "; " + add
}

// DismissItem drops an open item for good: it ends as failed with the console
// mark, and a later maintenance_add of the same media and issue returns it
// instead of queueing the candidate again. Dismissing a dismissed item is a no-op.
func (s *Store) DismissItem(id int64, note string) (MaintenanceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.getItemLocked(id)
	if err != nil {
		return MaintenanceItem{}, err
	}
	if it.ConsoleClosed() == "dismissed" {
		return it, nil
	}
	if it.Status == MaintDone || it.Status == MaintFailed {
		return MaintenanceItem{}, fmt.Errorf("item %d is already %s", id, it.Status)
	}
	reason := ConsoleDismissedMark
	if note != "" {
		reason += " " + note
	}
	if _, err := s.db.Exec(`UPDATE maintenance_items SET status=?, notes=?,
		claimed_by='', lease_until=NULL, updated_at=? WHERE id=?`,
		MaintFailed, appendNote(it.Notes, reason), nowStr(), id); err != nil {
		return MaintenanceItem{}, err
	}
	return s.getItemLocked(id)
}

// MarkItemProcessed closes an open item the owner says is already handled. It
// bypasses the rule that done comes only from replacing, because the owner, not
// a workflow, is vouching for it; the mark in the notes keeps that visible.
func (s *Store) MarkItemProcessed(id int64, note string) (MaintenanceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.getItemLocked(id)
	if err != nil {
		return MaintenanceItem{}, err
	}
	if it.ConsoleClosed() == "processed" {
		return it, nil
	}
	if it.Status == MaintDone || it.Status == MaintFailed {
		return MaintenanceItem{}, fmt.Errorf("item %d is already %s", id, it.Status)
	}
	reason := ConsoleProcessedMark
	if note != "" {
		reason += " " + note
	}
	if _, err := s.db.Exec(`UPDATE maintenance_items SET status=?, notes=?,
		claimed_by='', lease_until=NULL, updated_at=? WHERE id=?`,
		MaintDone, appendNote(it.Notes, reason), nowStr(), id); err != nil {
		return MaintenanceItem{}, err
	}
	return s.getItemLocked(id)
}

// RestoreItem puts an item the owner closed from the console back to pending.
// Items the workflow closed are not touched: done stays immutable.
func (s *Store) RestoreItem(id int64) (MaintenanceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.getItemLocked(id)
	if err != nil {
		return MaintenanceItem{}, err
	}
	if it.ConsoleClosed() == "" {
		if it.Status != MaintDone && it.Status != MaintFailed {
			return it, nil // already open
		}
		return MaintenanceItem{}, fmt.Errorf("item %d was closed by the workflow, not from the console", id)
	}
	notes := strings.NewReplacer(ConsoleDismissedMark, "[consola: restaurada]", ConsoleProcessedMark, "[consola: restaurada]").Replace(it.Notes)
	if _, err := s.db.Exec(`UPDATE maintenance_items SET status=?, notes=?,
		claimed_by='', lease_until=NULL, updated_at=? WHERE id=?`,
		MaintPending, notes, nowStr(), id); err != nil {
		return MaintenanceItem{}, err
	}
	return s.getItemLocked(id)
}

// consoleDismissedLocked returns the item the owner dropped for this media and
// issue, if any, so AddItem does not queue it again.
func (s *Store) consoleDismissedLocked(it MaintenanceItem) (MaintenanceItem, bool) {
	var found MaintenanceItem
	err := s.db.QueryRow(`SELECT `+maintColumns+` FROM maintenance_items
		WHERE service=? AND media_type=? AND media_id=? AND issue_type=?
		AND status=? AND instr(notes, ?) > 0 ORDER BY id DESC LIMIT 1`,
		it.Service, it.MediaType, it.MediaID, it.IssueType, MaintFailed, ConsoleDismissedMark).
		Scan(maintScan(&found)...)
	return found, err == nil
}
