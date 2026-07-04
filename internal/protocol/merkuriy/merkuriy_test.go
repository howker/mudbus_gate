package merkuriy
import "testing"
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
