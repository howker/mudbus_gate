package merkuriy
import "testing"
func TestParseStatus(t *testing.T) {
cases := []struct {
code byte
want Status
}{
{0x00, StatusOK},
{0x01, StatusNoAccess},
{0x02, StatusInternalError},
{0x03, StatusChannelBusy},
{0x04, StatusUnknownCommand},
{0x05, StatusFrameError},
}
for _, c := range cases {
if got := ParseStatus(c.code); got != c.want {
t.Fatalf("ParseStatus(0x%02X) = %v, want %v", c.code, got, c.want)
}
}
}
func TestIsOK(t *testing.T) {
if !IsOK(0x00) {
t.Fatal("expected 0x00 to be ok")
}
if IsOK(0x03) {
t.Fatal("expected 0x03 to not be ok")
}
}
