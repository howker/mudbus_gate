package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// InitESSyncCursorSchema creates the per-(device, point) cursor used by
// the ordinary ЭС sync loop. The cursor stores only CONFIRMED progress:
// a timestamp is written here only after the corresponding point is known
// to exist in PointMains (already existed, was inserted successfully, or
// a duplicate-key insert proved another writer had already created it).
//
// ForceResyncRange deliberately does not use or advance this cursor.
func (r *Repo) InitESSyncCursorSchema(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS es_sync_cursor (
    device_id  TEXT NOT NULL,
    point_id   INTEGER NOT NULL,
    last_ts    DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    PRIMARY KEY (device_id, point_id)
);
`)
	if err != nil {
		return fmt.Errorf("init es sync cursor schema: %w", err)
	}
	return nil
}

// GetESSyncCursor returns the last confirmed timestamp for one configured
// ЭС point. found=false is the normal first-run state.
func (r *Repo) GetESSyncCursor(ctx context.Context, deviceID string, pointID int) (last time.Time, found bool, err error) {
	err = r.db.QueryRowContext(ctx, `
SELECT last_ts
FROM es_sync_cursor
WHERE device_id = ? AND point_id = ?
`, deviceID, pointID).Scan(&last)

	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("get es sync cursor: %w", err)
	}
	return last, true, nil
}

// SetESSyncCursor advances the confirmed cursor for one configured ЭС
// point. It is monotonic: an older timestamp can never move the cursor
// backwards, which protects ordinary sync from accidental rewind after a
// retry or out-of-order call.
func (r *Repo) SetESSyncCursor(ctx context.Context, deviceID string, pointID int, ts time.Time) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO es_sync_cursor (device_id, point_id, last_ts, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(device_id, point_id) DO UPDATE SET
    last_ts = CASE
        WHEN excluded.last_ts > es_sync_cursor.last_ts THEN excluded.last_ts
        ELSE es_sync_cursor.last_ts
    END,
    updated_at = excluded.updated_at
`, deviceID, pointID, ts, time.Now())
	if err != nil {
		return fmt.Errorf("set es sync cursor: %w", err)
	}
	return nil
}
