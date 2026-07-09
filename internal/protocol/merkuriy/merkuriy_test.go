package merkuriy
import (
    "bytes"
    "testing"
)
func TestBuildTestLinkGolden(t *testing.T) {
got := BuildTestLink(1)
want := []byte{0x01, 0x00, 0x00, 0x20}
if len(got) != len(want) {
t.Fatalf("length mismatch: got %d want %d (% X)", len(got), len(want), got)
}
for i := range want {
if got[i] != want[i] {
t.Fatalf("byte %d mismatch: got 0x%02X want 0x%02X (% X)", i, got[i], want[i], got)
}
}
}
func TestBuildOpenChannelGolden(t *testing.T) {
pwd := [6]byte{0x01, 0x01, 0x01, 0x01, 0x01, 0x01}
got := BuildOpenChannel(1, 1, pwd)
want := []byte{0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x7A, 0x11}
if len(got) != len(want) {
t.Fatalf("length mismatch: got %d want %d (% X)", len(got), len(want), got)
}
for i := range want {
if got[i] != want[i] {
t.Fatalf("byte %d mismatch: got 0x%02X want 0x%02X (% X)", i, got[i], want[i], got)
}
}
}
func TestParseFrameRoundTrip(t *testing.T) {
frame := BuildTestLink(1)
addr, code, data, err := ParseFrame(frame)
if err != nil {
t.Fatalf("unexpected error: %v", err)
}
if addr != 1 || code != 0x00 || len(data) != 0 {
t.Fatalf("unexpected parse result: addr=%d code=%d data=% X", addr, code, data)
}
}
func TestParseFrameBadCRC(t *testing.T) {
frame := BuildTestLink(1)
frame[len(frame)-1] ^= 0xFF
if _, _, _, err := ParseFrame(frame); err == nil {
t.Fatal("expected CRC error")
}
}

func TestBuildReadRelative_Golden(t *testing.T) {
    // Section 4.6 worked example: read from device addr 0x80, memory #3,
    // offset 1, 1 record.
    got := BuildReadRelative(0x80, 0x03, 0x0001, 0x01)
    want := []byte{0x80, 0x16, 0x03, 0x00, 0x01, 0x01, 0x96, 0x0C}
    if !bytes.Equal(got, want) {
        t.Fatalf("got % X, want % X", got, want)
    }
}

func TestParseResponse_Golden(t *testing.T) {
    // Same section 4.6 example's response: a single 15-byte power-profile
    // record (status byte + BCD time + integration period + A+ value).
    frame := []byte{0x80, 0x0A, 0x10, 0x00, 0x05, 0x03, 0x08, 0x1E, 0x04, 0x29, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00, 0xFE, 0xAF}
    addr, data, err := ParseResponse(frame)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if addr != 0x80 {
        t.Fatalf("want addr 0x80, got 0x%02X", addr)
    }
    wantData := []byte{0x0A, 0x10, 0x00, 0x05, 0x03, 0x08, 0x1E, 0x04, 0x29, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00}
    if !bytes.Equal(data, wantData) {
        t.Fatalf("got data % X, want % X", data, wantData)
    }
}

func TestParseResponse_BadCRC(t *testing.T) {
    frame := []byte{0x80, 0x0A, 0x00, 0xFE, 0xAF}
    if _, _, err := ParseResponse(frame); err == nil {
        t.Fatal("expected error for corrupted CRC")
    }
}

func TestBuildReadRelativePDU_Golden(t *testing.T) {
    // Bare PDU (no address, no CRC) for the same section 4.6 worked
    // example as TestBuildReadRelative_Golden: memory #3, offset 1, 1
    // record. PDU: 16 03 00 01 01
    got := BuildReadRelativePDU(0x03, 0x0001, 0x01)
    want := []byte{0x16, 0x03, 0x00, 0x01, 0x01}
    if !bytes.Equal(got, want) {
        t.Fatalf("got % X, want % X", got, want)
    }
}
