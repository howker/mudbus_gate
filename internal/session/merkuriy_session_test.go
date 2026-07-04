package session
import (
    "context"
    "testing"
    "time"

    "mbgw/internal/protocol/merkuriy"
    "mbgw/internal/transport"
)
type fakeMerkuriyTransport struct {
    responses  map[byte][]byte // keyed by request command code
    closed     bool
    lastAddr   byte
    lastCode   byte
}

func newFakeMerkuriyTransport() *fakeMerkuriyTransport {
    return &fakeMerkuriyTransport{responses: map[byte][]byte{}}
}

func (f *fakeMerkuriyTransport) Open(ctx context.Context) error { return nil }

func (f *fakeMerkuriyTransport) Send(ctx context.Context, frame []byte) error {
    addr, code, _, err := merkuriy.ParseFrame(frame)
    if err != nil {
        return err
    }
    f.lastAddr = addr
    f.lastCode = code
    return nil
}

func (f *fakeMerkuriyTransport) Receive(ctx context.Context, timeout time.Duration) ([]byte, error) {
    if resp, ok := f.responses[f.lastCode]; ok {
        return resp, nil
    }
    // Default: success response (status X0).
    return merkuriy.BuildFrame(f.lastAddr, 0x00, nil), nil
}

func (f *fakeMerkuriyTransport) Close() error {
    f.closed = true
    return nil
}

func (f *fakeMerkuriyTransport) Info() transport.Params {
    return transport.Params{Retries: 1}
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
tr.responses[0x00] = merkuriy.BuildFrame(1, 0x03, nil) // X3 insufficient access level
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
tr.responses[0x01] = merkuriy.BuildFrame(1, 0x01, nil) // X1 invalid command/parameter
s := NewMerkuriySession(1, 1, [6]byte{1, 1, 1, 1, 1, 1})
if err := s.Open(context.Background(), tr); err == nil {
t.Fatal("expected error on open-channel failure")
}
if s.State() != StateError {
t.Fatalf("expected StateError, got %v", s.State())
}
}
func TestMerkuriySessionOpenChannelNotOpenStatus(t *testing.T) {
    tr := newFakeMerkuriyTransport()
    tr.responses[0x01] = merkuriy.BuildFrame(1, 0x05, nil) // X5 channel not open

    s := NewMerkuriySession(1, 1, [6]byte{1, 1, 1, 1, 1, 1})
    if err := s.Open(context.Background(), tr); err == nil {
        t.Fatal("expected error on X5 channel-not-open status")
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
