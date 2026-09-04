package sqlite

import (
	"context"
	"fmt"
)

// Ping verifies that the SQLite handle can still execute through its
// underlying connection. It is intentionally read-only and lightweight.
func (r *Repo) Ping(ctx context.Context) error {
	if err := r.db.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlite ping: %w", err)
	}
	return nil
}
