package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const defaultWatchdogTimeoutMinutes = 10

func (r *Repo) InitAppSettingsSchema(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS app_settings (
    id                       INTEGER PRIMARY KEY CHECK (id = 1),
    configured_port          INTEGER NOT NULL DEFAULT 8080,
    actual_port              INTEGER NOT NULL DEFAULT 0,
    debug_log_enabled        INTEGER NOT NULL DEFAULT 0,
    watchdog_timeout_minutes INTEGER NOT NULL DEFAULT 10,
    updated_at               DATETIME NOT NULL
);
`)
	if err != nil {
		return fmt.Errorf("не удалось инициализировать схему настроек: %w", err)
	}

	// Миграция старой БД c93ae82 и более ранних: ALTER ADD COLUMN не имеет
	// IF NOT EXISTS в целевой SQLite, поэтому duplicate column безопасно
	// игнорируем, остальные ошибки возвращаем.
	if _, err := r.db.ExecContext(ctx, `ALTER TABLE app_settings ADD COLUMN watchdog_timeout_minutes INTEGER NOT NULL DEFAULT 10`); err != nil &&
		!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return fmt.Errorf("не удалось добавить настройку таймаута контроля зависания: %w", err)
	}
	return nil
}

type AppSettings struct {
	ConfiguredPort         int
	ActualPort             int
	DebugLogEnabled        bool
	WatchdogTimeoutMinutes int
}

func (r *Repo) GetAppSettings(ctx context.Context) (AppSettings, bool, error) {
	var s AppSettings
	var debugLog int
	err := r.db.QueryRowContext(ctx, `
SELECT configured_port, actual_port, debug_log_enabled, watchdog_timeout_minutes
FROM app_settings WHERE id = 1
`).Scan(&s.ConfiguredPort, &s.ActualPort, &debugLog, &s.WatchdogTimeoutMinutes)
	if err == sql.ErrNoRows {
		return AppSettings{}, false, nil
	}
	if err != nil {
		return AppSettings{}, false, fmt.Errorf("не удалось прочитать настройки приложения: %w", err)
	}
	s.DebugLogEnabled = debugLog != 0
	if s.WatchdogTimeoutMinutes <= 0 {
		s.WatchdogTimeoutMinutes = defaultWatchdogTimeoutMinutes
	}
	return s, true, nil
}

func (r *Repo) SetConfiguredPort(ctx context.Context, port int) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO app_settings (id, configured_port, actual_port, debug_log_enabled, watchdog_timeout_minutes, updated_at)
VALUES (1, ?, 0, 0, ?, ?)
ON CONFLICT(id) DO UPDATE SET configured_port = excluded.configured_port, updated_at = excluded.updated_at
`, port, defaultWatchdogTimeoutMinutes, time.Now())
	if err != nil {
		return fmt.Errorf("не удалось сохранить настроенный порт: %w", err)
	}
	return nil
}

func (r *Repo) SetActualPort(ctx context.Context, port int) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO app_settings (id, configured_port, actual_port, debug_log_enabled, watchdog_timeout_minutes, updated_at)
VALUES (1, ?, ?, 0, ?, ?)
ON CONFLICT(id) DO UPDATE SET actual_port = excluded.actual_port, updated_at = excluded.updated_at
`, port, port, defaultWatchdogTimeoutMinutes, time.Now())
	if err != nil {
		return fmt.Errorf("не удалось сохранить фактический порт: %w", err)
	}
	return nil
}

func (r *Repo) SetDebugLogEnabled(ctx context.Context, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO app_settings (id, configured_port, actual_port, debug_log_enabled, watchdog_timeout_minutes, updated_at)
VALUES (1, 8080, 0, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET debug_log_enabled = excluded.debug_log_enabled, updated_at = excluded.updated_at
`, v, defaultWatchdogTimeoutMinutes, time.Now())
	if err != nil {
		return fmt.Errorf("не удалось сохранить настройку отладочного лога: %w", err)
	}
	return nil
}

func (r *Repo) SetWatchdogTimeoutMinutes(ctx context.Context, minutes int) error {
	if minutes <= 0 {
		minutes = defaultWatchdogTimeoutMinutes
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO app_settings (id, configured_port, actual_port, debug_log_enabled, watchdog_timeout_minutes, updated_at)
VALUES (1, 8080, 0, 0, ?, ?)
ON CONFLICT(id) DO UPDATE SET watchdog_timeout_minutes = excluded.watchdog_timeout_minutes, updated_at = excluded.updated_at
`, minutes, time.Now())
	if err != nil {
		return fmt.Errorf("не удалось сохранить таймаут контроля зависания: %w", err)
	}
	return nil
}
