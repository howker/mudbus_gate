package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mbgw/internal/storage"
)

func newPassportRepo(t *testing.T) *Repo {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "passport.db")
	r, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.db.Close() })
	if err := r.InitPassportSchema(context.Background()); err != nil {
		t.Fatalf("InitPassportSchema: %v", err)
	}
	return r
}

func TestDevicePassport_SaveGet(t *testing.T) {
	ctx := context.Background()
	r := newPassportRepo(t)

	want := storage.DevicePassport{
		DeviceID:   "acron-1",
		Serial:     12345,
		DeviceType: 0x00,
		Firmware:   "3.7",
		UpdatedAt:  time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC),
	}
	if err := r.SaveDevicePassport(ctx, want); err != nil {
		t.Fatalf("SaveDevicePassport: %v", err)
	}

	got, found, err := r.GetDevicePassport(ctx, "acron-1")
	if err != nil {
		t.Fatalf("GetDevicePassport: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if got.Serial != want.Serial || got.DeviceType != want.DeviceType || got.Firmware != want.Firmware {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("UpdatedAt = %v, want %v", got.UpdatedAt, want.UpdatedAt)
	}
}

func TestDevicePassport_NotFound(t *testing.T) {
	ctx := context.Background()
	r := newPassportRepo(t)

	_, found, err := r.GetDevicePassport(ctx, "nope")
	if err != nil {
		t.Fatalf("GetDevicePassport returned error for missing row: %v", err)
	}
	if found {
		t.Fatal("found = true for a device with no passport")
	}
}

func TestDevicePassport_UpsertOverwrites(t *testing.T) {
	ctx := context.Background()
	r := newPassportRepo(t)

	base := storage.DevicePassport{DeviceID: "acron-1", Serial: 111, DeviceType: 0, Firmware: "3.7", UpdatedAt: time.Now().UTC()}
	if err := r.SaveDevicePassport(ctx, base); err != nil {
		t.Fatal(err)
	}
	// Re-identify: same device, corrected serial/firmware.
	upd := storage.DevicePassport{DeviceID: "acron-1", Serial: 222, DeviceType: 0, Firmware: "5.2", UpdatedAt: time.Now().UTC()}
	if err := r.SaveDevicePassport(ctx, upd); err != nil {
		t.Fatal(err)
	}

	got, found, err := r.GetDevicePassport(ctx, "acron-1")
	if err != nil || !found {
		t.Fatalf("get after upsert: found=%v err=%v", found, err)
	}
	if got.Serial != 222 || got.Firmware != "5.2" {
		t.Fatalf("upsert did not overwrite: got serial=%d fw=%q", got.Serial, got.Firmware)
	}
}
