package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mbgw/internal/storage"
)

func TestHourlyArchiveDirtyRangeIsDurableVersionedAndRequeuedOnSuccessfulRefresh(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dirty.db")

	r, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.InitArchiveSchema(ctx); err != nil {
		t.Fatal(err)
	}

	ts := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	rec := storage.HourlyArchiveRecord{
		DeviceID: "osmos",
		Param:    "V",
		TsHour:   ts,
		Value:    100,
		Unit:     "m3",
	}

	if err := r.SaveHourlyArchive(ctx, rec); err != nil {
		t.Fatal(err)
	}
	ranges, err := r.ListESDirtyRanges(ctx, "osmos", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 {
		t.Fatalf("want 1 dirty range, got %d: %#v", len(ranges), ranges)
	}
	first := ranges[0]
	if !first.From.Equal(ts) || !first.To.Equal(ts.Add(time.Hour)) || first.Version != 1 {
		t.Fatalf("unexpected first dirty range: %#v", first)
	}

	// A successful forced refresh must re-dirty even when the local value is
	// unchanged: PointMains can have an independent gap behind its cursor.
	if err := r.SaveHourlyArchive(ctx, rec); err != nil {
		t.Fatal(err)
	}
	ranges, err = r.ListESDirtyRanges(ctx, "osmos", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 || ranges[0].Version != 2 {
		t.Fatalf("successful identical refresh must re-dirty: %#v", ranges)
	}

	// A real source correction re-dirties the same range again. A worker
	// holding the old token must therefore be unable to delete the new request.
	rec.Value = 101
	if err := r.SaveHourlyArchive(ctx, rec); err != nil {
		t.Fatal(err)
	}
	ranges, err = r.ListESDirtyRanges(ctx, "osmos", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 || ranges[0].Version != 3 {
		t.Fatalf("changed save must bump dirty version: %#v", ranges)
	}
	if completed, err := r.CompleteESDirtyRange(ctx, first); err != nil {
		t.Fatal(err)
	} else if completed {
		t.Fatal("stale dirty token must not delete a re-dirtied range")
	}

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// Queue survives process/repository restart.
	r, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := r.InitArchiveSchema(ctx); err != nil {
		t.Fatal(err)
	}
	ranges, err = r.ListESDirtyRanges(ctx, "osmos", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 || ranges[0].Version != 3 {
		t.Fatalf("dirty range did not survive reopen: %#v", ranges)
	}

	completed, err := r.CompleteESDirtyRange(ctx, ranges[0])
	if err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatal("current dirty token must complete")
	}
	n, err := r.CountESDirtyRanges(ctx, "osmos")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("want empty dirty queue after completion, got %d", n)
	}
}

func TestVKMRawChangeQueuesImmediateDirtySignal(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	ch := r.ESDirtySignal("vkm")

	ts := time.Date(2026, 9, 24, 13, 30, 0, 0, time.Local)
	if err := r.SaveVKMRawString(ctx, "vkm", 1, ts, "S=12.5;"); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ch:
	default:
		t.Fatal("changed local archive must wake the ES worker immediately")
	}

	ranges, err := r.ListESDirtyRanges(ctx, "vkm", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 {
		t.Fatalf("want one VKM dirty range, got %d", len(ranges))
	}
}

func TestDirtyRangesMergeOverlappingArchivePeriods(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)

	t13 := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	save := func(ts time.Time, value float64) {
		t.Helper()
		if err := r.SaveHourlyArchive(ctx, storage.HourlyArchiveRecord{
			DeviceID: "akron_merge",
			Param:    "V",
			TsHour:   ts,
			Value:    value,
			Unit:     "m3",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Each save marks [ts, ts+1h]. Touching ranges must collapse into one
	// durable interval, so a long backfill does not create thousands of rows.
	save(t13, 100)
	save(t13.Add(time.Hour), 110)
	save(t13.Add(2*time.Hour), 120)

	ranges, err := r.ListESDirtyRanges(ctx, "akron_merge", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 {
		t.Fatalf("want one merged range, got %d: %#v", len(ranges), ranges)
	}
	if !ranges[0].From.Equal(t13) || !ranges[0].To.Equal(t13.Add(3*time.Hour)) {
		t.Fatalf("unexpected merged bounds: %#v", ranges[0])
	}

	// A genuinely disjoint repair stays separate.
	save(t13.Add(5*time.Hour), 150)
	ranges, err = r.ListESDirtyRanges(ctx, "akron_merge", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 2 {
		t.Fatalf("want two disjoint ranges, got %d: %#v", len(ranges), ranges)
	}
}
