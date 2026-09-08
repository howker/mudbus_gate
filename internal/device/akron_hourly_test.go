package device

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/profile"
	"mbgw/internal/storage"
)

func TestAkronRowTime_Valid(t *testing.T) {
	// 16:00 on 17 July 2026 → year field carries 26 (year-2000).
	fields := map[string]any{
		"volume": float64(582.7),
		"hour":   int64(16),
		"day":    int64(17),
		"month":  int64(7),
		"year":   int64(26),
	}
	got, ok := akronRowTime(fields)
	if !ok {
		t.Fatal("akronRowTime returned ok=false for a valid row")
	}
	want := time.Date(2026, 7, 17, 16, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Minute/second must be zeroed to the hour boundary.
	if got.Minute() != 0 || got.Second() != 0 {
		t.Fatalf("hour boundary not clean: %v", got)
	}
}

func TestAkronRowTime_RejectsGarbage(t *testing.T) {
	cases := map[string]map[string]any{
		"all-zero filler": {"hour": int64(0), "day": int64(0), "month": int64(0), "year": int64(0)},
		"month 13":        {"hour": int64(1), "day": int64(1), "month": int64(13), "year": int64(24)},
		"hour 24":         {"hour": int64(24), "day": int64(1), "month": int64(1), "year": int64(24)},
		"day 0":           {"hour": int64(1), "day": int64(0), "month": int64(1), "year": int64(24)},
		"missing hour":    {"day": int64(1), "month": int64(1), "year": int64(24)},
		"wrong type":      {"hour": "16", "day": int64(1), "month": int64(1), "year": int64(24)},
	}
	for name, fields := range cases {
		if _, ok := akronRowTime(fields); ok {
			t.Errorf("%s: expected ok=false, got ok=true", name)
		}
	}
}

// TestAkronRowTime_RejectsImplausibleDates is a regression test for a real
// production incident: a corrupted single-row archive read decoded to
// hour=4, day=7, month=6, year=44 — every field individually within its
// valid BCD range, so TestAkronRowTime_RejectsGarbage's checks let it
// through. The row (07.06.2044) was then stored and, being later than any
// real row, permanently won GetHourlyArchiveDesc's "ORDER BY ts_hour DESC
// LIMIT 1", hiding all genuine archive data collected afterwards. Dates
// computed relative to time.Now() (not hardcoded) so this test keeps
// working regardless of when it's actually run.
func TestAkronRowTime_RejectsImplausibleDates(t *testing.T) {
	now := time.Now()

	farFuture := now.AddDate(0, 0, 2) // 2 days ahead — past the 24h future bound
	future := map[string]any{
		"hour":  int64(farFuture.Hour()),
		"day":   int64(farFuture.Day()),
		"month": int64(int(farFuture.Month())),
		"year":  int64(farFuture.Year() % 100),
	}
	if _, ok := akronRowTime(future); ok {
		t.Fatal("far-future date should be rejected — this is the exact bug class that produced 07.06.2044 in production")
	}

	farPast := now.AddDate(-2, 0, 0) // 2 years back — past the 400-day past bound
	past := map[string]any{
		"hour":  int64(farPast.Hour()),
		"day":   int64(farPast.Day()),
		"month": int64(int(farPast.Month())),
		"year":  int64(farPast.Year() % 100),
	}
	if _, ok := akronRowTime(past); ok {
		t.Fatal("far-past date should be rejected")
	}
}

func TestFieldFloat_Types(t *testing.T) {
	if v, ok := fieldFloat(map[string]any{"volume": float64(582.7)}, "volume"); !ok || v != 582.7 {
		t.Fatalf("float64 volume: got %v ok=%v", v, ok)
	}
	// akron_volume always decodes to float64, but be defensive about int64.
	if v, ok := fieldFloat(map[string]any{"volume": int64(583)}, "volume"); !ok || v != 583 {
		t.Fatalf("int64 volume: got %v ok=%v", v, ok)
	}
	if _, ok := fieldFloat(map[string]any{"volume": "x"}, "volume"); ok {
		t.Fatal("string volume should be rejected")
	}
	if _, ok := fieldFloat(map[string]any{}, "volume"); ok {
		t.Fatal("missing volume should be rejected")
	}
}

type akronRolloverRepo struct {
	storage.Repo
	rows []storage.HourlyArchiveRecord
}

func (r *akronRolloverRepo) SaveHourlyArchive(_ context.Context, rec storage.HourlyArchiveRecord) error {
	for i := range r.rows {
		if r.rows[i].DeviceID == rec.DeviceID && r.rows[i].Channel == rec.Channel && r.rows[i].Param == rec.Param && r.rows[i].TsHour.Equal(rec.TsHour) {
			r.rows[i] = rec
			return nil
		}
	}
	r.rows = append(r.rows, rec)
	return nil
}

func (r *akronRolloverRepo) GetPreviousHourlyValue(_ context.Context, deviceID, channel, param string, before time.Time) (float64, bool, error) {
	var best storage.HourlyArchiveRecord
	found := false
	for _, row := range r.rows {
		if row.DeviceID != deviceID || row.Channel != channel || row.Param != param || !row.TsHour.Before(before) {
			continue
		}
		if !found || row.TsHour.After(best.TsHour) {
			best = row
			found = true
		}
	}
	return best.Value, found, nil
}

func (r *akronRolloverRepo) valueAt(ts time.Time) (float64, bool) {
	for _, row := range r.rows {
		if row.TsHour.Equal(ts) {
			return row.Value, true
		}
	}
	return 0, false
}

func akronRolloverArchive() profile.Archive {
	return profile.Archive{
		ID: "hourly",
		RecordLayout: []profile.RecordField{
			{Name: "volume", Unit: "m3"},
		},
	}
}

func akronRolloverRecord(ts time.Time, raw int32, pu byte) archive.ArchiveRecord {
	b := make([]byte, 9)
	binary.LittleEndian.PutUint32(b[:4], uint32(raw))
	b[4] = pu
	scale := math.Pow10(int(pu) - 3)
	return archive.ArchiveRecord{
		Fields: map[string]any{
			"volume": float64(raw) * scale,
			"hour":   int64(ts.Hour()),
			"day":    int64(ts.Day()),
			"month":  int64(ts.Month()),
			"year":   int64(ts.Year() % 100),
		},
		Raw: b,
	}
}

func TestPersistAkronHourly_ConfirmedInt32RolloverContinuesCounter(t *testing.T) {
	base := time.Now().In(time.Local).Truncate(time.Hour).Add(-4 * time.Hour)
	t0, t1, t2 := base, base.Add(time.Hour), base.Add(2*time.Hour)
	const prev = 2147483547.0 // MaxInt32 - 100, Pu=3 => scale 1

	repo := &akronRolloverRepo{rows: []storage.HourlyArchiveRecord{{
		DeviceID: "ak", Param: "V", TsHour: t0, Value: prev, Unit: "m3",
	}}}
	// Device command 104 returns newest-first. Two consecutive negative int32
	// rows confirm that the sign boundary was crossed rather than one row being
	// corrupted by line noise.
	records := []archive.ArchiveRecord{
		akronRolloverRecord(t2, int32(-2147483648+180), 3),
		akronRolloverRecord(t1, int32(-2147483648+50), 3),
	}
	if got := persistAkronHourly(context.Background(), repo, "ak", akronRolloverArchive(), records); got != 2 {
		t.Fatalf("saved=%d, want 2", got)
	}

	v1, ok := repo.valueAt(t1)
	if !ok {
		t.Fatal("first rollover hour was not persisted")
	}
	v2, ok := repo.valueAt(t2)
	if !ok {
		t.Fatal("second rollover hour was not persisted")
	}
	if v1 != 2147483698.0 || v2 != 2147483828.0 {
		t.Fatalf("normalized values = %v, %v; want 2147483698, 2147483828", v1, v2)
	}
	if v1-prev != 151 || v2-v1 != 130 {
		t.Fatalf("rollover deltas = %v, %v; want 151, 130", v1-prev, v2-v1)
	}
}

func TestPersistAkronHourly_DoesNotAcceptUnconfirmedNegativeSample(t *testing.T) {
	base := time.Now().In(time.Local).Truncate(time.Hour).Add(-4 * time.Hour)
	t0, t1 := base, base.Add(time.Hour)
	const prev = 2147483547.0
	repo := &akronRolloverRepo{rows: []storage.HourlyArchiveRecord{{
		DeviceID: "ak", Param: "V", TsHour: t0, Value: prev, Unit: "m3",
	}}}

	// Near the sign boundary, but with no following hour to confirm the trend.
	// The safe behavior is to defer this row; the next regular command-104 page
	// will contain it again together with the next hour.
	records := []archive.ArchiveRecord{
		akronRolloverRecord(t1, int32(-2147483648+50), 3),
	}
	if got := persistAkronHourly(context.Background(), repo, "ak", akronRolloverArchive(), records); got != 0 {
		t.Fatalf("saved=%d, want 0 for unconfirmed rollover", got)
	}
	if _, ok := repo.valueAt(t1); ok {
		t.Fatal("unconfirmed rollover row must not be persisted")
	}
}

func TestPersistAkronHourly_LineNoiseStillRejected(t *testing.T) {
	base := time.Now().In(time.Local).Truncate(time.Hour).Add(-4 * time.Hour)
	t0, t1, t2 := base, base.Add(time.Hour), base.Add(2*time.Hour)
	repo := &akronRolloverRepo{rows: []storage.HourlyArchiveRecord{{
		DeviceID: "ak", Param: "V", TsHour: t0, Value: 2200000, Unit: "m3",
	}}}

	// Two negative raw values are not enough: the previous real counter is
	// nowhere near the int32 boundary, so this remains ordinary corruption.
	records := []archive.ArchiveRecord{
		akronRolloverRecord(t2, -50, 3),
		akronRolloverRecord(t1, -100, 3),
	}
	if got := persistAkronHourly(context.Background(), repo, "ak", akronRolloverArchive(), records); got != 0 {
		t.Fatalf("saved=%d, want 0 for line-noise decrease", got)
	}
}

func TestPersistAkronHourly_ConfirmedFullUint32Rollover(t *testing.T) {
	base := time.Now().In(time.Local).Truncate(time.Hour).Add(-4 * time.Hour)
	t0, t1, t2 := base, base.Add(time.Hour), base.Add(2*time.Hour)
	const modulus = 4294967296.0
	repo := &akronRolloverRepo{rows: []storage.HourlyArchiveRecord{{
		DeviceID: "ak", Param: "V", TsHour: t0, Value: modulus - 100, Unit: "m3",
	}}}

	records := []archive.ArchiveRecord{
		akronRolloverRecord(t2, 180, 3),
		akronRolloverRecord(t1, 50, 3),
	}
	if got := persistAkronHourly(context.Background(), repo, "ak", akronRolloverArchive(), records); got != 2 {
		t.Fatalf("saved=%d, want 2", got)
	}
	v1, _ := repo.valueAt(t1)
	v2, _ := repo.valueAt(t2)
	if v1 != modulus+50 || v2 != modulus+180 {
		t.Fatalf("normalized full-wrap values = %v, %v; want %v, %v", v1, v2, modulus+50, modulus+180)
	}
	if v1-(modulus-100) != 150 || v2-v1 != 130 {
		t.Fatalf("full-wrap deltas = %v, %v; want 150, 130", v1-(modulus-100), v2-v1)
	}
}
