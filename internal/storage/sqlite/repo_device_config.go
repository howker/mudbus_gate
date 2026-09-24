package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// This file adds device/channel CONFIGURATION storage to Repo — separate
// from repo_archive.go's DATA storage (archive_hourly, archive_vkm_raw).
// It exists so the future Web UI (T14, minimal slice) has somewhere to
// write "add a device" / "map ВКМ tag X to Энергосфера channel N" instead
// of the operator hand-editing config.yaml/es_sync.txt on the server.
//
// Deliberately NOT the full FINAL_TRD §7 domain model (sites, device_
// profiles, poll_schedules, users, roles, audit_log, ...) — that is a much
// larger, currently-unimplemented contract (sql/schema.sql does not exist
// in this repo). This is the minimal slice actually needed right now: one
// devices table shared by both device kinds (vkm360/akron), plus two
// kind-specific child tables for how each kind's data reaches Энергосфера
// (see ADR: ВКМ writes directly into Энергосфера's SQL Server Mains table
// via internal/integration; Akron is instead served upstream through a
// device-emulating northbound carrier that Энергосфера's own driver
// polls). Growing toward the full FINAL_TRD model later does not require
// reworking this — it is additive, matching the same "queries live beside
// the schema that answers them" pattern repo_archive.go already
// established for archive_hourly/archive_vkm_raw.

