package web

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"mbgw/internal/storage"
	sqliterepo "mbgw/internal/storage/sqlite"
)

func newVKMMultipipeWebTestServer(t *testing.T) (*Server, *sqliterepo.Repo) {
	t.Helper()
	repo, err := sqliterepo.New(filepath.Join(t.TempDir(), "vkm_multipipe.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	ctx := context.Background()
	if err := repo.InitSchema(ctx); err != nil {
		t.Fatalf("init base/archive schema: %v", err)
	}
	if err := repo.InitDeviceConfigSchema(ctx); err != nil {
		t.Fatalf("init device config schema: %v", err)
	}
	if err := repo.UpsertDevice(ctx, sqliterepo.DeviceRecord{
		ID:              "vkm_multi",
		Kind:            "vkm360",
		ArchiveAtMinute: -1,
		Enabled:         true,
	}); err != nil {
		t.Fatalf("upsert device: %v", err)
	}
	if err := repo.SetVKMActivePipes(ctx, "vkm_multi", []int{1, 2}); err != nil {
		t.Fatalf("set active pipes: %v", err)
	}
	return NewServer(repo, 0), repo
}

func TestLoadArchiveTableVKMSelectsPipeChannel(t *testing.T) {
	s, repo := newVKMMultipipeWebTestServer(t)
	ctx := context.Background()
	ts := time.Date(2026, 9, 24, 12, 30, 0, 0, time.Local)

	for _, tc := range []struct {
		channel string
		value   float64
	}{
		{channel: "", value: 101.5},
		{channel: "2", value: 202.5},
	} {
		if err := repo.SaveHourlyArchive(ctx, storage.HourlyArchiveRecord{
			DeviceID: "vkm_multi",
			Channel:  tc.channel,
			Param:    "T",
			TsHour:   ts,
			Value:    tc.value,
			Unit:     "C",
		}); err != nil {
			t.Fatalf("save channel %q: %v", tc.channel, err)
		}
	}

	req := httptest.NewRequest("GET", "/api/archive?device_id=vkm_multi&from=2026-09-24T12:00&to=2026-09-24T13:00&granularity=raw&pipe=2", nil)
	resp, err := s.loadArchiveTable(req)
	if err != nil {
		t.Fatalf("load pipe 2: %v", err)
	}
	if resp.Pipe != 2 || len(resp.Pipes) != 2 || resp.Pipes[0] != 1 || resp.Pipes[1] != 2 {
		t.Fatalf("pipe metadata=%d/%v, want selected=2 active=[1 2]", resp.Pipe, resp.Pipes)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("rows=%d, want 1", len(resp.Rows))
	}
	if got := resp.Rows[0].Values["T"]; got != 202.5 {
		t.Fatalf("pipe 2 T=%v, want 202.5 (pipe 1 value must not leak)", got)
	}

	bad := httptest.NewRequest("GET", "/api/archive?device_id=vkm_multi&from=2026-09-24T12:00&to=2026-09-24T13:00&granularity=raw&pipe=3", nil)
	if _, err := s.loadArchiveTable(bad); err == nil {
		t.Fatal("inactive pipe 3 unexpectedly accepted")
	}
}

func TestComputeVKMArchiveInfoUsesWorstActivePipe(t *testing.T) {
	s, repo := newVKMMultipipeWebTestServer(t)
	ctx := context.Background()
	now := time.Now().Truncate(30 * time.Minute)
	pipe1Latest := now.Add(-30 * time.Minute)
	pipe2Latest := now.Add(-60 * time.Minute)

	if err := repo.SaveVKMRawString(ctx, "vkm_multi", 1, pipe1Latest, "T=1;"); err != nil {
		t.Fatalf("save pipe1 raw: %v", err)
	}
	if err := repo.SaveVKMRawString(ctx, "vkm_multi", 2, pipe2Latest, "V=2;"); err != nil {
		t.Fatalf("save pipe2 raw: %v", err)
	}

	info := s.computeVKMArchiveInfo(ctx, "vkm_multi", -1)
	if !info.LagKnown {
		t.Fatal("LagKnown=false, want true when every active pipe has raw data")
	}
	if want := pipe2Latest.Format("02.01.2006 15:04"); info.LastPeriod != want {
		t.Fatalf("LastPeriod=%q, want worst active pipe %q", info.LastPeriod, want)
	}
}

func TestComputeVKMArchiveInfoUnknownWhenActivePipeHasNoRawData(t *testing.T) {
	s, repo := newVKMMultipipeWebTestServer(t)
	ctx := context.Background()
	latest := time.Now().Truncate(30 * time.Minute).Add(-30 * time.Minute)
	if err := repo.SaveVKMRawString(ctx, "vkm_multi", 1, latest, "T=1;"); err != nil {
		t.Fatalf("save pipe1 raw: %v", err)
	}

	info := s.computeVKMArchiveInfo(ctx, "vkm_multi", -1)
	if info.LagKnown {
		t.Fatalf("LagKnown=true with no raw data for active pipe 2: %+v", info)
	}
}
