package nodes

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func legacyDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "main.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE builds (id TEXT PRIMARY KEY, target TEXT NOT NULL, source TEXT NOT NULL, holder TEXT NOT NULL, kind TEXT NOT NULL, title TEXT NOT NULL, preempt_mode TEXT NOT NULL, priority TEXT NOT NULL DEFAULT 'normal', status TEXT NOT NULL, created_at TEXT NOT NULL, started_at TEXT NOT NULL DEFAULT '', last_heartbeat_at TEXT NOT NULL, expires_at TEXT NOT NULL, ended_at TEXT NOT NULL DEFAULT '', progress TEXT NOT NULL DEFAULT '', exit_code INTEGER, seq INTEGER NOT NULL DEFAULT 0) WITHOUT ROWID`,
		`CREATE TABLE build_pauses (worker TEXT PRIMARY KEY, since TEXT NOT NULL, updated_at TEXT NOT NULL) WITHOUT ROWID`,
		`INSERT INTO builds(id,target,source,holder,kind,title,preempt_mode,priority,status,created_at,started_at,last_heartbeat_at,expires_at,seq) VALUES('bld_old00001','nodeA','ci','runner1','gradle','old','graceful','normal','running','2026-11-13T09:00:00Z','2026-11-13T09:00:00Z','2026-11-13T09:00:00Z','2026-11-13T12:00:00Z',1)`,
		`INSERT INTO build_pauses(worker,since,updated_at) VALUES('nodeB','2026-11-13T08:00:00Z','2026-11-13T08:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestImportLegacyIsOnceAndLeavesTheOldTables(t *testing.T) {
	legacy := legacyDB(t)
	s, _ := buildSvc(t)
	ctx := context.Background()
	res, err := s.ImportLegacy(ctx, legacy)
	if err != nil || res.Builds != 1 || res.Pauses != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if b, err := s.GetBuild("bld_old00001"); err != nil || b.Title != "old" || b.Status != "running" {
		t.Fatal(b, err)
	}
	if v, err := s.Node(ctx, "nodeB"); err != nil || !v.BuildsPaused {
		t.Fatal("the pause came along", v, err)
	}
	// Second run does nothing, even after the record is pruned or finished.
	finishBuild(t, s, "bld_old00001", "succeeded")
	res, err = s.ImportLegacy(ctx, legacy)
	if err != nil || res.Skipped == "" || res.Builds != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := s.GetBuild("bld_old00001"); b.Status != "succeeded" {
		t.Fatal("not overwritten", b)
	}
	// The old table is untouched.
	db, _ := sql.Open("sqlite", legacy)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM builds WHERE status='running'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("legacy table must stay as it was", n, err)
	}
}

func TestImportLegacySkipsWhenTheRegistryHasBuilds(t *testing.T) {
	s, _ := buildSvc(t)
	startBuild(t, s, "nodeA", "graceful", 0)
	res, err := s.ImportLegacy(context.Background(), legacyDB(t))
	if err != nil || res.Skipped != "registry not empty" || res.Builds != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestImportLegacyWithoutAMainDatabase(t *testing.T) {
	s, _ := buildSvc(t)
	for _, p := range []string{"", filepath.Join(t.TempDir(), "missing.db")} {
		res, err := s.ImportLegacy(context.Background(), p)
		if err != nil || res.Skipped == "" {
			t.Fatalf("%q: %+v %v", p, res, err)
		}
	}
}
