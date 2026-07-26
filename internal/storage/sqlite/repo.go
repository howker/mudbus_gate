package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"

	"mbgw/internal/storage"
)

type Repo struct {
	db *sql.DB
}

func New(path string) (*Repo, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	// PRAGMAs are set via an explicit exec, not DSN query parameters
	// (`?_busy_timeout=...&_journal_mode=...`), because that DSN syntax
	// is mattn/go-sqlite3's convention — this project uses
	// modernc.org/sqlite (pure Go, no cgo), which does not necessarily
	// recognize the same query-parameter names, and unrecognized DSN
	// keys are typically ignored rather than erroring. That silent
	// ignoring is exactly what caused "database is locked (SQLITE_BUSY)"
	// errors to appear IMMEDIATELY (no retry delay) once a second
	// process (the northbound carrier) started reading the same file
	// while the southbound poller was writing to it — the busy_timeout
	// was never actually taking effect. Executing PRAGMA statements
	// directly against the open connection works reliably regardless of
	// driver-specific DSN parsing quirks.
	//
	// journal_mode=WAL lets readers and a writer proceed concurrently
	// (the normal DELETE journal mode locks the whole file for any
	// write); busy_timeout makes a connection that DOES hit a lock wait
	// and retry internally for up to 5s instead of failing immediately.
	// SetMaxOpenConns(1) above still serializes access WITHIN this one
	// process (matters when multiple devices poll in parallel); WAL +
	// busy_timeout is what makes two SEPARATE mbgw.exe processes (e.g.
	// `mbgw run` and `mbgw northbound --serve-akron`) sharing one
	// mbgw.db file work correctly together.
	if _, err := db.ExecContext(context.Background(), "PRAGMA journal_mode=WAL;"); err != nil {
		return nil, fmt.Errorf("set journal_mode=WAL: %w", err)
	}
	if _, err := db.ExecContext(context.Background(), "PRAGMA busy_timeout=5000;"); err != nil {
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}

	return &Repo{db: db}, nil
}

func (r *Repo) InitSchema(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS readings_current (
device_id TEXT NOT NULL,
point_id TEXT NOT NULL,
instance TEXT NOT NULL DEFAULT '',
value_text TEXT,
unit TEXT,
quality TEXT,
quality_reason TEXT,
ts DATETIME NOT NULL,
PRIMARY KEY (device_id, point_id, instance)
);

CREATE TABLE IF NOT EXISTS readings_history (
id INTEGER PRIMARY KEY AUTOINCREMENT,
device_id TEXT NOT NULL,
point_id TEXT NOT NULL,
instance TEXT NOT NULL DEFAULT '',
value_text TEXT,
unit TEXT,
quality TEXT,
quality_reason TEXT,
ts DATETIME NOT NULL
);
`)
	if err != nil {
		return err
	}
	// M4: create the hourly-archive and device-passport tables too, so a
	// single InitSchema call at startup provisions the full schema. Both
	// are idempotent (CREATE IF NOT EXISTS) and also callable directly in
	// tests that only need one store.
	if err := r.InitArchiveSchema(ctx); err != nil {
		return err
	}
	if err := r.InitPassportSchema(ctx); err != nil {
		return err
	}
	return nil
}

func (r *Repo) SaveReadingCurrent(ctx context.Context, rd storage.ReadingCurrent) error {
	valueText := fmt.Sprint(rd.Value)

	_, err := r.db.ExecContext(ctx, `
INSERT INTO readings_current (
device_id, point_id, instance, value_text, unit, quality, quality_reason, ts
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(device_id, point_id, instance) DO UPDATE SET
value_text = excluded.value_text,
unit = excluded.unit,
quality = excluded.quality,
quality_reason = excluded.quality_reason,
ts = excluded.ts
`, rd.DeviceID, rd.PointID, rd.Instance, valueText, rd.Unit, rd.Quality, rd.QualityReason, rd.Timestamp)
	if err != nil {
		return err
	}

	_, err = r.db.ExecContext(ctx, `
INSERT INTO readings_history (
device_id, point_id, instance, value_text, unit, quality, quality_reason, ts
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`, rd.DeviceID, rd.PointID, rd.Instance, valueText, rd.Unit, rd.Quality, rd.QualityReason, rd.Timestamp)
	return err
}

func (r *Repo) GetLatestReadings(ctx context.Context, deviceID string) ([]storage.ReadingCurrent, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT device_id, point_id, instance, value_text, unit, quality, quality_reason, ts
FROM readings_current
WHERE (? = '' OR device_id = ?)
ORDER BY device_id, point_id, instance
`, deviceID, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []storage.ReadingCurrent
	for rows.Next() {
		var rd storage.ReadingCurrent
		var valueText string
		if err := rows.Scan(
			&rd.DeviceID,
			&rd.PointID,
			&rd.Instance,
			&valueText,
			&rd.Unit,
			&rd.Quality,
			&rd.QualityReason,
			&rd.Timestamp,
		); err != nil {
			return nil, err
		}
		rd.Value = valueText
		out = append(out, rd)
	}

	return out, rows.Err()
}
