package sqlite

import (
	"context"
	"testing"
)

func newTestRepoWithDeviceConfig(t *testing.T) *Repo {
	t.Helper()
	repo, err := New(":memory:")
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { repo.Close() })
	ctx := context.Background()
	if err := repo.InitSchema(ctx); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := repo.InitDeviceConfigSchema(ctx); err != nil {
		t.Fatalf("init device config schema: %v", err)
	}
	return repo
}

func TestUpsertGetListDevice(t *testing.T) {
	repo := newTestRepoWithDeviceConfig(t)
	ctx := context.Background()

	rec := DeviceRecord{
		ID:                 "vkm_test",
		Name:               "ВКМ тест",
		Kind:               "vkm360",
		Profile:            "profiles/vkm360.yaml",
		TransportKind:      "modbus_tcp",
		Host:               "10.0.0.1",
		Port:               502,
		UnitID:             2,
		CurrentPollSeconds: 3600,
		ArchiveAtMinute:    -1, // unset sentinel
		Enabled:            true,
	}
	if err := repo.UpsertDevice(ctx, rec); err != nil {
		t.Fatalf("upsert device: %v", err)
	}

	got, found, err := repo.GetDevice(ctx, "vkm_test")
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if !found {
		t.Fatal("expected device to be found")
	}
	if got.Name != rec.Name || got.Kind != rec.Kind || got.Host != rec.Host || got.Port != rec.Port {
		t.Fatalf("round-trip mismatch: got %+v, want fields from %+v", got, rec)
	}
	if got.ArchiveAtMinute != -1 {
		t.Fatalf("expected ArchiveAtMinute sentinel -1 preserved, got %d", got.ArchiveAtMinute)
	}
	if !got.Enabled {
		t.Fatal("expected Enabled=true to round-trip")
	}

	// Upsert again with a changed field — verify it's an UPDATE, not a
	// duplicate row.
	rec.Name = "ВКМ тест (переименован)"
	rec.Enabled = false
	if err := repo.UpsertDevice(ctx, rec); err != nil {
		t.Fatalf("upsert device (update): %v", err)
	}

	list, err := repo.ListDevices(ctx)
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 device after re-upsert, got %d", len(list))
	}
	if list[0].Name != "ВКМ тест (переименован)" {
		t.Fatalf("expected updated name, got %q", list[0].Name)
	}
	if list[0].Enabled {
		t.Fatal("expected Enabled=false after update")
	}
}

func TestGetDevice_NotFound(t *testing.T) {
	repo := newTestRepoWithDeviceConfig(t)
	_, found, err := repo.GetDevice(context.Background(), "does_not_exist")
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if found {
		t.Fatal("expected found=false for a device that was never inserted")
	}
}

func TestDeleteDevice_RemovesChannelsToo(t *testing.T) {
	repo := newTestRepoWithDeviceConfig(t)
	ctx := context.Background()

	if err := repo.UpsertDevice(ctx, DeviceRecord{ID: "d1", Kind: "vkm360", ArchiveAtMinute: -1}); err != nil {
		t.Fatalf("upsert device: %v", err)
	}
	if err := repo.SetVKMChannels(ctx, "d1", []VKMChannelRecord{
		{DeviceID: "d1", Tag: "ST", ESChannelID: 230001, Factor: 1.0},
	}); err != nil {
		t.Fatalf("set vkm channels: %v", err)
	}
	if err := repo.SetAkronNorthboundAddr(ctx, "d1", "127.0.0.1:15021"); err != nil {
		t.Fatalf("set akron northbound addr: %v", err)
	}

	if err := repo.DeleteDevice(ctx, "d1"); err != nil {
		t.Fatalf("delete device: %v", err)
	}

	_, found, err := repo.GetDevice(ctx, "d1")
	if err != nil {
		t.Fatalf("get device after delete: %v", err)
	}
	if found {
		t.Fatal("expected device to be gone after delete")
	}
	chans, err := repo.GetVKMChannels(ctx, "d1")
	if err != nil {
		t.Fatalf("get vkm channels after delete: %v", err)
	}
	if len(chans) != 0 {
		t.Fatalf("expected channels to be deleted alongside device, got %d rows", len(chans))
	}
	addr, found, err := repo.GetAkronNorthboundAddr(ctx, "d1")
	if err != nil {
		t.Fatalf("get akron northbound addr after delete: %v", err)
	}
	if found || addr != "" {
		t.Fatal("expected akron northbound addr to be deleted alongside device")
	}
}

