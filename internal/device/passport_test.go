package device

import (
	"context"
	"path/filepath"
	"testing"

	"mbgw/internal/lease"
	"mbgw/internal/profile"
	"mbgw/internal/session"
	"mbgw/internal/storage/sqlite"
)

// idClient is a PointClient whose Transact returns a canned command-101
// reply and counts how many times it was called (to prove the non-Akron
// gate skips the call entirely).
type idClient struct {
	calls   int
	respPDU []byte
	err     error
}

func (c *idClient) ReadRaw(ctx context.Context, space string, addr int, dataType string) ([]byte, error) {
	return nil, nil
}
func (c *idClient) Transact(ctx context.Context, req []byte) ([]byte, error) {
	c.calls++
	return c.respPDU, c.err
}

// identity101 builds a command-101 response PDU the way the device/simulator
// does: [101][6][type][fwBCD][serial 4B little-endian].
func identity101(devType, fwBCD byte, serial uint32) []byte {
	return []byte{
		101, 6, devType, fwBCD,
		byte(serial), byte(serial >> 8), byte(serial >> 16), byte(serial >> 24),
	}
}

func akronTestProfile() *profile.Profile {
	return &profile.Profile{
		Points:   []profile.Point{{Name: "V", Type: "float"}},
		Archives: []profile.Archive{{ID: "hourly", Strategy: "akron_archive"}},
	}
}

func newPassportDevRepo(t *testing.T) *sqlite.Repo {
	t.Helper()
	r, err := sqlite.New(filepath.Join(t.TempDir(), "passport_dev.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := r.InitPassportSchema(context.Background()); err != nil {
		t.Fatalf("InitPassportSchema: %v", err)
	}
	return r
}

func TestDetectPassport_Akron_SavesPassport(t *testing.T) {
	ctx := context.Background()
	repo := newPassportDevRepo(t)
	cli := &idClient{respPDU: identity101(0x00, 0x37, 12345)}

	dev := New("acron-1", akronTestProfile(), cli, &session.NoopSession{}, repo, lease.New())
	dev.DetectPassport(ctx)

	if cli.calls != 1 {
		t.Fatalf("Transact calls = %d, want 1", cli.calls)
	}
	p, found, err := repo.GetDevicePassport(ctx, "acron-1")
	if err != nil {
		t.Fatalf("GetDevicePassport: %v", err)
	}
	if !found {
		t.Fatal("passport not saved")
	}
	if p.Serial != 12345 || p.Firmware != "3.7" || p.DeviceType != 0x00 {
		t.Fatalf("passport = %+v, want serial=12345 fw=3.7 type=0", p)
	}
}

func TestDetectPassport_NonAkron_Skips(t *testing.T) {
	ctx := context.Background()
	repo := newPassportDevRepo(t)
	cli := &idClient{respPDU: identity101(0x00, 0x37, 999)}

	// Profile with no akron_archive strategy → not an Akron.
	p := &profile.Profile{
		Points:   []profile.Point{{Name: "x", Type: "float"}},
		Archives: []profile.Archive{{ID: "h", Strategy: "mb_func65"}},
	}
	dev := New("merkuriy-1", p, cli, &session.NoopSession{}, repo, lease.New())
	dev.DetectPassport(ctx)

	if cli.calls != 0 {
		t.Fatalf("Transact should not be called for non-Akron, got %d calls", cli.calls)
	}
	if _, found, _ := repo.GetDevicePassport(ctx, "merkuriy-1"); found {
		t.Fatal("passport should not be saved for non-Akron device")
	}
}

func TestDetectPassport_BadReply_NoSave(t *testing.T) {
	ctx := context.Background()
	repo := newPassportDevRepo(t)
	// Wrong length reply — ParseIdentification must reject, nothing saved.
	cli := &idClient{respPDU: []byte{101, 3, 0, 0x37, 1}}

	dev := New("acron-2", akronTestProfile(), cli, &session.NoopSession{}, repo, lease.New())
	dev.DetectPassport(ctx)

	if _, found, _ := repo.GetDevicePassport(ctx, "acron-2"); found {
		t.Fatal("passport should not be saved on unparseable reply")
	}
}
