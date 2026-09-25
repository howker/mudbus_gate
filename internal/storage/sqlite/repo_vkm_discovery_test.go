package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestVKMPipeDiscoveryPersistsSeparatelyFromActivePipes(t *testing.T) {
	repo, err := New(filepath.Join(t.TempDir(), "vkm_discovery.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	ctx := context.Background()
	if err := repo.InitSchema(ctx); err != nil {
		t.Fatalf("init base schema: %v", err)
	}
	if err := repo.InitDeviceConfigSchema(ctx); err != nil {
		t.Fatalf("init device config schema: %v", err)
	}
	if err := repo.SetVKMActivePipes(ctx, "vkm-1", []int{1, 2}); err != nil {
		t.Fatalf("set active pipes: %v", err)
	}

	scannedAt := time.Date(2026, 9, 25, 21, 0, 0, 0, time.Local)
	rows := make([]VKMPipeDiscoveryRecord, 0, 10)
	for pipe := 1; pipe <= 10; pipe++ {
		status := VKMDiscoveryAbsent
		if pipe == 1 || pipe == 2 {
			status = VKMDiscoveryAvailable
		}
		tags := []string(nil)
		if status == VKMDiscoveryAvailable {
			tags = []string{"T", "Pi"}
		}
		rows = append(rows, VKMPipeDiscoveryRecord{
			DeviceID: "vkm-1", PipeNo: pipe, Status: status, Tags: tags, ScannedAt: scannedAt,
		})
	}
	if err := repo.ReplaceVKMPipeDiscovery(ctx, "vkm-1", rows); err != nil {
		t.Fatalf("replace discovery: %v", err)
	}

	got, err := repo.GetVKMPipeDiscovery(ctx, "vkm-1")
	if err != nil {
		t.Fatalf("get discovery: %v", err)
	}
	if len(got) != 10 || got[0].Status != VKMDiscoveryAvailable || got[1].Status != VKMDiscoveryAvailable || got[2].Status != VKMDiscoveryAbsent {
		t.Fatalf("discovery=%+v", got)
	}
	if len(got[0].Tags) != 2 || got[0].Tags[0] != "T" || got[0].Tags[1] != "Pi" {
		t.Fatalf("pipe1 tags=%v, want [T Pi]", got[0].Tags)
	}

	active, err := repo.GetVKMActivePipes(ctx, "vkm-1")
	if err != nil {
		t.Fatalf("get active pipes: %v", err)
	}
	if len(active) != 2 || active[0] != 1 || active[1] != 2 {
		t.Fatalf("active pipes changed by discovery: %v", active)
	}
}

func TestReplaceVKMPipeDiscoveryRejectsDuplicatePipe(t *testing.T) {
	repo, err := New(filepath.Join(t.TempDir(), "vkm_discovery_duplicate.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	ctx := context.Background()
	if err := repo.InitSchema(ctx); err != nil {
		t.Fatalf("init base schema: %v", err)
	}
	if err := repo.InitDeviceConfigSchema(ctx); err != nil {
		t.Fatalf("init device config schema: %v", err)
	}

	err = repo.ReplaceVKMPipeDiscovery(ctx, "vkm-1", []VKMPipeDiscoveryRecord{
		{PipeNo: 1, Status: VKMDiscoveryAvailable},
		{PipeNo: 1, Status: VKMDiscoveryAbsent},
	})
	if err == nil {
		t.Fatal("duplicate discovery pipe unexpectedly accepted")
	}
}
