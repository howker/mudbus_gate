package errs

import (
    "errors"
    "fmt"
)

// Sentinel errors per CONTRACTS.md section 9. Library functions wrap these
// with fmt.Errorf("...: %w", ErrX); callers compare via errors.Is.
var (
    ErrTimeout   = errors.New("timeout")
    ErrClosed    = errors.New("transport closed")
    ErrTransport = errors.New("transport error")
    ErrCRC       = errors.New("crc mismatch")
    ErrFrame     = errors.New("malformed frame")
    ErrNoData    = errors.New("no data")
    ErrBusy      = errors.New("device busy") // from Modbus exception 06
    ErrAuth      = errors.New("auth failed")
    ErrLease     = errors.New("device lease held by another owner")
)

// ErrException represents a protocol-level exception response (e.g. Modbus
// exception codes 01-08). Code meanings are protocol-specific; see
// CONTRACTS.md section 2.2 for the Modbus exception code table.
type ErrException struct {
    Code uint8
}

func (e ErrException) Error() string {
    return fmt.Sprintf("modbus exception %d", e.Code)
}

// Is allows errors.Is(err, ErrBusy) to match an ErrException with the
// Modbus BUSY code (0x06), so callers can use the sentinel ErrBusy without
// needing to know about ErrException's internals.
func (e ErrException) Is(target error) bool {
    if target == ErrBusy {
        return e.Code == 0x06
    }
    var other ErrException
    if errors.As(target, &other) {
        return e.Code == other.Code
    }
    return false
}