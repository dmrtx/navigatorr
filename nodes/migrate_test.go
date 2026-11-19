package nodes

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func legacyDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "main.db")
	writeLegacyDB(t, path)
	return path
}

const legacyBuildsDDL = `CREATE TABLE builds (id TEXT PRIMARY KEY, target TEXT NOT NULL, source TEXT NOT NULL, holder TEXT NOT NULL, kind TEXT NOT NULL, title TEXT NOT NULL, preempt_mode TEXT NOT NULL, priority TEXT NOT NULL DEFAULT 'normal', status TEXT NOT NULL, created_at TEXT NOT NULL, started_at TEXT NOT NULL DEFAULT '', last_heartbeat_at TEXT NOT NULL, expires_at TEXT NOT NULL, ended_at TEXT NOT NULL DEFAULT '', progress TEXT NOT NULL DEFAULT '', exit_code INTEGER, seq INTEGER NOT NULL DEFAULT 0) WITHOUT ROWID`

// writeLegacyDB writes the old main-database tables with a running build, a
// waiting one, two ended ones (one with an exit code) and a builds pause.
func writeLegacyDB(t *testing.T, path string, extra ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const cols = `id,target,source,holder,kind,title,preempt_mode,priority,status,created_at,started_at,last_heartbeat_at,expires_at,ended_at,progress,exit_code,seq`
	stmts := []string{
		legacyBuildsDDL,
		`CREATE TABLE build_pauses (worker TEXT PRIMARY KEY, since TEXT NOT NULL, updated_at TEXT NOT NULL) WITHOUT ROWID`,
		`INSERT INTO builds(` + cols + `) VALUES('bld_old00001','nodeA','ci','runner1','gradle','old','graceful','normal','running','2026-11-13T09:00:00Z','2026-11-13T09:00:00Z','2026-11-13T09:00:00Z','2026-11-13T12:00:00Z','','',NULL,1)`,
		`INSERT INTO builds(` + cols + `) VALUES('bld_wait0001','nodeB','ci','runner2','gradle','queued','graceful','low','waiting','2026-11-13T09:10:00Z','','2026-11-13T09:10:00Z','2026-11-13T12:00:00Z','','',NULL,2)`,
		`INSERT INTO builds(` + cols + `) VALUES('bld_done0001','nodeA','ci','runner1','gradle','done','suspend','normal','succeeded','2026-11-13T08:00:00Z','2026-11-13T08:00:00Z','2026-11-13T08:05:00Z','2026-11-13T09:00:00Z','2026-11-13T08:06:00Z','',0,3)`,
		`INSERT INTO builds(` + cols + `) VALUES('bld_fail0001','nodeA','ci','runner1','gradle','broke','suspend','normal','failed','2026-11-13T08:10:00Z','2026-11-13T08:10:00Z','2026-11-13T08:15:00Z','2026-11-13T09:00:00Z','2026-11-13T08:16:00Z','compile error',3,4)`,
		`INSERT INTO build_pauses(worker,since,updated_at) VALUES('nodeB','2026-11-13T08:00:00Z','2026-11-13T08:30:00Z')`,
	}
	for _, q := range append(stmts, extra...) {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestImportLegacyIsOnceAndLeavesTheOldTables(t *testing.T) {
	legacy := legacyDB(t)
	s, _ := buildSvc(t)
	ctx := context.Background()
	res, err := s.ImportLegacy(ctx, legacy)
	if err != nil || res.Builds != 4 || res.Pauses != 1 {
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

func TestImportLegacyWithoutAMainDatabase(t *testing.T) {
	s, _ := buildSvc(t)
	missing := filepath.Join(t.TempDir(), "missing.db")
	for _, p := range []string{"", missing} {
		res, err := s.ImportLegacy(context.Background(), p)
		if err != nil || res.Skipped == "" {
			t.Fatalf("%q: %+v %v", p, res, err)
		}
	}
	// A path that names nothing never creates a database file, and it leaves
	// the flag unset so a main database that shows up later is still imported.
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing legacy database must not be created: %v", err)
	}
	if _, ok, _ := s.reg.metaGet(metaLegacyImport); ok {
		t.Fatal("no flag when there was nothing to read")
	}
}

// The import is decided by its flag alone. A first import that fails is logged
// and retried; builds and pauses the registry took in the meantime stay as they
// are (the registry's own row wins) and the legacy rows arrive next to them,
// the pause with its original start time.
func TestImportLegacyFailedThenRetriedAfterTheRegistryWasWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.db")
	// A builds table that lacks columns: reading it fails.
	writeBadLegacy(t, path)
	s, _ := buildSvc(t)
	ctx := context.Background()
	if _, err := s.ImportLegacy(ctx, path); err == nil {
		t.Fatal("a legacy database that cannot be read is an error")
	}
	if _, ok, _ := s.reg.metaGet(metaLegacyImport); ok {
		t.Fatal("a failed import must not set the flag")
	}
	// The registry is used meanwhile: a build and an operator pause on nodeB.
	own := startBuild(t, s, "nodeA", "graceful", 0)
	pauseBuilds(t, s, "nodeB", true)
	ownSince := pausedSince(t, s, "nodeB")

	// The main database is fixed (here: rewritten) and the next start retries.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeLegacyDB(t, path)
	res, err := s.ImportLegacy(ctx, path)
	if err != nil || res.Builds != 4 || res.Skipped != "" {
		t.Fatalf("the retry imports even though the registry is not empty: %+v %v", res, err)
	}
	for _, id := range []string{"bld_old00001", "bld_wait0001", "bld_done0001", "bld_fail0001", own.ID} {
		if _, err := s.GetBuild(id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	// nodeB was paused by the operator in the meantime: that row stays.
	if got := pausedSince(t, s, "nodeB"); got != ownSince {
		t.Fatalf("the registry's own pause is kept: %s, want %s", got, ownSince)
	}
	// The main database changes after the import (a new pause): not read again.
	db, _ := sql.Open("sqlite", path)
	if _, err := db.Exec(`INSERT INTO build_pauses(worker,since,updated_at) VALUES('nodeA','2026-11-13T07:00:00Z','2026-11-13T07:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if res, _ := s.ImportLegacy(ctx, path); res.Skipped == "" {
		t.Fatal("imported once, never again")
	}
	if got, _ := s.BuildsPauses(); got["nodeA"].Since != "" {
		t.Fatal("not re-imported", got)
	}
	// Insertion order stays unique: seq never repeats.
	rows, err := s.reg.db.Query(`SELECT COUNT(*), COUNT(DISTINCT seq) FROM builds`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n, d int
	rows.Next()
	if err := rows.Scan(&n, &d); err != nil || n != d {
		t.Fatalf("seq must be unique: %d rows, %d distinct: %v", n, d, err)
	}
}

// A legacy-only pause arrives with the since it had.
func TestImportLegacyPauseKeepsItsSince(t *testing.T) {
	s, _ := buildSvc(t)
	if _, err := s.ImportLegacy(context.Background(), legacyDB(t)); err != nil {
		t.Fatal(err)
	}
	if got := pausedSince(t, s, "nodeB"); got != "2026-11-13T08:00:00Z" {
		t.Fatal("since is the legacy one", got)
	}
}

func pausedSince(t *testing.T, s *Service, node string) string {
	t.Helper()
	v, err := s.Node(context.Background(), node)
	if err != nil || !v.BuildsPaused {
		t.Fatalf("%s is not paused: %+v %v", node, v, err)
	}
	return v.BuildsPausedSince
}

func writeBadLegacy(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE builds (id TEXT PRIMARY KEY, target TEXT)`); err != nil {
		t.Fatal(err)
	}
}

// Once imported, a registry pruned down to nothing is not filled again.
func TestImportLegacyPrunedRegistryIsNotReimported(t *testing.T) {
	legacy := legacyDB(t)
	s, clock := buildSvc(t)
	ctx := context.Background()
	if _, err := s.ImportLegacy(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	// Everything ends, then the retention passes and the tick prunes it all.
	tick(s) // the waiting row is cancelled
	for _, id := range []string{"bld_old00001"} {
		finishBuild(t, s, id, "succeeded")
	}
	clock.advance(DefaultRetention + 24*time.Hour)
	tick(s)
	if list, _ := s.ListBuilds(BuildFilter{Limit: -1}); len(list) != 0 {
		t.Fatalf("everything pruned: %+v", list)
	}
	res, err := s.ImportLegacy(ctx, legacy)
	if err != nil || res.Skipped == "" || res.Builds != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if list, _ := s.ListBuilds(BuildFilter{Limit: -1}); len(list) != 0 {
		t.Fatalf("nothing came back: %+v", list)
	}
}

// Old waiting rows are imported as they were and cancelled by the service's own
// housekeeping, with the reason; finished rows keep their exit code.
func TestImportLegacyWaitingRowsAreCancelledByReconcile(t *testing.T) {
	s, _ := buildSvc(t)
	if _, err := s.ImportLegacy(context.Background(), legacyDB(t)); err != nil {
		t.Fatal(err)
	}
	if b, err := s.GetBuild("bld_wait0001"); err != nil || b.Status != BuildWaiting {
		t.Fatalf("imported as it was: %+v %v", b, err)
	}
	tick(s)
	b, err := s.GetBuild("bld_wait0001")
	if err != nil || b.Status != BuildCancelled || b.Progress != LegacyWaitingReason || b.EndedAt == "" {
		t.Fatalf("Reconcile cancels it with the reason: %+v %v", b, err)
	}
	if run, _ := s.GetBuild("bld_old00001"); run.Status != BuildRunning {
		t.Fatalf("a running row is left alone: %+v", run)
	}
	done, _ := s.GetBuild("bld_done0001")
	if done.Status != BuildSucceeded || done.ExitCode == nil || *done.ExitCode != 0 || done.EndedAt != "2026-11-13T08:06:00Z" {
		t.Fatalf("exit code 0 is kept: %+v", done)
	}
	fail, _ := s.GetBuild("bld_fail0001")
	if fail.Status != BuildFailed || fail.ExitCode == nil || *fail.ExitCode != 3 || fail.Progress != "compile error" {
		t.Fatalf("exit code 3 is kept: %+v", fail)
	}
	if run, _ := s.GetBuild("bld_old00001"); run.ExitCode != nil {
		t.Fatalf("no exit code on a running row: %+v", run)
	}
}

// Two services on one nodes.db (two processes) importing at once import once;
// what is pruned or resumed afterwards is never brought back by the loser or by
// a later call.
func TestImportLegacyConcurrentImportsRunOnce(t *testing.T) {
	legacy := legacyDB(t)
	path := filepath.Join(t.TempDir(), "nodes.db")
	a, _ := buildSvcAt(t, path)
	b, _ := buildSvcAt(t, path)
	ctx := context.Background()

	results := make([]ImportResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, s := range []*Service{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.ImportLegacy(ctx, legacy)
		}()
	}
	wg.Wait()
	imported := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("import %d: %v", i, errs[i])
		}
		if results[i].Skipped == "" {
			imported++
			if results[i].Builds != 4 || results[i].Pauses != 1 {
				t.Fatalf("the import that ran copies everything: %+v", results[i])
			}
		} else if results[i].Builds != 0 || results[i].Pauses != 0 {
			t.Fatalf("a skipped import copies nothing: %+v", results[i])
		}
	}
	if imported != 1 {
		t.Fatalf("exactly one of two concurrent imports runs, got %d: %+v", imported, results)
	}

	// An operator resumes nodeB and a build is pruned; a second import (even
	// from a service that never saw the flag) brings neither back.
	if err := a.SetBuildsPause(ctx, "nodeB", false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.reg.db.Exec(`DELETE FROM builds WHERE id='bld_done0001'`); err != nil {
		t.Fatal(err)
	}
	c, _ := buildSvcAt(t, path)
	res, err := c.ImportLegacy(ctx, legacy)
	if err != nil || res.Skipped == "" || res.Builds != 0 || res.Pauses != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := c.GetBuild("bld_done0001"); err == nil {
		t.Fatal("a pruned build must not come back")
	}
	if p, _ := c.BuildsPauses(); len(p) != 0 {
		t.Fatalf("a resumed node must not be paused again: %+v", p)
	}
}

// A retry after the registry was written to: the registry's own builds are the
// newest, the legacy ones follow oldest-last, and seq stays unique and >= 1.
func TestImportLegacyRetryKeepsTheRegistrysBuildsNewest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.db")
	writeBadLegacy(t, path)
	s, _ := buildSvc(t)
	ctx := context.Background()
	if _, err := s.ImportLegacy(ctx, path); err == nil {
		t.Fatal("the first import fails")
	}
	own := startBuild(t, s, "nodeA", "graceful", 0)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeLegacyDB(t, path)
	if res, err := s.ImportLegacy(ctx, path); err != nil || res.Builds != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	page, err := s.PageBuilds(BuildFilter{}, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	// Newest first: the own build, then the legacy ones by created_at, newest
	// first (waiting 09:10, old 09:00, fail 08:10, done 08:00).
	want := []string{own.ID, "bld_wait0001", "bld_old00001", "bld_fail0001", "bld_done0001"}
	if len(page.Builds) != len(want) {
		t.Fatalf("%d builds, want %d", len(page.Builds), len(want))
	}
	for i, b := range page.Builds {
		if b.ID != want[i] {
			t.Fatalf("position %d: %s, want %s", i, b.ID, want[i])
		}
	}
	var n, d, lo int
	if err := s.reg.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT seq), MIN(seq) FROM builds`).Scan(&n, &d, &lo); err != nil {
		t.Fatal(err)
	}
	if n != d || lo < 1 {
		t.Fatalf("seq must be unique and >= 1: %d rows, %d distinct, min %d", n, d, lo)
	}
}
