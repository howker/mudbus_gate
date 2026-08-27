package sqlite

import (
	"context"
	"database/sql"
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

// DeleteHourlyArchiveRange удаляет все строки archive_hourly для
// (deviceID, channel, param), чья метка попадает в [from, to] —
// используется «Принудительным переопросом» ПЕРЕД повторным сбором с
// прибора (см. akron_reload.go/vkm_reload.go): без предварительного
// удаления повторный сбор просто ДОБАВИЛ бы новые строки поверх старых
// через upsert по точному совпадению метки времени, а если старые
// строки размечены НЕ ТЕМ соглашением (например, после фикса 2026-08-27,
// когда подпись получасовок ВКМ поменялась с начала периода на конец) —
// их метки просто не совпадут с новыми, и старые ошибочные строки
// останутся висеть в базе нетронутыми рядом с новыми верными. Удаление
// диапазона целиком перед сбором устраняет это — какой бы ни была старая
// разметка, после переопроса в диапазоне останутся только свежие,
// правильно размеченные записи.
func (r *Repo) DeleteHourlyArchiveRange(ctx context.Context, deviceID, channel, param string, from, to time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
DELETE FROM archive_hourly
WHERE device_id = ? AND channel = ? AND param = ? AND ts_hour >= ? AND ts_hour <= ?
`, deviceID, channel, param, from, to)
	if err != nil {
		return 0, fmt.Errorf("delete hourly archive range: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete hourly archive range: rows affected: %w", err)
	}
	return n, nil
}

// GetPreviousHourlyValue возвращает значение последней (по времени)
// записи СТРОГО ДО указанного момента — используется для проверки
// правдоподобия нового показания накопительного счётчика Akron (V):
// счётчик — одометр, он физически не может уменьшаться, поэтому новое
// показание, которое оказывается МЕНЬШЕ показания предыдущего часа —
// почти наверняка испорченное чтение (помеха на линии RS-485), а не
// реальные данные. found=false (без ошибки), если для устройства ещё
// нет ни одной более ранней записи — тогда сравнивать не с чем, это не
// ошибка.
func (r *Repo) GetPreviousHourlyValue(ctx context.Context, deviceID, channel, param string, before time.Time) (value float64, found bool, err error) {
	err = r.db.QueryRowContext(ctx, `
SELECT value FROM archive_hourly
WHERE device_id = ? AND channel = ? AND param = ? AND ts_hour < ?
ORDER BY ts_hour DESC LIMIT 1
`, deviceID, channel, param, before).Scan(&value)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("get previous hourly value: %w", err)
	}
	return value, true, nil
}
