package nodes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
)

const metaLegacyImport = "legacy_import"

// ImportResult says what ImportLegacy did.
type ImportResult struct {
	// Skipped is why nothing was read: "already imported" or "no legacy
	// database"; empty when the legacy database was read.
	Skipped string
	Builds  int
	Pauses  int
}

// ImportLegacy is the one-time move of the build records and the builds pauses
// from the main database (where they lived before the registry had its own) to
// this registry. It runs at startup and is idempotent:
//
//   - it is decided only by a flag in this database, which is written inside
//     the transaction of a successful import and nowhere else. Once the flag is
//     there it does nothing, so records pruned later are never brought back;
//   - it does not look at what this database holds: a first import that failed
//     is retried at the next start even if builds or pauses were written here
//     in the meantime. The copy is INSERT OR IGNORE, so a row already here (the
//     registry's own, newer one) always wins and nothing is overwritten;
//   - the flag is checked once more inside the transaction, under the write
//     lock, so two concurrent imports (callers or processes sharing the file)
//     import once and the second is Skipped;
//   - a failed import changes nothing (one transaction) and is reported to the
//     caller, who logs it and starts anyway;
//   - the legacy builds take the lowest insertion numbers, in their own order
//     (oldest first), and the registry's rows move up behind them, so history
//     imported by a retry pages after the builds created in the meantime.
//
// Known and accepted: a retry after a failed first import brings back a legacy
// builds pause for a node the operator resumed in between (the registry only
// keeps a row while paused, so the resume left nothing to tell the import to
// leave it). This fails safe (the node refuses builds until resumed again) and
// the retry logs the worker name of each pause it brings back.
//
// The old tables in the main database are only read, never changed or dropped.
// legacyPath is the main database file; a missing file, or one without the old
// tables, imports nothing.
func (s *Service) ImportLegacy(ctx context.Context, legacyPath string) (ImportResult, error) {
	if done, ok, err := s.reg.metaGet(metaLegacyImport); err != nil {
		return ImportResult{}, err
	} else if ok {
		return ImportResult{Skipped: "already imported (" + done + ")"}, nil
	}
	if legacyPath == "" {
		return ImportResult{Skipped: "no legacy database"}, nil
	}
	if _, err := os.Stat(legacyPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ImportResult{Skipped: "no legacy database"}, nil
		}
		return ImportResult{}, err
	}
	old, err := sql.Open("sqlite", "file:"+(&url.URL{Path: legacyPath}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return ImportResult{}, fmt.Errorf("opening the main database to import builds: %w", err)
	}
	defer old.Close()

	var res ImportResult
	builds, err := readLegacyBuilds(ctx, old)
	if err != nil {
		return ImportResult{}, err
	}
	pauses, err := readLegacyPauses(ctx, old)
	if err != nil {
		return ImportResult{}, err
	}

	s.reg.mu.Lock()
	defer s.reg.mu.Unlock()
	tx, err := s.reg.db.BeginTx(ctx, nil)
	if err != nil {
		return ImportResult{}, err
	}
	defer tx.Rollback()
	// A statement that writes (even one that matches no row) takes the
	// database's write lock now, so the flag is read below as the latest
	// committed state and no other process can finish an import between that
	// read and our commit.
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET value = value WHERE key = ?`, metaLegacyImport); err != nil {
		return ImportResult{}, err
	}
	// Checked again under the lock: two imports (two callers, or two processes
	// on one file) that both passed the early check above import only once.
	var done string
	switch err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaLegacyImport).Scan(&done); {
	case err == nil:
		return ImportResult{Skipped: "already imported (" + done + ")"}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return ImportResult{}, err
	}
	// A registry that already holds rows means an earlier import failed and
	// this is the retry.
	var held int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM builds) + (SELECT COUNT(*) FROM build_pauses)`).Scan(&held); err != nil {
		return ImportResult{}, err
	}
	retry := held > 0
	// The legacy history is older than anything the registry already holds, so
	// it takes the lowest insertion numbers, in the order it was read
	// (created_at, seq): the registry's own rows move up by the number of
	// legacy rows. Gaps left by rows that already exist are harmless; only the
	// order matters. seq is never below 1, and builds_seq is not unique, so
	// the shift cannot collide midway.
	if len(builds) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE builds SET seq = seq + ?`, len(builds)); err != nil {
			return ImportResult{}, err
		}
	}
	for i, b := range builds {
		var code any
		if b.ExitCode != nil {
			code = *b.ExitCode
		}
		r, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO builds(`+buildColumns+`, seq) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.ID, b.Target, b.Source, b.Holder, b.Kind, b.Title, b.PreemptMode, b.Priority, b.Status, b.CreatedAt, b.StartedAt, b.LastHeartbeatAt, b.ExpiresAt, b.EndedAt, b.Progress, code, i+1)
		if err != nil {
			return ImportResult{}, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.Builds++
		}
	}
	var pauseNames []string
	for _, p := range pauses {
		r, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO build_pauses(worker, since, updated_at) VALUES(?, ?, ?)`, p.Worker, p.Since, p.UpdatedAt)
		if err != nil {
			return ImportResult{}, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.Pauses++
			pauseNames = append(pauseNames, p.Worker)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES(?, ?)`, metaLegacyImport, fmt.Sprintf("%d builds, %d pauses", res.Builds, res.Pauses)); err != nil {
		return ImportResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ImportResult{}, err
	}
	log.Printf("nodes: imported %d build record(s) and %d builds pause(s) from the main database (the old tables stay as they were)", res.Builds, res.Pauses)
	if retry && len(pauseNames) > 0 {
		log.Printf("nodes: retry of an earlier failed import: builds pause(s) re-imported for %s; resume them if they were lifted meanwhile", strings.Join(pauseNames, ", "))
	}
	return res, nil
}

func noTable(err error) bool { return err != nil && strings.Contains(err.Error(), "no such table") }

func readLegacyBuilds(ctx context.Context, db *sql.DB) ([]Build, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+buildColumns+` FROM builds ORDER BY created_at, seq`)
	if err != nil {
		if noTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the old builds table: %w", err)
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func readLegacyPauses(ctx context.Context, db *sql.DB) ([]BuildPause, error) {
	rows, err := db.QueryContext(ctx, `SELECT worker, since, updated_at FROM build_pauses`)
	if err != nil {
		if noTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the old build_pauses table: %w", err)
	}
	defer rows.Close()
	var out []BuildPause
	for rows.Next() {
		var p BuildPause
		if err := rows.Scan(&p.Worker, &p.Since, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
