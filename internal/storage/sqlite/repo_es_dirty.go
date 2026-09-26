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
	DeviceID    string
	PointID     int
	Ts          time.Time
	Value       float64 // diagnostic snapshot only; retries recalculate from local archive
	State       int     // diagnostic snapshot only
	SourcePipe  int
	SourceTag   string
	Attempts    int
	NextRetryAt time.Time
	LastError   string
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
    source_pipe INTEGER NOT NULL DEFAULT 0,
    source_tag TEXT NOT NULL DEFAULT '',
    attempts  INTEGER NOT NULL DEFAULT 1,
    first_error_at DATETIME NOT NULL,
    last_error_at  DATETIME NOT NULL,
    next_retry_at DATETIME,
    last_error TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (device_id, point_id, ts)
);
CREATE INDEX IF NOT EXISTS idx_es_dirty_point_failures_device_time
    ON es_dirty_point_failures(device_id, ts);
`)
	if err != nil {
		return fmt.Errorf("init es dirty ranges schema: %w", err)
	}
	if err := r.migrateESDirtyPointFailuresSchema(ctx); err != nil {
		return err
	}
	return nil
}

// migrateESDirtyPointFailuresSchema upgrades databases that were already
// started by the first P0 build. Those rows stored only the target ID/value;
// keeping them is safe, but retries without a source identity are discarded
// rather than risking a stale write after the operator changes a mapping.
func (r *Repo) migrateESDirtyPointFailuresSchema(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `PRAGMA table_info(es_dirty_point_failures)`)
	if err != nil {
		return fmt.Errorf("inspect es dirty point failure schema: %w", err)
	}
	cols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var def any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &def, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("scan es dirty point failure schema: %w", err)
		}
		cols[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	adds := []struct {
		name string
		sql  string
	}{
		{"source_pipe", `ALTER TABLE es_dirty_point_failures ADD COLUMN source_pipe INTEGER NOT NULL DEFAULT 0`},
		{"source_tag", `ALTER TABLE es_dirty_point_failures ADD COLUMN source_tag TEXT NOT NULL DEFAULT ''`},
		{"next_retry_at", `ALTER TABLE es_dirty_point_failures ADD COLUMN next_retry_at DATETIME`},
	}
	for _, add := range adds {
		if cols[add.name] {
			continue
		}
		if _, err := r.db.ExecContext(ctx, add.sql); err != nil {
			return fmt.Errorf("migrate es dirty point failure column %s: %w", add.name, err)
		}
	}
	if _, err := r.db.ExecContext(ctx, `UPDATE es_dirty_point_failures SET next_retry_at=last_error_at WHERE next_retry_at IS NULL`); err != nil {
		return fmt.Errorf("initialize es dirty point retry schedule: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_es_dirty_point_failures_retry ON es_dirty_point_failures(device_id, next_retry_at)`); err != nil {
		return fmt.Errorf("index es dirty point retry schedule: %w", err)
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

var esDirtyPointRetryDelays = []time.Duration{
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

func esDirtyPointRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	idx := attempt - 1
	if idx >= len(esDirtyPointRetryDelays) {
		idx = len(esDirtyPointRetryDelays) - 1
	}
	return esDirtyPointRetryDelays[idx]
}

// RecordESDirtyPointFailure moves a failed ES write/check into a bounded
// per-point retry queue. Value/PointID are retained only for diagnostics and
// row identity; the retry path must recalculate from the local archive and the
// CURRENT source mapping before it touches Energosphere.
func (r *Repo) RecordESDirtyPointFailure(ctx context.Context, f ESDirtyPointFailure) error {
	now := time.Now()
	currentAttempts := 0
	err := r.db.QueryRowContext(ctx, `
SELECT attempts FROM es_dirty_point_failures WHERE device_id=? AND point_id=? AND ts=?`,
		f.DeviceID, f.PointID, f.Ts).Scan(&currentAttempts)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read es dirty point failure attempts: %w", err)
	}
	attempts := currentAttempts + 1
	nextRetry := now.Add(esDirtyPointRetryDelay(attempts))
	_, err = r.db.ExecContext(ctx, `
INSERT INTO es_dirty_point_failures
    (device_id, point_id, ts, value, state, source_pipe, source_tag, attempts, first_error_at, last_error_at, next_retry_at, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(device_id, point_id, ts) DO UPDATE SET
    value = excluded.value, state = excluded.state,
    source_pipe = excluded.source_pipe, source_tag = excluded.source_tag,
    attempts = excluded.attempts, last_error_at = excluded.last_error_at,
    next_retry_at = excluded.next_retry_at, last_error = excluded.last_error
`, f.DeviceID, f.PointID, f.Ts, f.Value, f.State, f.SourcePipe, f.SourceTag, attempts, now, now, nextRetry, f.LastError)
	if err != nil {
		return fmt.Errorf("record es dirty point failure: %w", err)
	}
	return nil
}

// ListESDirtyPointFailures returns only failures whose backoff has expired.
// Persistent bad points therefore do not hit SQL Server or the service log on
// every worker pass.
func (r *Repo) ListESDirtyPointFailures(ctx context.Context, deviceID string, limit int) ([]ESDirtyPointFailure, error) {
	if limit <= 0 {
		limit = 64
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT device_id, point_id, ts, value, state, source_pipe, source_tag, attempts, next_retry_at, last_error
FROM es_dirty_point_failures
WHERE device_id = ? AND (next_retry_at IS NULL OR next_retry_at <= ?)
ORDER BY next_retry_at, ts
LIMIT ?`, deviceID, time.Now(), limit)
	if err != nil {
		return nil, fmt.Errorf("list es dirty point failures: %w", err)
	}
	defer rows.Close()
	var out []ESDirtyPointFailure
	for rows.Next() {
		var f ESDirtyPointFailure
		if err := rows.Scan(&f.DeviceID, &f.PointID, &f.Ts, &f.Value, &f.State, &f.SourcePipe, &f.SourceTag, &f.Attempts, &f.NextRetryAt, &f.LastError); err != nil {
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
