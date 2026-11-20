package nodes

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// schema is applied on every open; every statement is idempotent.
var schema = []string{
	// seq is the insertion order (created_at has second precision), so builds
	// created in the same second keep their creation order.
	`CREATE TABLE IF NOT EXISTS builds (id TEXT PRIMARY KEY, target TEXT NOT NULL, source TEXT NOT NULL, holder TEXT NOT NULL, kind TEXT NOT NULL, title TEXT NOT NULL, preempt_mode TEXT NOT NULL, priority TEXT NOT NULL DEFAULT 'normal', status TEXT NOT NULL, created_at TEXT NOT NULL, started_at TEXT NOT NULL DEFAULT '', last_heartbeat_at TEXT NOT NULL, expires_at TEXT NOT NULL, ended_at TEXT NOT NULL DEFAULT '', progress TEXT NOT NULL DEFAULT '', exit_code INTEGER, seq INTEGER NOT NULL DEFAULT 0) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS builds_status ON builds(status, expires_at)`,
	// seq is always >= 1 here (never the column default 0). The index is not
	// unique: an import shifts every existing seq by an offset in one UPDATE,
	// and a unique index would be checked row by row in the middle of it.
	`CREATE INDEX IF NOT EXISTS builds_seq ON builds(seq)`,
	// A node whose builds the operator paused: one row while it is paused.
	`CREATE TABLE IF NOT EXISTS build_pauses (worker TEXT PRIMARY KEY, since TEXT NOT NULL, updated_at TEXT NOT NULL) WITHOUT ROWID`,
	// Facts about this database (the one-time import from the main database).
	`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID`,
}

// registry is the SQLite side of the registry; one writer at a time.
type registry struct {
	mu sync.Mutex
	db *sql.DB
	// lock holds the flock on <path>.lock for the life of the registry.
	lock *os.File
}

func openRegistry(path string) (*registry, error) {
	if path == "" {
		return nil, errors.New("nodes database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating nodes database directory: %w", err)
	}
	// One process owns the database: the lock comes before anything opens it.
	lock, err := lockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	reg, err := openDB(path)
	if err != nil {
		unlock(lock)
		return nil, err
	}
	reg.lock = lock
	return reg, nil
}

// openDB opens the database and applies the schema, without the process lock.
func openDB(path string) (*registry, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("opening nodes database %s: %w", path, err)
	}
	db.SetMaxOpenConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening nodes database %s: %w", path, err)
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("nodes database schema: %w", err)
		}
	}
	return &registry{db: db}, nil
}

// unlock releases the lock file's flock by closing it. The file stays on disk.
func unlock(f *os.File) {
	if f != nil {
		f.Close()
	}
}

func (r *registry) close() error {
	err := r.db.Close()
	unlock(r.lock)
	return err
}

// BuildPause is a node whose builds are paused.
type BuildPause struct {
	Worker    string `json:"worker"`
	Since     string `json:"since"`
	UpdatedAt string `json:"updated_at"`
}

const buildColumns = `id, target, source, holder, kind, title, preempt_mode, priority, status, created_at, started_at, last_heartbeat_at, expires_at, ended_at, progress, exit_code`

type rowScanner interface{ Scan(dest ...any) error }

func scanBuild(r rowScanner) (Build, error) {
	var b Build
	var exit sql.NullInt64
	err := r.Scan(&b.ID, &b.Target, &b.Source, &b.Holder, &b.Kind, &b.Title, &b.PreemptMode, &b.Priority, &b.Status, &b.CreatedAt, &b.StartedAt, &b.LastHeartbeatAt, &b.ExpiresAt, &b.EndedAt, &b.Progress, &exit)
	if exit.Valid {
		v := int(exit.Int64)
		b.ExitCode = &v
	}
	return b, err
}

func buildTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// create records a running build whose heartbeat is due within ttl. The caller
// has validated the fields; id and times are set here.
func (r *registry) create(b Build, now time.Time, ttl time.Duration) (Build, error) {
	if b.ID == "" {
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			return Build{}, err
		}
		b.ID = "bld_" + hex.EncodeToString(raw)
	}
	b.Status = BuildRunning
	if b.Priority == "" {
		b.Priority = BuildPriorityNormal
	}
	b.CreatedAt = buildTime(now)
	b.StartedAt = b.CreatedAt
	b.LastHeartbeatAt = b.CreatedAt
	b.ExpiresAt = buildTime(now.Add(ttl))
	b.EndedAt, b.ExitCode = "", nil
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(`INSERT INTO builds(`+buildColumns+`, seq) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, NULL, (SELECT COALESCE(MAX(seq), 0) + 1 FROM builds))`,
		b.ID, b.Target, b.Source, b.Holder, b.Kind, b.Title, b.PreemptMode, b.Priority, b.Status, b.CreatedAt, b.StartedAt, b.LastHeartbeatAt, b.ExpiresAt, b.Progress)
	return b, err
}

