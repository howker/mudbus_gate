package modbus

import (
"context"
"encoding/binary"
"fmt"

"mbgw/internal/codec"
"mbgw/internal/transport"
)

// CRC16 computes Modbus RTU CRC16.
func CRC16(data []byte) uint16 {
crc := uint16(0xFFFF)
for _, b := range data {
crc ^= uint16(b)
for i := 0; i < 8; i++ {
if crc&1 != 0 {
crc = (crc >> 1) ^ 0xA001
} else {
crc >>= 1
}
}
}
return crc
}

// BuildReadPDU builds a read request PDU for function 03 or 04.
func BuildReadPDU(space string, addr int, dataType string) ([]byte, error) {
var funcCode byte
switch space {
case "HR":
funcCode = 0x03
case "IR":
funcCode = 0x04
default:
return nil, fmt.Errorf("unknown space: %s", space)
}

qty := uint16(codec.RegisterCount(dataType))

pdu := make([]byte, 5)
pdu[0] = funcCode
binary.BigEndian.PutUint16(pdu[1:3], uint16(addr))
binary.BigEndian.PutUint16(pdu[3:5], qty)

return pdu, nil
}

// BuildReadPDUWithQty builds a read request PDU for function 03/04 with an
// explicit register count, bypassing dataType-based sizing. Required for
// variable-length reads (e.g. archive strings) where the register count is
// only known at runtime (see archive.mb_request_poll_string).
func BuildReadPDUWithQty(space string, addr int, qty uint16) ([]byte, error) {
var funcCode byte
switch space {
case "HR":
funcCode = 0x03
case "IR":
funcCode = 0x04
default:
return nil, fmt.Errorf("unknown space: %s", space)
}

pdu := make([]byte, 5)
pdu[0] = funcCode
binary.BigEndian.PutUint16(pdu[1:3], uint16(addr))
binary.BigEndian.PutUint16(pdu[3:5], qty)

return pdu, nil
}

// BuildWriteSingleRegisterPDU builds function 06 request PDU.
func BuildWriteSingleRegisterPDU(addr int, value uint16) []byte {
pdu := make([]byte, 5)
pdu[0] = 0x06
binary.BigEndian.PutUint16(pdu[1:3], uint16(addr))
binary.BigEndian.PutUint16(pdu[3:5], value)
return pdu
}

// BuildWriteMultipleRegistersPDU builds function 16 (0x10) request PDU.
//
// This is required for future session/auth/archive flows where several
// contiguous holding registers must be written in one transaction.
func BuildWriteMultipleRegistersPDU(addr int, values []uint16) ([]byte, error) {
if len(values) == 0 {
return nil, fmt.Errorf("values must not be empty")
}
if len(values) > 123 {
return nil, fmt.Errorf("too many registers for function 16: %d", len(values))
}

qty := len(values)
byteCount := qty * 2

pdu := make([]byte, 6+byteCount)
pdu[0] = 0x10
binary.BigEndian.PutUint16(pdu[1:3], uint16(addr))
binary.BigEndian.PutUint16(pdu[3:5], uint16(qty))
pdu[5] = byte(byteCount)

offset := 6
for _, v := range values {
binary.BigEndian.PutUint16(pdu[offset:offset+2], v)
offset += 2
}

return pdu, nil
}

// BuildTCPFrame wraps a PDU into MBAP header.
func BuildTCPFrame(txID uint16, unitID uint8, pdu []byte) []byte {
length := uint16(len(pdu) + 1)
frame := make([]byte, 7+len(pdu))

binary.BigEndian.PutUint16(frame[0:2], txID)
binary.BigEndian.PutUint16(frame[2:4], 0x0000)
binary.BigEndian.PutUint16(frame[4:6], length)
frame[6] = unitID

copy(frame[7:], pdu)
return frame
}

// ParseTCPFrame validates MBAP header and returns transaction id, unit id and PDU.
func ParseTCPFrame(frame []byte) (uint16, uint8, []byte, error) {
if len(frame) < 8 {
return 0, 0, nil, fmt.Errorf("tcp frame too short")
}

txID := binary.BigEndian.Uint16(frame[0:2])
protocolID := binary.BigEndian.Uint16(frame[2:4])
if protocolID != 0 {
return 0, 0, nil, fmt.Errorf("invalid protocol id: %d", protocolID)
}

length := int(binary.BigEndian.Uint16(frame[4:6]))
if length <= 1 {
return 0, 0, nil, fmt.Errorf("invalid tcp payload length: %d", length)
}

if len(frame) != 6+length {
return 0, 0, nil, fmt.Errorf("tcp frame length mismatch: mbap=%d actual=%d", length, len(frame)-6)
}

unitID := frame[6]
pdu := make([]byte, len(frame[7:]))
copy(pdu, frame[7:])

return txID, unitID, pdu, nil
}

// TransactTCP sends one Modbus TCP request through transport and returns response PDU.
//
// This helper centralizes the protocol-layer contract between session logic
// and raw transport bytes:
// - build MBAP-wrapped request
// - parse MBAP response
// - verify tx id and unit id consistency
// - surface Modbus exception responses as errors
func TransactTCP(ctx context.Context, tr transport.Transport, txID uint16, unitID uint8, reqPDU []byte) ([]byte, error) {
if tr == nil {
return nil, fmt.Errorf("nil transport")
}
if len(reqPDU) == 0 {
return nil, fmt.Errorf("empty request PDU")
}

reqFrame := BuildTCPFrame(txID, unitID, reqPDU)

respFrame, err := tr.Read(ctx, reqFrame)
if err != nil {
return nil, err
}

respTxID, respUnitID, respPDU, err := ParseTCPFrame(respFrame)
if err != nil {
return nil, err
}

if respTxID != txID {
return nil, fmt.Errorf("transaction id mismatch: want 0x%04X, got 0x%04X", txID, respTxID)
}
if respUnitID != unitID {
return nil, fmt.Errorf("unit id mismatch: want %d, got %d", unitID, respUnitID)
}

if isExc, code := IsException(respPDU); isExc {
return nil, &ExceptionError{Code: code}
}

return respPDU, nil
}

