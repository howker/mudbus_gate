package device

import (
    "context"
    "testing"
    "time"

    "mbgw/internal/lease"
    "mbgw/internal/profile"
    "mbgw/internal/session"
    "mbgw/internal/storage/sqlite"
)

type mockClient struct{}

func (m *mockClient) ReadRaw(ctx context.Context, space string, addr int, dataType string) ([]byte, error) {
    // митируем ответ чистого PDU (123.4567).
    return []byte{0x42, 0xF6, 0xE9, 0xD5}, nil
}

func (m *mockClient) Transact(ctx context.Context, req []byte) ([]byte, error) {
    // Not exercised by TestDevicePoll (current-values path only); archive
    // polling tests will need a more capable fake once wired into Device.
    return nil, nil
}

func TestDevicePoll(t *testing.T) {
    p := &profile.Profile{
        Codec: profile.Codec{WordOrder32: "0123"},
        Points: []profile.Point{
            {Name: "Тестовый расход", Type: "float", Unit: "кг/с"},
        },
    }
    cli := &mockClient{}
    sess := &session.NoopSession{}
    repo, _ := sqlite.New("test_worker.json")
    leaseMgr := lease.New()
    dev := New("vkm_test", p, cli, sess, repo, leaseMgr)
    dev.poll(context.Background())
}
func TestDecodePoint_ByteOffset_PackedBCDRegister(t *testing.T) {
    // Simulates Akron-01/02's clock register (e.g. 0x0010), which packs
    // two BCD fields into one 16-bit register: byte 0 = seconds, byte 1 =
    // minutes. Two Points share the same register data, differing only
    // by byte_offset - which is exactly the scenario this feature exists
    // for.
    p := &profile.Profile{}
    dev := &Device{ID: "akron_test", Profile: p}

    regData := []byte{0x00, 0x55} // seconds=0x00, minutes=0x55 (BCD)

    secondPt := profile.Point{Name: "second", Type: "bcd", ByteOffset: 0}
    val, err := dev.decodePoint(secondPt, regData)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if val != int64(0) {
        t.Fatalf("second: want 0, got %v", val)
    }

    minutePt := profile.Point{Name: "minute", Type: "bcd", ByteOffset: 1}
    val, err = dev.decodePoint(minutePt, regData)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if val != int64(55) {
        t.Fatalf("minute: want 55, got %v", val)
    }
}

func TestDecodePoint_ByteOffset_OutOfRangeIsError(t *testing.T) {
    p := &profile.Profile{}
    dev := &Device{ID: "akron_test", Profile: p}

    pt := profile.Point{Name: "bogus", Type: "bcd", ByteOffset: 5}
    if _, err := dev.decodePoint(pt, []byte{0x00, 0x55}); err == nil {
        t.Fatal("expected error for byte_offset beyond the read data")
    }
}

func TestDecodePoint_ByteOffset_ZeroIsUnchanged(t *testing.T) {
    // byte_offset=0 (the default/common case) must behave exactly as
    // before this feature existed.
    p := &profile.Profile{Codec: profile.Codec{WordOrder32: "0123"}}
    dev := &Device{ID: "vkm_test", Profile: p}

    pt := profile.Point{Name: "flow", Type: "float", ByteOffset: 0}
    val, err := dev.decodePoint(pt, []byte{0x42, 0xF6, 0xE9, 0xD5})
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    got, ok := val.(float64)
    if !ok {
        t.Fatalf("expected float64, got %T", val)
    }
    if got < 123.45 || got > 123.46 {
        t.Fatalf("expected ~123.4567, got %v", got)
    }
}

func TestDecodePoint_Epoch_ReturnsTime(t *testing.T) {
    p := &profile.Profile{Codec: profile.Codec{WordOrder32: "0123"}}
    dev := &Device{ID: "ivk_test", Profile: p}

    // Unix epoch 1700000000 = 2023-11-14 22:13:20 UTC, big-endian on wire.
    pt := profile.Point{Name: "current_time", Type: "uint32", Epoch: "1970-01-01"}
    val, err := dev.decodePoint(pt, []byte{0x65, 0x53, 0xF1, 0x00})
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    got, ok := val.(time.Time)
    if !ok {
        t.Fatalf("expected time.Time, got %T", val)
    }
    want := time.Unix(1700000000, 0).UTC()
    if !got.Equal(want) {
        t.Fatalf("got %v, want %v", got, want)
    }
}

func TestDecodePoint_NoEpoch_ReturnsInt64(t *testing.T) {
    // Regression check: uint32 points without epoch must keep returning
    // a plain int64, unaffected by the new Epoch field.
    p := &profile.Profile{Codec: profile.Codec{WordOrder32: "0123"}}
    dev := &Device{ID: "ivk_test", Profile: p}

    pt := profile.Point{Name: "some_counter", Type: "uint32"}
    val, err := dev.decodePoint(pt, []byte{0x00, 0x00, 0x00, 0x2A})
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if val != int64(42) {
        t.Fatalf("expected int64(42), got %v (%T)", val, val)
    }
}

// trackingClient wraps mockClient to record which addresses ReadRaw was
// called for, so tests can verify write-only points are skipped by poll().
type trackingClient struct {
    mockClient
    readAddrs []int
}

func (t *trackingClient) ReadRaw(ctx context.Context, space string, addr int, dataType string) ([]byte, error) {
    t.readAddrs = append(t.readAddrs, addr)
    return t.mockClient.ReadRaw(ctx, space, addr, dataType)
}

func TestPoll_SkipsWriteOnlyPoints(t *testing.T) {
    p := &profile.Profile{
        Codec: profile.Codec{WordOrder32: "0123"},
        Points: []profile.Point{
            {Name: "readable", Type: "float", Addr: intPtr(100), Access: "read"},
            {Name: "time_set", Type: "uint32", Addr: intPtr(32768), Access: "write"},
        },
    }
    cli := &trackingClient{}
    sess := &session.NoopSession{}
    repo, _ := sqlite.New("test_worker.json")
    leaseMgr := lease.New()
    dev := New("vzlet_test", p, cli, sess, repo, leaseMgr)
    dev.poll(context.Background())

    if len(cli.readAddrs) != 1 {
        t.Fatalf("expected exactly 1 ReadRaw call (write-only point must be skipped), got %d: %v", len(cli.readAddrs), cli.readAddrs)
    }
    if cli.readAddrs[0] != 100 {
        t.Fatalf("expected the read to be for addr 100, got %d", cli.readAddrs[0])
    }
}

func intPtr(v int) *int { return &v }
