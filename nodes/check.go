package nodes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// CheckReport is what Check found in a nodes.db that no process has open.
type CheckReport struct {
	Path string
	// Owner and mode of the database file and of its directory. The uid is -1
	// where the platform cannot say.
	FileUID, DirUID   int
	FileMode, DirMode os.FileMode
	// WALPresent and SHMPresent say whether the -wal and -shm files existed
	// before the check touched anything.
	WALPresent, SHMPresent bool
	// Checkpointed is true when the WAL was folded into the database
	// (wal_checkpoint(TRUNCATE)) and nothing was left busy.
	Checkpointed bool
	// Integrity is "ok" or what PRAGMA integrity_check said; empty when it was
	// not run.
	Integrity string
	// LegacyImport is the value of the legacy_import mark, when LegacyImportSet.
	LegacyImport    string
	LegacyImportSet bool
	// Problems is every reason the database is not ready to be served; empty
	// when it is.
	Problems []string
}

// Ok reports that the check found nothing wrong.
func (r CheckReport) Ok() bool { return len(r.Problems) == 0 }

// Check inspects the registry database at path while no Nodes process serves it:
// it takes the process lock without waiting (an error if another process holds
// it, so a running Nodes is never inspected underneath), optionally folds the
// WAL into the database, runs the integrity check and reads the legacy_import
// mark. It releases the lock before returning.
//
// An error means the check could not run (no path, no database file, lock held).
// Anything wrong with the database itself is in Problems: a -wal file left
// behind without checkpoint (the database is then not opened at all, so the
// check changes nothing), a failed checkpoint, a failed integrity check, a
// missing legacy_import mark. Check never creates the database.
func Check(ctx context.Context, path string, checkpoint bool) (CheckReport, error) {
	rep := CheckReport{Path: path, FileUID: -1, DirUID: -1}
	if path == "" {
		return rep, errors.New("nodes database path is required")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return rep, fmt.Errorf("nodes database: %w", err)
	}
	if fi.IsDir() {
		return rep, fmt.Errorf("nodes database %s is a directory", path)
	}
	lp, err := lockPath(path)
	if err != nil {
		return rep, err
	}
	lock, err := lockFile(lp)
	if err != nil {
		return rep, err
	}
	defer unlock(lock)

	rep.FileUID, rep.FileMode = ownerOf(fi)
	if di, err := os.Stat(dirOf(path)); err == nil {
		rep.DirUID, rep.DirMode = ownerOf(di)
	}
	rep.WALPresent = exists(path + "-wal")
	rep.SHMPresent = exists(path + "-shm")

	if rep.WALPresent && !checkpoint {
		rep.Problems = append(rep.Problems, path+"-wal exists: the last process did not close the database cleanly; run check --checkpoint")
		return rep, nil
	}

	// Without --checkpoint the check only reads: the file is opened read-only
	// and immutable (no -wal exists and we hold the lock, so nothing can change
	// it), so a read-only file passes and nothing is created next to it.
	params := "?mode=ro&immutable=1"
	if checkpoint {
		params = "?mode=rw&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+params)
	if err != nil {
		rep.Problems = append(rep.Problems, "opening: "+err.Error())
		return rep, nil
	}
	db.SetMaxOpenConns(1)
	// The connection closes before the lock is released (defers run last in,
	// first out), so a clean close can drop the -wal and -shm files.
	defer db.Close()

	if checkpoint {
		var busy, logFrames, done int
		if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done); err != nil {
			rep.Problems = append(rep.Problems, "checkpoint: "+err.Error())
		} else if busy != 0 {
			rep.Problems = append(rep.Problems, "checkpoint: the WAL could not be folded in completely")
		} else {
			rep.Checkpointed = true
		}
	}

	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		rep.Integrity = err.Error()
	} else {
		var msgs []string
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				msgs = append(msgs, err.Error())
				break
			}
			msgs = append(msgs, m)
		}
		if err := rows.Err(); err != nil {
			msgs = append(msgs, err.Error())
		}
		rows.Close()
		rep.Integrity = strings.Join(msgs, "; ")
	}
	if rep.Integrity != "ok" {
		rep.Problems = append(rep.Problems, "integrity_check: "+rep.Integrity)
		// A database that cannot be read cannot say what it imported.
		return rep, nil
	}

	var v string
	switch err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaLegacyImport).Scan(&v); {
	case err == nil:
		rep.LegacyImport, rep.LegacyImportSet = v, true
	case errors.Is(err, sql.ErrNoRows):
		rep.Problems = append(rep.Problems, "the "+metaLegacyImport+" mark is missing: the builds of the main database were never imported here")
	default:
		rep.Problems = append(rep.Problems, "reading the "+metaLegacyImport+" mark: "+err.Error())
	}
	return rep, nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
