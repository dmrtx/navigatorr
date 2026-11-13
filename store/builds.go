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
	// BuildWaiting is a low priority build whose node is not idle yet: it
	// holds nothing and starts (running) once the node is idle.
	BuildWaiting   = "waiting"
	BuildRunning   = "running"
	BuildSucceeded = "succeeded"
	BuildFailed    = "failed"
	BuildCancelled = "cancelled"
	BuildLost      = "lost"
)

// Build priorities. A normal build starts at once and preempts the node's
// encodes; a low build never preempts and waits for an idle node.
const (
	BuildPriorityNormal = "normal"
	BuildPriorityLow    = "low"
)

// BuildMinTTL is the shortest window a heartbeat without a ttl keeps.
const BuildMinTTL = time.Minute

// ErrBuildNotFound and ErrBuildEnded are what the build calls report for an
// unknown build and for a change to a build that already ended.
var (
	ErrBuildNotFound = errors.New("build not found")
	ErrBuildEnded    = errors.New("build already ended")
)

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
	// HoldsTranscodeNode is whether the target is a configured transcode node, so the
	// build holds it while running. It is derived by the engine, never stored.
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

// CreateBuild records a build whose heartbeat is due within ttl: running, or
// waiting when wait is set. The caller has validated the fields; id and times
// are set here.
func (s *Store) CreateBuild(b Build, now time.Time, ttl time.Duration, wait bool) (Build, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return Build{}, err
	}
	b.ID = "bld_" + hex.EncodeToString(raw)
	b.Status = BuildRunning
	if b.Priority == "" {
		b.Priority = BuildPriorityNormal
	}
	b.CreatedAt = buildTime(now)
	b.StartedAt = b.CreatedAt
	if wait {
		b.Status, b.StartedAt = BuildWaiting, ""
	}
	b.LastHeartbeatAt = b.CreatedAt
	b.ExpiresAt = buildTime(now.Add(ttl))
	b.EndedAt, b.ExitCode = "", nil
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO builds(`+buildColumns+`) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, NULL)`,
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

// RunningBuilds returns the builds that are running, oldest first.
func (s *Store) RunningBuilds() ([]Build, error) {
	list, err := s.ListBuilds(BuildFilter{Status: BuildRunning, Limit: -1})
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list, err
}

// WaitingBuilds returns the waiting builds, oldest first.
func (s *Store) WaitingBuilds() ([]Build, error) {
	list, err := s.ListBuilds(BuildFilter{Status: BuildWaiting, Limit: -1})
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list, err
}

// PromoteBuild turns a waiting build into a running one. It reports whether it
// did (false when the build is no longer waiting).
func (s *Store) PromoteBuild(id string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE builds SET status='running', started_at=? WHERE id=? AND status='waiting'`, buildTime(now), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ActiveBuilds returns the builds that are waiting or running, oldest first.
func (s *Store) ActiveBuilds() ([]Build, error) {
	list, err := s.ListBuilds(BuildFilter{Limit: -1})
	out := make([]Build, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Status == BuildRunning || list[i].Status == BuildWaiting {
			out = append(out, list[i])
		}
	}
	return out, err
}

// ListBuilds returns the matching builds, newest first. Limit 0 means 100; a
// negative limit means no limit.
func (s *Store) ListBuilds(f BuildFilter) ([]Build, error) {
	q := `SELECT ` + buildColumns + ` FROM builds WHERE 1=1`
	var args []any
	for _, c := range []struct{ col, v string }{{"target", f.Target}, {"status", f.Status}, {"source", f.Source}} {
		if c.v != "" {
			q += ` AND ` + c.col + `=?`
			args = append(args, c.v)
		}
	}
	q += ` ORDER BY created_at DESC, id`
	limit := f.Limit
	if limit == 0 {
		limit = 100
	}
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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

// HeartbeatBuild extends a running build's expiry to now+ttl (a ttl of 0 keeps the window it had) and optionally
// replaces its progress line. A build that ended gives ErrBuildEnded (with the
// stored record).
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
	if ttl <= 0 {
		// Keep the window the build already has.
		ttl = BuildMinTTL
		last, e1 := time.Parse(time.RFC3339, b.LastHeartbeatAt)
		exp, e2 := time.Parse(time.RFC3339, b.ExpiresAt)
		if e1 == nil && e2 == nil && exp.Sub(last) > ttl {
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

// ExpireBuilds marks the running builds whose expiry has passed as lost and
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
