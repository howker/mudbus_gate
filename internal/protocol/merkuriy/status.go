package merkuriy

import "fmt"

// Status represents the Merkuriy channel response status nibble (X0..X5),
// carried in the low nibble of the response code byte, per CONTRACTS.md
// section 3:
//   X0 - normal (ok)
//   X1 - invalid command/parameter
//   X2 - internal meter error
//   X3 - insufficient access level
//   X4 - internal clock not corrected
//   X5 - communication channel not open
type Status byte

const (
    StatusOK                Status = 0x0 // X0 - normal
    StatusInvalidCommand    Status = 0x1 // X1 - invalid command/parameter
    StatusInternalError     Status = 0x2 // X2 - internal meter error
    StatusInsufficientLevel Status = 0x3 // X3 - insufficient access level
    StatusClockNotCorrected Status = 0x4 // X4 - internal clock not corrected
    StatusChannelNotOpen    Status = 0x5 // X5 - communication channel not open
)

func (s Status) String() string {
    switch s {
    case StatusOK:
        return "ok"
    case StatusInvalidCommand:
        return "invalid_command"
    case StatusInternalError:
        return "internal_error"
    case StatusInsufficientLevel:
        return "insufficient_access_level"
    case StatusClockNotCorrected:
        return "clock_not_corrected"
    case StatusChannelNotOpen:
        return "channel_not_open"
    default:
        return fmt.Sprintf("unknown_status(0x%X)", byte(s))
    }
}

// ParseStatus extracts the status nibble (X0..X5) from a response code byte.
func ParseStatus(code byte) Status {
    return Status(code & 0x0F)
}

// IsOK reports whether the response code indicates success (X0).
func IsOK(code byte) bool {
    return ParseStatus(code) == StatusOK
}