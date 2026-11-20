package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// A database at version 25 with rows keeps them when it moves to 26, and gains
// the (empty) builds pause table. (The builds registry itself moved to package
// nodes; the old tables stay here untouched.)
func TestMigrationV25ToV26KeepsRowsAndAddsBuildPauses(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v25.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version > 25 {
			continue
		}
		for _, stmt := range m.statements {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("applying migration %d: %v", m.version, err)
			}
		}
		if _, err := db.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`, m.version, time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO worker_pauses(worker, mode, since, revision, updated_at) VALUES('node-a', 'graceful', 'then', 3, 'then')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO builds(id, target, source, holder, kind, title, preempt_mode, priority, status, created_at, started_at, last_heartbeat_at, expires_at, seq)
		VALUES('bld_old', 'node-a', 'ci', '', '', 'old run', 'suspend', 'normal', 'running', 'then', 'then', 'then', 'later', 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("opening a v25 database: %v", err)
	}
	defer s.Close()
	var ver int
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&ver); err != nil || ver != SchemaVersion {
		t.Fatalf("version %d, %v", ver, err)
	}
	if pauses, err := s.ListWorkerPauses(); err != nil || pauses["node-a"].Mode != WorkerPauseGraceful || pauses["node-a"].Revision != 3 {
		t.Fatalf("the transcode pause is kept: %+v %v", pauses, err)
	}
	var title, status string
	if err := s.db.QueryRow(`SELECT title, status FROM builds WHERE id='bld_old'`).Scan(&title, &status); err != nil || title != "old run" || status != "running" {
		t.Fatalf("the build row is kept: %q %q %v", title, status, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM build_pauses`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("nothing is builds-paused after the migration: %d %v", n, err)
	}
}
