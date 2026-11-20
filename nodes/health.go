package nodes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Healthy reports whether the registry can serve: its database is open and its
// meta table can be read. It takes no registry lock (a long write must not make
// a liveness probe wait) and changes nothing. An error means "do not send me
// traffic"; a worker still fails open on it (N3).
func (s *Service) Healthy(ctx context.Context) error {
	if s == nil || s.reg == nil || s.reg.db == nil {
		return errors.New("nodes registry is not open")
	}
	if err := s.reg.db.PingContext(ctx); err != nil {
		return fmt.Errorf("nodes database: %w", err)
	}
	var key string
	if err := s.reg.db.QueryRowContext(ctx, `SELECT key FROM meta LIMIT 1`).Scan(&key); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("nodes database meta table: %w", err)
	}
	return nil
}
