package store

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	// ErrMaintNotFound is returned when no maintenance item has the id.
	ErrMaintNotFound = errors.New("no such maintenance item")
	// ErrMaintConflict is returned when the item's state does not allow the change.
	ErrMaintConflict = errors.New("maintenance item cannot be changed this way")
)

// MaxConsoleNoteRunes bounds the note a person adds when closing an item.
const MaxConsoleNoteRunes = 500

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
	case it.Status == MaintFailed && hasClosingMark(it.Notes, ConsoleDismissedMark):
		return "dismissed"
	case it.Status == MaintDone && hasClosingMark(it.Notes, ConsoleProcessedMark):
		return "processed"
	}
	return ""
}

// hasClosingMark is true when the mark begins the last "; "-separated part of the
// notes, which is where the console puts it. The same words inside a model's
// note do not count.
func hasClosingMark(notes, mark string) bool {
	last := notes
	if i := strings.LastIndex(notes, "; "); i >= 0 {
		last = notes[i+2:]
	}
	return strings.HasPrefix(last, mark)
}

// ListItemsForConsole returns the items in the given statuses (all when empty),
// highest priority first, then newest. Unlike ListItems it is not capped at 100
// (limit defaults to and is capped at 5000).
func (s *Store) ListItemsForConsole(statuses []string, issueType string, limit int) ([]MaintenanceItem, error) {
	if limit <= 0 || limit > 5000 {
		limit = 5000
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

// OpenStatuses are every status that is neither done nor failed.
func OpenStatuses() []string {
	return []string{MaintPending, MaintResearching, MaintCandidate, MaintDownloading, MaintDownloaded,
		MaintVerifying, MaintImporting, MaintReplacing, MaintBlocked}
}

// consoleClosable are the open states the console may close. Once a worker holds
// the item (downloading to replacing), the workflow owns it and only it closes it.
func consoleClosable(status string) bool {
	switch status {
	case MaintPending, MaintResearching, MaintCandidate, MaintBlocked:
		return true
	}
	return false
}

func leaseHeld(it MaintenanceItem) bool {
	return it.ClaimedBy != "" && it.LeaseUntil != nil && *it.LeaseUntil > time.Now().UTC().Format(time.RFC3339)
}

// cleanNote makes a person's note one short line of printable text.
func cleanNote(note string) string {
	note = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, note)
	note = strings.Join(strings.Fields(note), " ")
	// The console's mark is the last "; " part of the notes; nothing in it may split.
	note = strings.ReplaceAll(note, "; ", ", ")
	if utf8.RuneCountInString(note) > MaxConsoleNoteRunes {
		note = string([]rune(note)[:MaxConsoleNoteRunes])
	}
	return note
}

// stripConsoleMark removes the console's closing mark, and the note after it,
// from the end of the notes. The mark is always the last thing appended, so a
// close and a restore leave the notes as they were.
func stripConsoleMark(notes string) string {
	cut := -1
	for _, mark := range []string{ConsoleDismissedMark, ConsoleProcessedMark} {
		if i := strings.LastIndex(notes, mark); i > cut {
			cut = i
		}
	}
	if cut < 0 {
		return notes
	}
	return strings.TrimSuffix(notes[:cut], "; ")
}

func appendNote(notes, add string) string {
	if notes == "" {
		return add
	}
	return notes + "; " + add
}

// closeFromConsole ends an item as status, with the console mark and the person's
// note appended. The item must be in a state the console may close and not leased.
func (s *Store) closeFromConsole(id int64, status, mark, note string) (MaintenanceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.getItemLocked(id)
	if err != nil {
		return MaintenanceItem{}, err
	}
	if it.ConsoleClosed() != "" && it.Status == status {
		return it, nil
	}
	switch {
	case it.Status == MaintFailed && status == MaintFailed:
		// A failure the workflow recorded can still be dropped for good.
	case !consoleClosable(it.Status):
		return MaintenanceItem{}, fmt.Errorf("%w: item %d is %s", ErrMaintConflict, id, it.Status)
	case leaseHeld(it):
		return MaintenanceItem{}, fmt.Errorf("%w: item %d is claimed by %s", ErrMaintConflict, id, it.ClaimedBy)
	}
	reason := mark
	if note = cleanNote(note); note != "" {
		reason += " " + note
	}
	if _, err := s.db.Exec(`UPDATE maintenance_items SET status=?, notes=?,
		claimed_by='', lease_until=NULL, updated_at=? WHERE id=?`,
		status, appendNote(it.Notes, reason), nowStr(), id); err != nil {
		return MaintenanceItem{}, err
	}
	return s.getItemLocked(id)
}

// DismissItem drops an item for good: it ends as failed with the console mark, and
// a later maintenance_add of the same media and issue returns it instead of queueing
// the candidate again. Dismissing a dismissed item is a no-op. An item a worker is
// handling (downloading to replacing), or that another agent has claimed, is refused.
func (s *Store) DismissItem(id int64, note string) (MaintenanceItem, error) {
	return s.closeFromConsole(id, MaintFailed, ConsoleDismissedMark, note)
}

// MarkItemProcessed closes an item the owner says is already handled. It bypasses
// the rule that done comes only from replacing, because the owner, not a workflow,
// is vouching for it; the mark in the notes keeps that visible. The same refusals
// as DismissItem apply, and a failed or done item cannot be marked.
func (s *Store) MarkItemProcessed(id int64, note string) (MaintenanceItem, error) {
	return s.closeFromConsole(id, MaintDone, ConsoleProcessedMark, note)
}

// RestoreItem puts a dropped item, or one the owner marked processed, back to
// pending. A failure the workflow recorded can be restored too (failed to pending is
// a legal edge); a done item the workflow verified stays immutable.
func (s *Store) RestoreItem(id int64) (MaintenanceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.getItemLocked(id)
	if err != nil {
		return MaintenanceItem{}, err
	}
	switch {
	case it.Status != MaintDone && it.Status != MaintFailed:
		return it, nil // already open
	case it.Status == MaintDone && it.ConsoleClosed() == "":
		return MaintenanceItem{}, fmt.Errorf("%w: item %d was verified done by the workflow", ErrMaintConflict, id)
	}
	var other int64
	if err := s.db.QueryRow(`SELECT id FROM maintenance_items
		WHERE service=? AND media_type=? AND media_id=? AND issue_type=?
		AND status NOT IN ('done', 'failed') AND id<>?`,
		it.Service, it.MediaType, it.MediaID, it.IssueType, id).Scan(&other); err == nil {
		return MaintenanceItem{}, fmt.Errorf("%w: the same entry is queued again as item %d", ErrMaintConflict, other)
	}
	if _, err := s.db.Exec(`UPDATE maintenance_items SET status=?, notes=?,
		claimed_by='', lease_until=NULL, updated_at=? WHERE id=?`,
		MaintPending, stripConsoleMark(it.Notes), nowStr(), id); err != nil {
		return MaintenanceItem{}, err
	}
	return s.getItemLocked(id)
}

// consoleDismissedLocked returns the item the owner dropped for this media and
// issue, if any, so AddItem does not queue it again.
func (s *Store) consoleDismissedLocked(it MaintenanceItem) (MaintenanceItem, bool) {
	rows, err := s.db.Query(`SELECT `+maintColumns+` FROM maintenance_items
		WHERE service=? AND media_type=? AND media_id=? AND issue_type=? AND status=?
		ORDER BY id DESC`, it.Service, it.MediaType, it.MediaID, it.IssueType, MaintFailed)
	if err != nil {
		return MaintenanceItem{}, false
	}
	defer rows.Close()
	for rows.Next() {
		var found MaintenanceItem
		if rows.Scan(maintScan(&found)...) == nil && found.ConsoleClosed() == "dismissed" {
			return found, true
		}
	}
	return MaintenanceItem{}, false
}
