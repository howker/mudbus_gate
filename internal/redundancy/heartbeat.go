package redundancy

import (
    "context"
    "time"
)

// Heartbeat is the interface a redundancy node uses to detect whether its
// peer (active watches standby, standby watches active) is alive. The
// real, networked implementation (post-MVP) will send/receive periodic
// signals over a dedicated channel between the two nodes; this MVP stub
// has no peer to talk to, so NoopHeartbeat below never reports loss.
type Heartbeat interface {
    // Send emits one heartbeat signal to the peer.
    Send(ctx context.Context) error
    // Lost reports whether the peer's heartbeat has been missing for
    // longer than grace.
    Lost(grace time.Duration) bool
}

// NoopHeartbeat is a placeholder Heartbeat that never reports the peer as
// lost, since there is no second node in this MVP deployment to lose
// contact with. It exists so callers can be written against the
// Heartbeat interface now, without a real peer to test against.
type NoopHeartbeat struct{}

func (NoopHeartbeat) Send(ctx context.Context) error { return nil }
func (NoopHeartbeat) Lost(grace time.Duration) bool  { return false }