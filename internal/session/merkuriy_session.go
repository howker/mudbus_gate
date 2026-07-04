package session
import (
"context"
"fmt"
"sync"
"time"
"mbgw/internal/protocol/merkuriy"
"mbgw/internal/transport"
)
const (
merkuriyKeepAliveTimeout  = 240 * time.Second
merkuriyKeepAliveInterval = 200 * time.Second
)
// MerkuriySession implements the merkuriy_channel session profile:
// Open = test-link (cmd 0x00) then open-channel (cmd 0x01, level+password).
// KeepAlive must be repeated before the 240s channel timeout expires.
// Close = close-channel (cmd 0x02).
type MerkuriySession struct {
state    State
mu       sync.RWMutex
tr       transport.Transport
addr     byte
level    byte
password [6]byte
lastKA   time.Time
}
// NewMerkuriySession creates a session for the given device address, access
// level and 6-byte password (per profile configuration).
func NewMerkuriySession(addr byte, level byte, password [6]byte) *MerkuriySession {
return &MerkuriySession{
state:    StateClosed,
addr:     addr,
level:    level,
password: password,
}
}
func (s *MerkuriySession) Open(ctx context.Context, tr transport.Transport) error {
s.mu.Lock()
defer s.mu.Unlock()
if tr == nil {
s.state = StateError
return fmt.Errorf("transport is not initialized")
}
s.state = StateInitializing
s.tr = tr
if err := s.testLink(ctx); err != nil {
s.state = StateError
return fmt.Errorf("merkuriy test-link failed: %w", err)
}
if err := s.openChannel(ctx); err != nil {
s.state = StateError
return fmt.Errorf("merkuriy open-channel failed: %w", err)
}
s.state = StateReady
s.lastKA = time.Now()
return nil
}
func (s *MerkuriySession) testLink(ctx context.Context) error {
frame := merkuriy.BuildTestLink(s.addr)
resp, err := s.tr.Read(ctx, frame)
if err != nil {
return err
}
_, code, _, err := merkuriy.ParseFrame(resp)
if err != nil {
return err
}
if !merkuriy.IsOK(code) {
return fmt.Errorf("test-link status: %s", merkuriy.ParseStatus(code))
}
return nil
}
func (s *MerkuriySession) openChannel(ctx context.Context) error {
frame := merkuriy.BuildOpenChannel(s.addr, s.level, s.password)
resp, err := s.tr.Read(ctx, frame)
if err != nil {
return err
}
_, code, _, err := merkuriy.ParseFrame(resp)
if err != nil {
return err
}
if !merkuriy.IsOK(code) {
return fmt.Errorf("open-channel status: %s", merkuriy.ParseStatus(code))
}
return nil
}
// KeepAlive re-opens the channel if the 240s timeout is close to expiring.
// Callers are expected to invoke this periodically (default interval 200s).
func (s *MerkuriySession) KeepAlive(ctx context.Context) error {
s.mu.Lock()
defer s.mu.Unlock()
if s.state != StateReady {
return fmt.Errorf("merkuriy session not ready")
}
if time.Since(s.lastKA) < merkuriyKeepAliveInterval {
return nil
}
if err := s.testLink(ctx); err != nil {
s.state = StateError
return fmt.Errorf("merkuriy keepalive test-link failed: %w", err)
}
if err := s.openChannel(ctx); err != nil {
s.state = StateError
return fmt.Errorf("merkuriy keepalive open-channel failed: %w", err)
}
s.lastKA = time.Now()
return nil
}
func (s *MerkuriySession) Close() error {
s.mu.Lock()
defer s.mu.Unlock()
if s.tr != nil && s.state == StateReady {
frame := merkuriy.BuildCloseChannel(s.addr)
_, _ = s.tr.Read(context.Background(), frame)
}
s.state = StateClosed
return nil
}
func (s *MerkuriySession) State() State {
s.mu.RLock()
defer s.mu.RUnlock()
return s.state
}
