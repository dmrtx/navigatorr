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
// types (tv, anime, movie) and drops repeats. An unknown type is an error.
func NormalizeWorkerWindowMediaTypes(types []string) ([]string, error) {
	seen := map[string]bool{}
	for _, t := range types {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
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
	have := int64(0)
	if found {
		have = current.Revision
	}
	if expectedRevision != nil && *expectedRevision != have {
		return WorkerWindow{}, &StaleWorkerWindowError{Worker: w.Worker, Current: have}
	}
	stored := strings.Join(types, ",")
	if found && current.Until == w.Until && current.MarginSec == w.MarginSec && strings.Join(current.MediaTypes, ",") == stored {
		return current, nil
	}
	if _, err := s.db.Exec(`INSERT INTO worker_windows(worker, until, margin_sec, media_types, revision, updated_at) VALUES(?, NULLIF(?, ''), ?, ?, 1, ?)
		ON CONFLICT(worker) DO UPDATE SET until=excluded.until, margin_sec=excluded.margin_sec, media_types=excluded.media_types,
		revision=worker_windows.revision+1, updated_at=excluded.updated_at`, w.Worker, w.Until, w.MarginSec, stored, nowStr()); err != nil {
		return WorkerWindow{}, err
	}
	out, _, err := s.getWorkerWindowLocked(w.Worker)
	return out, err
}

// DeleteWorkerWindow removes a node's window. With expectedRevision set it is
// removed only at that revision, otherwise a *StaleWorkerWindowError. It
// reports whether a window was there.
func (s *Store) DeleteWorkerWindow(worker string, expectedRevision *int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, found, err := s.getWorkerWindowLocked(worker)
	if err != nil {
		return false, err
	}
	have := int64(0)
	if found {
		have = current.Revision
	}
	if expectedRevision != nil && *expectedRevision != have {
		return false, &StaleWorkerWindowError{Worker: worker, Current: have}
	}
	if !found {
		return false, nil
	}
	_, err = s.db.Exec(`DELETE FROM worker_windows WHERE worker=?`, worker)
	return err == nil, err
}
