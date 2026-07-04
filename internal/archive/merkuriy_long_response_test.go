package archive
import "testing"
func TestMerkuriyLongResponseRegistered(t *testing.T) {
r, ok := Get("merkuriy_long_response")
if !ok {
t.Fatal("expected merkuriy_long_response reader to be registered")
}
if r.Strategy() != "merkuriy_long_response" {
t.Fatalf("unexpected strategy: %s", r.Strategy())
}
}
