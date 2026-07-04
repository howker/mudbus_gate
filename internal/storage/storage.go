package storage

import "context"

type Repo interface {
InitSchema(ctx context.Context) error
SaveReadingCurrent(ctx context.Context, r ReadingCurrent) error
GetLatestReadings(ctx context.Context, deviceID string) ([]ReadingCurrent, error)
}
