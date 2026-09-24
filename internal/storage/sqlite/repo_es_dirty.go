package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ESDirtyRange is a durable request to reconcile a local archive source
// interval with Energosphere. Version protects against a lost wake-up when
// the same source range changes again while a worker is reconciling it.
type ESDirtyRange struct {
	DeviceID string
	From     time.Time
	To       time.Time
	Version  int64
}

// InitESDirtyRangeSchema creates the durable queue used by the automatic
// insert-missing-only Energosphere healer.
func (r *Repo) InitESDirtyRangeSchema(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS es_dirty_ranges (
    device_id TEXT NOT NULL,
    from_ts   DATETIME NOT NULL,
    to_ts     DATETIME NOT NULL,
    version   INTEGER NOT NULL DEFAULT 1,
    queued_at DATETIME NOT NULL,
    PRIMARY KEY (device_id, from_ts, to_ts)
);
CREATE INDEX IF NOT EXISTS idx_es_dirty_ranges_device_time
    ON es_dirty_ranges(device_id, from_ts, to_ts);
`)
	if err != nil {
		return fmt.Errorf("init es dirty ranges schema: %w", err)
	}
	return nil
}

// enqueueESDirtyRangeTx adds (or re-dirties) one source range in the same
// SQLite transaction that changed the archive row. That removes the crash
// window "archive committed but repair marker lost".
func enqueueESDirtyRangeTx(ctx context.Context, tx *sql.Tx, deviceID string, from, to time.Time) error {
	if deviceID == "" {
		return fmt.Errorf("enqueue es dirty range: empty device id")
	}
	if to.Before(from) {
		from, to = to, from
	}

	mergedFrom := from
	mergedTo := to
	nextVersion := int64(1)
	hadOverlap := false

	// Keep the durable queue compact: backfill/re-poll usually writes many
	// neighboring periods. Merge every range that overlaps or touches this
	// one, instead of leaving thousands of one-period rows after an ES outage.
	rows, err := tx.QueryContext(ctx, `
SELECT from_ts, to_ts, version
FROM es_dirty_ranges
WHERE device_id = ? AND to_ts >= ? AND from_ts <= ?
`, deviceID, from, to)
	if err != nil {
		return fmt.Errorf("find overlapping es dirty ranges: %w", err)
	}
	for rows.Next() {
		var existingFrom, existingTo time.Time
		var version int64
		if err := rows.Scan(&existingFrom, &existingTo, &version); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan overlapping es dirty range: %w", err)
		}
		hadOverlap = true
		if existingFrom.Before(mergedFrom) {
			mergedFrom = existingFrom
		}
		if existingTo.After(mergedTo) {
			mergedTo = existingTo
		}
		if version >= nextVersion {
			nextVersion = version + 1
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	if hadOverlap {
		if _, err := tx.ExecContext(ctx, `
DELETE FROM es_dirty_ranges
WHERE device_id = ? AND to_ts >= ? AND from_ts <= ?
`, deviceID, mergedFrom, mergedTo); err != nil {
			return fmt.Errorf("merge es dirty ranges: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO es_dirty_ranges (device_id, from_ts, to_ts, version, queued_at)
VALUES (?, ?, ?, ?, ?)
`, deviceID, mergedFrom, mergedTo, nextVersion, time.Now()); err != nil {
		return fmt.Errorf("enqueue es dirty range: %w", err)
	}
	return nil
}

// ListESDirtyRanges returns oldest pending ranges for one device.
func (r *Repo) ListESDirtyRanges(ctx context.Context, deviceID string, limit int) ([]ESDirtyRange, error) {
	if limit <= 0 {
		limit = 256
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT device_id, from_ts, to_ts, version
FROM es_dirty_ranges
WHERE device_id = ?
ORDER BY from_ts, to_ts
LIMIT ?
`, deviceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list es dirty ranges: %w", err)
	}
	defer rows.Close()

	out := make([]ESDirtyRange, 0)
	for rows.Next() {
		var dr ESDirtyRange
		if err := rows.Scan(&dr.DeviceID, &dr.From, &dr.To, &dr.Version); err != nil {
			return nil, fmt.Errorf("scan es dirty range: %w", err)
		}
		out = append(out, dr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// CompleteESDirtyRange deletes a reconciled range only if it has not been
// re-dirtied since the worker read it. If Version changed, the row stays
// queued for another pass.
func (r *Repo) CompleteESDirtyRange(ctx context.Context, dr ESDirtyRange) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
DELETE FROM es_dirty_ranges
WHERE device_id = ? AND from_ts = ? AND to_ts = ? AND version = ?
`, dr.DeviceID, dr.From, dr.To, dr.Version)
	if err != nil {
		return false, fmt.Errorf("complete es dirty range: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("complete es dirty range rows affected: %w", err)
	}
	return n == 1, nil
}

// CountESDirtyRanges is primarily diagnostic/test support.
func (r *Repo) CountESDirtyRanges(ctx context.Context, deviceID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM es_dirty_ranges WHERE (? = '' OR device_id = ?)
`, deviceID, deviceID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count es dirty ranges: %w", err)
	}
	return n, nil
}

// ESDirtySignal returns a process-local buffered wake-up channel for one
// device. The durable queue remains the source of truth; losing/coalescing
// wake-ups is harmless because the pending row stays in SQLite.
func (r *Repo) ESDirtySignal(deviceID string) <-chan struct{} {
	r.esDirtyMu.Lock()
	defer r.esDirtyMu.Unlock()
	if r.esDirtySignals == nil {
		r.esDirtySignals = make(map[string]chan struct{})
	}
	ch := r.esDirtySignals[deviceID]
	if ch == nil {
		ch = make(chan struct{}, 1)
		r.esDirtySignals[deviceID] = ch
	}
	return ch
}

func (r *Repo) notifyESDirty(deviceID string) {
	r.esDirtyMu.Lock()
	if r.esDirtySignals == nil {
		r.esDirtySignals = make(map[string]chan struct{})
	}
	ch := r.esDirtySignals[deviceID]
	if ch == nil {
		ch = make(chan struct{}, 1)
		r.esDirtySignals[deviceID] = ch
	}
	r.esDirtyMu.Unlock()

	select {
	case ch <- struct{}{}:
	default:
	}
}
