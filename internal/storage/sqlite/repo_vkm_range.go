package sqlite

import (
	"context"
	"fmt"
	"time"
)

// VKMRawRow is one stored ВКМ-360 archive half-hour: the raw (cp1251-decoded,
// not yet field-parsed) string plus the period timestamp it belongs to.
// Used by the es-sync command to walk everything collected in a window and
// push it into the Энергосфера SQL Server database.
type VKMRawRow struct {
	TsHour    time.Time
	RawString string
}

// GetVKMRawStringsRange returns every stored ВКМ archive row for the device
// and pipe whose period timestamp falls in [fromTs, toTs], oldest-first.
//
// This complements GetVKMRawString (single exact period) in repo_archive.go:
// es-sync needs the whole set collected in a window to diff against what the
// ЭС database already has, not one period at a time. Kept in a separate file
// so repo_archive.go stays untouched.
//
// NOTE on "ts_hour": despite the column name, ВКМ-360 archives in 30-minute
// periods (confirmed against the working mass/temperature channel in ЭС,
// whose MeasureDate rows land on :00 and :30). The column simply stores those
// half-hour-aligned timestamps; this query does not assume whole hours.
func (r *Repo) GetVKMRawStringsRange(ctx context.Context, deviceID string, pipe int, fromTs, toTs time.Time) ([]VKMRawRow, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT ts_hour, raw_string FROM archive_vkm_raw
WHERE device_id = ? AND pipe = ? AND ts_hour >= ? AND ts_hour <= ?
ORDER BY ts_hour ASC
`, deviceID, pipe, fromTs, toTs)
	if err != nil {
		return nil, fmt.Errorf("get vkm raw strings range: %w", err)
	}
	defer rows.Close()

	var out []VKMRawRow
	for rows.Next() {
		var row VKMRawRow
		if err := rows.Scan(&row.TsHour, &row.RawString); err != nil {
			return nil, fmt.Errorf("scan vkm raw row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// CountVKMRaw reports how many ВКМ archive rows are stored for the device and
// pipe, plus the newest and oldest period timestamps. es-sync logs this at
// startup as a self-check so the operator can see at a glance what the source
// database actually holds (period alignment, depth) instead of guessing.
// found=false means the store holds nothing for this device/pipe yet.
func (r *Repo) CountVKMRaw(ctx context.Context, deviceID string, pipe int) (count int, oldest, newest time.Time, found bool, err error) {
	err = r.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM archive_vkm_raw WHERE device_id = ? AND pipe = ?
`, deviceID, pipe).Scan(&count)
	if err != nil {
		return 0, time.Time{}, time.Time{}, false, fmt.Errorf("count vkm raw: %w", err)
	}
	if count == 0 {
		return 0, time.Time{}, time.Time{}, false, nil
	}

	// Oldest and newest via ORDER BY + LIMIT 1 (not MIN/MAX) for the same
	// modernc.org/sqlite reason LatestHourlyArchiveTS documents: an aggregate
	// loses the column's DATETIME type and fails to Scan into time.Time.
	if err = r.db.QueryRowContext(ctx, `
SELECT ts_hour FROM archive_vkm_raw WHERE device_id = ? AND pipe = ?
ORDER BY ts_hour ASC LIMIT 1
`, deviceID, pipe).Scan(&oldest); err != nil {
		return 0, time.Time{}, time.Time{}, false, fmt.Errorf("oldest vkm raw: %w", err)
	}
	if err = r.db.QueryRowContext(ctx, `
SELECT ts_hour FROM archive_vkm_raw WHERE device_id = ? AND pipe = ?
ORDER BY ts_hour DESC LIMIT 1
`, deviceID, pipe).Scan(&newest); err != nil {
		return 0, time.Time{}, time.Time{}, false, fmt.Errorf("newest vkm raw: %w", err)
	}
	return count, oldest, newest, true, nil
}
