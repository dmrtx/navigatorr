// Package store persists Navigatorr's maintenance agent state in SQLite.
//
// It holds user preferences, the maintenance work queue, release decisions,
// media inspections, an audit log and a release blocklist. The database file
// lives next to the existing OpenAPI cache so it survives container restarts
// on the same persistent volume.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// SchemaVersion is the current schema revision. Migrations run in order.
const SchemaVersion = 1

// Maintenance statuses. The safe-replacement workflow moves items forward
// through them; direct jumps (e.g. pending -> done) are rejected.
const (
	MaintPending     = "pending"
	MaintResearching = "researching"
	MaintCandidate   = "candidate_found"
	MaintDownloading = "downloading"
	MaintDownloaded  = "downloaded"
	MaintVerifying   = "verifying"
	MaintImporting   = "importing"
	MaintReplacing   = "replacing"
	MaintBlocked     = "blocked"
	MaintDone        = "done"
	MaintFailed      = "failed"
)

// ActionableStatuses are the states maintenance_next considers workable.
var ActionableStatuses = []string{MaintPending, MaintResearching, MaintCandidate}

// TerminalStatuses are never returned as actionable and accept no transitions.
var TerminalStatuses = []string{MaintDone}

// allowedTransitions lists every legal state-machine edge.
var allowedTransitions = map[string][]string{
	MaintPending:     {MaintResearching, MaintBlocked, MaintFailed},
	MaintResearching: {MaintCandidate, MaintBlocked, MaintFailed, MaintPending},
	MaintCandidate:   {MaintDownloading, MaintBlocked, MaintFailed, MaintResearching},
	MaintDownloading: {MaintDownloaded, MaintBlocked, MaintFailed},
	MaintDownloaded:  {MaintVerifying, MaintBlocked, MaintFailed},
	MaintVerifying:   {MaintImporting, MaintBlocked, MaintFailed, MaintDownloading},
	MaintImporting:   {MaintReplacing, MaintBlocked, MaintFailed, MaintVerifying},
	MaintReplacing:   {MaintDone, MaintBlocked, MaintFailed},
	MaintBlocked:     {MaintResearching, MaintPending, MaintFailed},
	MaintFailed:      {MaintPending},
	MaintDone:        {},
}

// ValidMaintStatus reports whether s is a known maintenance status.
func ValidMaintStatus(s string) bool {
	_, ok := allowedTransitions[s]
	return ok
}

