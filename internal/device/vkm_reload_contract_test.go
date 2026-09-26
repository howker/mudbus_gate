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

func TestForceReloadVKMContinuesPastNoRecords(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2026, 9, 26, 9, 0, 0, 0, time.Local)
	to := from.Add(2 * vkmArchivePeriod)
	var calls []time.Time
	progress := 0

	saved, err := forceReloadVKMPeriods(ctx, []int{1}, from, to,
		func(_ context.Context, _ int, periodStart time.Time) (int, error) {
			calls = append(calls, periodStart)
			if periodStart.Equal(from.Add(vkmArchivePeriod)) {
				return 0, archive.ErrVKMNoRecords
			}
			return 4, nil
		},
		func(done, total int) {
			progress = done
			if total != 3 {
				t.Fatalf("total=%d want 3", total)
			}
		})
	if err != nil {
		t.Fatalf("no-record period must not abort range: %v", err)
	}
	if len(calls) != 3 || progress != 3 {
		t.Fatalf("calls=%v progress=%d, want all 3 periods", calls, progress)
	}
	if saved != 8 {
		t.Fatalf("saved=%d want 8", saved)
	}
}
