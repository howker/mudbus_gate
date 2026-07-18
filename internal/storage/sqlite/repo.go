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
	// _busy_timeout makes SQLite wait and retry internally instead of
	// immediately returning SQLITE_BUSY when another writer holds the lock.
	// journal_mode=WAL reduces reader/writer contention. SetMaxOpenConns(1)
	// serializes all access through a single connection, which is the
	// simplest correct way to avoid concurrent-writer lock errors when
	// multiple device goroutines write to the same SQLite file from one
	// process (see: two devices polling in parallel caused
	// "database is locked (SQLITE_BUSY)" once a second device was added).
	dsn := path + "?_busy_timeout=5000&_journal_mode=WAL"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
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