func (r *registry) get(id string) (Build, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getLocked(id)
}

func (r *registry) getLocked(id string) (Build, error) {
	b, err := scanBuild(r.db.QueryRow(`SELECT `+buildColumns+` FROM builds WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, ErrBuildNotFound
	}
	return b, err
}

// active returns the builds that are waiting or running, oldest first (by
// creation time, then insertion order).
func (r *registry) active() ([]Build, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.queryLocked(`SELECT ` + buildColumns + ` FROM builds WHERE status IN ('running','waiting') ORDER BY created_at, seq`)
}

func (r *registry) queryLocked(q string, args ...any) ([]Build, error) {
	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Build{}
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// list returns the matching builds: every active one first, newest first, then
// the ended history newest first. The limit applies to the history only: all
// the active builds are always returned, followed by up to limit ended ones.
// Limit 0 means 100; a negative limit means no limit.
func (r *registry) list(f BuildFilter) ([]Build, error) {
	where := ` WHERE 1=1`
	var args []any
	for _, c := range []struct{ col, v string }{{"target", f.Target}, {"status", f.Status}, {"source", f.Source}} {
		if c.v != "" {
			where += ` AND ` + c.col + `=?`
			args = append(args, c.v)
		}
	}
	limit := f.Limit
	if limit == 0 {
		limit = BuildDefaultLimit
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out, err := r.queryLocked(`SELECT `+buildColumns+` FROM builds`+where+` AND status IN ('running','waiting') ORDER BY created_at DESC, seq DESC`, args...)
	if err != nil {
		return nil, err
	}
	hq := `SELECT ` + buildColumns + ` FROM builds` + where + ` AND status NOT IN ('running','waiting') ORDER BY created_at DESC, seq DESC`
	if limit > 0 {
		hq += fmt.Sprintf(` LIMIT %d`, limit)
	}
	history, err := r.queryLocked(hq, args...)
	if err != nil {
		return nil, err
	}
	return append(out, history...), nil
}

// page returns one page of the matching builds in a single stable order: newest
// insertion first (seq, then id as a tie-break, so the order is total). A build
// changing status never moves it, and rows pruned between pages cannot shift
// the ones after the cursor. before is the seq of the last build of the previous
// page (0 for the first page). It returns up to limit builds and whether more
// follow. Only the rows of the page are read.
func (r *registry) page(f BuildFilter, before int64, limit int) (out []Build, more bool, err error) {
	where := ` WHERE 1=1`
	var args []any
	for _, c := range []struct{ col, v string }{{"target", f.Target}, {"status", f.Status}, {"source", f.Source}} {
		if c.v != "" {
			where += ` AND ` + c.col + `=?`
			args = append(args, c.v)
		}
	}
	if before > 0 {
		where += ` AND seq < ?`
		args = append(args, before)
	}
	args = append(args, limit+1)
	r.mu.Lock()
	rows, err := r.db.Query(`SELECT `+buildColumns+`, seq FROM builds`+where+` ORDER BY seq DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		r.mu.Unlock()
		return nil, false, err
	}
	defer r.mu.Unlock()
	defer rows.Close()
	for rows.Next() {
		var b Build
		var exit sql.NullInt64
		if err := rows.Scan(&b.ID, &b.Target, &b.Source, &b.Holder, &b.Kind, &b.Title, &b.PreemptMode, &b.Priority, &b.Status, &b.CreatedAt, &b.StartedAt, &b.LastHeartbeatAt, &b.ExpiresAt, &b.EndedAt, &b.Progress, &exit, &b.seq); err != nil {
			return nil, false, err
		}
		if exit.Valid {
			v := int(exit.Int64)
			b.ExitCode = &v
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		out, more = out[:limit], true
	}
	return out, more, nil
}

// heartbeat extends a running build's expiry to now+ttl (a ttl of 0 keeps the
// window it had) and optionally replaces its progress line. A build that ended
// gives ErrBuildEnded (with the stored record). A heartbeat that arrives after
// the build's expiry never extends it: the build is marked lost on the spot,
// with ended_at set to its expires_at, and the call gives ErrBuildExpired with
// the lost record.
func (r *registry) heartbeat(id string, now time.Time, ttl time.Duration, progress *string) (Build, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := r.getLocked(id)
	if err != nil {
		return Build{}, err
	}
	if !b.Active() {
		return b, ErrBuildEnded
	}
	if exp, perr := time.Parse(time.RFC3339, b.ExpiresAt); perr == nil && !exp.After(now) {
		b.Status, b.EndedAt = BuildLost, b.ExpiresAt
		_, err = r.db.Exec(`UPDATE builds SET status='lost', ended_at=? WHERE id=? AND status IN ('running','waiting')`, b.EndedAt, id)
		if err != nil {
			return b, err
		}
		return b, ErrBuildExpired
	}
	if ttl <= 0 {
		// Keep the window the build already has, as it is: no floor. Only a
		// window that cannot be read falls back.
		ttl = BuildMinTTL
		last, e1 := time.Parse(time.RFC3339, b.LastHeartbeatAt)
		exp, e2 := time.Parse(time.RFC3339, b.ExpiresAt)
		if e1 == nil && e2 == nil && exp.Sub(last) > 0 {
			ttl = exp.Sub(last)
		}
	}
	b.LastHeartbeatAt, b.ExpiresAt = buildTime(now), buildTime(now.Add(ttl))
	if progress != nil {
		b.Progress = *progress
	}
	_, err = r.db.Exec(`UPDATE builds SET last_heartbeat_at=?, expires_at=?, progress=? WHERE id=? AND status IN ('running','waiting')`, b.LastHeartbeatAt, b.ExpiresAt, b.Progress, id)
	return b, err
}

// finish ends a running build with a status. changed is false, and the stored
// record is returned untouched, when the build had already ended.
func (r *registry) finish(id, status string, exit *int, now time.Time) (b Build, changed bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err = r.getLocked(id)
	if err != nil {
		return Build{}, false, err
	}
	if !b.Active() {
		return b, false, nil
	}
	b.Status, b.EndedAt, b.ExitCode = status, buildTime(now), exit
	var code any
	if exit != nil {
		code = *exit
	}
	_, err = r.db.Exec(`UPDATE builds SET status=?, ended_at=?, exit_code=? WHERE id=? AND status IN ('running','waiting')`, b.Status, b.EndedAt, code, id)
	return b, err == nil, err
}

// expire marks the running (or old waiting) builds whose expiry has passed as
// lost and returns them.
func (r *registry) expire(now time.Time) ([]Build, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lost, err := r.queryLocked(`SELECT `+buildColumns+` FROM builds WHERE status IN ('running','waiting') AND expires_at <= ?`, buildTime(now))
	if err != nil {
		return nil, err
	}
	for i := range lost {
		lost[i].Status, lost[i].EndedAt = BuildLost, buildTime(now)
		if _, err := r.db.Exec(`UPDATE builds SET status='lost', ended_at=? WHERE id=? AND status IN ('running','waiting')`, lost[i].EndedAt, lost[i].ID); err != nil {
			return lost[:i], err
		}
	}
	return lost, nil
}

// cancelWaiting ends every build an earlier server left waiting (nothing
// promotes them any more) as cancelled, with the reason as its progress, and
// returns them.
func (r *registry) cancelWaiting(now time.Time) ([]Build, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out, err := r.queryLocked(`SELECT ` + buildColumns + ` FROM builds WHERE status='waiting'`)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Status, out[i].EndedAt, out[i].Progress = BuildCancelled, buildTime(now), LegacyWaitingReason
		if _, err := r.db.Exec(`UPDATE builds SET status='cancelled', ended_at=?, progress=? WHERE id=? AND status='waiting'`, out[i].EndedAt, out[i].Progress, out[i].ID); err != nil {
			return out[:i], err
		}
	}
	return out, nil
}

// prune deletes the builds that ended before the cutoff and returns how many.
// A running or waiting build is never deleted.
func (r *registry) prune(before time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, err := r.db.Exec(`DELETE FROM builds WHERE status NOT IN ('running','waiting') AND ended_at <> '' AND ended_at < ?`, buildTime(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *registry) listPauses() (map[string]BuildPause, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := r.db.Query(`SELECT worker, since, updated_at FROM build_pauses`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]BuildPause{}
	for rows.Next() {
		var p BuildPause
		if err := rows.Scan(&p.Worker, &p.Since, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out[p.Worker] = p
	}
	return out, rows.Err()
}

func (r *registry) paused(node string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var one int
	err := r.db.QueryRow(`SELECT 1 FROM build_pauses WHERE worker=?`, node).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// setPause pauses (true) or resumes (false) new builds on a node. Repeating the
// same request changes nothing and keeps the start time.
func (r *registry) setPause(node string, paused bool, now time.Time) error {
	if node == "" {
		return errors.New("node name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !paused {
		_, err := r.db.Exec(`DELETE FROM build_pauses WHERE worker=?`, node)
		return err
	}
	ts := buildTime(now)
	_, err := r.db.Exec(`INSERT INTO build_pauses(worker, since, updated_at) VALUES(?, ?, ?) ON CONFLICT(worker) DO NOTHING`, node, ts, ts)
	return err
}

func (r *registry) metaGet(key string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var v string
	err := r.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}
