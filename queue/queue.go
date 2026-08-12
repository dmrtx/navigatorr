// Package queue holds media requests submitted over HTTP until an agent works them.
//
// The queue deliberately stores free-form text rather than structured fields.
// Deciding which TVDB entry a request means, which quality profile fits, or
// whether it duplicates something already in the library is judgment work that
// belongs to the agent draining the queue, not to this package.
package queue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status values for a queued request.
const (
	StatusPending = "pending"
	StatusClaimed = "claimed"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// MaxTextLen bounds a single request. The text is free-form and ends up in an
// agent's context verbatim, and the whole queue is re-marshaled on every write,
// so one oversized item would tax every later operation.
const MaxTextLen = 4096

// Statuses lists every valid status, for validation and error messages.
var Statuses = []string{StatusPending, StatusClaimed, StatusDone, StatusFailed}

// ValidStatus reports whether s is one of the four queue statuses.
func ValidStatus(s string) bool {
	for _, v := range Statuses {
		if s == v {
			return true
		}
	}
	return false
}

type Item struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Source    string    `json:"source,omitempty"`
	Status    string    `json:"status"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type state struct {
	NextID int     `json:"next_id"`
	Items  []*Item `json:"items"`
}

type Store struct {
	mu    sync.Mutex
	path  string
	lock  *os.File
	state state
}

// Open loads the queue from path, creating an empty one if it does not exist.
//
// It takes an advisory lock on the queue file for the life of the process. MCP
// servers are spawned per client, so without one, navigatorr running under two
// clients at once would give two processes independent in-memory copies of the
// same file, each overwriting the other's requests on save.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	s.state.NextID = 1

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating queue directory: %w", err)
	}
	lock, err := lockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	s.lock = lock

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("reading queue %s: %w", path, err)
	}
	// A zero-length file is what a crash between create and write leaves
	// behind. Treating it as a parse error would make the whole MCP server
	// refuse to start, taking every unrelated *arr tool down with it.
	if len(strings.TrimSpace(string(data))) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		s.Close()
		return nil, fmt.Errorf("parsing queue %s: %w", path, err)
	}

	// Drop null entries rather than panicking on them later.
	items := s.state.Items[:0]
	for _, it := range s.state.Items {
		if it != nil {
			items = append(items, it)
		}
	}
	s.state.Items = items

	// NextID is reconciled against the items actually present. A file written
	// by an older build, hand-edited, or partially restored can carry items
	// with no next_id, which would otherwise mint a duplicate ID on the next
	// Add and make find() return the wrong request.
	if s.state.NextID < 1 {
		s.state.NextID = 1
	}
	for _, it := range s.state.Items {
		if n, err := strconv.Atoi(strings.TrimPrefix(it.ID, "r")); err == nil && n >= s.state.NextID {
			s.state.NextID = n + 1
		}
	}
	return s, nil
}

// Close releases the advisory lock.
func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

// save writes the queue atomically. Callers must hold s.mu.
func (s *Store) save() error {
	data, err := json.MarshalIndent(&s.state, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// A unique temp file, not path+".tmp": a fixed name is shared state between
	// processes and would let two saves interleave into one corrupt file.
	f, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds

	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	// Sync before rename, so a power loss cannot land the rename without the
	// bytes it points at.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Add appends a new pending request.
func (s *Store) Add(text, source string) (Item, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Item{}, fmt.Errorf("text is required")
	}
	if len(text) > MaxTextLen {
		return Item{}, fmt.Errorf("text is %d bytes, limit is %d", len(text), MaxTextLen)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	it := &Item{
		ID:        fmt.Sprintf("r%d", s.state.NextID),
		Text:      text,
		Source:    source,
		Status:    StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.state.NextID++
	s.state.Items = append(s.state.Items, it)
	if err := s.save(); err != nil {
		// Roll back, or the caller sees a failure while the item is live in
		// memory: a retry then produces a duplicate, and the ghost is written
		// to disk by the next successful save.
		s.state.Items = s.state.Items[:len(s.state.Items)-1]
		s.state.NextID--
		return Item{}, err
	}
	return *it, nil
}

// List returns items filtered by status. An empty status returns everything.
func (s *Store) List(status string) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Item, 0, len(s.state.Items))
	for _, it := range s.state.Items {
		if status == "" || it.Status == status {
			out = append(out, *it)
		}
	}
	return out
}

// Counts returns how many items sit in each status.
func (s *Store) Counts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]int, len(Statuses))
	for _, it := range s.state.Items {
		out[it.Status]++
	}
	return out
}

// find locates an item by ID. Callers must hold s.mu.
func (s *Store) find(id string) *Item {
	for _, it := range s.state.Items {
		if it.ID == id {
			return it
		}
	}
	return nil
}

// update applies mutate to the item, persists, and rolls the item back if the
// write fails so memory and disk cannot diverge. Returns a copy, taken under
// the lock: handing out the *Item would let callers read fields while another
// goroutine writes them.
func (s *Store) update(id string, mutate func(*Item) error) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	it := s.find(id)
	if it == nil {
		return Item{}, fmt.Errorf("no such request %q", id)
	}
	before := *it
	if err := mutate(it); err != nil {
		return Item{}, err
	}
	it.UpdatedAt = time.Now().UTC()
	if err := s.save(); err != nil {
		*it = before
		return Item{}, err
	}
	return *it, nil
}

// Claim marks a pending item as claimed so concurrent agents do not double-work it.
func (s *Store) Claim(id string) (Item, error) {
	return s.update(id, func(it *Item) error {
		if it.Status != StatusPending {
			return fmt.Errorf("request %s is %s, not pending", id, it.Status)
		}
		it.Status = StatusClaimed
		return nil
	})
}

// Resolve closes an item as done or failed, recording why.
func (s *Store) Resolve(id, status, note string) (Item, error) {
	if status != StatusDone && status != StatusFailed {
		return Item{}, fmt.Errorf("status must be %q or %q, got %q", StatusDone, StatusFailed, status)
	}
	return s.update(id, func(it *Item) error {
		// Resolving twice would clobber the first outcome's note, which is the
		// only record of what actually happened.
		if it.Status == StatusDone || it.Status == StatusFailed {
			return fmt.Errorf("request %s is already %s", id, it.Status)
		}
		it.Status = status
		it.Note = note
		return nil
	})
}

// Release returns a claimed item to pending, e.g. when an agent gives up without resolving.
func (s *Store) Release(id string) (Item, error) {
	return s.update(id, func(it *Item) error {
		// Without this check, releasing a done item puts finished work back in
		// the pending queue and the agent actions it a second time.
		if it.Status != StatusClaimed {
			return fmt.Errorf("request %s is %s, not claimed", id, it.Status)
		}
		it.Status = StatusPending
		it.Note = ""
		return nil
	})
}