// CanTransition reports whether moving from -> to is a legal edge.
func CanTransition(from, to string) bool {
	for _, next := range allowedTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// Store wraps the SQLite database.
type Store struct {
	mu sync.Mutex
	db *sql.DB
}

// Open creates the parent directory, opens (or creates) the database,
// enables WAL with a busy timeout, and applies migrations.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}
	// A small pool is enough for an MCP server, and a single writer avoids
	// SQLITE_BUSY surprises between the two processes that may share the file.
	db.SetMaxOpenConns(4)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("applying %s: %w", pragma, err)
		}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("creating schema_version: %w", err)
	}
	var version int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	for _, m := range migrations {
		if m.version <= version {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		for _, stmt := range m.statements {
			if _, err := tx.Exec(stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", m.version, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`,
			m.version, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return fmt.Errorf("recording migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %d: %w", m.version, err)
		}
	}
	return nil
}

type migration struct {
	version    int
	statements []string
}

var migrations = []migration{
	{version: 1, statements: []string{
		`CREATE TABLE IF NOT EXISTS preferences (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scope TEXT NOT NULL DEFAULT 'global',
			key TEXT NOT NULL,
			value_json TEXT NOT NULL DEFAULT 'null',
			source TEXT NOT NULL DEFAULT 'user',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			expires_at TEXT,
			UNIQUE (scope, key))`,
		`CREATE INDEX IF NOT EXISTS idx_preferences_scope ON preferences (scope)`,
		`CREATE TABLE IF NOT EXISTS maintenance_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			media_type TEXT NOT NULL,
			service TEXT NOT NULL,
			media_id TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL,
			issue_type TEXT NOT NULL,
			priority INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending',
			current_file_id TEXT,
			current_size INTEGER,
			replacement_release_guid TEXT,
			replacement_torrent_hash TEXT,
			replacement_size INTEGER,
			requires_subtitles INTEGER NOT NULL DEFAULT 0,
			notes TEXT NOT NULL DEFAULT '',
			claimed_by TEXT NOT NULL DEFAULT '',
			lease_until TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL)`,
		// Idempotency at the DB level: no two ACTIVE items for the same
		// media+issue. Finished items (done/failed) fall outside the partial
		// index so history is kept while a re-scan can open fresh work.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_maintenance_active
			ON maintenance_items (service, media_type, media_id, issue_type)
			WHERE status NOT IN ('done', 'failed')`,
		`CREATE INDEX IF NOT EXISTS idx_maintenance_status ON maintenance_items (status, priority DESC, updated_at)`,
		`CREATE TABLE IF NOT EXISTS release_decisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			maintenance_item_id INTEGER NOT NULL REFERENCES maintenance_items (id) ON DELETE CASCADE,
			release_guid TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL,
			release_group TEXT NOT NULL DEFAULT '',
			size INTEGER,
			seeders_at_decision INTEGER,
			decision TEXT NOT NULL,
			score REAL,
			reasons_json TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_decisions_item ON release_decisions (maintenance_item_id)`,
		`CREATE TABLE IF NOT EXISTS media_checks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			media_type TEXT NOT NULL,
			media_id TEXT NOT NULL DEFAULT '',
			file_id TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL DEFAULT '',
			container TEXT NOT NULL DEFAULT '',
			video_codec TEXT NOT NULL DEFAULT '',
			resolution TEXT NOT NULL DEFAULT '',
			bit_depth INTEGER,
			audio_languages TEXT NOT NULL DEFAULT '[]',
			subtitle_languages TEXT NOT NULL DEFAULT '[]',
			dangerous_files TEXT NOT NULL DEFAULT '[]',
			duration_sec REAL,
			size_bytes INTEGER,
			checked_at TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_checks_media ON media_checks (media_type, media_id, checked_at)`,
		`CREATE TABLE IF NOT EXISTS action_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			action TEXT NOT NULL,
			service TEXT NOT NULL DEFAULT '',
			media TEXT NOT NULL DEFAULT '',
			args_json TEXT NOT NULL DEFAULT '{}',
			result TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_action_log_time ON action_log (created_at)`,
		`CREATE TABLE IF NOT EXISTS blocked_releases (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			identifier TEXT NOT NULL UNIQUE,
			reason TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'manual',
			created_at TEXT NOT NULL)`,
	}},
}

// Preference is one scoped key/value entry.
type Preference struct {
	ID        int64   `json:"id"`
	Scope     string  `json:"scope"`
	Key       string  `json:"key"`
	ValueJSON string  `json:"value_json"`
	Source    string  `json:"source"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// Value unmarshals ValueJSON into v.
func (p Preference) Value(v any) error { return json.Unmarshal([]byte(p.ValueJSON), v) }

func nowStr() string { return time.Now().UTC().Format(time.RFC3339) }

// vigente filters out expired facts. Facts like "159 seeders" must never
// outlive their TTL and become permanent preferences.
func vigente() string { return "(expires_at IS NULL OR expires_at > '" + nowStr() + "')" }

// SetPreference upserts a scoped preference. ttl<=0 means no expiry.
func (s *Store) SetPreference(scope, key, valueJSON, source string, ttl time.Duration) (Preference, error) {
	if scope == "" {
		scope = "global"
	}
	if key == "" {
		return Preference{}, fmt.Errorf("key is required")
	}
	if valueJSON == "" {
		valueJSON = "null"
	}
	if source == "" {
		source = "user"
	}
	var expires *string
	if ttl > 0 {
		e := time.Now().UTC().Add(ttl).Format(time.RFC3339)
		expires = &e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := nowStr()
	_, err := s.db.Exec(`INSERT INTO preferences (scope, key, value_json, source, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (scope, key) DO UPDATE SET value_json=excluded.value_json,
			source=excluded.source, updated_at=excluded.updated_at, expires_at=excluded.expires_at`,
		scope, key, valueJSON, source, now, now, expires)
	if err != nil {
		return Preference{}, err
	}
	// Read back without the vigente filter: a fact expiring imminently is
	// still a successful write; reads decide vigencia.
	var p Preference
	err = s.db.QueryRow(`SELECT id, scope, key, value_json, source, created_at, updated_at, expires_at
		FROM preferences WHERE scope=? AND key=?`, scope, key).
		Scan(&p.ID, &p.Scope, &p.Key, &p.ValueJSON, &p.Source, &p.CreatedAt, &p.UpdatedAt, &p.ExpiresAt)
	return p, err
}

func (s *Store) getPreferenceLocked(scope, key string) (Preference, error) {
	var p Preference
	err := s.db.QueryRow(`SELECT id, scope, key, value_json, source, created_at, updated_at, expires_at
		FROM preferences WHERE scope=? AND key=? AND `+vigente(), scope, key).
		Scan(&p.ID, &p.Scope, &p.Key, &p.ValueJSON, &p.Source, &p.CreatedAt, &p.UpdatedAt, &p.ExpiresAt)
	if err == sql.ErrNoRows {
		return Preference{}, fmt.Errorf("no such preference %q in scope %q", key, scope)
	}
	return p, err
}

// GetPreference returns one vigente preference.
func (s *Store) GetPreference(scope, key string) (Preference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getPreferenceLocked(scope, key)
}

// ListPreferences returns all vigente preferences in a scope ("" = all).
func (s *Store) ListPreferences(scope string) ([]Preference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := `SELECT id, scope, key, value_json, source, created_at, updated_at, expires_at
		FROM preferences WHERE ` + vigente()
	var args []any
	if scope != "" {
		q += ` AND scope=?`
		args = append(args, scope)
	}
	q += ` ORDER BY scope, key`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Preference{}
	for rows.Next() {
		var p Preference
		if err := rows.Scan(&p.ID, &p.Scope, &p.Key, &p.ValueJSON, &p.Source, &p.CreatedAt, &p.UpdatedAt, &p.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SearchPreferences finds vigente preferences whose scope/key/value match.
func (s *Store) SearchPreferences(query string) ([]Preference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	like := "%" + query + "%"
	rows, err := s.db.Query(`SELECT id, scope, key, value_json, source, created_at, updated_at, expires_at
		FROM preferences WHERE `+vigente()+` AND (scope LIKE ? OR key LIKE ? OR value_json LIKE ?)
		ORDER BY scope, key LIMIT 50`, like, like, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Preference{}
	for rows.Next() {
		var p Preference
		if err := rows.Scan(&p.ID, &p.Scope, &p.Key, &p.ValueJSON, &p.Source, &p.CreatedAt, &p.UpdatedAt, &p.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePreference removes a preference regardless of expiry.
func (s *Store) DeletePreference(scope, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM preferences WHERE scope=? AND key=?`, scope, key)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("no such preference %q in scope %q", key, scope)
	}
	return nil
}

// MaintenanceItem is one unit of library maintenance work. It is deliberately
// separate from the queue package's content-request queue: that queue holds
// free-form "please add X" texts, while this table holds structured,
// state-machined replacement/repair jobs.
type MaintenanceItem struct {
	ID                     int64   `json:"id"`
	MediaType              string  `json:"media_type"`
	Service                string  `json:"service"`
	MediaID                string  `json:"media_id"`
	Title                  string  `json:"title"`
	IssueType              string  `json:"issue_type"`
	Priority               int     `json:"priority"`
	Status                 string  `json:"status"`
	CurrentFileID          *string `json:"current_file_id,omitempty"`
	CurrentSize            *int64  `json:"current_size,omitempty"`
	ReplacementReleaseGUID *string `json:"replacement_release_guid,omitempty"`
	ReplacementTorrentHash *string `json:"replacement_torrent_hash,omitempty"`
	ReplacementSize        *int64  `json:"replacement_size,omitempty"`
	RequiresSubtitles      bool    `json:"requires_subtitles"`
	Notes                  string  `json:"notes"`
	ClaimedBy              string  `json:"claimed_by,omitempty"`
	LeaseUntil             *string `json:"lease_until,omitempty"`
	CreatedAt              string  `json:"created_at"`
	UpdatedAt              string  `json:"updated_at"`
}

// AddItem creates a maintenance job. It is idempotent: when an ACTIVE item
// already exists for the same service+media+issue, the existing item is
// returned instead of creating a duplicate.
func (s *Store) AddItem(it MaintenanceItem) (MaintenanceItem, error) {
	if it.Title == "" || it.IssueType == "" || it.Service == "" || it.MediaType == "" {
		return MaintenanceItem{}, fmt.Errorf("media_type, service, title and issue_type are required")
	}
	if it.Status == "" {
		it.Status = MaintPending
	}
	if !ValidMaintStatus(it.Status) {
		return MaintenanceItem{}, fmt.Errorf("unknown status %q", it.Status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := nowStr()
	res, err := s.db.Exec(`INSERT INTO maintenance_items
		(media_type, service, media_id, title, issue_type, priority, status,
		 current_file_id, current_size, replacement_release_guid, replacement_torrent_hash,
		 replacement_size, requires_subtitles, notes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (service, media_type, media_id, issue_type)
		WHERE status NOT IN ('done', 'failed') DO NOTHING`,
		it.MediaType, it.Service, it.MediaID, it.Title, it.IssueType, it.Priority, it.Status,
		it.CurrentFileID, it.CurrentSize, it.ReplacementReleaseGUID, it.ReplacementTorrentHash,
		it.ReplacementSize, boolToInt(it.RequiresSubtitles), it.Notes, now, now)
	if err != nil {
		return MaintenanceItem{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Duplicate of an active item: return the survivor so retries and
		// re-scans converge instead of multiplying work.
		var existing MaintenanceItem
		err := s.db.QueryRow(`SELECT `+maintColumns+` FROM maintenance_items
			WHERE service=? AND media_type=? AND media_id=? AND issue_type=?
			AND status NOT IN ('done', 'failed')`,
			it.Service, it.MediaType, it.MediaID, it.IssueType).Scan(maintScan(&existing)...)
		if err != nil {
			return MaintenanceItem{}, err
		}
		return existing, nil
	}
	id, _ := res.LastInsertId()
	return s.getItemLocked(id)
}

const maintColumns = `id, media_type, service, media_id, title, issue_type, priority, status,
	current_file_id, current_size, replacement_release_guid, replacement_torrent_hash,
	replacement_size, requires_subtitles, notes, claimed_by, lease_until, created_at, updated_at`

func maintScan(it *MaintenanceItem) []any {
	return []any{&it.ID, &it.MediaType, &it.Service, &it.MediaID, &it.Title,
		&it.IssueType, &it.Priority, &it.Status, &it.CurrentFileID, &it.CurrentSize,
		&it.ReplacementReleaseGUID, &it.ReplacementTorrentHash, &it.ReplacementSize,
		&it.RequiresSubtitles, &it.Notes, &it.ClaimedBy, &it.LeaseUntil,
		&it.CreatedAt, &it.UpdatedAt}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) getItemLocked(id int64) (MaintenanceItem, error) {
	var it MaintenanceItem
	err := s.db.QueryRow(`SELECT `+maintColumns+` FROM maintenance_items WHERE id=?`, id).
		Scan(maintScan(&it)...)
	if err == sql.ErrNoRows {
		return MaintenanceItem{}, fmt.Errorf("no such maintenance item %d", id)
	}
	return it, err
}

// GetItem returns one maintenance item by id.
func (s *Store) GetItem(id int64) (MaintenanceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getItemLocked(id)
}

// ItemFilter narrows maintenance_list. Empty fields match everything.
type ItemFilter struct {
	Status    string
	Service   string
	IssueType string
	Priority  *int
	Limit     int
}

// ListItems returns items matching the filter, newest-touch order flipped:
// highest priority first, then oldest update (starvation-free).
func (s *Store) ListItems(f ItemFilter) ([]MaintenanceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := `SELECT ` + maintColumns + ` FROM maintenance_items WHERE 1=1`
	var args []any
	if f.Status != "" {
		if !ValidMaintStatus(f.Status) {
			return nil, fmt.Errorf("unknown status %q", f.Status)
		}
		q += ` AND status=?`
		args = append(args, f.Status)
	}
	if f.Service != "" {
		q += ` AND service=?`
		args = append(args, f.Service)
	}
	if f.IssueType != "" {
		q += ` AND issue_type=?`
		args = append(args, f.IssueType)
	}
	if f.Priority != nil {
		q += ` AND priority>=?`
		args = append(args, *f.Priority)
	}
	q += ` ORDER BY priority DESC, updated_at ASC`
	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q += ` LIMIT ?`
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

// NextItem returns the highest-priority actionable item whose lease is free.
func (s *Store) NextItem() (MaintenanceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := nowStr()
	placeholders := strings.Repeat("?,", len(ActionableStatuses)-1) + "?"
	args := make([]any, 0, len(ActionableStatuses)+1)
	for _, st := range ActionableStatuses {
		args = append(args, st)
	}
	args = append(args, now)
	var it MaintenanceItem
	err := s.db.QueryRow(`SELECT `+maintColumns+` FROM maintenance_items
		WHERE status IN (`+placeholders+`)
		AND (lease_until IS NULL OR lease_until <= ?)
		ORDER BY priority DESC, updated_at ASC LIMIT 1`, args...).
		Scan(maintScan(&it)...)
	if err == sql.ErrNoRows {
		return MaintenanceItem{}, fmt.Errorf("no actionable maintenance items")
	}
	return it, err
}

// Transition moves an item along one legal state-machine edge.
func (s *Store) Transition(id int64, to, notes string) (MaintenanceItem, error) {
	if !ValidMaintStatus(to) {
		return MaintenanceItem{}, fmt.Errorf("unknown status %q", to)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.getItemLocked(id)
	if err != nil {
		return MaintenanceItem{}, err
	}
	if !CanTransition(it.Status, to) {
		return MaintenanceItem{}, fmt.Errorf("invalid transition %s -> %s", it.Status, to)
	}
	if _, err := s.db.Exec(`UPDATE maintenance_items SET status=?, notes=?,
		claimed_by='', lease_until=NULL, updated_at=? WHERE id=?`,
		to, notes, nowStr(), id); err != nil {
		return MaintenanceItem{}, err
	}
	return s.getItemLocked(id)
}

// UpdateItem patches the mutable replacement/notes fields of an open item.
func (s *Store) UpdateItem(id int64, patch MaintenanceItem) (MaintenanceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.getItemLocked(id)
	if err != nil {
		return MaintenanceItem{}, err
	}
	if it.Status == MaintDone {
		return MaintenanceItem{}, fmt.Errorf("item %d is done and immutable", id)
	}
	if patch.ReplacementReleaseGUID != nil {
		it.ReplacementReleaseGUID = patch.ReplacementReleaseGUID
	}
	if patch.ReplacementTorrentHash != nil {
		it.ReplacementTorrentHash = patch.ReplacementTorrentHash
	}
	if patch.ReplacementSize != nil {
		it.ReplacementSize = patch.ReplacementSize
	}
	if patch.CurrentFileID != nil {
		it.CurrentFileID = patch.CurrentFileID
	}
	if patch.CurrentSize != nil {
		it.CurrentSize = patch.CurrentSize
	}
	if patch.Notes != "" {
		it.Notes = patch.Notes
	}
	if patch.Priority != 0 {
		it.Priority = patch.Priority
	}
	it.RequiresSubtitles = patch.RequiresSubtitles || it.RequiresSubtitles
	if _, err := s.db.Exec(`UPDATE maintenance_items
		SET replacement_release_guid=?, replacement_torrent_hash=?, replacement_size=?,
		    current_file_id=?, current_size=?, notes=?, priority=?, requires_subtitles=?, updated_at=?
		WHERE id=?`,
		it.ReplacementReleaseGUID, it.ReplacementTorrentHash, it.ReplacementSize,
		it.CurrentFileID, it.CurrentSize, it.Notes, it.Priority,
		boolToInt(it.RequiresSubtitles), nowStr(), id); err != nil {
		return MaintenanceItem{}, err
	}
	return s.getItemLocked(id)
}

// ClaimItem takes a temporary lease so parallel agents do not double-work
// an item. Claiming an already-leased item fails unless the lease expired.
func (s *Store) ClaimItem(id int64, owner string, lease time.Duration) (MaintenanceItem, error) {
	if owner == "" {
		return MaintenanceItem{}, fmt.Errorf("owner is required")
	}
	if lease <= 0 {
		lease = 15 * time.Minute
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.getItemLocked(id)
	if err != nil {
		return MaintenanceItem{}, err
	}
	if it.Status == MaintDone || it.Status == MaintFailed {
		return MaintenanceItem{}, fmt.Errorf("item %d is %s and cannot be claimed", id, it.Status)
	}
	if it.LeaseUntil != nil && *it.LeaseUntil > nowStr() && it.ClaimedBy != "" {
		return MaintenanceItem{}, fmt.Errorf("item %d is already claimed by %s", id, it.ClaimedBy)
	}
	until := time.Now().UTC().Add(lease).Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE maintenance_items SET claimed_by=?, lease_until=?, updated_at=? WHERE id=?`,
		owner, until, nowStr(), id); err != nil {
		return MaintenanceItem{}, err
	}
	return s.getItemLocked(id)
}

// ReleaseItem returns a claimed item to the pool without changing its status.
func (s *Store) ReleaseItem(id int64) (MaintenanceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.getItemLocked(id); err != nil {
		return MaintenanceItem{}, err
	}
	if _, err := s.db.Exec(`UPDATE maintenance_items SET claimed_by='', lease_until=NULL, updated_at=? WHERE id=?`,
		nowStr(), id); err != nil {
		return MaintenanceItem{}, err
	}
	return s.getItemLocked(id)
}

// ResolveItem closes an item. Resolving as done is only legal from the
// replacing state: the safe-replacement workflow must verify and import
// before anything is ever marked done. Resolving as failed is allowed from
// any open state.
func (s *Store) ResolveItem(id int64, status, notes string) (MaintenanceItem, error) {
	if status != MaintDone && status != MaintFailed {
		return MaintenanceItem{}, fmt.Errorf("status must be %q or %q, got %q", MaintDone, MaintFailed, status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.getItemLocked(id)
	if err != nil {
		return MaintenanceItem{}, err
	}
	if it.Status == MaintDone || it.Status == MaintFailed {
		return MaintenanceItem{}, fmt.Errorf("item %d is already %s", id, it.Status)
	}
	if status == MaintDone && it.Status != MaintReplacing {
		return MaintenanceItem{}, fmt.Errorf("invalid transition %s -> %s: only a verified replacement may resolve as done", it.Status, status)
	}
	if _, err := s.db.Exec(`UPDATE maintenance_items SET status=?, notes=?,
		claimed_by='', lease_until=NULL, updated_at=? WHERE id=?`,
		status, notes, nowStr(), id); err != nil {
		return MaintenanceItem{}, err
	}
	return s.getItemLocked(id)
}

// ReleaseDecision records why a release was selected, rejected or kept as
// an alternate. decisions are append-only: the history of "why did we pick
// this release" must survive later changes of mind.
type ReleaseDecision struct {
	ID                int64    `json:"id"`
	MaintenanceItemID int64    `json:"maintenance_item_id"`
	ReleaseGUID       string   `json:"release_guid"`
	Title             string   `json:"title"`
	ReleaseGroup      string   `json:"release_group"`
	Size              *int64   `json:"size,omitempty"`
	SeedersAtDecision *int     `json:"seeders_at_decision,omitempty"`
	Decision          string   `json:"decision"`
	Score             *float64 `json:"score,omitempty"`
	ReasonsJSON       string   `json:"reasons_json"`
	CreatedAt         string   `json:"created_at"`
}

// Decision values.
const (
	DecisionSelected  = "selected"
	DecisionRejected  = "rejected"
	DecisionAlternate = "alternate"
)

// RecordDecision appends a release decision for an open maintenance item.
func (s *Store) RecordDecision(d ReleaseDecision) (ReleaseDecision, error) {
	if d.MaintenanceItemID == 0 || d.Title == "" {
		return ReleaseDecision{}, fmt.Errorf("maintenance_item_id and title are required")
	}
	switch d.Decision {
	case DecisionSelected, DecisionRejected, DecisionAlternate:
	default:
		return ReleaseDecision{}, fmt.Errorf("decision must be selected, rejected or alternate, got %q", d.Decision)
	}
	if d.ReasonsJSON == "" {
		d.ReasonsJSON = "[]"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.getItemLocked(d.MaintenanceItemID); err != nil {
		return ReleaseDecision{}, err
	}
	now := nowStr()
	res, err := s.db.Exec(`INSERT INTO release_decisions
		(maintenance_item_id, release_guid, title, release_group, size, seeders_at_decision,
		 decision, score, reasons_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.MaintenanceItemID, d.ReleaseGUID, d.Title, d.ReleaseGroup, d.Size,
		d.SeedersAtDecision, d.Decision, d.Score, d.ReasonsJSON, now)
	if err != nil {
		return ReleaseDecision{}, err
	}
	d.ID, _ = res.LastInsertId()
	d.CreatedAt = now
	return d, nil
}

// ListDecisions returns the decision history for an item, newest first.
func (s *Store) ListDecisions(itemID int64, limit int) ([]ReleaseDecision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id, maintenance_item_id, release_guid, title, release_group,
		size, seeders_at_decision, decision, score, reasons_json, created_at
		FROM release_decisions WHERE maintenance_item_id=? ORDER BY id DESC LIMIT ?`, itemID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReleaseDecision{}
	for rows.Next() {
		var d ReleaseDecision
		if err := rows.Scan(&d.ID, &d.MaintenanceItemID, &d.ReleaseGUID, &d.Title,
			&d.ReleaseGroup, &d.Size, &d.SeedersAtDecision, &d.Decision, &d.Score,
			&d.ReasonsJSON, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MediaCheck records one inspection of a real file.
type MediaCheck struct {
	ID                int64    `json:"id"`
	MediaType         string   `json:"media_type"`
	MediaID           string   `json:"media_id"`
	FileID            string   `json:"file_id"`
	Path              string   `json:"path"`
	Container         string   `json:"container"`
	VideoCodec        string   `json:"video_codec"`
	Resolution        string   `json:"resolution"`
	BitDepth          *int     `json:"bit_depth,omitempty"`
	AudioLanguages    []string `json:"audio_languages"`
	SubtitleLanguages []string `json:"subtitle_languages"`
	DangerousFiles    []string `json:"dangerous_files"`
	DurationSec       *float64 `json:"duration_sec,omitempty"`
	SizeBytes         *int64   `json:"size_bytes,omitempty"`
	CheckedAt         string   `json:"checked_at"`
}

// RecordCheck stores a media inspection.
func (s *Store) RecordCheck(c MediaCheck) (MediaCheck, error) {
	if c.MediaType == "" {
		return MediaCheck{}, fmt.Errorf("media_type is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := nowStr()
	res, err := s.db.Exec(`INSERT INTO media_checks
		(media_type, media_id, file_id, path, container, video_codec, resolution, bit_depth,
		 audio_languages, subtitle_languages, dangerous_files, duration_sec, size_bytes, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.MediaType, c.MediaID, c.FileID, c.Path, c.Container, c.VideoCodec, c.Resolution,
		c.BitDepth, jsonList(c.AudioLanguages), jsonList(c.SubtitleLanguages),
		jsonList(c.DangerousFiles), c.DurationSec, c.SizeBytes, now)
	if err != nil {
		return MediaCheck{}, err
	}
	c.ID, _ = res.LastInsertId()
	c.CheckedAt = now
	return c, nil
}

// LatestCheck returns the most recent inspection for a media item.
func (s *Store) LatestCheck(mediaType, mediaID string) (MediaCheck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c MediaCheck
	var audio, subs, dangerous string
	err := s.db.QueryRow(`SELECT id, media_type, media_id, file_id, path, container, video_codec,
		resolution, bit_depth, audio_languages, subtitle_languages, dangerous_files,
		duration_sec, size_bytes, checked_at FROM media_checks
		WHERE media_type=? AND media_id=? ORDER BY id DESC LIMIT 1`, mediaType, mediaID).
		Scan(&c.ID, &c.MediaType, &c.MediaID, &c.FileID, &c.Path, &c.Container, &c.VideoCodec,
			&c.Resolution, &c.BitDepth, &audio, &subs, &dangerous,
			&c.DurationSec, &c.SizeBytes, &c.CheckedAt)
	if err == sql.ErrNoRows {
		return MediaCheck{}, fmt.Errorf("no checks recorded for %s %s", mediaType, mediaID)
	}
	if err != nil {
		return MediaCheck{}, err
	}
	c.AudioLanguages = unjsonList(audio)
	c.SubtitleLanguages = unjsonList(subs)
	c.DangerousFiles = unjsonList(dangerous)
	return c, nil
}

func jsonList(in []string) string {
	if len(in) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(in)
	return string(b)
}

func unjsonList(raw string) []string {
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}
	}
	return out
}

// secretKeys matches argument names that must never reach the audit log.
var secretKeys = []string{"api_key", "apikey", "password", "passwd", "token", "cookie", "authorization", "secret"}

// RedactSecrets replaces secret values in a JSON-ish argument summary.
func RedactSecrets(argsJSON string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return argsJSON
	}
	redacted := false
	for k := range m {
		lk := strings.ToLower(k)
		for _, sk := range secretKeys {
			if strings.Contains(lk, sk) {
				m[k] = "[REDACTED]"
				redacted = true
				break
			}
		}
	}
	if !redacted {
		return argsJSON
	}
	b, err := json.Marshal(m)
	if err != nil {
		return `{"args":"[REDACTED]"}`
	}
	return string(b)
}

// LogAction appends an audit entry. Secrets in args are redacted.
func (s *Store) LogAction(action, service, media, argsJSON, result string) error {
	if action == "" {
		return fmt.Errorf("action is required")
	}
	if len(argsJSON) > 4096 {
		argsJSON = argsJSON[:4096] + "…(truncated)"
	}
	if len(result) > 4096 {
		result = result[:4096] + "…(truncated)"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO action_log (action, service, media, args_json, result, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		action, service, media, RedactSecrets(argsJSON), result, nowStr())
	return err
}

// RecentActions returns the latest audit entries, newest first.
func (s *Store) RecentActions(limit int) ([]map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT action, service, media, args_json, result, created_at
		FROM action_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var action, service, media, args, result, at string
		if err := rows.Scan(&action, &service, &media, &args, &result, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"action": action, "service": service, "media": media,
			"args": args, "result": result, "at": at,
		})
	}
	return out, rows.Err()
}

// BlockRelease adds a release identifier (guid, hash or infohash) to the
// manual/automatic blocklist. Re-adding is a no-op that refreshes the reason.
func (s *Store) BlockRelease(identifier, reason, source string) error {
	if identifier == "" {
		return fmt.Errorf("identifier is required")
	}
	if source == "" {
		source = "manual"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO blocked_releases (identifier, reason, source, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (identifier) DO UPDATE SET reason=excluded.reason, source=excluded.source`,
		identifier, reason, source, nowStr())
	return err
}

// IsBlocked reports whether an identifier is blocklisted.
func (s *Store) IsBlocked(identifier string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM blocked_releases WHERE identifier=?`, identifier).Scan(&n)
	return n > 0
}

// ListBlocked returns blocklist entries, newest first.
func (s *Store) ListBlocked(limit int) ([]map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT identifier, reason, source, created_at
		FROM blocked_releases ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, reason, source, at string
		if err := rows.Scan(&id, &reason, &source, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"identifier": id, "reason": reason, "source": source, "at": at,
		})
	}
	return out, rows.Err()
}

// Context is the compact LLM briefing produced by GetContext.
type Context struct {
	Preferences []Preference      `json:"preferences"`
	ActiveItems []MaintenanceItem `json:"active_items"`
	Decisions   []ReleaseDecision `json:"decisions"`
	RecentLog   []map[string]any  `json:"recent_actions"`
}

// GetContext builds a strictly bounded briefing: relevant preferences, active
// maintenance work and recent decisions/actions. It never dumps the database.
func (s *Store) GetContext(scope, mediaType, mediaID string, itemLimit int) (Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if itemLimit <= 0 || itemLimit > 20 {
		itemLimit = 5
	}
	ctx := Context{}
	scopes := []string{"global"}
	if scope != "" && scope != "global" {
		scopes = append(scopes, scope)
	}
	ph := strings.Repeat("?,", len(scopes)-1) + "?"
	args := make([]any, 0, len(scopes))
	for _, sc := range scopes {
		args = append(args, sc)
	}
	rows, err := s.db.Query(`SELECT id, scope, key, value_json, source, created_at, updated_at, expires_at
		FROM preferences WHERE scope IN (`+ph+`) AND `+vigente()+` ORDER BY scope, key LIMIT 50`, args...)
	if err != nil {
		return ctx, err
	}
	for rows.Next() {
		var p Preference
		if err := rows.Scan(&p.ID, &p.Scope, &p.Key, &p.ValueJSON, &p.Source, &p.CreatedAt, &p.UpdatedAt, &p.ExpiresAt); err != nil {
			rows.Close()
			return ctx, err
		}
		ctx.Preferences = append(ctx.Preferences, p)
	}
	rows.Close()

	itemQ := `SELECT ` + maintColumns + ` FROM maintenance_items WHERE status NOT IN ('done','failed')`
	var iargs []any
	if mediaType != "" && mediaID != "" {
		itemQ += ` AND media_type=? AND media_id=?`
		iargs = append(iargs, mediaType, mediaID)
	} else if scope == "anime" || scope == "movies" {
		// Soft media-type hint: anime lives in Sonarr, movies in Radarr.
		if scope == "anime" {
			itemQ += ` AND media_type IN ('series','anime')`
		} else {
			itemQ += ` AND media_type IN ('movie','movies')`
		}
	}
	itemQ += ` ORDER BY priority DESC, updated_at ASC LIMIT ?`
	iargs = append(iargs, itemLimit)
	irows, err := s.db.Query(itemQ, iargs...)
	if err != nil {
		return ctx, err
	}
	for irows.Next() {
		var it MaintenanceItem
		if err := irows.Scan(maintScan(&it)...); err != nil {
			irows.Close()
			return ctx, err
		}
		ctx.ActiveItems = append(ctx.ActiveItems, it)
	}
	irows.Close()

	if len(ctx.ActiveItems) > 0 {
		dph := strings.Repeat("?,", len(ctx.ActiveItems)-1) + "?"
		dargs := make([]any, 0, len(ctx.ActiveItems))
		for _, it := range ctx.ActiveItems {
			dargs = append(dargs, it.ID)
		}
		drows, err := s.db.Query(`SELECT id, maintenance_item_id, release_guid, title, release_group,
			size, seeders_at_decision, decision, score, reasons_json, created_at
			FROM release_decisions WHERE maintenance_item_id IN (`+dph+`)
			ORDER BY id DESC LIMIT 20`, dargs...)
		if err != nil {
			return ctx, err
		}
		for drows.Next() {
			var d ReleaseDecision
			if err := drows.Scan(&d.ID, &d.MaintenanceItemID, &d.ReleaseGUID, &d.Title,
				&d.ReleaseGroup, &d.Size, &d.SeedersAtDecision, &d.Decision, &d.Score,
				&d.ReasonsJSON, &d.CreatedAt); err != nil {
				drows.Close()
				return ctx, err
			}
			ctx.Decisions = append(ctx.Decisions, d)
		}
		drows.Close()
	}

	lrows, err := s.db.Query(`SELECT action, service, media, args_json, result, created_at
		FROM action_log ORDER BY id DESC LIMIT 10`)
	if err != nil {
		return ctx, err
	}
	defer lrows.Close()
	for lrows.Next() {
		var action, service, media, args, result, at string
		if err := lrows.Scan(&action, &service, &media, &args, &result, &at); err != nil {
			return ctx, err
		}
		ctx.RecentLog = append(ctx.RecentLog, map[string]any{
			"action": action, "service": service, "media": media,
			"args": args, "result": result, "at": at,
		})
	}
	return ctx, lrows.Err()
}
