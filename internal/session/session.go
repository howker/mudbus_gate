package session
import (
"context"
"fmt"
"mbgw/internal/profile"
"mbgw/internal/transport"
)
// State describes the lifecycle state of a device session.
type State int
const (
StateClosed State = iota
StateInitializing
StateReady
StateError
)
// Session defines the lifecycle contract required by the architecture.
// This replaces the old single Init() call and allows stateful protocols
// to implement open / keepalive / close logic explicitly.
type Session interface {
Open(ctx context.Context, tr transport.Transport) error
KeepAlive(ctx context.Context) error
Close() error
State() State
}
func asByte(v interface{}, def byte) byte {
switch x := v.(type) {
case int:
return byte(x)
case int64:
return byte(x)
case float64:
return byte(x)
default:
return def
}
}
func asPassword6(v interface{}) [6]byte {
var out [6]byte
switch x := v.(type) {
case string:
for i := 0; i < len(out) && i < len(x); i++ {
out[i] = x[i]
}
case []interface{}:
for i := 0; i < len(out) && i < len(x); i++ {
out[i] = asByte(x[i], 0)
}
}
return out
}
// New preserves backward compatibility for existing code/tests that only know
// the session type string.
func New(sessionType string) (Session, error) {
switch sessionType {
case "none", "":
return NewNoopSession(), nil
case "modbus_byteorder_auth":
return NewModbusByteOrderAuth(), nil
case "merkuriy_channel":
return NewMerkuriySession(1, 1, [6]byte{}), nil
default:
return nil, fmt.Errorf("unknown session type: %s", sessionType)
}
}
// NewFromProfile constructs a session using full profile session config.
// This is required for merkuriy_channel, where addr/level/password come
// from profile params/auth.
func NewFromProfile(cfg profile.Session) (Session, error) {
switch cfg.Type {
case "merkuriy_channel":
addr := asByte(cfg.Params["addr"], 1)
level := asByte(cfg.Auth["level"], 1)
password := asPassword6(cfg.Auth["password"])
return NewMerkuriySession(addr, level, password), nil
default:
return New(cfg.Type)
}
}
