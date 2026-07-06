package device

import (
    "context"
    "testing"

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