func TestSetVKMChannels_ReplacesWholeSet(t *testing.T) {
	repo := newTestRepoWithDeviceConfig(t)
	ctx := context.Background()

	if err := repo.UpsertDevice(ctx, DeviceRecord{ID: "d1", Kind: "vkm360", ArchiveAtMinute: -1}); err != nil {
		t.Fatalf("upsert device: %v", err)
	}

	// First submission: 2 tags.
	if err := repo.SetVKMChannels(ctx, "d1", []VKMChannelRecord{
		{DeviceID: "d1", Tag: "ST", ESChannelID: 230001, Factor: 1.0},
		{DeviceID: "d1", Tag: "S", ESChannelID: 229994, Factor: 1.0},
	}); err != nil {
		t.Fatalf("set vkm channels (first): %v", err)
	}

	// Second submission: operator removed one tag and added a different
	// one — SetVKMChannels must reflect exactly this, not merge with the
	// first call's rows (the UI form always submits the whole table).
	if err := repo.SetVKMChannels(ctx, "d1", []VKMChannelRecord{
		{DeviceID: "d1", Tag: "ST", ESChannelID: 230001, Factor: 1.0},
		{DeviceID: "d1", Tag: "Pi", ESChannelID: 230004, Factor: 1.0},
	}); err != nil {
		t.Fatalf("set vkm channels (second): %v", err)
	}

	got, err := repo.GetVKMChannels(ctx, "d1")
	if err != nil {
		t.Fatalf("get vkm channels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 channels after replace, got %d: %+v", len(got), got)
	}
	tags := map[string]bool{}
	for _, c := range got {
		tags[c.Tag] = true
	}
	if !tags["ST"] || !tags["Pi"] || tags["S"] {
		t.Fatalf("expected {ST,Pi} after replace, got %+v", got)
	}
}

func TestESConnection_RoundTripAndNotFound(t *testing.T) {
	repo := newTestRepoWithDeviceConfig(t)
	ctx := context.Background()

	_, found, err := repo.GetESConnection(ctx)
	if err != nil {
		t.Fatalf("get es connection (before set): %v", err)
	}
	if found {
		t.Fatal("expected found=false before any es connection is configured")
	}

	conn := ESConnection{
		SQLServer:   "localhost",
		SQLDatabase: "CSD_Astrakhan",
		SQLUser:     "AdminBaz",
		SQLPassword: "test-password-not-real",
		SQLPort:     1433,
	}
	if err := repo.SetESConnection(ctx, conn); err != nil {
		t.Fatalf("set es connection: %v", err)
	}

	got, found, err := repo.GetESConnection(ctx)
	if err != nil {
		t.Fatalf("get es connection (after set): %v", err)
	}
	if !found {
		t.Fatal("expected found=true after set")
	}
	if got != conn {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, conn)
	}

	// Setting again must UPDATE the single row, not fail/duplicate (the
	// id=1 CHECK constraint would reject a second row outright if this
	// were a plain INSERT).
	conn.SQLPassword = "changed-password"
	if err := repo.SetESConnection(ctx, conn); err != nil {
		t.Fatalf("set es connection (update): %v", err)
	}
	got2, _, err := repo.GetESConnection(ctx)
	if err != nil {
		t.Fatalf("get es connection (after update): %v", err)
	}
	if got2.SQLPassword != "changed-password" {
		t.Fatalf("expected updated password, got %q", got2.SQLPassword)
	}
}

func TestAkronNorthboundAddr_RoundTripAndNotFound(t *testing.T) {
	repo := newTestRepoWithDeviceConfig(t)
	ctx := context.Background()

	_, found, err := repo.GetAkronNorthboundAddr(ctx, "akron_test")
	if err != nil {
		t.Fatalf("get akron northbound addr (before set): %v", err)
	}
	if found {
		t.Fatal("expected found=false before any address is configured")
	}

	if err := repo.SetAkronNorthboundAddr(ctx, "akron_test", "127.0.0.1:15021"); err != nil {
		t.Fatalf("set akron northbound addr: %v", err)
	}
	addr, found, err := repo.GetAkronNorthboundAddr(ctx, "akron_test")
	if err != nil {
		t.Fatalf("get akron northbound addr (after set): %v", err)
	}
	if !found || addr != "127.0.0.1:15021" {
		t.Fatalf("round-trip mismatch: found=%v addr=%q", found, addr)
	}
}
