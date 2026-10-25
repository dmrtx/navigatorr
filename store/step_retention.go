package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	// pollRetention keeps recent polls whole, so a person watching a job sees
	// its last hour as it happened.
	pollRetention = time.Hour
	// pruneBatch is how many rows one page reads under the store lock; a row
	// can hold a whole batch's payload, so a page is kept short.
	pruneBatch = 100
	// reclaimMinBytes and reclaimMinShare say when freed pages are worth giving
	// back to the filesystem.
	reclaimMinBytes = 64 << 20
	reclaimMinShare = 4
	// retentionEvery is how often the store tidies itself.
	retentionEvery = 6 * time.Hour
)

// CollapseOldPolls merges the poll rows that earlier versions wrote one per
// poll. Within a step, each unbroken run of waiting_external rows older than
// before becomes its last row, which takes the run's observation count and
// keeps the run's latest outputs and time. A run is cut by any other row of the
// step, or by a poll newer than before, so the record of a wait (that it
// happened, how many polls, the last thing seen) stays and only the copies go.
//
// The steps are found through the (instance, step) index, which holds neither
// the status nor the payload. Rows are then read in small keyset pages, each
// under the store lock for a moment, because an old row can hold a whole
// batch's payload and reading its status means reading past it.
func (s *Store) CollapseOldPolls(ctx context.Context, before time.Time) (int64, error) {
	type group struct {
		instance string
		step     int
	}
	var groups []group
	s.mu.Lock()
	rows, err := s.db.QueryContext(ctx, `SELECT instance_id, step_index FROM action_steps_log
		GROUP BY instance_id, step_index HAVING COUNT(*) > 1`)
	if err != nil {
		s.mu.Unlock()
		return 0, err
	}
	for rows.Next() {
		var g group
		if err := rows.Scan(&g.instance, &g.step); err != nil {
			rows.Close()
			s.mu.Unlock()
			return 0, err
		}
		groups = append(groups, g)
	}
	err = rows.Err()
	rows.Close()
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}

	cutoff := before.UTC().Format(time.RFC3339)
	type poll struct {
		id, observations int64
		old              bool
		waiting          bool
	}
	var merged int64
	for _, g := range groups {
		var pending *poll
		var after int64
		for {
			if err := ctx.Err(); err != nil {
				return merged, err
			}
			s.mu.Lock()
			page, err := s.db.QueryContext(ctx, `SELECT id, status, observations, created_at FROM action_steps_log
				WHERE instance_id=? AND step_index=? AND id>? ORDER BY id LIMIT ?`, g.instance, g.step, after, pruneBatch)
			var polls []poll
			if err == nil {
				for page.Next() {
					var p poll
					var status, created string
					if err = page.Scan(&p.id, &status, &p.observations, &created); err != nil {
						break
					}
					p.waiting = status == "waiting_external"
					p.old = created < cutoff
					polls = append(polls, p)
				}
				if err == nil {
					err = page.Err()
				}
				page.Close()
			}
			s.mu.Unlock()
			if err != nil {
				return merged, err
			}
			if len(polls) == 0 {
				break
			}
			after = polls[len(polls)-1].id
			for i := range polls {
				p := polls[i]
				if !p.waiting || !p.old {
					pending = nil
					continue
				}
				if pending != nil {
					// The earlier poll hands its count to this one and goes.
					if err := s.mergePoll(ctx, pending.id, p.id, pending.observations); err != nil {
						return merged, err
					}
					p.observations += pending.observations
					merged++
				}
				pending = &p
			}
			if len(polls) < pruneBatch {
				break
			}
			// Let a writer in between pages.
			time.Sleep(10 * time.Millisecond)
		}
	}
	return merged, nil
}

// mergePoll adds the count of poll row from to row into, then deletes from, in
// one transaction. The addition is relative, so a poll landing on into at the
// same moment is not lost.
func (s *Store) mergePoll(ctx context.Context, from, into, observations int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE action_steps_log SET observations=observations+? WHERE id=?`, observations, into); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM action_steps_log WHERE id=?`, from); err != nil {
		return err
	}
	return tx.Commit()
}

// ReclaimSpace gives free pages back to the filesystem once enough have piled
// up. Deleting rows only frees pages inside the database file; the file keeps
// its size until they are released. A database that does not yet release pages
// incrementally is converted once, by the VACUUM that this runs.
func (s *Store) ReclaimSpace(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var pageSize, pages, free, mode int64
	for query, dest := range map[string]*int64{
		`PRAGMA page_size`: &pageSize, `PRAGMA page_count`: &pages,
		`PRAGMA freelist_count`: &free, `PRAGMA auto_vacuum`: &mode,
	} {
		if err := conn.QueryRowContext(ctx, query).Scan(dest); err != nil {
			return 0, err
		}
	}
	if free*pageSize < reclaimMinBytes || free*reclaimMinShare < pages {
		return 0, nil
	}
	if mode == 0 {
		if _, err := conn.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
			return 0, err
		}
		if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
			return 0, fmt.Errorf("vacuum: %w", err)
		}
	} else if _, err := conn.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
		return 0, err
	}
	if err := checkpointTruncate(ctx, conn); err != nil {
		return 0, err
	}
	return free * pageSize, nil
}

// checkpointTruncate empties the WAL so the pages just released leave the disk.
// A reader in another process can keep it from finishing; that is reported, not
// counted as success.
func checkpointTruncate(ctx context.Context, conn *sql.Conn) error {
	var busy, logFrames, checkpointed int64
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("the log could not be emptied: another connection is reading")
	}
	return nil
}

// RunRetention tidies the store shortly after start and then every few hours,
// until ctx ends. Failures are logged and retried at the next turn.
func (s *Store) RunRetention(ctx context.Context, logf func(format string, args ...any)) {
	wait := 30 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = retentionEvery
		merged, err := s.CollapseOldPolls(ctx, time.Now().Add(-pollRetention))
		if err != nil {
			logf("step log retention: %v", err)
		} else if merged > 0 {
			logf("step log retention: merged %d repeated poll records", merged)
		}
		freed, err := s.ReclaimSpace(ctx)
		if err != nil {
			logf("step log retention: reclaiming space: %v", err)
		} else if freed > 0 {
			logf("step log retention: returned %d MiB to the filesystem", freed>>20)
		}
	}
}
