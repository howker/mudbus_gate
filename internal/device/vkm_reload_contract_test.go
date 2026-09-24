package device

import (
	"context"
	"testing"
	"time"

	"mbgw/internal/archive"
)

func TestPersistVKMHourlyOverwritesExistingPeriod(t *testing.T) {
	d := newTestDeviceForVKM(t)
	ts := time.Date(2026, 9, 8, 12, 30, 0, 0, time.Local)

	oldRec := archive.ArchiveRecord{Fields: map[string]any{
		"S":  10.0,
		"ST": 20.0,
		"T":  30.0,
		"Pi": 40.0,
	}}
	newRec := archive.ArchiveRecord{Fields: map[string]any{
		"S":  11.0,
		"ST": 21.0,
		"T":  31.0,
		"Pi": 41.0,
	}}

	if got := persistVKMHourly(context.Background(), d, 1, ts, oldRec); got != 4 {
		t.Fatalf("first persist saved=%d, want 4", got)
	}
	if got := persistVKMHourly(context.Background(), d, 1, ts, newRec); got != 4 {
		t.Fatalf("second persist saved=%d, want 4", got)
	}

	rows, err := d.Repo.GetHourlyArchiveDesc(context.Background(), d.ID, "", "S", 0, 10)
	if err != nil {
		t.Fatalf("read back S: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("S rows=%d, want exactly 1 after UPSERT", len(rows))
	}
	if rows[0].Value != 11.0 {
		t.Fatalf("S value=%v, want overwritten value 11", rows[0].Value)
	}
	if !rows[0].TsHour.Equal(ts) {
		t.Fatalf("S timestamp=%v, want %v", rows[0].TsHour, ts)
	}
}
