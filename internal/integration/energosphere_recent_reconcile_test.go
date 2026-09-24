package integration

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeRecentWriter struct {
	fakeInsertOnlyWriter
	existingByPoint map[int]map[string]struct{}
	queryErrByPoint map[int]error
	queryCount      int
}

func (w *fakeRecentWriter) ExistingPointTimes(_ context.Context, pointID int, _, _ time.Time) (map[string]struct{}, error) {
	w.queryCount++
	if err := w.queryErrByPoint[pointID]; err != nil {
		return nil, err
	}
	out := make(map[string]struct{})
	for k := range w.existingByPoint[pointID] {
		out[k] = struct{}{}
	}
	return out, nil
}

func TestRecentReconcileFindsMissingPointsWithoutDirtyMarker(t *testing.T) {
	ctx := context.Background()
	repo := newIntegrationTestRepo(t)
	loc := time.Local
	t1 := time.Date(2026, 9, 24, 13, 0, 0, 0, loc)
	t2 := t1.Add(time.Hour)
	from := t1.Add(-time.Minute)
	to := t2.Add(time.Minute)

	if err := repo.SaveVKMRawString(ctx, "vkm", 1, t1, "S=10;T=20;"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveVKMRawString(ctx, "vkm", 1, t2, "S=11;T=21;"); err != nil {
		t.Fatal(err)
	}

	// Prove the periodic scan is independent of the durable dirty queue.
	for {
		ranges, err := repo.ListESDirtyRanges(ctx, "vkm", 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(ranges) == 0 {
			break
		}
		for _, dr := range ranges {
			if _, err := repo.CompleteESDirtyRange(ctx, dr); err != nil {
				t.Fatal(err)
			}
		}
	}

	const pointS = 7101
	const pointT = 7102
	writer := &fakeRecentWriter{
		existingByPoint: map[int]map[string]struct{}{
			pointS: {pointMainsTimeKey(t1): {}},
			pointT: {pointMainsTimeKey(t2): {}},
		},
		queryErrByPoint: map[int]error{},
	}
	cfg := Config{
		DeviceID: "vkm",
		Pipe:     1,
		Kind:     "vkm360",
		Points: []PointMapping{
			{Tag: "S", PointID: pointS, Factor: 1, Label: "масса"},
			{Tag: "T", PointID: pointT, Factor: 1, Label: "температура"},
		},
	}

	stats, err := reconcileRecentESWindow(ctx, repo, writer, cfg, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if stats.LocalReadings != 4 || stats.BatchQueries != 2 || stats.Existing != 2 || stats.Inserted != 2 || stats.Failed != 0 {
		t.Fatalf("unexpected recent-reconcile stats: %+v", stats)
	}
	if writer.queryCount != 2 {
		t.Fatalf("want one batch existence query per ID_PP, got %d", writer.queryCount)
	}
	if len(writer.inserted) != 2 {
		t.Fatalf("want 2 missing points inserted, got %d: %#v", len(writer.inserted), writer.inserted)
	}
}

func TestRecentReconcileDoesNotMoveOrdinaryCursor(t *testing.T) {
	ctx := context.Background()
	repo := newIntegrationTestRepo(t)
	if err := repo.InitESSyncCursorSchema(ctx); err != nil {
		t.Fatal(err)
	}

	loc := time.Local
	ts := time.Date(2026, 9, 24, 13, 0, 0, 0, loc)
	cursor := time.Date(2026, 9, 24, 15, 0, 0, 0, loc)
	const pointID = 7201
	if err := repo.SetESSyncCursor(ctx, "vkm", pointID, cursor); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveVKMRawString(ctx, "vkm", 1, ts, "S=12.5;"); err != nil {
		t.Fatal(err)
	}

	writer := &fakeRecentWriter{
		existingByPoint: map[int]map[string]struct{}{},
		queryErrByPoint: map[int]error{},
	}
	cfg := Config{
		DeviceID: "vkm",
		Pipe:     1,
		Kind:     "vkm360",
		Points: []PointMapping{
			{Tag: "S", PointID: pointID, Factor: 1, Label: "масса"},
		},
	}

	stats, err := reconcileRecentESWindow(ctx, repo, writer, cfg, ts.Add(-time.Minute), ts.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Inserted != 1 {
		t.Fatalf("want one late point inserted, got %+v", stats)
	}
	got, found, err := repo.GetESSyncCursor(ctx, "vkm", pointID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !got.Equal(cursor) {
		t.Fatalf("periodic reconciliation changed ordinary cursor: found=%v got=%v want=%v", found, got, cursor)
	}
}

func TestRecentReconcileBatchQueryFailureDoesNotBlockOtherPoints(t *testing.T) {
	ctx := context.Background()
	repo := newIntegrationTestRepo(t)
	ts := time.Date(2026, 9, 24, 13, 0, 0, 0, time.Local)
	if err := repo.SaveVKMRawString(ctx, "vkm", 1, ts, "S=10;T=20;"); err != nil {
		t.Fatal(err)
	}

	const badPoint = 7301
	const goodPoint = 7302
	writer := &fakeRecentWriter{
		existingByPoint: map[int]map[string]struct{}{},
		queryErrByPoint: map[int]error{badPoint: errors.New("query failed")},
	}
	cfg := Config{
		DeviceID: "vkm",
		Pipe:     1,
		Kind:     "vkm360",
		Points: []PointMapping{
			{Tag: "S", PointID: badPoint, Factor: 1, Label: "масса"},
			{Tag: "T", PointID: goodPoint, Factor: 1, Label: "температура"},
		},
	}

	stats, err := reconcileRecentESWindow(ctx, repo, writer, cfg, ts.Add(-time.Minute), ts.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if stats.BatchQueries != 2 || stats.Failed != 1 || stats.Inserted != 1 {
		t.Fatalf("unexpected partial-failure stats: %+v", stats)
	}
	if len(writer.inserted) != 1 || writer.inserted[0].pointID != goodPoint {
		t.Fatalf("good point must still reconcile: %#v", writer.inserted)
	}
}

func TestPointMainsTimeKeyIgnoresLocationButKeepsWallClock(t *testing.T) {
	fixed := time.FixedZone("UTC+4", 4*60*60)
	a := time.Date(2026, 9, 24, 13, 30, 0, 0, fixed)
	b := time.Date(2026, 9, 24, 13, 30, 0, 0, time.UTC)
	if pointMainsTimeKey(a) != pointMainsTimeKey(b) {
		t.Fatalf("same PointMains wall-clock must have same key: %q vs %q", pointMainsTimeKey(a), pointMainsTimeKey(b))
	}
}
