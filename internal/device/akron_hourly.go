package device

import (
	"context"
	"encoding/binary"
	"log"
	"math"
	"sort"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/profile"
	"mbgw/internal/storage"
)

// prevValueChecker — узкий локальный интерфейс с ОДНИМ методом, который
// умеет только *sqliterepo.Repo (см. internal/storage/sqlite/
// repo_archive_range.go, GetPreviousHourlyValue). Сделан отдельным
// маленьким интерфейсом, а не добавлением метода в общий storage.Repo
// (внешний контракт, который меняется только сознательно) — Go сам
// проверит через приведение типа (type assertion), поддерживает ли
// переданный repo этот метод; для боевого *sqliterepo.Repo — да,
// проверка сработает; для тестовых заглушек без этого метода — проверка
// просто тихо пропускается, ничего не ломая.
type prevValueChecker interface {
	GetPreviousHourlyValue(ctx context.Context, deviceID, channel, param string, before time.Time) (value float64, found bool, err error)
}

// akronHourlyCandidate is one structurally valid command-104 row. Raw
// counter metadata is optional: old tests/fakes may not populate Raw, and in
// that case the original monotonic guard still applies without rollover
// recognition.
type akronHourlyCandidate struct {
	sourceIndex int
	ts          time.Time
	value       float64
	rawOK       bool
	rawSigned   int32
	rawUnsigned float64
	pu          byte
	scale       float64
}

const (
	akronCounterBitsModulus = 4294967296.0 // 2^32
	akronSignBoundary       = 2147483648.0 // 2^31
	akronRolloverGuard      = 0.90
	akronRolloverLowGuard   = 0.10
)

// akronRawCounter decodes the original U/Pu pair from a command-104 row.
// DecodeAkronVolume deliberately interprets U as int32; here we additionally
// retain the same 32 bits as uint32 so a confirmed signed-boundary crossing
// (and, much later, a full 32-bit modulo rollover) can be represented as one
// monotonically increasing normalized totalizer.
func akronRawCounter(rec archive.ArchiveRecord, decodedValue float64) (signed int32, unsignedValue float64, pu byte, scale float64, ok bool) {
	if len(rec.Raw) < 5 {
		return 0, 0, 0, 0, false
	}
	pu = rec.Raw[4]
	// The Akron encoder/decoder contract in this project uses Pu 0..5.
	// Refuse to infer rollover from an unexpected exponent; the ordinary
	// monotonic guard remains active for such a row.
	if pu > 5 {
		return 0, 0, 0, 0, false
	}
	bits := binary.LittleEndian.Uint32(rec.Raw[:4])
	signed = int32(bits)
	scale = math.Pow10(int(pu) - 3)
	signedValue := float64(signed) * scale
	// Raw must describe the same value that archive decoding put in Fields.
	// This protects the rollover path from a wrong/misaligned Raw payload.
	tolerance := math.Max(1e-9, math.Abs(decodedValue)*1e-9)
	if math.Abs(signedValue-decodedValue) > tolerance {
		return 0, 0, 0, 0, false
	}
	return signed, float64(bits) * scale, pu, scale, true
}

func buildAkronHourlyCandidates(deviceID string, a profile.Archive, records []archive.ArchiveRecord) []akronHourlyCandidate {
	out := make([]akronHourlyCandidate, 0, len(records))
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
		signed, unsignedValue, pu, scale, rawOK := akronRawCounter(rec, value)
		out = append(out, akronHourlyCandidate{
			sourceIndex: i, ts: ts, value: value,
			rawOK: rawOK, rawSigned: signed, rawUnsigned: unsignedValue, pu: pu, scale: scale,
		})
	}

	// Command 104 normally returns newest-first. Persist oldest-first so each
	// freshly accepted row becomes the monotonic baseline for the next hour;
	// this is also what lets a two-row rollover confirmation work in one page.
	sort.SliceStable(out, func(i, j int) bool { return out[i].ts.Before(out[j].ts) })
	return out
}

// confirmAkronRollover requires the next chronological hour to continue the
// same raw U/Pu trend. A single negative/corrupted sample is therefore never
// enough to turn a huge apparent drop into a huge positive counter jump.
func confirmAkronRollover(current akronHourlyCandidate, next *akronHourlyCandidate, normalizedCurrent, cycleBase float64) bool {
	if next == nil || !current.rawOK || !next.rawOK {
		return false
	}
	if next.ts.Sub(current.ts) != time.Hour || next.pu != current.pu {
		return false
	}
	modulus := akronCounterBitsModulus * current.scale
	nextValue := cycleBase + next.rawUnsigned
	// A confirmation row cannot itself cross a full modulo boundary. Such a
	// transition is astronomically far from a normal hourly increment and is
	// safer to defer to the next poll than to guess.
	if nextValue < normalizedCurrent || nextValue >= cycleBase+modulus {
		return false
	}
	return nextValue > normalizedCurrent
}

