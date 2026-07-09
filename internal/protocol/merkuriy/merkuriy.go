package merkuriy
import (
"context"
"fmt"

"mbgw/internal/protocol/modbus"
"mbgw/internal/transport"
)

// Transact sends a Merkuriy frame through the given (already-open)
// transport and returns the raw response frame bytes. Merkuriy's frame
// boundary is determined by the interframe silence gap (CONTRACTS.md
// section 3), which is Transport.Receive's responsibility for serial
// transports; for TCP/optical transports the same Send/Receive contract
// applies uniformly.
func Transact(ctx context.Context, tr transport.Transport, frame []byte) ([]byte, error) {
    if tr == nil {
        return nil, fmt.Errorf("nil transport")
    }
    if err := tr.Send(ctx, frame); err != nil {
        return nil, err
    }
    timeout := tr.Info().ResponseTimeout
    return tr.Receive(ctx, timeout)
}
// BuildFrame builds a Merkuriy channel frame:
// [addr 1B][code 1B][param 0..1B][ext 0..1B][data N][CRC16-modbus 2B]
func BuildFrame(addr byte, code byte, payload []byte) []byte {
body := make([]byte, 0, 2+len(payload)+2)
body = append(body, addr, code)
body = append(body, payload...)
crc := modbus.CRC16(body)
body = append(body, byte(crc&0xFF), byte(crc>>8))
return body
}
// ParseFrame validates CRC and returns addr, code, data (without CRC).
func ParseFrame(frame []byte) (addr byte, code byte, data []byte, err error) {
if len(frame) < 4 {
return 0, 0, nil, fmt.Errorf("merkuriy frame too short")
}
body := frame[:len(frame)-2]
calcCRC := modbus.CRC16(body)
frameCRC := uint16(frame[len(frame)-2]) | (uint16(frame[len(frame)-1]) << 8)
if calcCRC != frameCRC {
return 0, 0, nil, fmt.Errorf("invalid CRC")
}
return frame[0], frame[1], frame[2 : len(frame)-2], nil
}
// BuildTestLink builds cmd 0x00 test-link frame (addr).
func BuildTestLink(addr byte) []byte {
return BuildFrame(addr, 0x00, nil)
}
// BuildOpenChannel builds cmd 0x01 open-channel frame: addr, level, 6-byte password.
func BuildOpenChannel(addr byte, level byte, password [6]byte) []byte {
payload := make([]byte, 0, 7)
payload = append(payload, level)
payload = append(payload, password[:]...)
return BuildFrame(addr, 0x01, payload)
}
// BuildCloseChannel builds cmd 0x02 close-channel frame.
func BuildCloseChannel(addr byte) []byte {
return BuildFrame(addr, 0x02, nil)
}

// BuildReadRelative builds a command 0x16 "relative addressing mode" read
// request: reads a ring-buffer array (power profile, event log, etc.) by
// memory number, starting OFFSET records back from the most recently
// formed record (0 = the last record), for up to `count` records
// (documented as "писание системы команд приборов учета еркурий",
// section 4.6, covering еркурий 230 among other models - distinct from
// the unrelated еркурий-200/206 CAN protocol, which uses a completely
// different 4-byte-address frame and is NOT what this package implements).
//
// Wire format: [addr 1B][code=0x16][memNumber 1B][offset 2B big-endian]
// [count 1B][CRC16-modbus 2B]. Golden vector (section 4.6 worked example,
// device addr 0x80, memory #3, offset 1, 1 record):
//   80 16 03 00 01 01 96 0C
//
// NOTE: this builds a COMPLETE frame (address+CRC included) - correct
// only for direct raw-transport calls like MerkuriySession's own
// testLink/openChannel (which use Transact(ctx, tr, frame) directly on
// the raw transport, bypassing any Modbus-level framing entirely). For
// archive strategies going through pollcore.Reader/modbus.Transact
// (which adds its own address+CRC via BuildRTUFrame), use
// BuildReadRelativePDU instead - passing this function's output there
// double-frames the request and silently breaks on the wire (this bit
// the archive strategy in exactly this way; see BuildReadRelativePDU).
func BuildReadRelative(addr byte, memNumber byte, offset uint16, count byte) []byte {
    payload := make([]byte, 0, 4)
    payload = append(payload, memNumber, byte(offset>>8), byte(offset&0xFF), count)
    return BuildFrame(addr, 0x16, payload)
}

// BuildReadRelativePDU builds the bare PDU (no address, no CRC) for a
// command 0x16 request - for use via pollcore.Reader/modbus.Transact,
// which adds address+CRC itself (RTU framing) exactly once. See
// BuildReadRelative's doc comment for why this distinction matters.
func BuildReadRelativePDU(memNumber byte, offset uint16, count byte) []byte {
    return []byte{0x16, memNumber, byte(offset >> 8), byte(offset & 0xFF), count}
}

// ParseResponse validates CRC and returns addr and the full data field of
// a standard Merkuriy response frame: [addr 1B][data 1..255B][CRC 2B]
// (section 1.5.4.1's figure 1.2, and figure 1.3 for the long-response
// variant used by relative-addressing/profile/log reads - both share the
// same three-field shape, only the data field's maximum length differs).
//
// Unlike ParseFrame, this does NOT assume the first data byte is a
// separate "response code" - that assumption only happens to hold for
// test-link/open-channel/close-channel, whose 1-byte status response
// makes data[0] look like a code. For any response carrying real
// multi-byte data (profile records, logs, energy registers), treating
// data[0] as a throwaway "code" would silently misalign every decoded
// field by one byte. Use ParseResponse for those; ParseFrame remains as-is
// for the three channel-control commands it already correctly serves.
func ParseResponse(frame []byte) (addr byte, data []byte, err error) {
    if len(frame) < 3 {
        return 0, nil, fmt.Errorf("merkuriy response too short")
    }
    body := frame[:len(frame)-2]
    calcCRC := modbus.CRC16(body)
    frameCRC := uint16(frame[len(frame)-2]) | (uint16(frame[len(frame)-1]) << 8)
    if calcCRC != frameCRC {
        return 0, nil, fmt.Errorf("invalid CRC")
    }
    return frame[0], frame[1 : len(frame)-2], nil
}
