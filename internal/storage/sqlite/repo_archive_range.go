package sqlite

import (
	"context"
	"fmt"
	"time"

	"mbgw/internal/storage"
)

// This file adds a DATE-RANGE query to archive_hourly, complementing
// repo_archive.go's GetHourlyArchiveDesc (which reads the newest N rows,
// offset/limit-paginated — built for the northbound carriers' "give me
// index i..i+n" access pattern) and repo_vkm_range.go's raw-string range
// query. The Web UI's archive-viewer tab needs a genuine "с ... по ..."
// window (calendar dates or quick presets), ascending in time, which
// neither existing query shape provides directly.

// GetHourlyArchiveRange returns every archive_hourly row for
// (deviceID, channel, param) whose ts_hour falls in [from, to] inclusive,
// OLDEST FIRST (ascending) — the natural order for a "показать период"
// table or a CSV export, unlike GetHourlyArchiveDesc's newest-first order
// built for a different caller (the northbound carriers walking backward
// by index).
func (r *Repo) GetHourlyArchiveRange(ctx context.Context, deviceID, channel, param string, from, to time.Time) ([]storage.HourlyArchiveRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT device_id, channel, param, ts_hour, value, unit, quality
FROM archive_hourly
WHERE device_id = ? AND channel = ? AND param = ? AND ts_hour >= ? AND ts_hour <= ?
ORDER BY ts_hour ASC
`, deviceID, channel, param, from, to)
	if err != nil {
		return nil, fmt.Errorf("get hourly archive range: %w", err)
	}
	defer rows.Close()

	var out []storage.HourlyArchiveRecord
	for rows.Next() {
		var rec storage.HourlyArchiveRecord
		if err := rows.Scan(
			&rec.DeviceID, &rec.Channel, &rec.Param,
			&rec.TsHour, &rec.Value, &rec.Unit, &rec.Quality,
		); err != nil {
			return nil, fmt.Errorf("scan hourly archive range: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
