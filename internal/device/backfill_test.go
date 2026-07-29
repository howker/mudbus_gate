package device

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"mbgw/internal/lease"
	"mbgw/internal/profile"
	"mbgw/internal/protocol/akron"
	"mbgw/internal/session"
	"mbgw/internal/storage"
	"mbgw/internal/storage/sqlite"
)

// fakeArchiveClient simulates a device whose own ring buffer is fully
// contiguous (index 0 = freshest complete hour, increasing index = older,
// no gaps) — matching how a real Akron behaves. rows[i] is the value/ts
// for archive index i. Requests past len(rows) get an empty response
// (device's own archive end), mirroring akron_live.go's real serving
// logic for command 104.
type fakeArchiveClient struct {
	rows []fakeRow
}

type fakeRow struct {
	ts    time.Time
	value float64
}

func (f *fakeArchiveClient) ReadRaw(ctx context.Context, space string, addr int, dataType string) ([]byte, error) {
	return nil, fmt.Errorf("ReadRaw not used by backfill test")
}

func (f *fakeArchiveClient) Transact(ctx context.Context, req []byte) ([]byte, error) {
	if len(req) < 4 || req[0] != akron.CmdHourlyArchive {
		return nil, fmt.Errorf("fakeArchiveClient: unexpected request %x", req)
	}
	startIndex := int(req[1])<<8 | int(req[2]) // 1-based, per Akron convention
	n := int(req[3])
	from := startIndex - 1 // 0-based

	var data []byte
	for i := from; i < from+n; i++ {
		if i < 0 || i >= len(f.rows) {
			break // device's own archive ends here
		}
		rowBytes, err := akron.EncodeHourlyRow(f.rows[i].value, f.rows[i].ts)
		if err != nil {
			return nil, err
		}
		data = append(data, rowBytes...)
	}
	resp := []byte{akron.CmdHourlyArchive, byte(len(data))}
	resp = append(resp, data...)
	return resp, nil
}

func hourlyArchiveProfile(maxRows, bufferDepthHours int) *profile.Profile {
	return &profile.Profile{
		Codec: profile.Codec{WordOrder32: "0123"},
		Archives: []profile.Archive{
			{
				ID:                "hourly",
				Strategy:          "akron_archive",
				MaxRowsPerRequest: maxRows,
				BufferDepthHours:  bufferDepthHours,
				Params:            map[string]any{"archive_kind": "hourly"},
				RecordLayout: []profile.RecordField{
					{Offset: 0, Name: "volume", Type: "akron_volume"},
					{Offset: 5, Name: "hour", Type: "bcd"},
					{Offset: 6, Name: "day", Type: "bcd"},
					{Offset: 7, Name: "month", Type: "bcd"},
					{Offset: 8, Name: "year", Type: "bcd"},
				},
			},
		},
	}
}

// TestBackfillArchives_ReachesGapBehindFreshData is a regression test for
// the 2026-07-29 production incident: live polling kept the newest hour
// current in the DB, but a ~58h outage left a multi-day hole further
// back. An earlier version of BackfillArchives stopped as soon as a
// page's oldest row was "older than the previously-newest saved row" —
// which is exactly always true on the very first page when the newest
// row is fresh, so it never paged deep enough to reach the real gap. The
// fix computes the actual missing-hours set up front and only stops once
// every one of those hours is covered.
func TestBackfillArchives_ReachesGapBehindFreshData(t *testing.T) {
	now := time.Now().Truncate(time.Hour)

	// Device's own ring buffer: fully contiguous, 40 hours deep.
	rows := make([]fakeRow, 40)
	for i := range rows {
		rows[i] = fakeRow{ts: now.Add(-time.Duration(i) * time.Hour), value: 2000.0 - float64(i)}
	}
	cli := &fakeArchiveClient{rows: rows}

	dbPath := filepath.Join(t.TempDir(), "backfill_test.db")
	repo, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.InitArchiveSchema(context.Background()); err != nil {
		t.Fatalf("init archive schema: %v", err)
	}

	// Seed the DB with ONLY the freshest hour (index 0) — simulating live
	// polling having kept it current while everything behind it is a gap.
	seedRec := storage.HourlyArchiveRecord{
		DeviceID: "akron_test",
		Channel:  "",
		Param:    "V",
		TsHour:   rows[0].ts,
		Value:    rows[0].value,
		Unit:     "m3",
	}
	if err := repo.SaveHourlyArchive(context.Background(), seedRec); err != nil {
		t.Fatalf("seed freshest row: %v", err)
	}

	p := hourlyArchiveProfile(5 /* small page to force many pages */, 30 /* buffer depth hours */)
	dev := New("akron_test", p, cli, &session.NoopSession{}, repo, lease.New())

	dev.BackfillArchives(context.Background(), BackfillOptions{}) // variant "В"

	// A deep hour (well behind the fresh index-0 row, but within the
	// 30h buffer depth) must now be present — this is the exact case the
	// bug missed.
	deepHour := now.Add(-15 * time.Hour)
	got, err := repo.GetHourlyArchiveDesc(context.Background(), "akron_test", "", "V", 0, 100)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	found := false
	for _, r := range got {
		if r.TsHour.Equal(deepHour) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("hour %v (behind the fresh row) was not backfilled — got %d rows total: %v",
			deepHour, len(got), got)
	}

	// No missing hours should remain anywhere in the 30h window.
	missing, err := repo.MissingHours(context.Background(), "akron_test", "", "V", now.Add(-29*time.Hour), now)
	if err != nil {
		t.Fatalf("MissingHours: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("expected no missing hours after backfill, got %d: %v", len(missing), missing)
	}
}
