package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mbgw/internal/storage"
)

func newTestRepo(t *testing.T) *Repo {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	r, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.db.Close() })
	ctx := context.Background()
	if err := r.InitArchiveSchema(ctx); err != nil {
		t.Fatalf("InitArchiveSchema: %v", err)
	}
	return r
}

// Insert 5 consecutive hours (oldest..newest) for one Akron stream.
func seedHours(t *testing.T, r *Repo, base time.Time) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		rec := storage.HourlyArchiveRecord{
			DeviceID: "akron1",
			Channel:  "",
			Param:    "V",
			TsHour:   base.Add(time.Duration(i) * time.Hour),
			Value:    582.7 + float64(i),
			Unit:     "m3",
		}
		if err := r.SaveHourlyArchive(ctx, rec); err != nil {
			t.Fatalf("SaveHourlyArchive[%d]: %v", i, err)
		}
	}
}

func TestHourlyArchive_NewestFirst_IndexMapping(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	base := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	seedHours(t, r, base)

	// i=1 (offset 0) must be the most recent hour (14:00), value 586.7.
	got, err := r.GetHourlyArchiveDesc(ctx, "akron1", "", "V", 0, 1)
	if err != nil {
		t.Fatalf("GetHourlyArchiveDesc: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	wantTop := base.Add(4 * time.Hour)
	if !got[0].TsHour.Equal(wantTop) {
		t.Fatalf("top row ts = %v, want %v", got[0].TsHour, wantTop)
	}
	if got[0].Value != 586.7 {
		t.Fatalf("top row value = %v, want 586.7", got[0].Value)
	}

	// offset 2, limit 2 → 3rd and 4th newest = 12:00 then 11:00, still DESC.
	got, err = r.GetHourlyArchiveDesc(ctx, "akron1", "", "V", 2, 2)
	if err != nil {
		t.Fatalf("GetHourlyArchiveDesc offset: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	if !got[0].TsHour.Equal(base.Add(2 * time.Hour)) {
		t.Fatalf("row[0] ts = %v, want %v", got[0].TsHour, base.Add(2*time.Hour))
	}
	if !got[1].TsHour.Equal(base.Add(1 * time.Hour)) {
		t.Fatalf("row[1] ts = %v, want %v", got[1].TsHour, base.Add(1*time.Hour))
	}
}

func TestHourlyArchive_UpsertNoDup(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	base := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	seedHours(t, r, base)

	// Re-collect the top hour with a new value: must update in place.
	top := base.Add(4 * time.Hour)
	if err := r.SaveHourlyArchive(ctx, storage.HourlyArchiveRecord{
		DeviceID: "akron1", Channel: "", Param: "V",
		TsHour: top, Value: 999.9, Unit: "m3",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	n, err := r.CountHourlyArchive(ctx, "akron1", "", "V")
	if err != nil {
		t.Fatalf("CountHourlyArchive: %v", err)
	}
	if n != 5 {
		t.Fatalf("want 5 rows after upsert, got %d", n)
	}

	got, err := r.GetHourlyArchiveDesc(ctx, "akron1", "", "V", 0, 1)
	if err != nil {
		t.Fatalf("GetHourlyArchiveDesc: %v", err)
	}
	if got[0].Value != 999.9 {
		t.Fatalf("upsert value = %v, want 999.9", got[0].Value)
	}
}

func TestHourlyArchive_StreamIsolation(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	base := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	seedHours(t, r, base)

	// A different device/channel/param must not leak into the Akron stream.
	if err := r.SaveHourlyArchive(ctx, storage.HourlyArchiveRecord{
		DeviceID: "vkm1", Channel: "ТП1", Param: "E",
		TsHour: base, Value: 42, Unit: "Gcal",
	}); err != nil {
		t.Fatalf("save other stream: %v", err)
	}

	n, err := r.CountHourlyArchive(ctx, "akron1", "", "V")
	if err != nil {
		t.Fatalf("CountHourlyArchive: %v", err)
	}
	if n != 5 {
		t.Fatalf("akron stream leaked: want 5, got %d", n)
	}

	// Asking past the end returns what's available, not an error.
	got, err := r.GetHourlyArchiveDesc(ctx, "akron1", "", "V", 4, 10)
	if err != nil {
		t.Fatalf("tail read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("tail read want 1 row (oldest), got %d", len(got))
	}
	if !got[0].TsHour.Equal(base) {
		t.Fatalf("tail row ts = %v, want %v (oldest)", got[0].TsHour, base)
	}

	// limit<=0 is a well-defined no-op.
	got, err = r.GetHourlyArchiveDesc(ctx, "akron1", "", "V", 0, 0)
	if err != nil {
		t.Fatalf("zero-limit: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("zero-limit want 0 rows, got %d", len(got))
	}
}

// TestLatestHourlyArchiveTS covers the empty-store case (found=false, the
// freshly-installed-meter path backfill must not treat as an error) and the
// populated case (returns the newest hour, which variant "В" uses as its
// "don't reach past this" mark).
func TestLatestHourlyArchiveTS(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)

	if _, found, err := r.LatestHourlyArchiveTS(ctx, "akron1", "", "V"); err != nil || found {
		t.Fatalf("empty store: want found=false nil err, got found=%v err=%v", found, err)
	}

	base := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	seedHours(t, r, base) // 5 hours, base..base+4h

	ts, found, err := r.LatestHourlyArchiveTS(ctx, "akron1", "", "V")
	if err != nil || !found {
		t.Fatalf("populated: want found=true nil err, got found=%v err=%v", found, err)
	}
	want := base.Add(4 * time.Hour)
	if !ts.Equal(want) {
		t.Fatalf("latest ts = %v, want %v", ts, want)
	}
}

// TestMissingHours verifies the gap list a periodic gap-scan drives from:
// hours with no row inside the inclusive window are returned, present ones
// are not.
func TestMissingHours(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)

	base := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	// Seed hours 0..4, then deliberately leave 5 and 6 missing, add 7.
	seedHours(t, r, base)
	rec := storage.HourlyArchiveRecord{
		DeviceID: "akron1", Channel: "", Param: "V",
		TsHour: base.Add(7 * time.Hour), Value: 999, Unit: "m3",
	}
	if err := r.SaveHourlyArchive(ctx, rec); err != nil {
		t.Fatalf("seed hour 7: %v", err)
	}

	missing, err := r.MissingHours(ctx, "akron1", "", "V", base, base.Add(7*time.Hour))
	if err != nil {
		t.Fatalf("MissingHours: %v", err)
	}
	if len(missing) != 2 {
		t.Fatalf("want 2 missing hours (5,6), got %d: %v", len(missing), missing)
	}
	if !missing[0].Equal(base.Add(5*time.Hour)) || !missing[1].Equal(base.Add(6*time.Hour)) {
		t.Fatalf("missing hours = %v, want [%v %v]", missing, base.Add(5*time.Hour), base.Add(6*time.Hour))
	}

	// A fully-covered sub-window yields nothing.
	none, err := r.MissingHours(ctx, "akron1", "", "V", base, base.Add(4*time.Hour))
	if err != nil {
		t.Fatalf("MissingHours covered: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("covered window want 0 missing, got %d: %v", len(none), none)
	}
}
