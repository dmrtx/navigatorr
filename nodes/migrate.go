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
	// Skipped is why nothing was read: "already imported", "registry not empty"
	// or "no legacy database"; empty when the legacy database was read.
	Skipped string
	Builds  int
	Pauses  int
}

// ImportLegacy is the one-time move of the build records and the builds pauses
// from the main database (where they lived before the registry had its own) to
// this registry. It runs at startup and is idempotent:
//
//   - it does nothing once it has run (a flag in this database), so records
//     pruned later are never brought back;
//   - it does nothing when this database already holds builds or pauses;
//   - it copies with INSERT OR IGNORE, so a run interrupted halfway is safe to
//     repeat.
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
	var have int
	s.reg.mu.Lock()
	err := s.reg.db.QueryRow(`SELECT (SELECT COUNT(*) FROM builds) + (SELECT COUNT(*) FROM build_pauses)`).Scan(&have)
	s.reg.mu.Unlock()
	if err != nil {
		return ImportResult{}, err
	}
	if have > 0 {
		s.markImported("registry not empty")
		return ImportResult{Skipped: "registry not empty"}, nil
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
	for i, b := range builds {
		var code any
		if b.ExitCode != nil {
			code = *b.ExitCode
		}
		r, err := tx.Exec(`INSERT OR IGNORE INTO builds(`+buildColumns+`, seq) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.ID, b.Target, b.Source, b.Holder, b.Kind, b.Title, b.PreemptMode, b.Priority, b.Status, b.CreatedAt, b.StartedAt, b.LastHeartbeatAt, b.ExpiresAt, b.EndedAt, b.Progress, code, i+1)
		if err != nil {
			return ImportResult{}, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.Builds++
		}
	}
	for _, p := range pauses {
		r, err := tx.Exec(`INSERT OR IGNORE INTO build_pauses(worker, since, updated_at) VALUES(?, ?, ?)`, p.Worker, p.Since, p.UpdatedAt)
		if err != nil {
			return ImportResult{}, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.Pauses++
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES(?, ?)`, metaLegacyImport, fmt.Sprintf("%d builds, %d pauses", res.Builds, res.Pauses)); err != nil {
		return ImportResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ImportResult{}, err
	}
	log.Printf("nodes: imported %d build record(s) and %d builds pause(s) from the main database (the old tables stay as they were)", res.Builds, res.Pauses)
	return res, nil
}

func (s *Service) markImported(note string) {
	s.reg.mu.Lock()
	defer s.reg.mu.Unlock()
	_, _ = s.reg.db.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES(?, ?)`, metaLegacyImport, note)
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