// BuildRTUFrame builds a serial RTU frame.
func BuildRTUFrame(unitID uint8, pdu []byte) []byte {
frame := make([]byte, 1+len(pdu)+2)
frame[0] = unitID
copy(frame[1:], pdu)
crc := CRC16(frame[:len(frame)-2])
frame[len(frame)-2] = byte(crc & 0xFF)
frame[len(frame)-1] = byte(crc >> 8)
return frame
}

// ParseRTUFrame validates CRC and returns unit id + PDU.
func ParseRTUFrame(frame []byte) (uint8, []byte, error) {
if len(frame) < 4 {
return 0, nil, fmt.Errorf("rtu frame too short")
}
calcCRC := CRC16(frame[:len(frame)-2])
frameCRC := uint16(frame[len(frame)-2]) | (uint16(frame[len(frame)-1]) << 8)
if calcCRC != frameCRC {
return 0, nil, fmt.Errorf("invalid CRC")
}
return frame[0], frame[1 : len(frame)-2], nil
}

// IsException checks whether a PDU is a Modbus exception response.
func IsException(pdu []byte) (bool, byte) {
if len(pdu) < 2 {
return false, 0
}
if pdu[0]&0x80 != 0 {
return true, pdu[1]
}
return false, 0
}

// FuncCodeReportSlaveID is the standard Modbus function 17 (0x11), used by
// VZLET devices (IVK-TER and others) to report device identification info
// including firmware version, needed to select the correct
// firmware_variant for archive record decoding (CONTRACTS.md section 2.3).
const FuncCodeReportSlaveID = 0x11

// BuildReportSlaveIDPDU builds a function 17 request PDU. This function
// takes no parameters beyond the function code itself.
func BuildReportSlaveIDPDU() []byte {
    return []byte{FuncCodeReportSlaveID}
}

// ReportSlaveIDResponse holds the raw device identification payload
// returned by function 17. The exact byte layout of RawData is vendor and
// model specific; VZLET's own firmware-version encoding within RawData is
// not fully documented here, so callers must interpret RawData themselves
// (e.g. against a known offset for their specific device family) - see
// backlog. RunStatus follows the standard Modbus convention: 0xFF = ON,
// 0x00 = OFF.
type ReportSlaveIDResponse struct {
    ByteCount int
    RunStatus byte
    RawData   []byte
}

// ParseReportSlaveIDResponse parses a function 17 response PDU:
// [funcCode][byteCount][slaveID...][runStatus][additional data...].
func ParseReportSlaveIDResponse(pdu []byte) (ReportSlaveIDResponse, error) {
    if len(pdu) < 2 {
        return ReportSlaveIDResponse{}, fmt.Errorf("report slave id response too short")
    }
    if isExc, code := IsException(pdu); isExc {
        return ReportSlaveIDResponse{}, &ExceptionError{Code: code}
    }
    if pdu[0] != FuncCodeReportSlaveID {
        return ReportSlaveIDResponse{}, fmt.Errorf("unexpected function code in report slave id response: 0x%02X", pdu[0])
    }

    byteCount := int(pdu[1])
    if len(pdu) < 2+byteCount {
        return ReportSlaveIDResponse{}, fmt.Errorf("report slave id response truncated: want %d bytes, got %d", byteCount, len(pdu)-2)
    }

    data := pdu[2 : 2+byteCount]
    if len(data) == 0 {
        return ReportSlaveIDResponse{}, fmt.Errorf("report slave id response has empty payload")
    }

    runStatus := data[0]
    rawData := data
    if len(data) > 1 {
        rawData = data[1:]
    } else {
        rawData = nil
    }

    return ReportSlaveIDResponse{
        ByteCount: byteCount,
        RunStatus: runStatus,
        RawData:   rawData,
    }, nil
}
// FuncCodeArchive65 is the VZLET-specific function code (0x41) used by
// TSRV-024 to read archive records by index or by time (CONTRACTS.md §2.3).
const FuncCodeArchive65 = 0x41

const (
ArchiveModeByIndex = 0
ArchiveModeByTime  = 1
)

// BuildArchive65PDU builds a request for the VZLET function 65 (0x41) archive
// read: archive type index, access mode (by index or by time), and a 4-byte
// value (either a record index or a Unix timestamp, per mode).
func BuildArchive65PDU(archiveType byte, mode byte, value uint32) []byte {
pdu := make([]byte, 7)
pdu[0] = FuncCodeArchive65
pdu[1] = archiveType
pdu[2] = mode
binary.BigEndian.PutUint32(pdu[3:7], value)
return pdu
}

// ParseArchive65Response extracts the raw archive record bytes from a
// function 65 response PDU: [funcCode][recordLen][record bytes...].
func ParseArchive65Response(pdu []byte) ([]byte, error) {
if len(pdu) < 2 {
return nil, fmt.Errorf("archive65 response too short")
}
if isExc, code := IsException(pdu); isExc {
return nil, &ExceptionError{Code: code}
}
if pdu[0] != FuncCodeArchive65 {
return nil, fmt.Errorf("unexpected function code in archive65 response: 0x%02X", pdu[0])
}
recordLen := int(pdu[1])
if len(pdu) < 2+recordLen {
return nil, fmt.Errorf("archive65 response record truncated: want %d bytes, got %d", recordLen, len(pdu)-2)
}
return pdu[2 : 2+recordLen], nil
}