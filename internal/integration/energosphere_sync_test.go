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
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.InitArchiveSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	return repo
}

func saveAkronV(t *testing.T, repo *sqliterepo.Repo, ts time.Time, value float64) {
	t.Helper()

	if err := repo.SaveHourlyArchive(
		context.Background(),
		storage.HourlyArchiveRecord{
			DeviceID: "osmos",
			Param:    "V",
			TsHour:   ts,
			Value:    value,
			Unit:     "m3",
		},
	); err != nil {
		t.Fatal(err)
	}
}

func TestCollectAkronReadingsUsesHourlyDeltaNotTotalizer(t *testing.T) {
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
		t17,
		t18,
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("want 2 readings, got %d: %#v", len(got), got)
	}

	if !got[0].ts.Equal(t17) || math.Abs(got[0].value-244) > 1e-9 {
		t.Fatalf(
			"17:00: want delta 244, got ts=%v value=%g",
			got[0].ts,
			got[0].value,
		)
	}

	if !got[1].ts.Equal(t18) || math.Abs(got[1].value-130) > 1e-9 {
		t.Fatalf(
			"18:00: want delta 130, got ts=%v value=%g",
			got[1].ts,
			got[1].value,
		)
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
