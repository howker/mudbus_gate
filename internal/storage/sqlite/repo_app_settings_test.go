package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

func TestInitAppSettingsSchemaMigratesLegacyRowAndPreservesSettings(t *testing.T) {
	repo, err := New(filepath.Join(t.TempDir(), "legacy_settings.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	ctx := context.Background()

	// Схема до пакета 08.09: колонки watchdog_timeout_minutes ещё нет.
	if _, err := repo.db.ExecContext(ctx, `
CREATE TABLE app_settings (
    id                INTEGER PRIMARY KEY CHECK (id = 1),
    configured_port   INTEGER NOT NULL DEFAULT 8080,
    actual_port       INTEGER NOT NULL DEFAULT 0,
    debug_log_enabled INTEGER NOT NULL DEFAULT 0,
    updated_at        DATETIME NOT NULL
);
INSERT INTO app_settings(id, configured_port, actual_port, debug_log_enabled, updated_at)
VALUES (1, 9090, 9091, 1, CURRENT_TIMESTAMP);
`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	if err := repo.InitAppSettingsSchema(ctx); err != nil {
		t.Fatalf("migrate settings: %v", err)
	}
	settings, found, err := repo.GetAppSettings(ctx)
	if err != nil || !found {
		t.Fatalf("get migrated settings: found=%v err=%v", found, err)
	}
	if settings.ConfiguredPort != 9090 || settings.ActualPort != 9091 || !settings.DebugLogEnabled {
		t.Fatalf("legacy settings changed during migration: %+v", settings)
	}
	if settings.WatchdogTimeoutMinutes != defaultWatchdogTimeoutMinutes {
		t.Fatalf("watchdog timeout = %d, want %d", settings.WatchdogTimeoutMinutes, defaultWatchdogTimeoutMinutes)
	}

	if err := repo.SetWatchdogTimeoutMinutes(ctx, 17); err != nil {
		t.Fatalf("set watchdog timeout: %v", err)
	}
	settings, found, err = repo.GetAppSettings(ctx)
	if err != nil || !found {
		t.Fatalf("get settings after update: found=%v err=%v", found, err)
	}
	if settings.WatchdogTimeoutMinutes != 17 {
		t.Fatalf("watchdog timeout = %d, want 17", settings.WatchdogTimeoutMinutes)
	}
	if settings.ConfiguredPort != 9090 || settings.ActualPort != 9091 || !settings.DebugLogEnabled {
		t.Fatalf("updating watchdog changed unrelated settings: %+v", settings)
	}
}