// InitDeviceConfigSchema creates the device-configuration tables. Call
// once at startup, alongside InitSchema/InitArchiveSchema:
//
//	if err := repo.InitSchema(ctx); err != nil { ... }
//	if err := repo.InitArchiveSchema(ctx); err != nil { ... }
//	if err := repo.InitDeviceConfigSchema(ctx); err != nil { ... }
func (r *Repo) InitDeviceConfigSchema(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS devices (
    id                       TEXT PRIMARY KEY,
    name                     TEXT NOT NULL DEFAULT '',
    kind                     TEXT NOT NULL,              -- 'vkm360' | 'akron' | 'ivk-ter'
    profile                  TEXT NOT NULL DEFAULT '',
    transport_kind           TEXT NOT NULL DEFAULT '',   -- 'modbus_tcp' | 'rtu_serial' | 'tcp_serial'
    host                     TEXT NOT NULL DEFAULT '',
    port                     INTEGER NOT NULL DEFAULT 0,
    com                      TEXT NOT NULL DEFAULT '',
    baudrate                 INTEGER NOT NULL DEFAULT 0,
    parity                   TEXT NOT NULL DEFAULT 'none',
    stopbits                 INTEGER NOT NULL DEFAULT 1,
    timeout_ms               INTEGER NOT NULL DEFAULT 1000,
    unit_id                  INTEGER NOT NULL DEFAULT 1,
    retries                  INTEGER NOT NULL DEFAULT 3,   -- see internal/protocol/modbus/core.go's
                                                             -- Transact: retries with a fixed 200/400/800ms
                                                             -- backoff between attempts (not itself configurable
                                                             -- yet — see this column's doc note in DeviceRecord).
    current_poll_seconds     INTEGER NOT NULL DEFAULT 0,   -- 0 -> config.CurrentPollDefault
    backfill_max_depth_hours INTEGER NOT NULL DEFAULT 0,   -- 0 -> "variant В" (fill everything missing)
    gap_scan_window_hours    INTEGER NOT NULL DEFAULT 0,   -- 0 -> config.GapScanDefault
    archive_at_minute        INTEGER NOT NULL DEFAULT -1,  -- -1 sentinel = "unset" -> default +5 minutes
    archive_every_periods    INTEGER NOT NULL DEFAULT 1,   -- 1 = every archive period from the profile
    archive_days_mask        INTEGER NOT NULL DEFAULT 127, -- bit0=Mon ... bit6=Sun
    archive_window_start     TEXT NOT NULL DEFAULT '',     -- HH:MM, empty = no daily window
    archive_window_end       TEXT NOT NULL DEFAULT '',     -- HH:MM, empty = no daily window
    time_correction_deadband_seconds INTEGER NOT NULL DEFAULT 0, -- 0 = без порога; автокоррекция включается двумя лимитами ниже
    time_correction_max_step_seconds INTEGER NOT NULL DEFAULT 0, -- 1..99 сек; 0 = автокоррекция выключена
    time_correction_daily_limit_seconds INTEGER NOT NULL DEFAULT 0, -- суммарный модуль коррекций за скользящие 24ч; 0 = автокоррекция выключена
    enabled                  INTEGER NOT NULL DEFAULT 1,   -- 0/1: poll paused without deleting the device
    created_at               DATETIME NOT NULL,
    updated_at               DATETIME NOT NULL
);

-- Active VKM archive pipes. No rows means the backward-compatible legacy
-- configuration "pipe 1 only". Discovery/UI writes explicit rows once
-- additional pipes are confirmed. This is configuration, not archive data.
CREATE TABLE IF NOT EXISTS vkm_active_pipes (
    device_id TEXT NOT NULL,
    pipe      INTEGER NOT NULL CHECK (pipe BETWEEN 1 AND 10),
    PRIMARY KEY (device_id, pipe)
);

-- История фактически выполненных коррекций часов ВКМ. Нужна для
-- ограничения суммарного модуля коррекций за скользящие 24 часа.
-- Записывается только ПОСЛЕ подтверждённой успешной команды коррекции.
CREATE TABLE IF NOT EXISTS device_time_corrections (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id          TEXT NOT NULL,
    corrected_at       DATETIME NOT NULL,
    correction_seconds INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_device_time_corrections_device_time
    ON device_time_corrections(device_id, corrected_at);

-- Per-tag mapping of a ВКМ device's archive values into Энергосфера
-- Mains channels — one row per (device, tag). Read by
-- internal/integration/energosphere_sync.go instead of es_sync.txt's
-- chan_heat/chan_mass/chan_temp/chan_pressure/factor_* keys.
CREATE TABLE IF NOT EXISTS es_vkm_channels (
    device_id     TEXT NOT NULL,
    tag           TEXT NOT NULL,           -- 'ST' | 'S' | 'T' | 'Pi' | 'V'
    es_channel_id INTEGER NOT NULL,
    factor        REAL NOT NULL DEFAULT 1.0,
    min_value     REAL NULL,               -- NULL = lower safety limit disabled
    max_value     REAL NULL,               -- NULL = upper safety limit disabled
    PRIMARY KEY (device_id, tag)
);

-- Single-row table: the one Энергосфера SQL Server connection every
-- ВКМ device's es_vkm_channels rows are written through. Not per-device
-- because there is exactly one Энергосфера instance in this deployment
-- (see FINAL_TRD's platform notes) — modeled as a table rather than a
-- bare config value so it can be edited the same way (INSERT/UPDATE)
-- as everything else here, and so a future multi-ЭС deployment has
-- somewhere to grow into without a schema rewrite (add an id column and
-- a fk from devices at that point — not needed now).
CREATE TABLE IF NOT EXISTS es_connection (
    id            INTEGER PRIMARY KEY CHECK (id = 1),  -- enforces single row
    sql_server    TEXT NOT NULL DEFAULT 'localhost',
    sql_database  TEXT NOT NULL DEFAULT '',
    sql_user      TEXT NOT NULL DEFAULT '',
    sql_password  TEXT NOT NULL DEFAULT '',
    sql_port      INTEGER NOT NULL DEFAULT 1433,
    time_shift_minutes INTEGER NOT NULL DEFAULT 0,  -- сдвиг метки времени при записи в Mains, см. ESConnection.TimeShiftMinutes
    updated_at    DATETIME NOT NULL
);

-- Where an Akron device's northbound carrier listens for Энергосфера's
-- own АКРОН-01-1 driver to connect (Raw TCP тип связи — see
-- docs/M3_DISCOVERY_FINDINGS_akron.md). One row per akron device.
CREATE TABLE IF NOT EXISTS es_akron_northbound (
    device_id   TEXT PRIMARY KEY,
    listen_addr TEXT NOT NULL  -- e.g. "127.0.0.1:15021"
);
`)
	if err != nil {
		return fmt.Errorf("init device config schema: %w", err)
	}

	// Lightweight migration for a devices table created before the
	// `retries` column existed (e.g. a dev-machine mbgw_server.db from
	// before 2026-08-23). CREATE TABLE IF NOT EXISTS above is a no-op on
	// an already-existing table, so a column added later needs its own
	// ALTER TABLE. The error is deliberately ignored: SQLite has no
	// "ADD COLUMN IF NOT EXISTS", and the only way ALTER TABLE ADD COLUMN
	// fails here is "duplicate column name" (harmless — means a fresh
	// install's CREATE TABLE above already included it).
	_, _ = r.db.ExecContext(ctx, `ALTER TABLE devices ADD COLUMN retries INTEGER NOT NULL DEFAULT 3`)

	// Настройки безопасной автокоррекции часов ВКМ. После миграции
	// MaxStep/DailyLimit остаются нулевыми, поэтому автокоррекция выключена.
	// Deadband=0 сам по себе коррекцию не выключает: это означает отсутствие
	// дополнительного порога расхождения.
	_, _ = r.db.ExecContext(ctx, `ALTER TABLE devices ADD COLUMN time_correction_deadband_seconds INTEGER NOT NULL DEFAULT 0`)
	_, _ = r.db.ExecContext(ctx, `ALTER TABLE devices ADD COLUMN time_correction_max_step_seconds INTEGER NOT NULL DEFAULT 0`)
	_, _ = r.db.ExecContext(ctx, `ALTER TABLE devices ADD COLUMN time_correction_daily_limit_seconds INTEGER NOT NULL DEFAULT 0`)
	_, _ = r.db.ExecContext(ctx, `ALTER TABLE devices ADD COLUMN archive_every_periods INTEGER NOT NULL DEFAULT 1`)
	_, _ = r.db.ExecContext(ctx, `ALTER TABLE devices ADD COLUMN archive_days_mask INTEGER NOT NULL DEFAULT 127`)
	_, _ = r.db.ExecContext(ctx, `ALTER TABLE devices ADD COLUMN archive_window_start TEXT NOT NULL DEFAULT ''`)
	_, _ = r.db.ExecContext(ctx, `ALTER TABLE devices ADD COLUMN archive_window_end TEXT NOT NULL DEFAULT ''`)

	// Та же лёгкая миграция для es_connection, добавленного позже
	// (сдвиг времени при записи в Mains) — ошибка "duplicate column
	// name" на уже обновлённой базе безвредна и намеренно игнорируется.
	_, _ = r.db.ExecContext(ctx, `ALTER TABLE es_connection ADD COLUMN time_shift_minutes INTEGER NOT NULL DEFAULT 0`)

	// Optional per-channel safety limits for direct writes to ЭС.
	// NULL means "limit disabled", so upgrading an existing production DB
	// does not invent or silently enforce any engineering range.
	_, _ = r.db.ExecContext(ctx, `ALTER TABLE es_vkm_channels ADD COLUMN min_value REAL NULL`)
	_, _ = r.db.ExecContext(ctx, `ALTER TABLE es_vkm_channels ADD COLUMN max_value REAL NULL`)

	return nil
}

// DeviceRecord is one row of the devices table — the DB-backed
// replacement for config.DeviceConfig, minus the YAML tags. Field names
// deliberately mirror DeviceConfig's so callers converting between the
// two (see cmd/mbgw's future server command) stay a straight field-by-
// field copy, not a reinterpretation.
type DeviceRecord struct {
	ID                    string
	Name                  string
	Kind                  string // "vkm360" | "akron" | "ivk-ter"
	Profile               string
	TransportKind         string
	Host                  string
	Port                  int
	COM                   string
	Baudrate              int
	Parity                string
	StopBits              int
	TimeoutMs             int
	UnitID                int
	Retries               int
	CurrentPollSeconds    int
	BackfillMaxDepthHours int
	GapScanWindowHours    int
	ArchiveAtMinute       int // offset in minutes from the archive-period boundary
	ArchiveEveryPeriods   int
	ArchiveDaysMask       int
	ArchiveWindowStart    string // HH:MM; both window fields empty = all day
	ArchiveWindowEnd      string
	// VKM clock auto-correction safety settings. Automatic correction is
	// enabled when MaxStepSeconds and DailyLimitSeconds are both > 0.
	// DeadbandSeconds may be 0, meaning no additional drift threshold.
	// MaxStepSeconds is additionally capped by the protocol limit of 99 seconds.
	TimeCorrectionDeadbandSeconds   int
	TimeCorrectionMaxStepSeconds    int
	TimeCorrectionDailyLimitSeconds int
	Enabled                         bool
}

// UpsertDevice inserts or replaces a device by ID — the Web UI's "add/
// edit device" form maps directly onto this (edit re-submits every field,
// not a partial patch; simpler and sufficient for a single-operator admin
// screen).
func (r *Repo) UpsertDevice(ctx context.Context, d DeviceRecord) error {
	if d.Retries <= 0 {
		d.Retries = 3
	}
	if d.TimeCorrectionDeadbandSeconds < 0 {
		return fmt.Errorf("допустимое расхождение времени не может быть отрицательным")
	}
	if d.TimeCorrectionMaxStepSeconds < 0 || d.TimeCorrectionMaxStepSeconds > 99 {
		return fmt.Errorf("максимальная коррекция времени должна быть в диапазоне 0..99 секунд")
	}
	if d.TimeCorrectionDailyLimitSeconds < 0 {
		return fmt.Errorf("лимит коррекции времени за 24 часа не может быть отрицательным")
	}
	if d.ArchiveAtMinute < -1 || d.ArchiveAtMinute > 59 {
		return fmt.Errorf("сдвиг архивного опроса должен быть -1 (по умолчанию) либо 0..59 минут")
	}
	if d.ArchiveEveryPeriods <= 0 {
		d.ArchiveEveryPeriods = 1
	}
	if d.ArchiveDaysMask == 0 {
		d.ArchiveDaysMask = 127
	}
	if d.ArchiveDaysMask < 1 || d.ArchiveDaysMask > 127 {
		return fmt.Errorf("маска дней архивного опроса должна быть в диапазоне 1..127")
	}
	if (d.ArchiveWindowStart == "") != (d.ArchiveWindowEnd == "") {
		return fmt.Errorf("начало и конец окна архивного опроса должны быть заданы вместе")
	}

	now := time.Now()
	enabled := 0
	if d.Enabled {
		enabled = 1
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO devices (
    id, name, kind, profile, transport_kind, host, port, com, baudrate,
    parity, stopbits, timeout_ms, unit_id, retries, current_poll_seconds,
    backfill_max_depth_hours, gap_scan_window_hours, archive_at_minute,
    archive_every_periods, archive_days_mask, archive_window_start, archive_window_end,
    time_correction_deadband_seconds, time_correction_max_step_seconds,
    time_correction_daily_limit_seconds, enabled, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    name = excluded.name,
    kind = excluded.kind,
    profile = excluded.profile,
    transport_kind = excluded.transport_kind,
    host = excluded.host,
    port = excluded.port,
    com = excluded.com,
    baudrate = excluded.baudrate,
    parity = excluded.parity,
    stopbits = excluded.stopbits,
    timeout_ms = excluded.timeout_ms,
    unit_id = excluded.unit_id,
    retries = excluded.retries,
    current_poll_seconds = excluded.current_poll_seconds,
    backfill_max_depth_hours = excluded.backfill_max_depth_hours,
    gap_scan_window_hours = excluded.gap_scan_window_hours,
    archive_at_minute = excluded.archive_at_minute,
    archive_every_periods = excluded.archive_every_periods,
    archive_days_mask = excluded.archive_days_mask,
    archive_window_start = excluded.archive_window_start,
    archive_window_end = excluded.archive_window_end,
    time_correction_deadband_seconds = excluded.time_correction_deadband_seconds,
    time_correction_max_step_seconds = excluded.time_correction_max_step_seconds,
    time_correction_daily_limit_seconds = excluded.time_correction_daily_limit_seconds,
    enabled = excluded.enabled,
    updated_at = excluded.updated_at
`, d.ID, d.Name, d.Kind, d.Profile, d.TransportKind, d.Host, d.Port, d.COM,
		d.Baudrate, d.Parity, d.StopBits, d.TimeoutMs, d.UnitID, d.Retries, d.CurrentPollSeconds,
		d.BackfillMaxDepthHours, d.GapScanWindowHours, d.ArchiveAtMinute,
		d.ArchiveEveryPeriods, d.ArchiveDaysMask, d.ArchiveWindowStart, d.ArchiveWindowEnd,
		d.TimeCorrectionDeadbandSeconds, d.TimeCorrectionMaxStepSeconds, d.TimeCorrectionDailyLimitSeconds,
		enabled, now, now)
	if err != nil {
		return fmt.Errorf("upsert device: %w", err)
	}
	return nil
}

// ListDevices returns every configured device, for the Web UI's device
// list screen and for the future single-process server to build its
// southbound poll set from at startup (replacing config.yaml's
// Devices []DeviceConfig).
func (r *Repo) ListDevices(ctx context.Context) ([]DeviceRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, name, kind, profile, transport_kind, host, port, com, baudrate,
       parity, stopbits, timeout_ms, unit_id, retries, current_poll_seconds,
       backfill_max_depth_hours, gap_scan_window_hours, archive_at_minute,
       archive_every_periods, archive_days_mask, archive_window_start, archive_window_end,
       time_correction_deadband_seconds, time_correction_max_step_seconds,
       time_correction_daily_limit_seconds, enabled
FROM devices ORDER BY id
`)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()

	var out []DeviceRecord
	for rows.Next() {
		var d DeviceRecord
		var enabled int
		if err := rows.Scan(&d.ID, &d.Name, &d.Kind, &d.Profile, &d.TransportKind,
			&d.Host, &d.Port, &d.COM, &d.Baudrate, &d.Parity, &d.StopBits,
			&d.TimeoutMs, &d.UnitID, &d.Retries, &d.CurrentPollSeconds, &d.BackfillMaxDepthHours,
			&d.GapScanWindowHours, &d.ArchiveAtMinute, &d.ArchiveEveryPeriods, &d.ArchiveDaysMask,
			&d.ArchiveWindowStart, &d.ArchiveWindowEnd, &d.TimeCorrectionDeadbandSeconds,
			&d.TimeCorrectionMaxStepSeconds, &d.TimeCorrectionDailyLimitSeconds, &enabled); err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		d.Enabled = enabled != 0
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDevice returns one device by ID. found=false (nil error) if it
// doesn't exist.
func (r *Repo) GetDevice(ctx context.Context, id string) (DeviceRecord, bool, error) {
	var d DeviceRecord
	var enabled int
	err := r.db.QueryRowContext(ctx, `
SELECT id, name, kind, profile, transport_kind, host, port, com, baudrate,
       parity, stopbits, timeout_ms, unit_id, retries, current_poll_seconds,
       backfill_max_depth_hours, gap_scan_window_hours, archive_at_minute,
       archive_every_periods, archive_days_mask, archive_window_start, archive_window_end,
       time_correction_deadband_seconds, time_correction_max_step_seconds,
       time_correction_daily_limit_seconds, enabled
FROM devices WHERE id = ?
`, id).Scan(&d.ID, &d.Name, &d.Kind, &d.Profile, &d.TransportKind,
		&d.Host, &d.Port, &d.COM, &d.Baudrate, &d.Parity, &d.StopBits,
		&d.TimeoutMs, &d.UnitID, &d.Retries, &d.CurrentPollSeconds, &d.BackfillMaxDepthHours,
		&d.GapScanWindowHours, &d.ArchiveAtMinute, &d.ArchiveEveryPeriods, &d.ArchiveDaysMask,
		&d.ArchiveWindowStart, &d.ArchiveWindowEnd, &d.TimeCorrectionDeadbandSeconds,
		&d.TimeCorrectionMaxStepSeconds, &d.TimeCorrectionDailyLimitSeconds, &enabled)
	if err == sql.ErrNoRows {
		return DeviceRecord{}, false, nil
	}
	if err != nil {
		return DeviceRecord{}, false, fmt.Errorf("get device: %w", err)
	}
	d.Enabled = enabled != 0
	return d, true, nil
}

// DeleteDevice removes a device and its channel mappings (both kinds —
// harmless no-op deletes on whichever table doesn't apply to this
// device's kind).
func (r *Repo) DeleteDevice(ctx context.Context, id string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete device: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM es_vkm_channels WHERE device_id = ?`, id); err != nil {
		return fmt.Errorf("delete device vkm channels: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM vkm_active_pipes WHERE device_id = ?`, id); err != nil {
		return fmt.Errorf("delete device vkm active pipes: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM es_akron_northbound WHERE device_id = ?`, id); err != nil {
		return fmt.Errorf("delete device akron northbound: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM device_time_corrections WHERE device_id = ?`, id); err != nil {
		return fmt.Errorf("delete device time corrections: %w", err)
	}
	if err := r.DeleteCurrentReadings(ctx, id); err != nil {
		return fmt.Errorf("delete device current readings: %w", err)
	}
	return nil
}

// DeleteCurrentReadings удаляет ВСЕ текущие показания (readings_current)
// для прибора — вызывается при удалении прибора, а также при сохранении
// УЖЕ СУЩЕСТВУЮЩЕГО прибора через UI (см. handleDevices в api_devices.go).
// Причина: readings_current хранит строки по (device_id, point_id,
// instance) — если у прибора когда-либо менялся тип (например, ВКМ360 →
// Akron), старые показания под именами точек предыдущего профиля (масса,
// давление...) никуда не деваются сами по себе и остаются в базе рядом
// со свежими показаниями нового профиля навсегда, показываясь на экране
// «Текущие данные» вперемешку — реальный случай, воспроизведённый
// 2026-08-23 (прибор был кратко сохранён как ВКМ, затем пересохранён как
// Akron, старые показания массы/давления/температуры остались висеть).
// history (readings_history) НЕ трогаем — это архив всех прошлых
// опросов, его чистить не нужно и не должно.
func (r *Repo) DeleteCurrentReadings(ctx context.Context, deviceID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM readings_current WHERE device_id = ?`, deviceID)
	if err != nil {
		return fmt.Errorf("delete current readings: %w", err)
	}
	return nil
}

// RecordTimeCorrection stores one successfully applied VKM clock correction.
// correctionSeconds is signed: positive moves the meter clock forward,
// negative moves it backward. The daily limiter intentionally sums ABS()
// values, so +20 followed by -20 consumes 40 seconds of the 24h budget.
func (r *Repo) RecordTimeCorrection(ctx context.Context, deviceID string, correctionSeconds int, correctedAt time.Time) error {
	if correctionSeconds == 0 {
		return nil
	}
	if _, err := r.db.ExecContext(ctx, `
INSERT INTO device_time_corrections (device_id, corrected_at, correction_seconds)
VALUES (?, ?, ?)
`, deviceID, correctedAt, correctionSeconds); err != nil {
		return fmt.Errorf("record device time correction: %w", err)
	}
	return nil
}

// TimeCorrectionUsedLast24Hours returns the sum of absolute values of
// successful corrections during the rolling 24 hours ending at now.
func (r *Repo) TimeCorrectionUsedLast24Hours(ctx context.Context, deviceID string, now time.Time) (int, error) {
	cutoff := now.Add(-24 * time.Hour)
	var used int
	if err := r.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(ABS(correction_seconds)), 0)
FROM device_time_corrections
WHERE device_id = ? AND corrected_at > ? AND corrected_at <= ?
`, deviceID, cutoff, now).Scan(&used); err != nil {
		return 0, fmt.Errorf("sum device time corrections: %w", err)
	}
	return used, nil
}

// TimeCorrectionRecord is one durable clock-correction audit row.
// CorrectionSeconds is signed: positive moved the device clock forward,
// negative moved it backward.
type TimeCorrectionRecord struct {
	ID                int64
	DeviceID          string
	CorrectedAt       time.Time
	CorrectionSeconds int
}

// LastTimeCorrection возвращает последнюю подтверждённую коррекцию конкретного
// прибора. Нужна runtime-монитору после перезапуска процесса: история уже
// хранится в SQLite, поэтому состояние «срабатываний не было» не должно
// ошибочно появляться только из-за рестарта МодбасШлюза.
func (r *Repo) LastTimeCorrection(ctx context.Context, deviceID string) (TimeCorrectionRecord, bool, error) {
	var rec TimeCorrectionRecord
	err := r.db.QueryRowContext(ctx, `
SELECT id, device_id, corrected_at, correction_seconds
FROM device_time_corrections
WHERE device_id = ?
ORDER BY corrected_at DESC, id DESC
LIMIT 1
`, deviceID).Scan(&rec.ID, &rec.DeviceID, &rec.CorrectedAt, &rec.CorrectionSeconds)
	if err == sql.ErrNoRows {
		return TimeCorrectionRecord{}, false, nil
	}
	if err != nil {
		return TimeCorrectionRecord{}, false, fmt.Errorf("last device time correction: %w", err)
	}
	return rec, true, nil
}

// ListTimeCorrections returns the newest correction events first.
// limit<=0 uses the operator UI default of 100; a hard cap protects the
// admin endpoint from accidentally loading an unbounded history.
func (r *Repo) ListTimeCorrections(ctx context.Context, limit int) ([]TimeCorrectionRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	rows, err := r.db.QueryContext(ctx, `
SELECT id, device_id, corrected_at, correction_seconds
FROM device_time_corrections
ORDER BY corrected_at DESC, id DESC
LIMIT ?
`, limit)
	if err != nil {
		return nil, fmt.Errorf("list device time corrections: %w", err)
	}
	defer rows.Close()

	out := make([]TimeCorrectionRecord, 0)
	for rows.Next() {
		var rec TimeCorrectionRecord
		if err := rows.Scan(&rec.ID, &rec.DeviceID, &rec.CorrectedAt, &rec.CorrectionSeconds); err != nil {
			return nil, fmt.Errorf("scan device time correction: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate device time corrections: %w", err)
	}
	return out, nil
}

// VKMChannelRecord is one tag->ЭС-channel mapping row.
type VKMChannelRecord struct {
	DeviceID    string
	Tag         string // "ST" | "S" | "T" | "Pi" | "V"
	ESChannelID int
	Factor      float64
	// MinValue/MaxValue are checked AFTER Factor is applied, immediately
	// before writing to ЭС. nil means that side of the safety range is
	// disabled. Both nil = no range check, preserving current behaviour.
	MinValue *float64
	MaxValue *float64
}

// SetVKMChannels replaces every channel mapping for a device in one call
// (delete-then-insert inside a transaction) — the Web UI's "channels"
// form submits the whole 4-row table for a device at once, not one tag
// at a time, so this matches that shape instead of requiring 4 separate
// upsert calls plus a separate "did the operator remove a row" diff.
// FindChannelConflicts проверяет, не заняты ли уже перечисленные номера
// каналов ЭС (ID_Channel) КАКИМ-ТО ДРУГИМ прибором (не тем, для которого
// сейчас сохраняются каналы) — защита от случайной ошибки при ручном
// вводе номера канала: если один и тот же канал ЭС окажется привязан
// сразу к двум разным нашим приборам, данные одного будут затирать
// данные другого в базе Энергосферы, и заметить это сразу непросто.
// Возвращает карту "номер канала -> ID прибора, которому он уже
// принадлежит" — пустая карта означает конфликтов нет.
func (r *Repo) FindChannelConflicts(ctx context.Context, deviceID string, channelIDs []int) (map[int]string, error) {
	conflicts := make(map[int]string)
	if len(channelIDs) == 0 {
		return conflicts, nil
	}

	placeholders := make([]string, len(channelIDs))
	args := make([]any, 0, len(channelIDs)+1)
	for i, ch := range channelIDs {
		placeholders[i] = "?"
		args = append(args, ch)
	}
	args = append(args, deviceID)

	query := fmt.Sprintf(`
SELECT es_channel_id, device_id FROM es_vkm_channels
WHERE es_channel_id IN (%s) AND device_id != ?
`, strings.Join(placeholders, ","))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("find channel conflicts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var chID int
		var otherDeviceID string
		if err := rows.Scan(&chID, &otherDeviceID); err != nil {
			return nil, fmt.Errorf("scan channel conflict: %w", err)
		}
		conflicts[chID] = otherDeviceID
	}
	return conflicts, rows.Err()
}

func (r *Repo) SetVKMChannels(ctx context.Context, deviceID string, rows []VKMChannelRecord) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set vkm channels: begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM es_vkm_channels WHERE device_id = ?`, deviceID); err != nil {
		return fmt.Errorf("set vkm channels: clear existing: %w", err)
	}
	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO es_vkm_channels (device_id, tag, es_channel_id, factor, min_value, max_value)
VALUES (?, ?, ?, ?, ?, ?)
`, deviceID, row.Tag, row.ESChannelID, row.Factor, row.MinValue, row.MaxValue); err != nil {
			return fmt.Errorf("set vkm channels: insert %s: %w", row.Tag, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set vkm channels: commit: %w", err)
	}
	return nil
}

// GetVKMChannels returns the channel mappings configured for a device
// (empty slice, not an error, if none are set yet).
func (r *Repo) GetVKMChannels(ctx context.Context, deviceID string) ([]VKMChannelRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT device_id, tag, es_channel_id, factor, min_value, max_value
FROM es_vkm_channels WHERE device_id = ? ORDER BY tag
`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("get vkm channels: %w", err)
	}
	defer rows.Close()

	var out []VKMChannelRecord
	for rows.Next() {
		var v VKMChannelRecord
		var minValue, maxValue sql.NullFloat64
		if err := rows.Scan(&v.DeviceID, &v.Tag, &v.ESChannelID, &v.Factor, &minValue, &maxValue); err != nil {
			return nil, fmt.Errorf("scan vkm channel: %w", err)
		}
		if minValue.Valid {
			value := minValue.Float64
			v.MinValue = &value
		}
		if maxValue.Valid {
			value := maxValue.Float64
			v.MaxValue = &value
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ESConnection holds the single Энергосфера SQL Server connection every
// ВКМ device's channel mappings are written through.
type ESConnection struct {
	SQLServer   string
	SQLDatabase string
	SQLUser     string
	SQLPassword string
	SQLPort     int
	// TimeShiftMinutes — сдвиг метки времени (в минутах), который
	// применяется к MeasureDate ПЕРЕД записью в Mains. Нужен потому, что
	// Энергосфера раскладывает записи, попавшие в её таблицу напрямую, по
	// своим строкам со сдвигом относительно того времени, что мы пишем
	// (наблюдалось живьём, 2026-08-23: наша последняя точка за период
	// 20:30 оказалась в строке отчёта ЭС за 22:00 — сдвиг в полтора часа).
	// Точная причина на стороне ЭС не установлена (возможно, она хранит
	// время в UTC и показывает с поправкой на свой часовой пояс UTC+4 —
	// он указан в шапке отчётов её родного ПО), поэтому значение сделано
	// НАСТРАИВАЕМЫМ, а не захардкоженным: оператор подбирает его по
	// факту и меняет через UI без пересборки.
	//
	// ВАЖНО: этот сдвиг относится ТОЛЬКО к прямой записи в Mains (путь
	// ВКМ). У Akron путь другой — ЭС сама забирает данные через
	// эмуляцию прибора и раскладывает их по меткам из протокола, там
	// сдвига не наблюдается и эта настройка не применяется.
	TimeShiftMinutes int
}

// SetESConnection upserts the single connection row (id=1 always — see
// the es_connection table's CHECK constraint).
func (r *Repo) SetESConnection(ctx context.Context, c ESConnection) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO es_connection (id, sql_server, sql_database, sql_user, sql_password, sql_port, time_shift_minutes, updated_at)
VALUES (1, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    sql_server = excluded.sql_server,
    sql_database = excluded.sql_database,
    sql_user = excluded.sql_user,
    sql_password = excluded.sql_password,
    sql_port = excluded.sql_port,
    time_shift_minutes = excluded.time_shift_minutes,
    updated_at = excluded.updated_at
`, c.SQLServer, c.SQLDatabase, c.SQLUser, c.SQLPassword, c.SQLPort, c.TimeShiftMinutes, time.Now())
	if err != nil {
		return fmt.Errorf("set es connection: %w", err)
	}
	return nil
}

// GetESConnection returns the configured connection. found=false (nil
// error) if the operator hasn't set it up yet — the Web UI/server should
// treat this as "ES sync not configured", not an error.
func (r *Repo) GetESConnection(ctx context.Context) (ESConnection, bool, error) {
	var c ESConnection
	err := r.db.QueryRowContext(ctx, `
SELECT sql_server, sql_database, sql_user, sql_password, sql_port, time_shift_minutes
FROM es_connection WHERE id = 1
`).Scan(&c.SQLServer, &c.SQLDatabase, &c.SQLUser, &c.SQLPassword, &c.SQLPort, &c.TimeShiftMinutes)
	if err == sql.ErrNoRows {
		return ESConnection{}, false, nil
	}
	if err != nil {
		return ESConnection{}, false, fmt.Errorf("get es connection: %w", err)
	}
	return c, true, nil
}

// SetAkronNorthboundAddr upserts the listen address an Akron device's
// northbound carrier binds to.
func (r *Repo) SetAkronNorthboundAddr(ctx context.Context, deviceID, listenAddr string) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO es_akron_northbound (device_id, listen_addr)
VALUES (?, ?)
ON CONFLICT(device_id) DO UPDATE SET listen_addr = excluded.listen_addr
`, deviceID, listenAddr)
	if err != nil {
		return fmt.Errorf("set akron northbound addr: %w", err)
	}
	return nil
}

// GetAkronNorthboundAddr returns the configured listen address.
// found=false (nil error) if not set yet.
func (r *Repo) GetAkronNorthboundAddr(ctx context.Context, deviceID string) (string, bool, error) {
	var addr string
	err := r.db.QueryRowContext(ctx, `
SELECT listen_addr FROM es_akron_northbound WHERE device_id = ?
`, deviceID).Scan(&addr)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get akron northbound addr: %w", err)
	}
	return addr, true, nil
}
