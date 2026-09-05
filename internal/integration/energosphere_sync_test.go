package integration

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"mbgw/internal/storage"
	sqliterepo "mbgw/internal/storage/sqlite"
)

func newIntegrationTestRepo(t *testing.T) *sqliterepo.Repo {
	t.Helper()

	repo, err := sqliterepo.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = repo.Close()
	})

	if err := repo.InitArchiveSchema(context.Background()); err != nil {
		t.Fatal(err)
	}

	return repo
}

func saveAkronV(t *testing.T, repo *sqliterepo.Repo, ts time.Time, value float64) {
	t.Helper()

	if err := repo.SaveHourlyArchive(context.Background(), storage.HourlyArchiveRecord{
		DeviceID: "osmos",
		Param:    "V",
		TsHour:   ts,
		Value:    value,
		Unit:     "m3",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCollectAkronReadingsSplitsHourlyDeltaIntoHalfHoursAndAppliesShift(t *testing.T) {
	repo := newIntegrationTestRepo(t)
	loc := time.Local

	t16 := time.Date(2026, 9, 2, 16, 0, 0, 0, loc)
	t17 := t16.Add(time.Hour)
	t18 := t17.Add(time.Hour)

	saveAkronV(t, repo, t16, 2238000)
	saveAkronV(t, repo, t17, 2238244)
	saveAkronV(t, repo, t18, 2238374)

	got, err := collectAkronReadings(
		context.Background(),
		repo,
		Config{
			DeviceID:         "osmos",
			Kind:             "akron",
			TimeShiftMinutes: -90,
			Points: []PointMapping{
				{
					Tag:     "V",
					PointID: 1,
					Factor:  1,
				},
			},
		},
		t17,
		t18,
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 4 {
		t.Fatalf("want 4 half-hour readings, got %d: %#v", len(got), got)
	}

	wantTS := []time.Time{
		time.Date(2026, 9, 2, 15, 0, 0, 0, loc),
		time.Date(2026, 9, 2, 15, 30, 0, 0, loc),
		time.Date(2026, 9, 2, 16, 0, 0, 0, loc),
		time.Date(2026, 9, 2, 16, 30, 0, 0, loc),
	}

	wantValue := []float64{
		122,
		122,
		65,
		65,
	}

	for i := range wantTS {
		if !got[i].ts.Equal(wantTS[i]) {
			t.Fatalf(
				"reading %d: want ts=%v, got %v",
				i,
				wantTS[i],
				got[i].ts,
			)
		}

		if math.Abs(got[i].value-wantValue[i]) > 1e-9 {
			t.Fatalf(
				"reading %d: want value=%g, got %g",
				i,
				wantValue[i],
				got[i].value,
			)
		}
	}
}

func TestCollectAkronReadingsDoesNotCollapseGapIntoOneHour(t *testing.T) {
	repo := newIntegrationTestRepo(t)
	loc := time.Local

	t16 := time.Date(2026, 9, 2, 16, 0, 0, 0, loc)
	t18 := t16.Add(2 * time.Hour)

	saveAkronV(t, repo, t16, 1000)
	saveAkronV(t, repo, t18, 1300)

	got, err := collectAkronReadings(
		context.Background(),
		repo,
		Config{
			DeviceID: "osmos",
			Kind:     "akron",
			Points: []PointMapping{
				{
					Tag:     "V",
					PointID: 1,
					Factor:  1,
				},
			},
		},
		t18,
		t18,
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 0 {
		t.Fatalf("want gap to be skipped, got %#v", got)
	}
}

func TestCollectReadingsForESRangeIncludesLastAkronHalfHourWithShift(t *testing.T) {
	repo := newIntegrationTestRepo(t)
	loc := time.Local

	t16 := time.Date(2026, 9, 5, 16, 0, 0, 0, loc)
	t17 := t16.Add(time.Hour)
	t18 := t17.Add(time.Hour)

	saveAkronV(t, repo, t16, 1000)
	saveAkronV(t, repo, t17, 1130)
	saveAkronV(t, repo, t18, 1261)

	cfg := Config{
		DeviceID:         "osmos",
		Kind:             "akron",
		TimeShiftMinutes: -90,
		Points: []PointMapping{
			{
				Tag:     "V",
				PointID: 1,
				Factor:  1,
			},
		},
	}

	fromES := time.Date(2026, 9, 5, 15, 0, 0, 0, loc)
	toES := time.Date(2026, 9, 5, 16, 30, 0, 0, loc)

	got, err := collectReadingsForESRange(
		context.Background(),
		repo,
		cfg,
		fromES,
		toES,
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 4 {
		t.Fatalf("want 4 readings in ES range, got %d: %#v", len(got), got)
	}

	last := got[len(got)-1]
	if !last.ts.Equal(toES) {
		t.Fatalf("want last ES timestamp %v, got %v", toES, last.ts)
	}
	if math.Abs(last.value-65.5) > 1e-9 {
		t.Fatalf("want last half-hour value 65.5, got %g", last.value)
	}
}
