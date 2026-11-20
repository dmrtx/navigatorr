//go:build linux || darwin

package nodes

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// importedDB is a clean nodes.db carrying the legacy_import mark (or not).
func importedDB(t *testing.T, mark bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nodes.db")
	s, err := Open(Options{Path: path, Topology: fakeTopology{}})
	if err != nil {
		t.Fatal(err)
	}
	if mark {
		if _, err := s.reg.db.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)`, metaLegacyImport, "2 builds, 1 pauses"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if exists(path + suffix) {
			t.Fatalf("a clean Close left %s%s", path, suffix)
		}
	}
	return path
}

// leftoverWAL is a copy of a database whose last process died with rows still
// in the WAL: the main file does not hold the row "pending", the -wal does.
func leftoverWAL(t *testing.T) string {
	t.Helper()
	src := importedDB(t, true)
	db, err := sql.Open("sqlite", src+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES('pending', 'row')`); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "nodes.db")
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(src + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst+suffix, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func problems(rep CheckReport) string { return strings.Join(rep.Problems, " | ") }

func TestCheckOkOnACleanDatabase(t *testing.T) {
	path := importedDB(t, true)
	rep, err := Check(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Ok() || rep.Integrity != "ok" || !rep.LegacyImportSet || rep.LegacyImport != "2 builds, 1 pauses" {
		t.Fatalf("report %+v", rep)
	}
	if rep.WALPresent || rep.SHMPresent {
		t.Errorf("clean database reported wal=%v shm=%v", rep.WALPresent, rep.SHMPresent)
	}
	if rep.FileUID != os.Getuid() || rep.DirUID != os.Getuid() {
		t.Errorf("owners %d/%d, want %d", rep.FileUID, rep.DirUID, os.Getuid())
	}
	if rep.FileMode == 0 || rep.DirMode == 0 {
		t.Errorf("modes %v/%v", rep.FileMode, rep.DirMode)
	}
	// The check leaves nothing behind and releases the lock.
	if exists(path+"-wal") || exists(path+"-shm") {
		t.Error("the check left a -wal or -shm file")
	}
	s, err := Open(Options{Path: path, Topology: fakeTopology{}})
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	s.Close()
}

func TestCheckFailsWhileTheLockIsHeld(t *testing.T) {
	path := importedDB(t, true)
	s, err := Open(Options{Path: path, Topology: fakeTopology{}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Check(context.Background(), path, true); err == nil || !strings.Contains(err.Error(), "held by another process") {
		t.Fatalf("want a lock-held error, got %v", err)
	}
}

func TestCheckDoesNotCreateTheDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.db")
	if _, err := Check(context.Background(), path, false); err == nil {
		t.Fatal("a missing database was accepted")
	}
	if exists(path) || exists(path+".lock") {
		t.Error("the check created files")
	}
	if _, err := Check(context.Background(), "", false); err == nil {
		t.Fatal("an empty path was accepted")
	}
}

func TestCheckWALWithoutCheckpointFailsAndChangesNothing(t *testing.T) {
	path := leftoverWAL(t)
	before, _ := os.ReadFile(path + "-wal")
	rep, err := Check(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ok() || !rep.WALPresent || !strings.Contains(problems(rep), "-wal") {
		t.Fatalf("report %+v", rep)
	}
	if after, _ := os.ReadFile(path + "-wal"); string(after) != string(before) {
		t.Error("the check without --checkpoint changed the -wal file")
	}
}

func TestCheckCheckpointFoldsTheWALIn(t *testing.T) {
	path := leftoverWAL(t)
	rep, err := Check(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Ok() || !rep.WALPresent || !rep.Checkpointed || rep.Integrity != "ok" {
		t.Fatalf("report %+v", rep)
	}
	if exists(path + "-wal") {
		t.Error("the -wal file survived the checkpoint and the close")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='pending'`).Scan(&v); err != nil || v != "row" {
		t.Fatalf("the row that lived in the WAL is gone: %q, %v", v, err)
	}
}

func TestCheckCheckpointOnACleanDatabaseIsFine(t *testing.T) {
	path := importedDB(t, true)
	rep, err := Check(context.Background(), path, true)
	if err != nil || !rep.Ok() {
		t.Fatalf("%v %+v", err, rep)
	}
}

func TestCheckMissingLegacyImport(t *testing.T) {
	path := importedDB(t, false)
	rep, err := Check(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ok() || rep.LegacyImportSet || !strings.Contains(problems(rep), metaLegacyImport) {
		t.Fatalf("report %+v", rep)
	}
	if rep.Integrity != "ok" {
		t.Errorf("integrity %q", rep.Integrity)
	}
}

func TestCheckCorruptDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.db")
	if err := os.WriteFile(path, []byte(strings.Repeat("this is not a database ", 400)), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := Check(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ok() || rep.Integrity == "ok" || !strings.Contains(problems(rep), "integrity_check") {
		t.Fatalf("report %+v", rep)
	}
}

func TestCheckCorruptPagesInAValidFile(t *testing.T) {
	path := importedDB(t, true)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the header, scramble the rest: the file still opens as a database.
	for i := 100; i < len(b); i++ {
		b[i] = byte(i * 7)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := Check(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ok() {
		t.Fatalf("a scrambled database passed: %+v", rep)
	}
}

// Without --checkpoint the check only reads: a read-only database passes and
// nothing is written next to it.
func TestCheckWithoutCheckpointWorksOnAReadOnlyFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	path := importedDB(t, true)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Check(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Ok() || rep.Integrity != "ok" || !rep.LegacyImportSet {
		t.Fatalf("report %+v: %s", rep, problems(rep))
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the check changed the database file")
	}
	if exists(path+"-wal") || exists(path+"-shm") {
		t.Error("the check left a -wal or -shm file")
	}
}

// The lock is released on every way out of Check, not only on success.
func TestCheckReleasesTheLockOnFailures(t *testing.T) {
	for name, path := range map[string]string{
		"wal without checkpoint": leftoverWAL(t),
		"missing legacy_import":  importedDB(t, false),
	} {
		rep, err := Check(context.Background(), path, false)
		if err == nil && rep.Ok() {
			t.Fatalf("%s: the check passed", name)
		}
		lp, err := lockPath(path)
		if err != nil {
			t.Fatal(err)
		}
		lk, err := lockFile(lp)
		if err != nil {
			t.Errorf("%s: the lock is still held: %v", name, err)
			continue
		}
		unlock(lk)
	}
}

func TestCheckAcceptsAPathWithALeadingDoubleSlash(t *testing.T) {
	path := importedDB(t, true)
	if !filepath.IsAbs(path) {
		t.Skip("needs an absolute temp path")
	}
	rep, err := Check(context.Background(), "/"+path, false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Ok() || rep.Integrity != "ok" {
		t.Fatalf("report %+v: %s", rep, problems(rep))
	}
}
