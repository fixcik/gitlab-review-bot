package store

import "context"

// Ping checks that the database answers (readiness probe).
func (s *SQLite) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return err
	}
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}
