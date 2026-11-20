package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Media types a node's availability window can be limited to.
var WorkerWindowMediaTypes = []string{"tv", "anime", "movie"}

// WorkerWindow is the operator's availability window for one node: it takes
// new conversions only while they can be finished before Until (less the
// margin) and only of the media types listed (all when none). It is one
// durable row per node, so a restart keeps it; an explicit delete ends it.
type WorkerWindow struct {
	Worker string `json:"worker"`
	// Until is when the node stops being available, an RFC3339 UTC time, or ""
	// for no end.
	Until string `json:"until,omitempty"`
	// MarginSec is how long before Until the node already takes nothing new.
	MarginSec int `json:"margin_sec"`
	// MediaTypes is the subset of WorkerWindowMediaTypes the node takes (empty:
	// all of them).
	MediaTypes []string `json:"media_types,omitempty"`
	Revision   int64    `json:"revision"`
	UpdatedAt  string   `json:"updated_at"`
}

// ClosesAt is when the node takes nothing new: Until less the margin. ok is
// false when the window has no end.
func (w WorkerWindow) ClosesAt() (time.Time, bool) {
	if w.Until == "" {
		return time.Time{}, false
	}
	until, err := time.Parse(time.RFC3339, w.Until)
	if err != nil {
		// A value that cannot be read closes the node: taking work on a node
		// whose end is unknown is the worse mistake.
		return time.Unix(0, 0).UTC(), true
	}
	return until.Add(-time.Duration(w.MarginSec) * time.Second).UTC(), true
}

// StaleWorkerWindowError is the refusal of a write made against a revision the
// window no longer has.
type StaleWorkerWindowError struct {
	Worker  string
	Current int64
}

func (e *StaleWorkerWindowError) Error() string {
	return fmt.Sprintf("the window of node %s is at revision %d; read it again", e.Worker, e.Current)
}

// NormalizeWorkerWindowMediaTypes lower-cases, checks and orders the media
// types (tv, anime, movie) and drops repeats. An unknown or blank type is an error.
func NormalizeWorkerWindowMediaTypes(types []string) ([]string, error) {
	seen := map[string]bool{}
	for _, t := range types {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			return nil, errors.New("a media type must not be empty (use tv, anime or movie)")
		}
		known := false
		for _, k := range WorkerWindowMediaTypes {
			known = known || k == t
		}
		if !known {
			return nil, fmt.Errorf("unknown media type %q (use tv, anime or movie)", t)
		}
		seen[t] = true
	}
	var out []string
	for _, k := range WorkerWindowMediaTypes {
		if seen[k] {
			out = append(out, k)
		}
	}
	return out, nil
}

type windowScanner interface{ Scan(dest ...any) error }

func scanWorkerWindow(sc windowScanner) (WorkerWindow, error) {
	var w WorkerWindow
	var types string
	if err := sc.Scan(&w.Worker, &w.Until, &w.MarginSec, &types, &w.Revision, &w.UpdatedAt); err != nil {
		return WorkerWindow{}, err
	}
	for _, t := range strings.Split(types, ",") {
		if t != "" {
			w.MediaTypes = append(w.MediaTypes, t)
		}
	}
	return w, nil
}

const workerWindowColumns = `worker, COALESCE(until, ''), margin_sec, media_types, revision, updated_at`