// normalizeAkronRollover is called only when the ordinary decoded value is
// lower than the previously persisted totalizer. It recognizes two cases:
//
//  1. int32 sign crossing: +2^31-1 -> -2^31. The wire bits keep increasing,
//     so the unsigned interpretation continues the totalizer.
//  2. full uint32 modulo rollover: 2^32-1 -> 0. We add one modulus to the
//     normalized counter cycle.
//
// The first observation of either boundary is accepted only near that
// boundary and only with the next consecutive hour as confirmation. Once a
// normalized cycle is established, later rows continue without special
// operator state or a reset button.
func normalizeAkronRollover(prevValue float64, current akronHourlyCandidate, next *akronHourlyCandidate) (float64, string, bool) {
	if !current.rawOK || current.scale <= 0 || prevValue < 0 {
		return 0, "", false
	}
	modulus := akronCounterBitsModulus * current.scale
	half := akronSignBoundary * current.scale
	if modulus <= 0 || math.IsInf(modulus, 0) || math.IsNaN(modulus) {
		return 0, "", false
	}

	cycle := math.Floor(prevValue / modulus)
	if cycle < 0 {
		return 0, "", false
	}
	cycleBase := cycle * modulus
	remainder := prevValue - cycleBase
	candidate := cycleBase + current.rawUnsigned

	if candidate >= prevValue {
		kind := "continuation"
		// decodedValue can only be lower here while the raw int32 is negative.
		// If this is the first sign crossing in this 32-bit cycle, demand both
		// proximity to +MaxInt32 and a second negative row one hour later.
		if current.rawSigned < 0 && remainder < half {
			if remainder < akronRolloverGuard*half {
				return 0, "", false
			}
			if next == nil || next.rawSigned >= 0 || !confirmAkronRollover(current, next, candidate, cycleBase) {
				return 0, "", false
			}
			kind = "int32"
		}
		return candidate, kind, true
	}

	// candidate < prevValue can be a genuine full 32-bit wrap only when the
	// previous value was close to the end of its modulo cycle and the new raw
	// value is close to the beginning of the next one.
	if remainder < akronRolloverGuard*modulus || current.rawUnsigned > akronRolloverLowGuard*modulus {
		return 0, "", false
	}
	nextCycleBase := cycleBase + modulus
	candidate = nextCycleBase + current.rawUnsigned
	if !confirmAkronRollover(current, next, candidate, nextCycleBase) {
		return 0, "", false
	}
	return candidate, "uint32", true
}

// persistAkronHourly turns decoded command-104 rows into durable hourly
// totalizer snapshots. Ordinary decreases are still rejected as line noise.
// A real int32/uint32 boundary is handled conservatively: the raw U/Pu bytes
// and the next consecutive hour must confirm the transition before the
// normalized counter is allowed to continue.
func persistAkronHourly(ctx context.Context, repo storage.Repo, deviceID string, a profile.Archive, records []archive.ArchiveRecord) int {
	unit := "m3"
	for _, f := range a.RecordLayout {
		if f.Name == "volume" && f.Unit != "" {
			unit = f.Unit
		}
	}

	checker, canCheckMonotonic := repo.(prevValueChecker)
	candidates := buildAkronHourlyCandidates(deviceID, a, records)

	saved := 0
	for i := range candidates {
		candidate := candidates[i]
		value := candidate.value
		var next *akronHourlyCandidate
		if i+1 < len(candidates) {
			next = &candidates[i+1]
		}

		if canCheckMonotonic {
			prevValue, found, err := checker.GetPreviousHourlyValue(ctx, deviceID, "", "V", candidate.ts)
			if err != nil {
				log.Printf("[%s] архив %s: строка %d — не удалось проверить правдоподобие (%v), сохраняю как есть\n",
					deviceID, a.ID, candidate.sourceIndex, err)
			} else if found && value < prevValue {
				if normalized, rolloverKind, rolloverOK := normalizeAkronRollover(prevValue, candidate, next); rolloverOK {
					if rolloverKind != "continuation" {
						log.Printf("[%s] архив %s: час %s — подтверждено переполнение счётчика Akron (%s): %v -> %v; сохраняю нормализованное значение %v\n",
							deviceID, a.ID, candidate.ts.Format("02.01.2006 15:04"), rolloverKind, prevValue, value, normalized)
					}
					value = normalized
				} else {
					log.Printf("[%s] архив %s: строка %d (час %s) — ОТБРАКОВАНО: новое значение %v МЕНЬШЕ предыдущего часа %v; переполнение не подтверждено, похоже на испорченное чтение\n",
						deviceID, a.ID, candidate.sourceIndex, candidate.ts.Format("02.01.2006 15:04"), value, prevValue)
					continue
				}
			}
		}

		if err := repo.SaveHourlyArchive(ctx, storage.HourlyArchiveRecord{
			DeviceID: deviceID,
			Channel:  "",
			Param:    "V",
			TsHour:   candidate.ts,
			Value:    value,
			Unit:     unit,
		}); err != nil {
			log.Printf("[%s] архив %s: строка %d — ошибка сохранения часовки: %v\n",
				deviceID, a.ID, candidate.sourceIndex, err)
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
