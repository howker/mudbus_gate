package storage

import "context"

// Repo is the storage contract used by the polling core. It was extended
// for M4 (the upstream Akron carrier) with the hourly-archive methods
// below: the northbound serves command 104 out of this store, and the
// downstream poller (device.PollArchives) fills it. *sqlite.Repo already
// implements these (see internal/storage/sqlite/repo_archive.go), so
// widening the interface here does not break the build.
type Repo interface {
	InitSchema(ctx context.Context) error
	SaveReadingCurrent(ctx context.Context, r ReadingCurrent) error
	GetLatestReadings(ctx context.Context, deviceID string) ([]ReadingCurrent, error)

	// Hourly archive store (M4). See HourlyArchiveRecord for the field
	// semantics (device-agnostic: Channel/Param select the stream).
	InitArchiveSchema(ctx context.Context) error
	SaveHourlyArchive(ctx context.Context, rec HourlyArchiveRecord) error
	// GetHourlyArchiveDesc returns rows newest-first (offset=i-1, limit=n
	// maps a command-104 (i, n) request directly).
	GetHourlyArchiveDesc(ctx context.Context, deviceID, channel, param string, offset, limit int) ([]HourlyArchiveRecord, error)
	CountHourlyArchive(ctx context.Context, deviceID, channel, param string) (int, error)
}
