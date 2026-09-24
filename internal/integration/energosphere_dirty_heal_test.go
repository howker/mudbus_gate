package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type fakeInsertOnlyWriter struct {
	existing       map[string]bool
	inserted       []fakeInsertedPoint
	pointExistsErr error
	insertErr      error
}

type fakeInsertedPoint struct {
	pointID int
	ts      time.Time
	value   float64
	state   int
}

func fakePointKey(pointID int, ts time.Time) string {
	return fmt.Sprintf("%d|%s", pointID, ts.Format(time.RFC3339Nano))
}

func (w *fakeInsertOnlyWriter) PointExists(_ context.Context, pointID int, ts time.Time) (bool, error) {
	if w.pointExistsErr != nil {
		return false, w.pointExistsErr
	}
	if w.existing == nil {
		return false, nil
	}
	return w.existing[fakePointKey(pointID, ts)], nil
}

func (w *fakeInsertOnlyWriter) InsertPoint(_ context.Context, pointID int, ts time.Time, value float64, state int) error {
	if w.insertErr != nil {
		return w.insertErr
	}
	w.inserted = append(w.inserted, fakeInsertedPoint{
		pointID: pointID,
		ts:      ts,
		value:   value,
		state:   state,
	})
	return nil
}

func TestDirtyHealerInsertsLateVKMDataBehindCursorWithoutMovingCursor(t *testing.T) {
	ctx := context.Background()
	repo := newIntegrationTestRepo(t)
	if err := repo.InitESSyncCursorSchema(ctx); err != nil {
		t.Fatal(err)
	}

	loc := time.Local
	lateTS := time.Date(2026, 9, 24, 13, 30, 0, 0, loc)
	cursorTS := time.Date(2026, 9, 24, 15, 0, 0, 0, loc)
	const pointID = 7001

	// Reproduce the real failure mode: ordinary sync already confirmed a
	// newer ES timestamp, then an older local archive row appears later.
	if err := repo.SetESSyncCursor(ctx, "vkm", pointID, cursorTS); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveVKMRawString(ctx, "vkm", 1, lateTS, "S=12.5;"); err != nil {
		t.Fatal(err)
	}

	writer := &fakeInsertOnlyWriter{}
	cfg := Config{
		DeviceID: "vkm",
		Pipe:     1,
		Kind:     "vkm360",
		Points: []PointMapping{
			{Tag: "S", PointID: pointID, Factor: 1, Label: "масса"},
		},
	}

	stats, err := healESDirtyRanges(ctx, repo, writer, cfg, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Inserted != 1 || stats.RangesCompleted != 1 {
		t.Fatalf("unexpected heal stats: %+v", stats)
	}
	if len(writer.inserted) != 1 {
		t.Fatalf("want 1 inserted late point, got %d: %#v", len(writer.inserted), writer.inserted)
	}
	if !writer.inserted[0].ts.Equal(lateTS) || writer.inserted[0].value != 12.5 {
		t.Fatalf("unexpected inserted point: %#v", writer.inserted[0])
	}

	// Dirty healing is deliberately independent of the monotonic ordinary
	// sync cursor: it must neither be blocked by it nor rewind/advance it.
	gotCursor, found, err := repo.GetESSyncCursor(ctx, "vkm", pointID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !gotCursor.Equal(cursorTS) {
		t.Fatalf("ordinary cursor changed by dirty healer: found=%v got=%v want=%v", found, gotCursor, cursorTS)
	}

	n, err := repo.CountESDirtyRanges(ctx, "vkm")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("want dirty queue drained after successful heal, got %d", n)
	}
}

func TestDirtyHealerNeverOverwritesExistingESPoint(t *testing.T) {
	ctx := context.Background()
	repo := newIntegrationTestRepo(t)
	ts := time.Date(2026, 9, 24, 13, 30, 0, 0, time.Local)

	if err := repo.SaveVKMRawString(ctx, "vkm", 1, ts, "S=99.25;"); err != nil {
		t.Fatal(err)
	}

	const pointID = 7002
	writer := &fakeInsertOnlyWriter{
		existing: map[string]bool{
			fakePointKey(pointID, ts): true,
		},
	}
	cfg := Config{
		DeviceID: "vkm",
		Pipe:     1,
		Kind:     "vkm360",
		Points: []PointMapping{
			{Tag: "S", PointID: pointID, Factor: 1, Label: "масса"},
		},
	}

	stats, err := healESDirtyRanges(ctx, repo, writer, cfg, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Existing != 1 || stats.Inserted != 0 || stats.RangesCompleted != 1 {
		t.Fatalf("unexpected insert-only stats: %+v", stats)
	}
	if len(writer.inserted) != 0 {
		t.Fatalf("existing ES point must never be overwritten/inserted again: %#v", writer.inserted)
	}
}

func TestDirtyHealerKeepsRangePendingOnESFailure(t *testing.T) {
	ctx := context.Background()
	repo := newIntegrationTestRepo(t)
	ts := time.Date(2026, 9, 24, 13, 30, 0, 0, time.Local)

	if err := repo.SaveVKMRawString(ctx, "vkm", 1, ts, "S=7.5;"); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		DeviceID: "vkm",
		Pipe:     1,
		Kind:     "vkm360",
		Points: []PointMapping{
			{Tag: "S", PointID: 7003, Factor: 1, Label: "масса"},
		},
	}

	failing := &fakeInsertOnlyWriter{pointExistsErr: errors.New("ЭС недоступна")}
	stats, err := healESDirtyRanges(ctx, repo, failing, cfg, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Failed != 1 || stats.RangesCompleted != 0 {
		t.Fatalf("unexpected failure stats: %+v", stats)
	}
	n, err := repo.CountESDirtyRanges(ctx, "vkm")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("failed ES check must leave dirty range durable, got %d", n)
	}

	// Once ES is available again, the same durable item is processed and
	// removed without needing another local archive write.
	okWriter := &fakeInsertOnlyWriter{}
	stats, err = healESDirtyRanges(ctx, repo, okWriter, cfg, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Inserted != 1 || stats.RangesCompleted != 1 {
		t.Fatalf("unexpected recovery stats: %+v", stats)
	}
	n, err = repo.CountESDirtyRanges(ctx, "vkm")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("dirty range must drain after ES recovery, got %d", n)
	}
}

func TestDirtyHealerAkronCorrectionRechecksFollowingDerivedHour(t *testing.T) {
	ctx := context.Background()
	repo := newIntegrationTestRepo(t)
	loc := time.Local
	t15 := time.Date(2026, 9, 24, 15, 0, 0, 0, loc)
	t16 := t15.Add(time.Hour)
	t17 := t16.Add(time.Hour)

	saveAkronV(t, repo, t15, 1000)
	saveAkronV(t, repo, t16, 1100)
	saveAkronV(t, repo, t17, 1250)

	// Clear setup dirtiness. The test starts from a fully reconciled state,
	// then corrects the 16:00 cumulative snapshot.
	for {
		ranges, err := repo.ListESDirtyRanges(ctx, "osmos", 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(ranges) == 0 {
			break
		}
		for _, dr := range ranges {
			ok, err := repo.CompleteESDirtyRange(ctx, dr)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("unexpected dirty version race in test setup")
			}
		}
	}

	// The corrected 16:00 cumulative value changes BOTH the hour ending
	// 16:00 (15->16) and the following hour ending 17:00 (16->17).
	saveAkronV(t, repo, t16, 1120)

	writer := &fakeInsertOnlyWriter{}
	cfg := Config{
		DeviceID: "osmos",
		Kind:     "akron",
		Points: []PointMapping{
			{Tag: "V", PointID: 7100, Factor: 1, Label: "объём"},
		},
	}

	stats, err := healESDirtyRanges(ctx, repo, writer, cfg, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats.RangesCompleted != 1 {
		t.Fatalf("want corrected source range completed, got %+v", stats)
	}
	if len(writer.inserted) != 4 {
		t.Fatalf("want 4 half-hour points for the two affected hours, got %d: %#v", len(writer.inserted), writer.inserted)
	}

	want := []float64{60, 60, 65, 65} // (1120-1000)/2, (1250-1120)/2
	for i, v := range want {
		if writer.inserted[i].value != v {
			t.Fatalf("inserted[%d] value=%g want=%g", i, writer.inserted[i].value, v)
		}
	}
}
