package session
import (
"context"
"testing"
"mbgw/internal/protocol/merkuriy"
)
type fakeMerkuriyTransport struct {
responses map[byte][]byte // keyed by request command code
closed    bool
}
func newFakeMerkuriyTransport() *fakeMerkuriyTransport {
return &fakeMerkuriyTransport{responses: map[byte][]byte{}}
}
func (f *fakeMerkuriyTransport) Read(ctx context.Context, req []byte) ([]byte, error) {
addr, code, _, err := merkuriy.ParseFrame(req)
if err != nil {
return nil, err
}
if resp, ok := f.responses[code]; ok {
return resp, nil
}
// Default: success response (status X0).
return merkuriy.BuildFrame(addr, 0x00, nil), nil
}
func (f *fakeMerkuriyTransport) Close() error {
f.closed = true
return nil
}
func TestMerkuriySessionOpenSuccess(t *testing.T) {
tr := newFakeMerkuriyTransport()
s := NewMerkuriySession(1, 1, [6]byte{1, 1, 1, 1, 1, 1})
if err := s.Open(context.Background(), tr); err != nil {
t.Fatalf("unexpected error: %v", err)
}
if s.State() != StateReady {
t.Fatalf("expected StateReady, got %v", s.State())
}
}
func TestMerkuriySessionOpenTestLinkFails(t *testing.T) {
tr := newFakeMerkuriyTransport()
tr.responses[0x00] = merkuriy.BuildFrame(1, 0x03, nil) // X3 channel busy
s := NewMerkuriySession(1, 1, [6]byte{1, 1, 1, 1, 1, 1})
if err := s.Open(context.Background(), tr); err == nil {
t.Fatal("expected error on test-link failure")
}
if s.State() != StateError {
t.Fatalf("expected StateError, got %v", s.State())
}
}
func TestMerkuriySessionOpenChannelFails(t *testing.T) {
tr := newFakeMerkuriyTransport()
tr.responses[0x01] = merkuriy.BuildFrame(1, 0x01, nil) // X1 no access
s := NewMerkuriySession(1, 1, [6]byte{1, 1, 1, 1, 1, 1})
if err := s.Open(context.Background(), tr); err == nil {
t.Fatal("expected error on open-channel failure")
}
if s.State() != StateError {
t.Fatalf("expected StateError, got %v", s.State())
}
}
func TestMerkuriySessionKeepAliveNoopWithinInterval(t *testing.T) {
tr := newFakeMerkuriyTransport()
s := NewMerkuriySession(1, 1, [6]byte{1, 1, 1, 1, 1, 1})
if err := s.Open(context.Background(), tr); err != nil {
t.Fatalf("unexpected error: %v", err)
}
if err := s.KeepAlive(context.Background()); err != nil {
t.Fatalf("unexpected keepalive error: %v", err)
}
if s.State() != StateReady {
t.Fatalf("expected StateReady after keepalive, got %v", s.State())
}
}
func TestMerkuriySessionKeepAliveNotReady(t *testing.T) {
s := NewMerkuriySession(1, 1, [6]byte{1, 1, 1, 1, 1, 1})
if err := s.KeepAlive(context.Background()); err == nil {
t.Fatal("expected error when session not ready")
}
}
func TestMerkuriySessionClose(t *testing.T) {
tr := newFakeMerkuriyTransport()
s := NewMerkuriySession(1, 1, [6]byte{1, 1, 1, 1, 1, 1})
if err := s.Open(context.Background(), tr); err != nil {
t.Fatalf("unexpected error: %v", err)
}
if err := s.Close(); err != nil {
t.Fatalf("unexpected close error: %v", err)
}
if s.State() != StateClosed {
t.Fatalf("expected StateClosed, got %v", s.State())
}
}
