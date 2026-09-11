package device

import (
	"testing"
	"time"

	"mbgw/internal/profile"
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
