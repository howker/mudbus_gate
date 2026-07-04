package merkuriy
import "fmt"
// Status represents Merkuriy channel response status nibble (X0..X5),
// carried in the low nibble of the response code byte.
type Status byte
const (
StatusOK             Status = 0x0 // X0 - ok
StatusNoAccess       Status = 0x1 // X1 - access denied / wrong level
StatusInternalError  Status = 0x2 // X2 - internal error
StatusChannelBusy    Status = 0x3 // X3 - channel busy
StatusUnknownCommand Status = 0x4 // X4 - unknown command
StatusFrameError     Status = 0x5 // X5 - frame/params error
)
func (s Status) String() string {
switch s {
case StatusOK:
return "ok"
case StatusNoAccess:
return "no_access"
case StatusInternalError:
return "internal_error"
case StatusChannelBusy:
return "channel_busy"
case StatusUnknownCommand:
return "unknown_command"
case StatusFrameError:
return "frame_error"
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
