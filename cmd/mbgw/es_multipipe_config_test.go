package main

import (
	"context"
	"path/filepath"
	"testing"

	sqliterepo "mbgw/internal/storage/sqlite"
)

func TestBuildIntegrationConfigKeepsVKMPipePerMapping(t *testing.T) {
	repo, err := sqliterepo.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx := context.Background()
	if err := repo.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repo.InitDeviceConfigSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetESConnection(ctx, sqliterepo.ESConnection{
		SQLServer: "server", SQLDatabase: "db", SQLUser: "user", SQLPassword: "pass", SQLPort: 1433,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetVKMChannels(ctx, "vkm", []sqliterepo.VKMChannelRecord{
		{PipeNo: 1, SlotNo: 1, Tag: "T", ESChannelID: 101, Factor: 1},
		{PipeNo: 2, SlotNo: 1, Tag: "T", ESChannelID: 202, Factor: 1},
	}); err != nil {
		t.Fatal(err)
	}

	cfg, found, err := buildIntegrationConfig(ctx, repo, "vkm", "vkm360")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("want integration config to be found")
	}
	if len(cfg.Points) != 2 {
		t.Fatalf("want 2 points, got %d: %+v", len(cfg.Points), cfg.Points)
	}
	if cfg.Points[0].PointID != 101 || cfg.Points[0].Pipe != 1 || cfg.Points[0].Tag != "T" {
		t.Fatalf("pipe 1 mapping mismatch: %+v", cfg.Points[0])
	}
	if cfg.Points[1].PointID != 202 || cfg.Points[1].Pipe != 2 || cfg.Points[1].Tag != "T" {
		t.Fatalf("pipe 2 mapping mismatch: %+v", cfg.Points[1])
	}
}
