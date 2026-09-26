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

func saveIVKHourly(t *testing.T, repo *sqliterepo.Repo, param string, ts time.Time, value float64, unit string) {
	t.Helper()

	if err := repo.SaveHourlyArchive(context.Background(), storage.HourlyArchiveRecord{
		DeviceID: "ivk",
		Param:    param,
		TsHour:   ts,
		Value:    value,
		Unit:     unit,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCollectIVKReadingsProjectsHourlyFieldsToHalfHours(t *testing.T) {
	repo := newIntegrationTestRepo(t)
	loc := time.Local
	ts := time.Date(2026, 9, 10, 15, 0, 0, 0, loc)
	storedTS := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)

	// Production IVK-TER archive_hourly keeps the device's local calendar
	// fields in a UTC-located time.Time. The requested range is local wall-clock.
	saveIVKHourly(t, repo, "v_plus", storedTS, 74.359, "м3")
	saveIVKHourly(t, repo, "q_avg", storedTS, 1239.318, "л/мин")
	saveIVKHourly(t, repo, "downtime", storedTS, 10, "мин")
	saveIVKHourly(t, repo, "errors", storedTS, 3, "")

	got, err := collectReadings(
		context.Background(),
		repo,
		Config{
			DeviceID:         "ivk",
			Kind:             "ivk-ter",
			TimeShiftMinutes: -90,
			Points: []PointMapping{
				{Tag: "v_plus", PointID: 101, Factor: 1},
				{Tag: "q_avg", PointID: 102, Factor: 0.001},
				{Tag: "downtime", PointID: 103, Factor: 1},
				{Tag: "errors", PointID: 104, Factor: 1},
			},
		},
		ts,
		ts,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 8 {
		t.Fatalf("want 8 IVK half-hour readings, got %d: %#v", len(got), got)
	}

	firstTS := time.Date(2026, 9, 10, 13, 0, 0, 0, loc)
	secondTS := time.Date(2026, 9, 10, 13, 30, 0, 0, loc)

	byTag := make(map[string][]pointReading)
	for _, r := range got {
		byTag[r.mapping.Tag] = append(byTag[r.mapping.Tag], r)
	}

	checks := []struct {
		tag           string
		first, second float64
	}{
		{tag: "v_plus", first: 74.359 / 2, second: 74.359 / 2},
		{tag: "q_avg", first: 1.239318, second: 1.239318},
		{tag: "downtime", first: 5, second: 5},
		{tag: "errors", first: 3, second: 3},
	}

	for _, tc := range checks {
		r := byTag[tc.tag]
		if len(r) != 2 {
			t.Fatalf("tag %s: want 2 readings, got %d: %#v", tc.tag, len(r), r)
		}
		if !r[0].ts.Equal(firstTS) || !r[1].ts.Equal(secondTS) {
			t.Fatalf("tag %s: want timestamps %v/%v, got %v/%v", tc.tag, firstTS, secondTS, r[0].ts, r[1].ts)
		}
		if math.Abs(r[0].value-tc.first) > 1e-9 || math.Abs(r[1].value-tc.second) > 1e-9 {
			t.Fatalf("tag %s: want values %g/%g, got %g/%g", tc.tag, tc.first, tc.second, r[0].value, r[1].value)
		}
	}
}

func TestCollectIVKReadingsRejectsUnknownHalfHourSemantics(t *testing.T) {
	repo := newIntegrationTestRepo(t)
	loc := time.Local
	ts := time.Date(2026, 9, 10, 15, 0, 0, 0, loc)

	saveIVKHourly(t, repo, "future_field", ts, 42, "")

	_, err := collectIVKReadings(
		context.Background(),
		repo,
		Config{
			DeviceID: "ivk",
			Kind:     "ivk-ter",
			Points: []PointMapping{
				{Tag: "future_field", PointID: 105, Factor: 1},
			},
		},
		ts,
		ts,
	)
	if err == nil {
		t.Fatal("want unknown IVK hourly semantics to be rejected")
	}
}

func TestCollectReadingsForESRangeIncludesFirstIVKHalfHourAtUpperBoundary(t *testing.T) {
	repo := newIntegrationTestRepo(t)
	loc := time.Local
	storedHourEnd := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)

	// Same production quirk as above: UTC Location carries local wall-clock fields.
	saveIVKHourly(t, repo, "v_plus", storedHourEnd, 80, "м3")

	cfg := Config{
		DeviceID:         "ivk",
		Kind:             "ivk-ter",
		TimeShiftMinutes: -90,
		Points: []PointMapping{
			{Tag: "v_plus", PointID: 101, Factor: 1},
		},
	}

	// 15:00 local source hour -> logical half-hours 14:30/15:00 ->
	// with -90 min shift -> ES 13:00/13:30. The range ends exactly on
	// the FIRST half-hour point, so source reading must look 30m ahead.
	fromES := time.Date(2026, 9, 10, 13, 0, 0, 0, loc)
	toES := fromES

	got, err := collectReadingsForESRange(context.Background(), repo, cfg, fromES, toES)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly first IVK half-hour reading, got %d: %#v", len(got), got)
	}
	if !got[0].ts.Equal(toES) {
		t.Fatalf("want ES timestamp %v, got %v", toES, got[0].ts)
	}
	if math.Abs(got[0].value-40) > 1e-9 {
		t.Fatalf("want first half-hour value 40, got %g", got[0].value)
	}
}

func TestCollectReadingsForESRangeIVKUsesWallClockNotAbsoluteInstant(t *testing.T) {
	repo := newIntegrationTestRepo(t)
	local := time.FixedZone("UTC+4", 4*60*60)

	// This reproduces the production IVK-TER storage quirk: archive_time is
	// decoded/stored with Location=UTC, but its calendar fields are local
	// wall-clock. These are the real 20:00 and 21:00 storage hours.
	saveIVKHourly(t, repo, "v_plus", time.Date(2026, 9, 14, 20, 0, 0, 0, time.UTC), 81.0330581665039, "м3")
	saveIVKHourly(t, repo, "v_plus", time.Date(2026, 9, 14, 21, 0, 0, 0, time.UTC), 81.0478134155273, "м3")

	cfg := Config{
		DeviceID:         "ivk",
		Kind:             "ivk-ter",
		TimeShiftMinutes: -90,
		Points: []PointMapping{
			{Tag: "v_plus", PointID: 101, Factor: 1},
		},
	}

	fromES := time.Date(2026, 9, 14, 0, 0, 0, 0, local)
	toES := time.Date(2026, 9, 14, 21, 42, 0, 0, local)

	got, err := collectReadingsForESRange(context.Background(), repo, cfg, fromES, toES)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 IVK half-hour readings for 20:00/21:00 source hours, got %d: %#v", len(got), got)
	}

	wantTS := []time.Time{
		time.Date(2026, 9, 14, 18, 0, 0, 0, local),
		time.Date(2026, 9, 14, 18, 30, 0, 0, local),
		time.Date(2026, 9, 14, 19, 0, 0, 0, local),
		time.Date(2026, 9, 14, 19, 30, 0, 0, local),
	}
	wantValue := []float64{
		81.0330581665039 / 2,
		81.0330581665039 / 2,
		81.0478134155273 / 2,
		81.0478134155273 / 2,
	}

	for i := range wantTS {
		if !got[i].ts.Equal(wantTS[i]) {
			t.Fatalf("reading %d: want ts=%v, got %v", i, wantTS[i], got[i].ts)
		}
		if got[i].ts.Location() != local {
			t.Fatalf("reading %d: want local wall-clock location, got %v", i, got[i].ts.Location())
		}
		if math.Abs(got[i].value-wantValue[i]) > 1e-9 {
			t.Fatalf("reading %d: want value=%g, got %g", i, wantValue[i], got[i].value)
		}
	}
}

