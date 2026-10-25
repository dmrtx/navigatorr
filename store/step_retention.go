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
	// pruneBatch bounds one delete: a row can hold a whole batch's payload, so
	// a batch holds the store lock for a moment and no longer.
	pruneBatch = 100
	// reclaimMinBytes and reclaimMinShare say when freed pages are worth giving
	// back to the filesystem.
	reclaimMinBytes = 64 << 20
	reclaimMinShare = 4
	// retentionEvery is how often the store tidies itself.
	retentionEvery = 6 * time.Hour
)

// PruneSupersededPolls deletes the poll rows of waiting steps that a later row
// of the same step has replaced and that are older than before. The last row of
// every step stays: it is the step's latest observation, the only one the job
// pages read. Rows with any other status stay whatever their age.
//
// The groups are found through the (instance, step) index, which holds neither
// the status nor the payload, so the scan does not read the large rows.
func (s *Store) PruneSupersededPolls(ctx context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	rows, err := s.db.QueryContext(ctx, `SELECT instance_id, step_index, MAX(id) FROM action_steps_log
		GROUP BY instance_id, step_index HAVING COUNT(*) > 1`)
	if err != nil {
		s.mu.Unlock()
		return 0, err
	}
	type group struct {
		instance string
		step     int
		last     int64
	}
	var groups []group
	for rows.Next() {
		var g group
		if err := rows.Scan(&g.instance, &g.step, &g.last); err != nil {
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
	var deleted int64
	for _, g := range groups {
		for {
			if err := ctx.Err(); err != nil {
				return deleted, err
			}
			s.mu.Lock()
			res, err := s.db.ExecContext(ctx, `DELETE FROM action_steps_log WHERE id IN (
					SELECT id FROM action_steps_log
					WHERE instance_id=? AND step_index=? AND id<? AND status='waiting_external' AND created_at<?
					LIMIT ?)`, g.instance, g.step, g.last, cutoff, pruneBatch)
			s.mu.Unlock()
			if err != nil {
				return deleted, err
			}
			n, _ := res.RowsAffected()
			deleted += n
			if n < pruneBatch {
				break
			}
			// Let a writer in between batches.
			time.Sleep(10 * time.Millisecond)
		}
	}
	return deleted, nil
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

func checkpointTruncate(ctx context.Context, conn *sql.Conn) error {
	var busy, logFrames, checkpointed int64
	return conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed)
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
		deleted, err := s.PruneSupersededPolls(ctx, time.Now().Add(-pollRetention))
		if err != nil {
			logf("step log retention: %v", err)
		} else if deleted > 0 {
			logf("step log retention: removed %d superseded poll records", deleted)
		}
		freed, err := s.ReclaimSpace(ctx)
		if err != nil {
			logf("step log retention: reclaiming space: %v", err)
		} else if freed > 0 {
			logf("step log retention: returned %d MiB to the filesystem", freed>>20)
		}
	}
}
