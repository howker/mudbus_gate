package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

const (
	vkmMinPipe = 1
	vkmMaxPipe = 10
)

// GetVKMActivePipes returns the explicitly configured archive pipes for a
// VKM-360 device, sorted ascending. An empty result is not an error: callers
// must interpret it as the legacy/default configuration "pipe 1 only".
func (r *Repo) GetVKMActivePipes(ctx context.Context, deviceID string) ([]int, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT pipe FROM vkm_active_pipes
WHERE device_id = ?
ORDER BY pipe ASC
`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("get vkm active pipes: %w", err)
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var pipe int
		if err := rows.Scan(&pipe); err != nil {
			return nil, fmt.Errorf("scan vkm active pipe: %w", err)
		}
		out = append(out, pipe)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get vkm active pipes rows: %w", err)
	}
	return out, nil
}

func normalizeVKMPipes(pipes []int) ([]int, error) {
	if len(pipes) == 0 {
		return nil, fmt.Errorf("active VKM pipe list must not be empty")
	}
	seen := make(map[int]bool, len(pipes))
	normalized := make([]int, 0, len(pipes))
	for _, pipe := range pipes {
		if pipe < vkmMinPipe || pipe > vkmMaxPipe {
			return nil, fmt.Errorf("VKM pipe %d is outside %d..%d", pipe, vkmMinPipe, vkmMaxPipe)
		}
		if seen[pipe] {
			return nil, fmt.Errorf("VKM pipe %d is duplicated", pipe)
		}
		seen[pipe] = true
		normalized = append(normalized, pipe)
	}
	sort.Ints(normalized)
	return normalized, nil
}

func replaceVKMActivePipesTx(ctx context.Context, tx *sql.Tx, deviceID string, pipes []int) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM vkm_active_pipes WHERE device_id = ?`, deviceID); err != nil {
		return fmt.Errorf("clear vkm active pipes: %w", err)
	}
	for _, pipe := range pipes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO vkm_active_pipes (device_id, pipe) VALUES (?, ?)`, deviceID, pipe); err != nil {
			return fmt.Errorf("insert vkm active pipe %d: %w", pipe, err)
		}
	}
	return nil
}

// SetVKMActivePipes atomically replaces the active archive-pipe set for one
// VKM-360. The protocol supports pipe numbers 1..10; duplicates and values
// outside that range are rejected rather than silently normalized.
func (r *Repo) SetVKMActivePipes(ctx context.Context, deviceID string, pipes []int) error {
	if deviceID == "" {
		return fmt.Errorf("device id is empty")
	}
	normalized, err := normalizeVKMPipes(pipes)
	if err != nil {
		return err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set vkm active pipes begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := replaceVKMActivePipesTx(ctx, tx, deviceID, normalized); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set vkm active pipes commit: %w", err)
	}
	return nil
}
