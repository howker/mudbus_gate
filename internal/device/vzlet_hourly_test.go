package device

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/profile"
	"mbgw/internal/storage/sqlite"
)

func TestPersistFunc65HourlySavesIVKFields(t *testing.T) {
	repo, err := sqlite.New(filepath.Join(t.TempDir(), "ivk_hourly_test.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	if err := repo.InitArchiveSchema(context.Background()); err != nil {
		t.Fatalf("init archive schema: %v", err)
	}

	a := profile.Archive{ID: "hourly", RecordLayout: []profile.RecordField{
		{Offset: 0, Name: "archive_time", Type: "uint32"},
		{Offset: 4, Name: "v_plus", Type: "float", Unit: "m3"},
		{Offset: 8, Name: "errors", Type: "bitfield"},
	}}
	ts := time.Date(2026, 9, 8, 11, 17, 0, 0, time.UTC)
	recs := []archive.ArchiveRecord{{
		RecordTS: ts,
		CRCOK:    true,
		Fields: map[string]any{
			"archive_time": ts,
			"v_plus":       123.5,
			"errors":       int64(2),
		},
	}}

	if got := persistFunc65Hourly(context.Background(), repo, "ivk_test", a, recs); got != 2 {
		t.Fatalf("saved = %d, want 2", got)
	}
	rows, err := repo.GetHourlyArchiveDesc(context.Background(), "ivk_test", "", "v_plus", 0, 10)
	if err != nil {
		t.Fatalf("read v_plus: %v", err)
	}
	if len(rows) != 1 || rows[0].Value != 123.5 || rows[0].Unit != "m3" {
		t.Fatalf("unexpected v_plus row: %+v", rows)
	}
	if !rows[0].TsHour.Equal(ts.Truncate(time.Hour)) {
		t.Fatalf("ts = %v, want %v", rows[0].TsHour, ts.Truncate(time.Hour))
	}
}