// ListWorkerWindows returns the nodes' windows by worker name.
func (s *Store) ListWorkerWindows() (map[string]WorkerWindow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT ` + workerWindowColumns + ` FROM worker_windows`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WorkerWindow{}
	for rows.Next() {
		w, err := scanWorkerWindow(rows)
		if err != nil {
			return nil, err
		}
		out[w.Worker] = w
	}
	return out, rows.Err()
}

// GetWorkerWindow returns one node's window; ok is false when it has none.
func (s *Store) GetWorkerWindow(worker string) (WorkerWindow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getWorkerWindowLocked(worker)
}

func (s *Store) getWorkerWindowLocked(worker string) (WorkerWindow, bool, error) {
	w, err := scanWorkerWindow(s.db.QueryRow(`SELECT `+workerWindowColumns+` FROM worker_windows WHERE worker=?`, worker))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerWindow{}, false, nil
	}
	return w, err == nil, err
}

// SetWorkerWindow creates or replaces a node's window. With expectedRevision
// set, it is written only when the node's window is at that revision (0 for
// none), otherwise a *StaleWorkerWindowError. Writing the window a node
// already has changes nothing; every real change moves the revision on.
func (s *Store) SetWorkerWindow(w WorkerWindow, expectedRevision *int64) (WorkerWindow, error) {
	if w.Worker == "" {
		return WorkerWindow{}, errors.New("worker name is required")
	}
	types, err := NormalizeWorkerWindowMediaTypes(w.MediaTypes)
	if err != nil {
		return WorkerWindow{}, err
	}
	if w.MarginSec < 0 {
		return WorkerWindow{}, errors.New("margin must not be negative")
	}
	if w.Until != "" {
		until, err := time.Parse(time.RFC3339, w.Until)
		if err != nil {
			return WorkerWindow{}, fmt.Errorf("until is not an RFC3339 time: %w", err)
		}
		w.Until = until.UTC().Format(time.RFC3339)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, found, err := s.getWorkerWindowLocked(w.Worker)
	if err != nil {
		return WorkerWindow{}, err
	}
	have, err := s.windowRevisionLocked(w.Worker)
	if err != nil {
		return WorkerWindow{}, err
	}
	if expectedRevision != nil && *expectedRevision != have {
		return WorkerWindow{}, &StaleWorkerWindowError{Worker: w.Worker, Current: have}
	}
	stored := strings.Join(types, ",")
	if found && current.Until == w.Until && current.MarginSec == w.MarginSec && strings.Join(current.MediaTypes, ",") == stored {
		return current, nil
	}
	next := have + 1
	tx, err := s.db.Begin()
	if err != nil {
		return WorkerWindow{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO worker_window_revisions(worker, revision) VALUES(?, ?)
		ON CONFLICT(worker) DO UPDATE SET revision=excluded.revision`, w.Worker, next); err != nil {
		return WorkerWindow{}, err
	}
	if _, err := tx.Exec(`INSERT INTO worker_windows(worker, until, margin_sec, media_types, revision, updated_at) VALUES(?, NULLIF(?, ''), ?, ?, ?, ?)
		ON CONFLICT(worker) DO UPDATE SET until=excluded.until, margin_sec=excluded.margin_sec, media_types=excluded.media_types,
		revision=excluded.revision, updated_at=excluded.updated_at`, w.Worker, w.Until, w.MarginSec, stored, next, nowStr()); err != nil {
		return WorkerWindow{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkerWindow{}, err
	}
	out, _, err := s.getWorkerWindowLocked(w.Worker)
	return out, err
}

// windowRevisionLocked is the revision a node's window has reached: that of
// the window it has, or of the one it had last (0 for a node that never had
// one). A write must be made against it.
func (s *Store) windowRevisionLocked(worker string) (int64, error) {
	var rev int64
	err := s.db.QueryRow(`SELECT revision FROM worker_window_revisions WHERE worker=?`, worker).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return rev, err
}

// WorkerWindowRevision is the revision to write a node's window against: the
// window's own, or when it has none the one its last window ended at.
func (s *Store) WorkerWindowRevision(worker string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.windowRevisionLocked(worker)
}

// DeleteWorkerWindow removes a node's window. With expectedRevision set it is
// removed only at that revision, otherwise a *StaleWorkerWindowError. It
// reports whether a window was there. A removal moves the node's revision on,
// so a client that read the window before cannot write over what is made next.
func (s *Store) DeleteWorkerWindow(worker string, expectedRevision *int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, found, err := s.getWorkerWindowLocked(worker)
	if err != nil {
		return false, err
	}
	have, err := s.windowRevisionLocked(worker)
	if err != nil {
		return false, err
	}
	if expectedRevision != nil && *expectedRevision != have {
		return false, &StaleWorkerWindowError{Worker: worker, Current: have}
	}
	if !found {
		return false, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE worker_window_revisions SET revision=? WHERE worker=?`, have+1, worker); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM worker_windows WHERE worker=?`, worker); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
