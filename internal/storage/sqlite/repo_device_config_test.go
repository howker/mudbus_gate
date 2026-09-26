package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"
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
		ID:                  "vkm_test",
		Name:                "ВКМ тест",
		Kind:                "vkm360",
		Profile:             "profiles/vkm360.yaml",
		TransportKind:       "modbus_tcp",
		Host:                "10.0.0.1",
		Port:                502,
		UnitID:              2,
		Retries:             3,
		CurrentPollSeconds:  3600,
		ArchiveAtMinute:     -1, // unset sentinel
		ArchiveEveryPeriods: 2,
		ArchiveDaysMask:     31, // Mon-Fri
		ArchiveWindowStart:  "08:00",
		ArchiveWindowEnd:    "18:00",
		Enabled:             true,
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
	if got.ArchiveEveryPeriods != 2 || got.ArchiveDaysMask != 31 || got.ArchiveWindowStart != "08:00" || got.ArchiveWindowEnd != "18:00" {
		t.Fatalf("archive schedule round-trip mismatch: %+v", got)
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
	if err := repo.SetVKMActivePipes(ctx, "d1", []int{1, 2}); err != nil {
		t.Fatalf("set vkm active pipes: %v", err)
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
	pipes, err := repo.GetVKMActivePipes(ctx, "d1")
	if err != nil {
		t.Fatalf("get vkm active pipes after delete: %v", err)
	}
	if len(pipes) != 0 {
		t.Fatalf("expected active pipes to be deleted alongside device, got %v", pipes)
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

func TestLastTimeCorrection(t *testing.T) {
	repo := newTestRepoWithDeviceConfig(t)
	ctx := context.Background()

	if _, found, err := repo.LastTimeCorrection(ctx, "vkm1"); err != nil {
		t.Fatalf("last correction before records: %v", err)
	} else if found {
		t.Fatal("expected no correction before records")
	}

	t1 := time.Date(2026, 9, 8, 8, 10, 0, 0, time.UTC)
	t2 := t1.Add(20 * time.Minute)
	if err := repo.RecordTimeCorrection(ctx, "vkm1", 4, t1); err != nil {
		t.Fatalf("record first correction: %v", err)
	}
	if err := repo.RecordTimeCorrection(ctx, "vkm1", -3, t2); err != nil {
		t.Fatalf("record second correction: %v", err)
	}
	if err := repo.RecordTimeCorrection(ctx, "other", 9, t2.Add(time.Minute)); err != nil {
		t.Fatalf("record other correction: %v", err)
	}

	rec, found, err := repo.LastTimeCorrection(ctx, "vkm1")
	if err != nil {
		t.Fatalf("last correction: %v", err)
	}
	if !found {
		t.Fatal("expected last correction to be found")
	}
	if rec.DeviceID != "vkm1" || rec.CorrectionSeconds != -3 || !rec.CorrectedAt.Equal(t2) {
		t.Fatalf("unexpected last correction: %+v", rec)
	}
}

func TestESVKMChannelsLegacyMigrationPreservesMappings(t *testing.T) {
	repo, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx := context.Background()

	if _, err := repo.db.ExecContext(ctx, `
CREATE TABLE es_vkm_channels (
    device_id TEXT NOT NULL,
    tag TEXT NOT NULL,
    es_channel_id INTEGER NOT NULL,
    factor REAL NOT NULL DEFAULT 1.0,
    PRIMARY KEY (device_id, tag)
);
INSERT INTO es_vkm_channels (device_id, tag, es_channel_id, factor) VALUES
    ('vkm1', 'S', 229994, 0.001),
    ('vkm1', 'ST', 230001, 0.000000000238846);
`); err != nil {
		t.Fatal(err)
	}

	if err := repo.InitDeviceConfigSchema(ctx); err != nil {
		t.Fatalf("migrate device config schema: %v", err)
	}

	got, err := repo.GetVKMChannels(ctx, "vkm1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 migrated mappings, got %d: %+v", len(got), got)
	}
	if got[0].PipeNo != 1 || got[0].SlotNo != 1 || got[0].Tag != "ST" || got[0].ESChannelID != 230001 {
		t.Fatalf("slot 1 migration mismatch: %+v", got[0])
	}
	if got[1].PipeNo != 1 || got[1].SlotNo != 2 || got[1].Tag != "S" || got[1].ESChannelID != 229994 {
		t.Fatalf("slot 2 migration mismatch: %+v", got[1])
	}
}

func TestSetVKMChannelsAllowsSameSourceTagOnDifferentPipes(t *testing.T) {
	repo := newTestRepoWithDeviceConfig(t)
	ctx := context.Background()

	err := repo.SetVKMChannels(ctx, "vkm1", []VKMChannelRecord{
		{PipeNo: 1, SlotNo: 1, Tag: "T", ESChannelID: 101, Factor: 1},
		{PipeNo: 2, SlotNo: 1, Tag: "T", ESChannelID: 202, Factor: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetVKMChannels(ctx, "vkm1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].PipeNo != 1 || got[1].PipeNo != 2 || got[0].Tag != "T" || got[1].Tag != "T" {
		t.Fatalf("unexpected mappings: %+v", got)
	}
}

func TestSetVKMChannelsRejectsDuplicateIDPPWithinDevice(t *testing.T) {
	repo := newTestRepoWithDeviceConfig(t)
	ctx := context.Background()

	err := repo.SetVKMChannels(ctx, "vkm1", []VKMChannelRecord{
		{PipeNo: 1, SlotNo: 1, Tag: "S", ESChannelID: 101, Factor: 1},
		{PipeNo: 2, SlotNo: 1, Tag: "T", ESChannelID: 101, Factor: 1},
	})
	if err == nil {
		t.Fatal("want duplicate ID_PP to be rejected")
	}
}

func TestSetVKMChannelsAndActivePipesUpdatesBoth(t *testing.T) {
	repo := newTestRepoWithDeviceConfig(t)
	ctx := context.Background()

	if err := repo.SetVKMActivePipes(ctx, "vkm-ui", []int{1}); err != nil {
		t.Fatalf("seed active pipes: %v", err)
	}
	if err := repo.SetVKMChannels(ctx, "vkm-ui", []VKMChannelRecord{
		{PipeNo: 1, SlotNo: 1, Tag: "T", ESChannelID: 101, Factor: 1},
	}); err != nil {
		t.Fatalf("seed channels: %v", err)
	}

	if err := repo.SetVKMChannelsAndActivePipes(ctx, "vkm-ui", []VKMChannelRecord{
		{PipeNo: 1, SlotNo: 1, Tag: "ST", ESChannelID: 201, Factor: 1},
		{PipeNo: 2, SlotNo: 1, Tag: "V", ESChannelID: 202, Factor: 1},
	}, []int{1, 2}); err != nil {
		t.Fatalf("save combined config: %v", err)
	}

	pipes, err := repo.GetVKMActivePipes(ctx, "vkm-ui")
	if err != nil {
		t.Fatalf("get active pipes: %v", err)
	}
	if len(pipes) != 2 || pipes[0] != 1 || pipes[1] != 2 {
		t.Fatalf("active pipes=%v, want [1 2]", pipes)
	}
	rows, err := repo.GetVKMChannels(ctx, "vkm-ui")
	if err != nil {
		t.Fatalf("get channels: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("channels=%d, want 2", len(rows))
	}
	if rows[0].PipeNo != 1 || rows[0].Tag != "ST" || rows[0].ESChannelID != 201 {
		t.Fatalf("pipe1 mapping=%+v", rows[0])
	}
	if rows[1].PipeNo != 2 || rows[1].Tag != "V" || rows[1].ESChannelID != 202 {
		t.Fatalf("pipe2 mapping=%+v", rows[1])
	}
}

func TestESVKMChannelsMigrationAllowsNineIVKSlotsIsIdempotentAndKeepsCursor(t *testing.T) {
	repo := newTestRepoWithDeviceConfig(t)
	ctx := context.Background()

	if err := repo.UpsertDevice(ctx, DeviceRecord{ID: "ivk1", Kind: "ivk-ter", ArchiveAtMinute: -1}); err != nil {
		t.Fatal(err)
	}
	if err := repo.InitESSyncCursorSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cursorTS := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	if err := repo.SetESSyncCursor(ctx, "ivk1", 5001, cursorTS); err != nil {
		t.Fatal(err)
	}

	// Recreate the pre-D1 tag-keyed table to exercise the real upgrade path.
	if _, err := repo.db.ExecContext(ctx, `DROP TABLE es_vkm_channels`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `
CREATE TABLE es_vkm_channels (
    device_id TEXT NOT NULL,
    tag TEXT NOT NULL,
    es_channel_id INTEGER NOT NULL,
    factor REAL NOT NULL DEFAULT 1.0,
    min_value REAL NULL,
    max_value REAL NULL,
    PRIMARY KEY (device_id, tag)
)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 9; i++ {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO es_vkm_channels(device_id,tag,es_channel_id,factor) VALUES(?,?,?,1)`,
			"ivk1", fmt.Sprintf("P%d", i), 5000+i); err != nil {
			t.Fatal(err)
		}
	}

	if err := repo.InitDeviceConfigSchema(ctx); err != nil {
		t.Fatalf("migration with 9 IVK points failed: %v", err)
	}
	got, err := repo.GetVKMChannels(ctx, "ivk1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 9 {
		t.Fatalf("migrated mappings=%d, want 9", len(got))
	}
	for i, row := range got {
		if row.PipeNo != 1 || row.SlotNo != i+1 {
			t.Fatalf("mapping[%d]=%+v, want pipe=1 slot=%d", i, row, i+1)
		}
	}
	if last, found, err := repo.GetESSyncCursor(ctx, "ivk1", 5001); err != nil || !found || !last.Equal(cursorTS) {
		t.Fatalf("cursor after migration: last=%v found=%v err=%v", last, found, err)
	}

	// Migration must be idempotent and must not renumber rows/cursors.
	if err := repo.InitDeviceConfigSchema(ctx); err != nil {
		t.Fatalf("second InitDeviceConfigSchema: %v", err)
	}
	got2, err := repo.GetVKMChannels(ctx, "ivk1")
	if err != nil || len(got2) != 9 {
		t.Fatalf("idempotent mappings=%d err=%v", len(got2), err)
	}
	if last, found, err := repo.GetESSyncCursor(ctx, "ivk1", 5001); err != nil || !found || !last.Equal(cursorTS) {
		t.Fatalf("cursor after second init: last=%v found=%v err=%v", last, found, err)
	}

	// Generic storage path allows IVK to keep all nine points on fresh schema.
	rows := make([]VKMChannelRecord, 0, 9)
	for i := 1; i <= 9; i++ {
		rows = append(rows, VKMChannelRecord{PipeNo: 1, SlotNo: i, Tag: fmt.Sprintf("P%d", i), ESChannelID: 6000 + i, Factor: 1})
	}
	if err := repo.SetVKMChannels(ctx, "ivk1", rows); err != nil {
		t.Fatalf("fresh-schema IVK 9 points rejected: %v", err)
	}

	if err := repo.UpsertDevice(ctx, DeviceRecord{ID: "vkm5", Kind: "vkm360", ArchiveAtMinute: -1}); err != nil {
		t.Fatal(err)
	}
	vkmRows := make([]VKMChannelRecord, 0, 5)
	for i := 1; i <= 5; i++ {
		vkmRows = append(vkmRows, VKMChannelRecord{PipeNo: 1, SlotNo: i, Tag: fmt.Sprintf("T%d", i), ESChannelID: 7000 + i, Factor: 1})
	}
	if err := repo.SetVKMChannels(ctx, "vkm5", vkmRows); err == nil {
		t.Fatal("VKM fifth slot must still be rejected")
	}
}

func TestESVKMChannelsRebuildsAlreadyD1ConstrainedSchema(t *testing.T) {
	repo := newTestRepoWithDeviceConfig(t)
	ctx := context.Background()
	if err := repo.UpsertDevice(ctx, DeviceRecord{ID: "ivk-d1", Kind: "ivk-ter", ArchiveAtMinute: -1}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `DROP TABLE es_vkm_channels`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `
CREATE TABLE es_vkm_channels (
 device_id TEXT NOT NULL,
 pipe_no INTEGER NOT NULL DEFAULT 1 CHECK (pipe_no BETWEEN 1 AND 10),
 slot_no INTEGER NOT NULL CHECK (slot_no BETWEEN 1 AND 4),
 source_tag TEXT NOT NULL,
 es_channel_id INTEGER NOT NULL,
 factor REAL NOT NULL DEFAULT 1.0,
 min_value REAL NULL,
 max_value REAL NULL,
 PRIMARY KEY(device_id,pipe_no,slot_no),
 UNIQUE(device_id,es_channel_id)
)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO es_vkm_channels VALUES(?,?,?,?,?,?,NULL,NULL)`,
			"ivk-d1", 1, i, fmt.Sprintf("P%d", i), 8000+i, 1.0); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.InitDeviceConfigSchema(ctx); err != nil {
		t.Fatalf("relax D1 schema: %v", err)
	}
	rows := make([]VKMChannelRecord, 0, 9)
	for i := 1; i <= 9; i++ {
		rows = append(rows, VKMChannelRecord{PipeNo: 1, SlotNo: i, Tag: fmt.Sprintf("P%d", i), ESChannelID: 8100 + i, Factor: 1})
	}
	if err := repo.SetVKMChannels(ctx, "ivk-d1", rows); err != nil {
		t.Fatalf("already-D1 database still rejects IVK slot 5..9: %v", err)
	}
}
