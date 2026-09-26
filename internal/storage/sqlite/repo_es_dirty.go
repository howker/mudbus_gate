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

const esDirtyMaxRangeDuration = 6 * time.Hour

// ESDirtyPointFailure isolates one ES point that could not be checked/inserted
// from the source range that produced it. The range can then be completed
// without making every healthy point behind the same range retry forever.
type ESDirtyPointFailure struct {
	DeviceID  string
	PointID   int
	Ts        time.Time
	Value     float64
	State     int
	Attempts  int
	LastError string
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

CREATE TABLE IF NOT EXISTS es_dirty_point_failures (
    device_id TEXT NOT NULL,
    point_id  INTEGER NOT NULL,
    ts        DATETIME NOT NULL,
    value     REAL NOT NULL,
    state     INTEGER NOT NULL DEFAULT 0,
    attempts  INTEGER NOT NULL DEFAULT 1,
    first_error_at DATETIME NOT NULL,
    last_error_at  DATETIME NOT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (device_id, point_id, ts)
);
CREATE INDEX IF NOT EXISTS idx_es_dirty_point_failures_device_time
    ON es_dirty_point_failures(device_id, ts);
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
	if to.Equal(from) {
		to = from.Add(time.Minute)
	}

	// Do not merge merely adjacent periods. Keeping bounded chunks prevents one
	// persistent ES error from turning the queue into an ever-growing range.
	for chunkFrom := from; chunkFrom.Before(to); {
		chunkTo := chunkFrom.Add(esDirtyMaxRangeDuration)
		if chunkTo.After(to) {
			chunkTo = to
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO es_dirty_ranges (device_id, from_ts, to_ts, version, queued_at)
VALUES (?, ?, ?, 1, ?)
ON CONFLICT(device_id, from_ts, to_ts) DO UPDATE SET
    version = es_dirty_ranges.version + 1,
    queued_at = excluded.queued_at
`, deviceID, chunkFrom, chunkTo, time.Now()); err != nil {
			return fmt.Errorf("enqueue es dirty range: %w", err)
		}
		chunkFrom = chunkTo
	}
	return nil
}

// splitOversizedESDirtyRanges upgrades old queues created by releases that
// merged adjacent ranges without a bound. It is intentionally run before a
// worker lists work, so an old multi-day row is never processed as one unit.
func (r *Repo) splitOversizedESDirtyRanges(ctx context.Context, deviceID string) error {
	rows, err := r.db.QueryContext(ctx, `
SELECT device_id, from_ts, to_ts, version
FROM es_dirty_ranges
WHERE device_id = ?
ORDER BY from_ts, to_ts
`, deviceID)
	if err != nil {
		return fmt.Errorf("inspect oversized es dirty ranges: %w", err)
	}
	var oversized []ESDirtyRange
	for rows.Next() {
		var dr ESDirtyRange
		if err := rows.Scan(&dr.DeviceID, &dr.From, &dr.To, &dr.Version); err != nil {
			_ = rows.Close()
			return err
		}
		if dr.To.Sub(dr.From) > esDirtyMaxRangeDuration {
			oversized = append(oversized, dr)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, dr := range oversized {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM es_dirty_ranges WHERE device_id=? AND from_ts=? AND to_ts=? AND version=?`, dr.DeviceID, dr.From, dr.To, dr.Version); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err = enqueueESDirtyRangeTx(ctx, tx, dr.DeviceID, dr.From, dr.To); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ListESDirtyRanges returns oldest pending ranges for one device.
func (r *Repo) ListESDirtyRanges(ctx context.Context, deviceID string, limit int) ([]ESDirtyRange, error) {
	if err := r.splitOversizedESDirtyRanges(ctx, deviceID); err != nil {
		return nil, err
	}
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

// RecordESDirtyPointFailure moves a failed ES write/check into a bounded
// per-point retry queue. Repeated failures update one row instead of holding
// the whole source range open.
func (r *Repo) RecordESDirtyPointFailure(ctx context.Context, f ESDirtyPointFailure) error {
	now := time.Now()
	_, err := r.db.ExecContext(ctx, `
INSERT INTO es_dirty_point_failures
    (device_id, point_id, ts, value, state, attempts, first_error_at, last_error_at, last_error)
VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?)
ON CONFLICT(device_id, point_id, ts) DO UPDATE SET
    value = excluded.value, state = excluded.state,
    attempts = es_dirty_point_failures.attempts + 1,
    last_error_at = excluded.last_error_at, last_error = excluded.last_error
`, f.DeviceID, f.PointID, f.Ts, f.Value, f.State, now, now, f.LastError)
	if err != nil {
		return fmt.Errorf("record es dirty point failure: %w", err)
	}
	return nil
}

func (r *Repo) ListESDirtyPointFailures(ctx context.Context, deviceID string, limit int) ([]ESDirtyPointFailure, error) {
	if limit <= 0 {
		limit = 64
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT device_id, point_id, ts, value, state, attempts, last_error
FROM es_dirty_point_failures
WHERE device_id = ?
ORDER BY last_error_at, ts
LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list es dirty point failures: %w", err)
	}
	defer rows.Close()
	var out []ESDirtyPointFailure
	for rows.Next() {
		var f ESDirtyPointFailure
		if err := rows.Scan(&f.DeviceID, &f.PointID, &f.Ts, &f.Value, &f.State, &f.Attempts, &f.LastError); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (r *Repo) CompleteESDirtyPointFailure(ctx context.Context, f ESDirtyPointFailure) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM es_dirty_point_failures WHERE device_id=? AND point_id=? AND ts=?`, f.DeviceID, f.PointID, f.Ts)
	if err != nil {
		return fmt.Errorf("complete es dirty point failure: %w", err)
	}
	return nil
}

func (r *Repo) CountESDirtyPointFailures(ctx context.Context, deviceID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM es_dirty_point_failures WHERE (?='' OR device_id=?)`, deviceID, deviceID).Scan(&n)
	return n, err
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
