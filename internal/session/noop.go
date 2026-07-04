package session

import (
"context"
"sync"

"mbgw/internal/transport"
)

// NoopSession is used for devices that do not require authorization,
// preamble exchange, byte-order negotiation, or keepalive traffic.
type NoopSession struct {
state State
mu    sync.RWMutex
}

func NewNoopSession() *NoopSession {
return &NoopSession{state: StateClosed}
}

func (s *NoopSession) Open(ctx context.Context, tr transport.Transport) error {
s.mu.Lock()
defer s.mu.Unlock()
s.state = StateReady
return nil
}

func (s *NoopSession) KeepAlive(ctx context.Context) error {
return nil
}

func (s *NoopSession) Close() error {
s.mu.Lock()
defer s.mu.Unlock()
s.state = StateClosed
return nil
}

func (s *NoopSession) State() State {
s.mu.RLock()
defer s.mu.RUnlock()
return s.state
}
