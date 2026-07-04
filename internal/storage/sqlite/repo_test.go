package sqlite

import (
"context"
"path/filepath"
"testing"
"time"

"mbgw/internal/storage"
)

func TestRepo_SaveAndGetLatestReadings(t *testing.T) {
ctx := context.Background()
dbPath := filepath.Join(t.TempDir(), "test.sqlite")

repo, err := New(dbPath)
if err != nil {
t.Fatalf("New(): %v", err)
}
t.Cleanup(func() {
_ = repo.db.Close()
})

if err := repo.InitSchema(ctx); err != nil {
t.Fatalf("InitSchema(): %v", err)
}

ts := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
in := storage.ReadingCurrent{
DeviceID:      "dev1",
PointID:       "mass_flow",
Instance:      "",
Value:         123.4567,
Unit:          "kg/s",
Quality:       "VALID",
QualityReason: "",
Timestamp:     ts,
}

if err := repo.SaveReadingCurrent(ctx, in); err != nil {
t.Fatalf("SaveReadingCurrent(): %v", err)
}

got, err := repo.GetLatestReadings(ctx, "dev1")
if err != nil {
t.Fatalf("GetLatestReadings(): %v", err)
}

if len(got) != 1 {
t.Fatalf("len(got) = %d, want 1", len(got))
}

if got[0].DeviceID != in.DeviceID {
t.Fatalf("DeviceID = %q, want %q", got[0].DeviceID, in.DeviceID)
}
if got[0].PointID != in.PointID {
t.Fatalf("PointID = %q, want %q", got[0].PointID, in.PointID)
}
if got[0].Unit != in.Unit {
t.Fatalf("Unit = %q, want %q", got[0].Unit, in.Unit)
}
if got[0].Quality != in.Quality {
t.Fatalf("Quality = %q, want %q", got[0].Quality, in.Quality)
}
if got[0].Value != "123.4567" {
t.Fatalf("Value = %#v, want %q", got[0].Value, "123.4567")
}
}
