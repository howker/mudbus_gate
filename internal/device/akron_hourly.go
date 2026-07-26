package device

import (
	"context"
	"log"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/profile"
	"mbgw/internal/storage"
)

// persistAkronHourly turns the decoded rows of an Akron command-104
// (hourly) archive read into HourlyArchiveRecord rows and saves them, so
// the upstream carrier (northbound) can later re-emit them to
// Энергосфера. It is the downstream half of M4's model-B carrier: collect
// once, serve many times, survive link outages.
//
// Each archive row's Fields carry (from the profile's record_layout,
// decoded by archive.decodeByLayout):
//   - "volume": float64  — already unit-scaled by codec.DecodeAkronVolume
//     (U * 10^(Pu-3)), so it goes straight into Value.
//   - "hour","day","month","year": int64 — packed-BCD calendar fields.
//     "year" is year-2000 (two low digits), so the full year is 2000+YY.
//
// The timestamp is assembled in time.Local (the Энергосфера server's
// wall-clock zone) at minute:second = 0, matching the meter's hourly
// boundary. Keeping it in the same wall frame end to end is what satisfies
// the archive-acceptance timestamp condition (see
// docs/M3_DISCOVERY_FINDINGS_akron.md).
//
// Rows whose BCD calendar is out of range (empty ring-buffer slots read
// back as 0x00/0xFF filler, or a decode error) are skipped with a log
// line rather than poisoning the store with a bogus 0000-00-00 timestamp.
// Returns the number of rows actually persisted.
func persistAkronHourly(ctx context.Context, repo storage.Repo, deviceID string, a profile.Archive, records []archive.ArchiveRecord) int {
	unit := "m3"
	for _, f := range a.RecordLayout {
		if f.Name == "volume" && f.Unit != "" {
			unit = f.Unit
		}
	}

	saved := 0
	for i, rec := range records {
		ts, ok := akronRowTime(rec.Fields)
		if !ok {
			log.Printf("[%s] архив %s: строка %d — некорректная BCD-дата, пропуск (поля=%v)\n",
				deviceID, a.ID, i, rec.Fields)
			continue
		}

		value, ok := fieldFloat(rec.Fields, "volume")
		if !ok {
			log.Printf("[%s] архив %s: строка %d — нет поля volume, пропуск (поля=%v)\n",
				deviceID, a.ID, i, rec.Fields)
			continue
		}

		if err := repo.SaveHourlyArchive(ctx, storage.HourlyArchiveRecord{
			DeviceID: deviceID,
			Channel:  "",
			Param:    "V",
			TsHour:   ts,
			Value:    value,
			Unit:     unit,
		}); err != nil {
			log.Printf("[%s] архив %s: строка %d — ошибка сохранения часовки: %v\n",
				deviceID, a.ID, i, err)
			continue
		}
		saved++
	}
	return saved
}

// akronRowTime assembles a wall-clock hour timestamp from an Akron hourly
// row's BCD calendar fields. Returns ok=false if any field is missing or
// out of the valid calendar range, so filler/garbage rows are dropped
// instead of producing a plausible-but-wrong time.
//
// A field-by-field range check (hour 0-23, day 1-31, etc.) is not enough:
// a corrupted/misaligned read (e.g. a serial timeout mid-frame) can still
// produce bytes that are individually in-range but jointly nonsense — a
// real incident produced "07.06.2044" this way, which then sat forever as
// the "newest" archive row for GetHourlyArchiveDesc's ORDER BY ts_hour
// DESC, silently shadowing every real row collected afterwards. The extra
// sanity window below (future/past bounds against wall-clock now) is a
// cheap backstop against exactly that class of bug: any BCD combination
// that decodes to a date outside a plausible operating window is treated
// the same as an out-of-range field — dropped, not stored.
const (
	akronMaxFutureSkew = 24 * time.Hour       // clock drift/timezone slop allowance
	akronMaxPastSkew   = 400 * 24 * time.Hour // > Akron's own ~80-day (1925h) ring buffer, with margin
)

func akronRowTime(fields map[string]any) (time.Time, bool) {
	hour, ok1 := fieldInt(fields, "hour")
	day, ok2 := fieldInt(fields, "day")
	month, ok3 := fieldInt(fields, "month")
	yy, ok4 := fieldInt(fields, "year")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return time.Time{}, false
	}
	if hour < 0 || hour > 23 || day < 1 || day > 31 || month < 1 || month > 12 || yy < 0 || yy > 99 {
		return time.Time{}, false
	}
	year := 2000 + yy
	ts := time.Date(year, time.Month(month), day, hour, 0, 0, 0, time.Local)

	now := time.Now()
	if ts.After(now.Add(akronMaxFutureSkew)) || ts.Before(now.Add(-akronMaxPastSkew)) {
		return time.Time{}, false
	}
	return ts, true
}

// fieldInt/fieldFloat read a decoded record_layout field with the concrete
// types archive.decodeByLayout produces (bcd -> int64, akron_volume ->
// float64), tolerating the alternate widths defensively.
func fieldInt(fields map[string]any, key string) (int, bool) {
	switch n := fields[key].(type) {
	case int64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

func fieldFloat(fields map[string]any, key string) (float64, bool) {
	switch n := fields[key].(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}
