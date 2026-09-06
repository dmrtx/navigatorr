package snapshot

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Snapshot represents a cached collection snapshot taken from an upstream service.
type Snapshot struct {
	ID        string    `json:"id"`
	Service   string    `json:"service"`
	Path      string    `json:"path"`
	Query     string    `json:"query"`
	Total     int       `json:"total"`
	Items     []any     `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Store holds temporary snapshots with TTL.
type Store struct {
	mu         sync.RWMutex
	snapshots  map[string]*Snapshot
	byKey      map[string]string // "service:path:query" -> snapshot ID
	defaultTTL time.Duration
}

// NewStore initializes a snapshot store.
func NewStore(defaultTTL time.Duration) *Store {
	if defaultTTL <= 0 {
		defaultTTL = 5 * time.Minute
	}
	return &Store{
		snapshots:  make(map[string]*Snapshot),
		byKey:      make(map[string]string),
		defaultTTL: defaultTTL,
	}
}

func cacheKey(service, path, query string) string {
	return fmt.Sprintf("%s:%s:%s", strings.ToLower(service), path, query)
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Create stores a new snapshot of items.
func (s *Store) Create(service, path, query string, items []any) *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked()

	id := randomID()
	now := time.Now().UTC()
	snap := &Snapshot{
		ID:        id,
		Service:   strings.ToLower(service),
		Path:      path,
		Query:     query,
		Total:     len(items),
		Items:     items,
		CreatedAt: now,
		ExpiresAt: now.Add(s.defaultTTL),
	}

	key := cacheKey(service, path, query)
	s.snapshots[id] = snap
	s.byKey[key] = id
	return snap
}

// Find looks up a recent non-expired snapshot matching service, path and query.
func (s *Store) Find(service, path, query string) (*Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := cacheKey(service, path, query)
	id, ok := s.byKey[key]
	if !ok {
		return nil, false
	}
	snap, ok := s.snapshots[id]
	if !ok || time.Now().UTC().After(snap.ExpiresAt) {
		return nil, false
	}
	return snap, true
}

// Get retrieves a snapshot by ID if still valid.
func (s *Store) Get(id string) (*Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap, ok := s.snapshots[id]
	if !ok || time.Now().UTC().After(snap.ExpiresAt) {
		return nil, false
	}
	return snap, true
}

// Invalidate removes all cached snapshots for a service (e.g. after mutations).
func (s *Store) Invalidate(service string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	svc := strings.ToLower(service)
	for id, snap := range s.snapshots {
		if snap.Service == svc {
			delete(s.snapshots, id)
		}
	}
	for key := range s.byKey {
		if strings.HasPrefix(key, svc+":") {
			delete(s.byKey, key)
		}
	}
}

// EncodeCursor builds an opaque cursor string.
func EncodeCursor(id string, offset int) string {
	return fmt.Sprintf("cursor_%s_%d", id, offset)
}

// DecodeCursor parses an opaque cursor string.
func DecodeCursor(cursor string) (id string, offset int, err error) {
	if !strings.HasPrefix(cursor, "cursor_") {
		return "", 0, fmt.Errorf("invalid cursor format")
	}
	parts := strings.Split(cursor, "_")
	if len(parts) != 3 {
		return "", 0, fmt.Errorf("invalid cursor format")
	}
	offset, err = strconv.Atoi(parts[2])
	if err != nil || offset < 0 {
		return "", 0, fmt.Errorf("invalid cursor offset")
	}
	return parts[1], offset, nil
}

// GetPage returns a sliced page of items from a snapshot, handling pagination and completion.
func (s *Store) GetPage(cursor string, limit int) (items []any, nextCursor string, complete bool, total int, offset int, err error) {
	snapID, off, err := DecodeCursor(cursor)
	if err != nil {
		return nil, "", false, 0, 0, err
	}

	snap, ok := s.Get(snapID)
	if !ok {
		return nil, "", false, 0, 0, fmt.Errorf("snapshot expired or not found; please refresh or restart query")
	}

	if limit <= 0 {
		limit = 50
	}

	total = snap.Total
	offset = off

	if offset >= total {
		return []any{}, "", true, total, offset, nil
	}

	end := offset + limit
	if end >= total {
		end = total
		complete = true
		nextCursor = ""
	} else {
		complete = false
		nextCursor = EncodeCursor(snapID, end)
	}

	items = snap.Items[offset:end]
	return items, nextCursor, complete, total, offset, nil
}

func (s *Store) purgeExpiredLocked() {
	now := time.Now().UTC()
	for id, snap := range s.snapshots {
		if now.After(snap.ExpiresAt) {
			delete(s.snapshots, id)
		}
	}
	for key, id := range s.byKey {
		if _, ok := s.snapshots[id]; !ok {
			delete(s.byKey, key)
		}
	}
}
