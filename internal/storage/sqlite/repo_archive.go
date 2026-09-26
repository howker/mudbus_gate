package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"mbgw/internal/storage"
)

// This file adds the hourly-archive store to Repo. It lives in a separate
// file (methods on *Repo, same package) so the existing repo.go does not
// need to be edited — just drop this in alongside it.
//
// Why a new table and not readings_history: readings_history logs every
// *current* poll sample (one row per poll, keyed by the poll instant),
// whereas archive_hourly holds the device's own hourly ARCHIVE records —
// one row per meter hour, with the meter's wall-clock timestamp. That is
// exactly what the upstream carrier serves on Akron command 104, and what
// lets Энергосфера back-fill (дозабрать) an ОИ debt after a link outage.

// InitArchiveSchema creates the hourly-archive store. Call once at startup,
// right after InitSchema:
//
//	if err := repo.InitSchema(ctx); err != nil { ... }
//	if err := repo.InitArchiveSchema(ctx); err != nil { ... }
func (r *Repo) InitArchiveSchema(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS archive_hourly (
    device_id TEXT NOT NULL,
    channel   TEXT NOT NULL DEFAULT '',
    param     TEXT NOT NULL DEFAULT '',
    ts_hour   DATETIME NOT NULL,
    value     REAL NOT NULL,
    unit      TEXT NOT NULL DEFAULT '',
    quality   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (device_id, channel, param, ts_hour)
);
CREATE INDEX IF NOT EXISTS idx_archive_hourly_lookup
    ON archive_hourly (device_id, channel, param, ts_hour DESC);

-- archive_vkm_raw хранит СЫРУЮ строку архива ВКМ-360 за каждый час
-- (уже раскодированную из cp1251 в UTF-8, но не разобранную на поля).
-- Нужна отдельно от archive_hourly, потому что там value REAL — под
-- текст не годится. northbound должен отдавать в ЭС именно ту строку,
-- что реально прислал прибор (не синтезировать её заново из S/ST) —
-- см. doc-комментарий VKMArchiveSource в internal/northbound.
CREATE TABLE IF NOT EXISTS archive_vkm_raw (
    device_id  TEXT NOT NULL,
    pipe       INTEGER NOT NULL,
    ts_hour    DATETIME NOT NULL,
    raw_string TEXT NOT NULL,
    PRIMARY KEY (device_id, pipe, ts_hour)
);

-- A meter can explicitly confirm that a valid pipe has no archive row for a
-- period. Remember that terminal answer so regular catch-up does not hammer the
-- same empty half-hour every scheduler tick. retry_after provides a rare
-- recheck in case device history is later repaired.
CREATE TABLE IF NOT EXISTS vkm_no_records (
    device_id  TEXT NOT NULL,
    pipe       INTEGER NOT NULL,
    ts_hour    DATETIME NOT NULL,
    confirmed_at DATETIME NOT NULL,
    retry_after  DATETIME NOT NULL,
    PRIMARY KEY (device_id, pipe, ts_hour)
);
`)
	if err != nil {
		return fmt.Errorf("init archive schema: %w", err)
	}
	if err := r.InitESDirtyRangeSchema(ctx); err != nil {
		return err
	}
	return nil
}

// MarkVKMNoRecords records an explicit meter answer "no records" for one
// period label (the end timestamp used by archive_vkm_raw).
func (r *Repo) MarkVKMNoRecords(ctx context.Context, deviceID string, pipe int, ts time.Time, retryAfter time.Time) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO vkm_no_records (device_id, pipe, ts_hour, confirmed_at, retry_after)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(device_id, pipe, ts_hour) DO UPDATE SET
    confirmed_at=excluded.confirmed_at, retry_after=excluded.retry_after
`, deviceID, pipe, ts, time.Now(), retryAfter)
	if err != nil {
		return fmt.Errorf("mark vkm no-records: %w", err)
	}
	return nil
}

func (r *Repo) VKMNoRecordsSuppressed(ctx context.Context, deviceID string, pipe int, ts, now time.Time) (bool, error) {
	var retryAfter time.Time
	err := r.db.QueryRowContext(ctx, `SELECT retry_after FROM vkm_no_records WHERE device_id=? AND pipe=? AND ts_hour=?`, deviceID, pipe, ts).Scan(&retryAfter)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read vkm no-records marker: %w", err)
	}
	return retryAfter.After(now), nil
}

