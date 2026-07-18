package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"mbgw/internal/storage"
)

// This file adds the device-passport store to Repo (methods on *Repo in a
// separate file, same package — repo.go is not edited). See
// storage.DevicePassport for why this is its own table rather than a row
// in readings_current.

// InitPassportSchema creates the passport store. Call once at startup,
// alongside InitSchema / InitArchiveSchema.
func (r *Repo) InitPassportSchema(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS device_passport (
    device_id   TEXT NOT NULL PRIMARY KEY,
    serial      INTEGER NOT NULL,
    device_type INTEGER NOT NULL,
    firmware    TEXT NOT NULL DEFAULT '',
    updated_at  DATETIME NOT NULL
);
`)
	if err != nil {
		return fmt.Errorf("init passport schema: %w", err)
	}
	return nil
}

// SaveDevicePassport upserts the passport for one device. Re-reading a
// device's identity at a later startup overwrites the previous record
// (device_id is the natural key) — a device only ever has one identity.
func (r *Repo) SaveDevicePassport(ctx context.Context, p storage.DevicePassport) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO device_passport (device_id, serial, device_type, firmware, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET
    serial      = excluded.serial,
    device_type = excluded.device_type,
    firmware    = excluded.firmware,
    updated_at  = excluded.updated_at
`, p.DeviceID, p.Serial, int(p.DeviceType), p.Firmware, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("save device passport: %w", err)
	}
	return nil
}

// GetDevicePassport looks up one device's passport. found=false (with a nil
// error) means there is simply no passport yet — the device hasn't been
// identified since startup. A non-nil error means the query itself failed
// (a real storage problem), which the caller should treat differently from
// "not identified yet".
func (r *Repo) GetDevicePassport(ctx context.Context, deviceID string) (p storage.DevicePassport, found bool, err error) {
	var deviceType int
	row := r.db.QueryRowContext(ctx, `
SELECT device_id, serial, device_type, firmware, updated_at
FROM device_passport
WHERE device_id = ?
`, deviceID)

	scanErr := row.Scan(&p.DeviceID, &p.Serial, &deviceType, &p.Firmware, &p.UpdatedAt)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return storage.DevicePassport{}, false, nil
	}
	if scanErr != nil {
		return storage.DevicePassport{}, false, fmt.Errorf("get device passport: %w", scanErr)
	}
	p.DeviceType = byte(deviceType)
	return p, true, nil
}