func TestIVKSyncCursorUsesWallClockSemanticsAcrossLocationChange(t *testing.T) {
	local := time.FixedZone("UTC+4", 4*60*60)
	cfg := Config{Kind: "ivk-ter", TimeShiftMinutes: -90}

	// Production cursor written before the wall-clock fix: the visible ES
	// timestamp 18:30 was stored with Location=UTC.
	oldCursor := time.Date(2026, 9, 14, 18, 30, 0, 0, time.UTC)

	// After the wall-clock fix the same visible ES timestamp is produced in
	// the server's local Location. It must compare equal to the old cursor,
	// not four hours earlier as an absolute instant.
	sameReading := time.Date(2026, 9, 14, 18, 30, 0, 0, local)
	newerReading := time.Date(2026, 9, 14, 19, 0, 0, 0, local)

	gotOld := normalizeESSyncCursorTime(cfg, oldCursor)
	gotSame := normalizeESSyncCursorTime(cfg, sameReading)
	gotNewer := normalizeESSyncCursorTime(cfg, newerReading)

	if gotOld.Location() != time.UTC || gotSame.Location() != time.UTC || gotNewer.Location() != time.UTC {
		t.Fatalf("IVK cursor timestamps must use UTC-located wall-clock semantics: old=%v same=%v newer=%v", gotOld, gotSame, gotNewer)
	}
	if !gotSame.Equal(gotOld) {
		t.Fatalf("same wall-clock timestamp must match old cursor: old=%v same=%v", gotOld, gotSame)
	}
	if !gotNewer.After(gotOld) {
		t.Fatalf("newer wall-clock timestamp must advance cursor: old=%v newer=%v", gotOld, gotNewer)
	}

	// Cursor is already an ES timestamp. Reversing -90 minutes must yield
	// the neutral source wall-clock lower bound 20:00.
	sourceCursor := gotOld.Add(-time.Duration(cfg.TimeShiftMinutes) * time.Minute)
	wantSource := time.Date(2026, 9, 14, 20, 0, 0, 0, time.UTC)
	if !sourceCursor.Equal(wantSource) {
		t.Fatalf("want source cursor %v, got %v", wantSource, sourceCursor)
	}

	// Other device kinds must retain the pre-existing instant semantics.
	akronTS := time.Date(2026, 9, 14, 18, 30, 0, 0, local)
	gotAkron := normalizeESSyncCursorTime(Config{Kind: "akron"}, akronTS)
	if gotAkron != akronTS {
		t.Fatalf("Akron cursor must remain unchanged: want %v, got %v", akronTS, gotAkron)
	}
}

