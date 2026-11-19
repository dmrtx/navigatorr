package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Build statuses. A build is running until it is finished by its owner
// (succeeded, failed or cancelled) or its heartbeat lapses (lost).
const (
	// BuildWaiting is no longer created: the server's builds never wait. Old
	// rows (and a worker's own leases) may still carry it; it is read as an
	// active build and expires like a running one.
	BuildWaiting   = "waiting"
	BuildRunning   = "running"
	BuildSucceeded = "succeeded"
	BuildFailed    = "failed"
	BuildCancelled = "cancelled"
	BuildLost      = "lost"
)

// Build priorities. The server only records them: a build created here is
// running at once, and what a priority means on a node is decided by the node's
// worker.
const (
	BuildPriorityNormal = "normal"
	BuildPriorityLow    = "low"
)

// BuildMinTTL is the window a heartbeat without a ttl falls back to only when
// the build's own window cannot be read.
const BuildMinTTL = time.Minute

// ErrBuildNotFound and ErrBuildEnded are what the build calls report for an
// unknown build and for a change to a build that already ended.
var (
	ErrBuildNotFound = errors.New("build not found")
	ErrBuildEnded    = errors.New("build already ended")
)

// ErrBuildExpired is what HeartbeatBuild reports for a heartbeat that arrives
// after the build's expiry but before the reconcile tick marked it lost. The
// build is marked lost right there; the error wraps ErrBuildEnded, so callers
// that only care that the build is over treat it the same.
var ErrBuildExpired = fmt.Errorf("%w: heartbeat after its expiry, marked lost", ErrBuildEnded)

// Build is one job another tool runs on a machine: a gradle build, an
// integration test, a warm-up. Target is free text; when it names a transcode
// node the build holds that node while it runs. The record stays as history
// after it ends.
type Build struct {
	ID          string `json:"id"`
	Target      string `json:"target"`
	Source      string `json:"source"`
	Holder      string `json:"holder"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	PreemptMode string `json:"preempt_mode"`
	Priority    string `json:"priority"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	// StartedAt is when the build began to run: its creation for a normal
	// build, its promotion for a low priority one; empty while it waits.
	StartedAt       string `json:"started_at,omitempty"`
	LastHeartbeatAt string `json:"last_heartbeat_at"`
	ExpiresAt       string `json:"expires_at"`
	EndedAt         string `json:"ended_at,omitempty"`
	Progress        string `json:"progress,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	// HoldsTranscodeNode is kept for compatibility and is always false: the
	// server never holds a node for a build, the node's worker does.
	HoldsTranscodeNode bool `json:"holds_transcode_node"`
}

// BuildFilter narrows ListBuilds. Empty fields match everything.
type BuildFilter struct {
	Target string
	Status string
	Source string
	Limit  int
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

// CreateBuild records a running build whose heartbeat is due within ttl. The
// caller has validated the fields; id and times are set here.
func (s *Store) CreateBuild(b Build, now time.Time, ttl time.Duration) (Build, error) {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO builds(`+buildColumns+`, seq) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, NULL, (SELECT COALESCE(MAX(seq), 0) + 1 FROM builds))`,
		b.ID, b.Target, b.Source, b.Holder, b.Kind, b.Title, b.PreemptMode, b.Priority, b.Status, b.CreatedAt, b.StartedAt, b.LastHeartbeatAt, b.ExpiresAt, b.Progress)
	return b, err
}

// GetBuild returns one build, or ErrBuildNotFound.
func (s *Store) GetBuild(id string) (Build, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getBuildLocked(id)
}