func (r *Repo) ClearVKMNoRecords(ctx context.Context, deviceID string, pipe int, ts time.Time) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM vkm_no_records WHERE device_id=? AND pipe=? AND ts_hour=?`, deviceID, pipe, ts)
	return err
}

// SaveHourlyArchive upserts one hourly record. Re-collecting the same hour
// overwrites the previous value (idempotent), so repeated archive sweeps
// of overlapping ranges never create duplicates — the (device, channel,
// param, ts_hour) tuple is the natural key of an hourly archive point.
func (r *Repo) SaveHourlyArchive(ctx context.Context, rec storage.HourlyArchiveRecord) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save hourly archive begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
INSERT INTO archive_hourly (device_id, channel, param, ts_hour, value, unit, quality)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(device_id, channel, param, ts_hour) DO UPDATE SET
    value   = excluded.value,
    unit    = excluded.unit,
    quality = excluded.quality
WHERE archive_hourly.value IS NOT excluded.value
   OR archive_hourly.unit IS NOT excluded.unit
   OR archive_hourly.quality IS NOT excluded.quality
`, rec.DeviceID, rec.Channel, rec.Param, rec.TsHour, rec.Value, rec.Unit, rec.Quality)
	if err != nil {
		return fmt.Errorf("save hourly archive: %w", err)
	}

	// Queue the ES check even when the local value is byte-for-byte the same.
	// A forced re-poll can confirm unchanged local data while PointMains still
	// has an independent gap behind its ordinary cursor. One hourly snapshot
	// can also influence the following derived interval (Akron V cumulative),
	// so keep one hour of forward context.
	if err := enqueueESDirtyRangeTx(ctx, tx, rec.DeviceID, rec.TsHour, rec.TsHour.Add(time.Hour)); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save hourly archive commit: %w", err)
	}
	r.notifyESDirty(rec.DeviceID)
	return nil
}

// GetHourlyArchiveDesc returns hourly records NEWEST-FIRST — exactly the
// order the Akron archive indexes ("i=1 is the top/most recent row, i=M
// the oldest"). A command-104 request for (i, n) maps directly to
// offset=i-1, limit=n. channel/param select the stream (Akron: channel="",
// param="V").
//
// The carrier stays in this package's 0-based world: pass offset=i-1. The
// Akron-side 1-based "i" translation happens only where the request is
// decoded, never here.
func (r *Repo) GetHourlyArchiveDesc(ctx context.Context, deviceID, channel, param string, offset, limit int) ([]storage.HourlyArchiveRecord, error) {
	if limit <= 0 {
		return nil, nil
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT device_id, channel, param, ts_hour, value, unit, quality
FROM archive_hourly
WHERE device_id = ? AND channel = ? AND param = ?
ORDER BY ts_hour DESC
LIMIT ? OFFSET ?
`, deviceID, channel, param, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("get hourly archive: %w", err)
	}
	defer rows.Close()

	var out []storage.HourlyArchiveRecord
	for rows.Next() {
		var rec storage.HourlyArchiveRecord
		if err := rows.Scan(
			&rec.DeviceID, &rec.Channel, &rec.Param,
			&rec.TsHour, &rec.Value, &rec.Unit, &rec.Quality,
		); err != nil {
			return nil, fmt.Errorf("scan hourly archive: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// CountHourlyArchive returns how many hourly rows exist for the stream —
// the effective archive depth "M" the carrier can expose, so it can clamp
// or reject out-of-range indexes instead of returning short/empty reads
// that would confuse the master.
func (r *Repo) CountHourlyArchive(ctx context.Context, deviceID, channel, param string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM archive_hourly
WHERE device_id = ? AND channel = ? AND param = ?
`, deviceID, channel, param).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count hourly archive: %w", err)
	}
	return n, nil
}