func TestCollectVKMReadingsSeparatesMappingsByPipe(t *testing.T) {
	repo := newIntegrationTestRepo(t)
	ctx := context.Background()
	ts := time.Date(2026, 9, 24, 12, 30, 0, 0, time.Local)

	if err := repo.SaveVKMRawString(ctx, "vkm", 1, ts, "T=10;"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveVKMRawString(ctx, "vkm", 2, ts, "T=20;"); err != nil {
		t.Fatal(err)
	}

	got, err := collectVKMReadings(ctx, repo, Config{
		DeviceID: "vkm",
		Kind:     "vkm360",
		Pipe:     1,
		Points: []PointMapping{
			{Tag: "T", PointID: 101, Pipe: 1, Factor: 1},
			{Tag: "T", PointID: 202, Pipe: 2, Factor: 1},
		},
	}, ts, ts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 readings, got %d: %#v", len(got), got)
	}
	byPoint := map[int]pointReading{}
	for _, r := range got {
		byPoint[r.mapping.PointID] = r
	}
	if r, ok := byPoint[101]; !ok || r.value != 10 || r.mapping.Pipe != 1 {
		t.Fatalf("pipe 1 mapping mismatch: %#v", r)
	}
	if r, ok := byPoint[202]; !ok || r.value != 20 || r.mapping.Pipe != 2 {
		t.Fatalf("pipe 2 mapping mismatch: %#v", r)
	}
}

func TestValidatePointReadingIgnoresLegacyMinMax(t *testing.T) {
	min := 100.0
	max := 200.0
	for _, value := range []float64{50, 250} {
		err := validatePointReading(pointReading{
			mapping: PointMapping{MinValue: &min, MaxValue: &max},
			value:   value,
		})
		if err != nil {
			t.Fatalf("legacy min/max must not block finite value %g: %v", value, err)
		}
	}
}
