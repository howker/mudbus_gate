package device

import (
	"context"
	"testing"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/health"
	"mbgw/internal/profile"
	"mbgw/internal/storage"
)

func periodEndArchiveForReloadTest() profile.Archive {
	return profile.Archive{
		ID:       "hourly",
		Strategy: "mb_func65",
		Params: map[string]interface{}{
			"archive_type":        0,
			"timestamp_semantics": "period_end",
		},
	}
}

func TestFunc65ReloadQueryTimePeriodEndUsesPreviousWallClockHour(t *testing.T) {
	a := periodEndArchiveForReloadTest()
	loc := time.FixedZone("UTC+4", 4*60*60)

	// Оператор просит строку МШ "10.09.2026 15:00".
	// Для ИВК-ТЭР эта строка соответствует сырой записи периода
	// 14:00..15:00, поэтому TIME-запрос функции 65 должен содержать 14:00.
	storageHour := time.Date(2026, 9, 10, 15, 0, 0, 0, loc)
	got := func65QueryTimeForStorageHour(a, storageHour)
	want := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)

	if !got.Equal(want) {
		t.Fatalf("query time = %v, want %v", got, want)
	}
}

func TestFunc65ReloadHourComparisonUsesWallClockNotAbsoluteInstant(t *testing.T) {
	a := periodEndArchiveForReloadTest()
	loc := time.FixedZone("UTC+4", 4*60*60)

	// Generic archive decoder returns Location=UTC for epoch fields.
	// Numerically this 14:00 is the IVK local wall-clock field; after the
	// period_end rule it must match operator/storage 15:00 despite differing
	// Go time locations.
	rawFromDecoder := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	gotStorageHour := func65WallClockHour(func65StorageHour(a, rawFromDecoder))
	operatorHour := func65WallClockHour(time.Date(2026, 9, 10, 15, 0, 0, 0, loc))

	if !gotStorageHour.Equal(operatorHour) {
		t.Fatalf("storage hour = %v, operator hour = %v", gotStorageHour, operatorHour)
	}
}

func TestFunc65ReloadQueryUsesTimeAccess(t *testing.T) {
	a := periodEndArchiveForReloadTest()
	d := &Device{
		ID: "ivk_test",
		Profile: &profile.Profile{
			Codec: profile.Codec{
				WordOrder32: "0123",
				WordOrder64: "01234567",
			},
		},
	}

	q := func65ReloadQuery(d, a, time.Date(2026, 9, 10, 15, 0, 0, 0, time.Local))
	if q.From.IsZero() {
		t.Fatal("forced reload query must use function-65 TIME access")
	}
	if q.FromIndex != 0 || q.ToIndex != 0 {
		t.Fatalf("forced reload unexpectedly uses index access: from=%d to=%d", q.FromIndex, q.ToIndex)
	}
	if q.From.Hour() != 14 || q.From.Day() != 10 || q.From.Month() != time.September || q.From.Year() != 2026 {
		t.Fatalf("query From = %v, want raw wall-clock 10.09.2026 14:00", q.From)
	}
}

func TestFunc65LatestQueryUsesTimeAccessForCurrentStorageHour(t *testing.T) {
	a := periodEndArchiveForReloadTest()
	d := &Device{ID: "ivk_test", Profile: &profile.Profile{Codec: profile.Codec{WordOrder32: "0123"}}}
	loc := time.FixedZone("local", 4*60*60)
	now := time.Date(2026, 9, 13, 10, 55, 14, 0, loc)

	q := func65LatestQueryAt(d, a, now)
	wantFrom := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	if !q.From.Equal(wantFrom) {
		t.Fatalf("latest query From=%v, want %v (TIME request for raw period 09:00 -> storage 10:00)", q.From, wantFrom)
	}
	if q.From.IsZero() {
		t.Fatal("latest query must use TIME access, got zero From")
	}
}

func TestFunc65LatestQueryDoesNotUseIndexZeroAsNewest(t *testing.T) {
	a := periodEndArchiveForReloadTest()
	d := &Device{ID: "ivk_test", Profile: &profile.Profile{Codec: profile.Codec{WordOrder32: "0123"}}}
	now := time.Date(2026, 9, 13, 10, 5, 0, 0, time.UTC)

	q := func65LatestQueryAt(d, a, now)
	if q.From.IsZero() {
		t.Fatal("regular IVK poll fell back to index access; live device proved index 0 is not the newest record")
	}
}

type fakeFunc65Reader struct {
	calls int
	read  func(q archive.ArchiveQuery) ([]archive.ArchiveRecord, error)
}

func (f *fakeFunc65Reader) Strategy() string { return "mb_func65" }
func (f *fakeFunc65Reader) Read(_ context.Context, _ archive.ArchiveSession, _ archive.Transactor, q archive.ArchiveQuery) ([]archive.ArchiveRecord, error) {
	f.calls++
	return f.read(q)
}

func TestIVKScheduledPollReadsLatestEvenWhenBackfillHasNoGaps(t *testing.T) {
	old, ok := archive.Get("mb_func65")
	fake := &fakeFunc65Reader{read: func(q archive.ArchiveQuery) ([]archive.ArchiveRecord, error) {
		return nil, nil // device answered, newest archive hour not closed yet
	}}
	archive.Register(fake)
	defer func() {
		if ok {
			archive.Register(old)
		}
	}()

	d := newTestDeviceForVKM(t)
	a := profile.Archive{
		ID: "hourly", Strategy: "mb_func65", BufferDepthHours: 24,
		Params: map[string]interface{}{"archive_type": 0, "timestamp_semantics": "period_end"},
		RecordLayout: []profile.RecordField{
			{Offset: 0, Name: "archive_time", Type: "uint32", Epoch: "1970-01-01"},
			{Offset: 4, Name: "v_plus", Type: "float", Unit: "m3"},
		},
	}
	d.Profile = &profile.Profile{Codec: profile.Codec{WordOrder32: "0123"}, Archives: []profile.Archive{a}}
	d.BackfillMaxDepthHours = 24
	now := func65WallClockHour(time.Now())
	for i := 1; i < 24; i++ {
		if err := d.Repo.SaveHourlyArchive(context.Background(), storage.HourlyArchiveRecord{
			DeviceID: d.ID, Param: "v_plus", TsHour: now.Add(-time.Duration(i) * time.Hour), Value: float64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	before := health.Get().Devices[d.ID].LastArchiveSuccess
	d.PollArchives(context.Background())
	after := health.Get().Devices[d.ID].LastArchiveSuccess
	if fake.calls != 1 {
		t.Fatalf("scheduled IVK poll calls=%d, want exactly one mandatory latest read when history has no gaps", fake.calls)
	}
	if !after.After(before) {
		t.Fatalf("empty but valid latest IVK response must confirm live contact: before=%v after=%v", before, after)
	}
}