// LatestHourlyArchiveTS returns the newest ts_hour stored for the stream.
// found=false (nil error) when the store holds nothing yet — the
// empty-database / freshly-installed-meter case the backfill must handle
// without treating it as an error.
//
// Deliberately ORDER BY + LIMIT 1 rather than SELECT MAX(ts_hour): under
// modernc.org/sqlite, an aggregate result loses the column's declared
// type (comes back as a plain string, and Scan into time.Time fails),
// whereas a direct column read applies the same DATETIME conversion
// GetHourlyArchiveDesc already relies on above.
func (r *Repo) LatestHourlyArchiveTS(ctx context.Context, deviceID, channel, param string) (time.Time, bool, error) {
	var ts time.Time
	err := r.db.QueryRowContext(ctx, `
SELECT ts_hour FROM archive_hourly
WHERE device_id = ? AND channel = ? AND param = ?
ORDER BY ts_hour DESC
LIMIT 1
`, deviceID, channel, param).Scan(&ts)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("latest hourly archive ts: %w", err)
	}
	return ts, true, nil
}

// MissingHours lists the whole-hour timestamps in [fromHour, toHour] that
// have no row for the stream. The expected set of hours is generated in Go
// and the present ones subtracted, rather than relying on a SQL recursive
// CTE — the ts_hour column's on-disk format under modernc.org/sqlite is
// not guaranteed identical to a CTE-generated datetime string, so an
// in-Go set difference is the reliable comparison. Bounds are inclusive
// and assumed already hour-truncated by the caller.
func (r *Repo) MissingHours(ctx context.Context, deviceID, channel, param string, fromHour, toHour time.Time) ([]time.Time, error) {
	if toHour.Before(fromHour) {
		return nil, nil
	}

	rows, err := r.db.QueryContext(ctx, `
SELECT ts_hour FROM archive_hourly
WHERE device_id = ? AND channel = ? AND param = ?
  AND ts_hour >= ? AND ts_hour <= ?
`, deviceID, channel, param, fromHour, toHour)
	if err != nil {
		return nil, fmt.Errorf("missing hours query: %w", err)
	}
	defer rows.Close()

	present := make(map[int64]bool)
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("missing hours scan: %w", err)
		}
		present[t.Truncate(time.Hour).Unix()] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var missing []time.Time
	for h := fromHour.Truncate(time.Hour); !h.After(toHour); h = h.Add(time.Hour) {
		if !present[h.Unix()] {
			missing = append(missing, h)
		}
	}
	return missing, nil
}

// SaveVKMRawString сохраняет сырую (уже раскодированную из cp1251, но не
// разобранную на поля) строку архива ВКМ-360 за конкретный час. Upsert —
// повторный сбор того же часа перезаписывает старое значение, как и у
// SaveHourlyArchive.
func (r *Repo) SaveVKMRawString(ctx context.Context, deviceID string, pipe int, hourStart time.Time, raw string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save vkm raw string begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
INSERT INTO archive_vkm_raw (device_id, pipe, ts_hour, raw_string)
VALUES (?, ?, ?, ?)
ON CONFLICT (device_id, pipe, ts_hour) DO UPDATE SET
    raw_string = excluded.raw_string
WHERE archive_vkm_raw.raw_string IS NOT excluded.raw_string
`, deviceID, pipe, hourStart, raw)
	if err != nil {
		return fmt.Errorf("save vkm raw string: %w", err)
	}

	// Same rule as SaveHourlyArchive: a successful forced refresh is enough
	// reason to re-check ES, even if the local raw string itself did not change.
	if err := enqueueESDirtyRangeTx(ctx, tx, deviceID, hourStart, hourStart.Add(time.Hour)); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save vkm raw string commit: %w", err)
	}
	r.notifyESDirty(deviceID)
	return nil
}

// GetVKMRawString возвращает сырую строку архива ВКМ-360 за конкретный
// час, если она есть. found=false (без ошибки) — если для этого часа
// ничего не собрано.
func (r *Repo) GetVKMRawString(ctx context.Context, deviceID string, pipe int, hourStart time.Time) (string, bool, error) {
	var raw string
	err := r.db.QueryRowContext(ctx, `
SELECT raw_string FROM archive_vkm_raw
WHERE device_id = ? AND pipe = ? AND ts_hour = ?
`, deviceID, pipe, hourStart).Scan(&raw)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get vkm raw string: %w", err)
	}
	return raw, true, nil
}
