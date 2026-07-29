package storage

import (
	"context"
	"time"
)

// Repo is the storage contract used by the polling core. It was extended
// for M4 (the upstream Akron carrier) with:
//   - hourly-archive methods: the northbound serves command 104 out of this
//     store, and the downstream poller (device.PollArchives) fills it;
//   - device-passport methods: the northbound answers command 101 (identity)
//     from the passport, which the poller reads once at startup.
//
// *sqlite.Repo implements all of these (see repo_archive.go / repo_passport.go),
// so widening the interface here does not break the build.
type Repo interface {
	InitSchema(ctx context.Context) error
	SaveReadingCurrent(ctx context.Context, r ReadingCurrent) error
	GetLatestReadings(ctx context.Context, deviceID string) ([]ReadingCurrent, error)

	// Hourly archive store (M4). See HourlyArchiveRecord for field semantics
	// (device-agnostic: Channel/Param select the stream).
	InitArchiveSchema(ctx context.Context) error
	SaveHourlyArchive(ctx context.Context, rec HourlyArchiveRecord) error
	// GetHourlyArchiveDesc returns rows newest-first (offset=i-1, limit=n
	// maps a command-104 (i, n) request directly).
	GetHourlyArchiveDesc(ctx context.Context, deviceID, channel, param string, offset, limit int) ([]HourlyArchiveRecord, error)
	CountHourlyArchive(ctx context.Context, deviceID, channel, param string) (int, error)
	// LatestHourlyArchiveTS reports the newest ts_hour stored for the
	// stream (found=false, nil error when the store holds nothing yet).
	// Backfill uses it to decide how far back to reach in variant "В":
	// grab everything newer than what we already have.
	LatestHourlyArchiveTS(ctx context.Context, deviceID, channel, param string) (ts time.Time, found bool, err error)
	// MissingHours returns the whole-hour timestamps in [fromHour, toHour]
	// that have NO row for the stream — the gaps a periodic gap-scan
	// should patch. Both bounds are inclusive and must already be
	// truncated to the hour by the caller.
	MissingHours(ctx context.Context, deviceID, channel, param string, fromHour, toHour time.Time) ([]time.Time, error)

	// Device passport store (M4). Static identity read once at startup and
	// served upstream on command 101. GetDevicePassport reports found=false
	// (nil error) when the device has not been identified yet.
	InitPassportSchema(ctx context.Context) error
	SaveDevicePassport(ctx context.Context, p DevicePassport) error
	GetDevicePassport(ctx context.Context, deviceID string) (p DevicePassport, found bool, err error)
}
