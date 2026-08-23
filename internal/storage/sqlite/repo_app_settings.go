package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// This file adds a single-row app_settings table — process-wide settings
// the operator changes at runtime through the Web UI (web port, debug
// logging), as opposed to per-device configuration (repo_device_config.go).
//
// WHY web port lives in the DB, not just a command-line flag: cmd/mbgw/
// server.go is meant to run as a Windows Service (see service_windows.go),
// whose command line is baked in ONCE at install time by sc.exe and not
// something the operator retypes. For "change the port from the Web UI at
// any time" to actually work across service restarts, the port has to be
// read from somewhere the UI can write to — this table — not solely from
// a flag frozen into the service definition. See server.go's startup
// logic for the exact precedence rule (DB wins once a row exists; the
// --port flag only seeds the very first bootstrap row).

// InitAppSettingsSchema creates the app_settings table. Call once at
// startup alongside the other Init*Schema calls.
func (r *Repo) InitAppSettingsSchema(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS app_settings (
    id                INTEGER PRIMARY KEY CHECK (id = 1),  -- enforces single row
    configured_port   INTEGER NOT NULL DEFAULT 8080,  -- operator's chosen port (via UI or bootstrap)
    actual_port       INTEGER NOT NULL DEFAULT 0,     -- what the CURRENT process actually bound to
                                                        -- (may differ from configured_port if that one
                                                        -- was occupied at startup and a fallback was
                                                        -- picked automatically — see server.go)
    debug_log_enabled INTEGER NOT NULL DEFAULT 0,      -- 0/1: verbose raw-byte diagnostic logging
    updated_at        DATETIME NOT NULL
);
`)
	if err != nil {
		return fmt.Errorf("init app settings schema: %w", err)
	}
	return nil
}

// AppSettings holds the process-wide operator-configurable settings.
type AppSettings struct {
	ConfiguredPort  int
	ActualPort      int
	DebugLogEnabled bool
}

// GetAppSettings returns the current settings. found=false (nil error)
// if the row doesn't exist yet (very first run before any bootstrap) —
// callers should treat this as "use built-in defaults", not an error.
func (r *Repo) GetAppSettings(ctx context.Context) (AppSettings, bool, error) {
	var s AppSettings
	var debugLog int
	err := r.db.QueryRowContext(ctx, `
SELECT configured_port, actual_port, debug_log_enabled FROM app_settings WHERE id = 1
`).Scan(&s.ConfiguredPort, &s.ActualPort, &debugLog)
	if err == sql.ErrNoRows {
		return AppSettings{}, false, nil
	}
	if err != nil {
		return AppSettings{}, false, fmt.Errorf("get app settings: %w", err)
	}
	s.DebugLogEnabled = debugLog != 0
	return s, true, nil
}

// SetConfiguredPort upserts just the operator-chosen port (what the Web
// UI's "Настройки" tab writes) — leaves actual_port/debug_log_enabled
// alone if the row already exists (partial update, not a full replace),
// since the UI's port field and debug-log checkbox are saved
// independently of each other.
func (r *Repo) SetConfiguredPort(ctx context.Context, port int) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO app_settings (id, configured_port, actual_port, debug_log_enabled, updated_at)
VALUES (1, ?, 0, 0, ?)
ON CONFLICT(id) DO UPDATE SET configured_port = excluded.configured_port, updated_at = excluded.updated_at
`, port, time.Now())
	if err != nil {
		return fmt.Errorf("set configured port: %w", err)
	}
	return nil
}

// SetActualPort records what the current process actually bound to —
// called once at startup after a successful bind, so the Web UI can show
// "настроенный порт: 8080, реально запущен на: 8081" if a conflict was
// auto-resolved (see server.go's port-picking logic).
func (r *Repo) SetActualPort(ctx context.Context, port int) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO app_settings (id, configured_port, actual_port, debug_log_enabled, updated_at)
VALUES (1, ?, ?, 0, ?)
ON CONFLICT(id) DO UPDATE SET actual_port = excluded.actual_port, updated_at = excluded.updated_at
`, port, port, time.Now())
	if err != nil {
		return fmt.Errorf("set actual port: %w", err)
	}
	return nil
}

// SetDebugLogEnabled upserts just the debug-logging toggle.
func (r *Repo) SetDebugLogEnabled(ctx context.Context, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO app_settings (id, configured_port, actual_port, debug_log_enabled, updated_at)
VALUES (1, 8080, 0, ?, ?)
ON CONFLICT(id) DO UPDATE SET debug_log_enabled = excluded.debug_log_enabled, updated_at = excluded.updated_at
`, v, time.Now())
	if err != nil {
		return fmt.Errorf("set debug log enabled: %w", err)
	}
	return nil
}
