package store

import (
	"context"
	"path/filepath"
	"testing"
)

// Every connection of the pool, not only the first, has the settings that
// keep a commit from waiting for the disk and a busy database from failing at once.
func TestEveryPooledConnectionHasTheSamePragmas(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		c, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		var sync, busy, fk int
		var mode string
		if err := c.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&sync); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if sync != 1 || busy != 5000 || mode != "wal" || fk != 1 {
			t.Fatalf("connection %d: synchronous=%d busy_timeout=%d journal_mode=%s foreign_keys=%d", i, sync, busy, mode, fk)
		}
	}
}