func (s *Store) getBuildLocked(id string) (Build, error) {
	b, err := scanBuild(s.db.QueryRow(`SELECT `+buildColumns+` FROM builds WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, ErrBuildNotFound
	}
	return b, err
}

// ActiveBuilds returns the builds that are waiting or running, oldest first.
func (s *Store) ActiveBuilds() ([]Build, error) {
	return s.activeBuilds(`status IN ('running','waiting')`)
}

// activeBuilds reads builds by a fixed condition, oldest first: by creation
// time, then by insertion order (created_at has second precision).
func (s *Store) activeBuilds(cond string) ([]Build, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queryBuildsLocked(`SELECT ` + buildColumns + ` FROM builds WHERE ` + cond + ` ORDER BY created_at, seq`)
}

func (s *Store) queryBuildsLocked(q string, args ...any) ([]Build, error) {
	rows, err := s.db.Query(q, args...)
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

// ListBuilds returns the matching builds: every active one (running, waiting)
// first, newest first, then the ended history newest first. The limit applies
// to the history only: all the active builds are always returned, followed by
// up to limit ended ones. Limit 0 means 100; a negative limit means no limit.
func (s *Store) ListBuilds(f BuildFilter) ([]Build, error) {
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
		limit = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, err := s.queryBuildsLocked(`SELECT `+buildColumns+` FROM builds`+where+` AND status IN ('running','waiting') ORDER BY created_at DESC, seq DESC`, args...)
	if err != nil {
		return nil, err
	}
	hq := `SELECT ` + buildColumns + ` FROM builds` + where + ` AND status NOT IN ('running','waiting') ORDER BY created_at DESC, seq DESC`
	if limit > 0 {
		hq += fmt.Sprintf(` LIMIT %d`, limit)
	}
	history, err := s.queryBuildsLocked(hq, args...)
	if err != nil {
		return nil, err
	}
	return append(out, history...), nil
}

// HeartbeatBuild extends a running build's expiry to now+ttl (a ttl of 0 keeps the window it had) and optionally
// replaces its progress line. A build that ended gives ErrBuildEnded (with the
// stored record). A heartbeat that arrives after the build's expiry never
// extends it: the build is marked lost on the spot, with ended_at set to its
// expires_at (when it really lapsed, not when the late call came), and the
// call gives ErrBuildExpired (which is an ErrBuildEnded) with the lost record.
func (s *Store) HeartbeatBuild(id string, now time.Time, ttl time.Duration, progress *string) (Build, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.getBuildLocked(id)
	if err != nil {
		return Build{}, err
	}
	if b.Status != BuildRunning && b.Status != BuildWaiting {
		return b, ErrBuildEnded
	}
	if exp, perr := time.Parse(time.RFC3339, b.ExpiresAt); perr == nil && !exp.After(now) {
		// Same boundary as ExpireBuilds: expires_at <= now.
		b.Status, b.EndedAt = BuildLost, b.ExpiresAt
		_, err = s.db.Exec(`UPDATE builds SET status='lost', ended_at=? WHERE id=? AND status IN ('running','waiting')`, b.EndedAt, id)
		if err != nil {
			return b, err
		}
		return b, ErrBuildExpired
	}
	if ttl <= 0 {
		// Keep the window the build already has, as it is: no floor. Only a
		// window that cannot be read (never written by this code) falls back.
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
	_, err = s.db.Exec(`UPDATE builds SET last_heartbeat_at=?, expires_at=?, progress=? WHERE id=? AND status IN ('running','waiting')`, b.LastHeartbeatAt, b.ExpiresAt, b.Progress, id)
	return b, err
}

// FinishBuild ends a running build with a status. changed is false, and the
// stored record is returned untouched, when the build had already ended.
func (s *Store) FinishBuild(id, status string, exit *int, now time.Time) (b Build, changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err = s.getBuildLocked(id)
	if err != nil {
		return Build{}, false, err
	}
	if b.Status != BuildRunning && b.Status != BuildWaiting {
		return b, false, nil
	}
	b.Status, b.EndedAt, b.ExitCode = status, buildTime(now), exit
	var code any
	if exit != nil {
		code = *exit
	}
	_, err = s.db.Exec(`UPDATE builds SET status=?, ended_at=?, exit_code=? WHERE id=? AND status IN ('running','waiting')`, b.Status, b.EndedAt, code, id)
	return b, err == nil, err
}

// ExpireBuilds marks the running (or old waiting) builds whose expiry has passed as lost and
// returns them.
func (s *Store) ExpireBuilds(now time.Time) ([]Build, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT `+buildColumns+` FROM builds WHERE status IN ('running','waiting') AND expires_at <= ?`, buildTime(now))
	if err != nil {
		return nil, err
	}
	var lost []Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		lost = append(lost, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range lost {
		lost[i].Status, lost[i].EndedAt = BuildLost, buildTime(now)
		if _, err := s.db.Exec(`UPDATE builds SET status='lost', ended_at=? WHERE id=? AND status IN ('running','waiting')`, lost[i].EndedAt, lost[i].ID); err != nil {
			return lost[:i], err
		}
	}
	return lost, nil
}

// LegacyWaitingReason is the progress an old waiting build ends with.
const LegacyWaitingReason = "server-side waiting builds were removed; retry"

// CancelWaitingBuilds ends every build an earlier server left waiting (it
// never promotes them any more) as cancelled, with the reason as its progress,
// and returns them. A client that polls such a build then sees it ended.
func (s *Store) CancelWaitingBuilds(now time.Time) ([]Build, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT ` + buildColumns + ` FROM builds WHERE status='waiting'`)
	if err != nil {
		return nil, err
	}
	var out []Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Status, out[i].EndedAt, out[i].Progress = BuildCancelled, buildTime(now), LegacyWaitingReason
		if _, err := s.db.Exec(`UPDATE builds SET status='cancelled', ended_at=?, progress=? WHERE id=? AND status='waiting'`, out[i].EndedAt, out[i].Progress, out[i].ID); err != nil {
			return out[:i], err
		}
	}
	return out, nil
}

// PruneBuilds deletes the builds that ended before the cutoff and returns how
// many. A running or waiting build is never deleted.
func (s *Store) PruneBuilds(before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM builds WHERE status NOT IN ('running','waiting') AND ended_at <> '' AND ended_at < ?`, buildTime(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
