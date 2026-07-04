package simulator
import "testing"
func TestRunMerkuriyUnsupported(t *testing.T) {
if err := Run("unknown", "127.0.0.1:0"); err == nil {
t.Fatal("expected error")
}
}
