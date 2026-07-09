package akron

import (
"fmt"
)

// Package akron implements the User-Defined Function Code protocol used
// by "Акрон-01" and "Акрон-02-1" ultrasonic flow meters (per
// "ВЗАИМОДЕЙСТВИЕ С КОНТРОЛЛЕРОМ СЕТИ MODBUS ПРИБОРОВ Акрон-01 и
// Акрон-02-1"): command codes 100-110 (decimal), multi-byte values
// little-endian on the wire.
//
// The document's own worked examples show complete Modbus RTU frames
// (address + code + params + CRC16) - RTU's address+CRC are transport
// (RTU framing) concerns, not protocol-level ones. This package builds
// and parses only the PDU (code + params / code + byteCount + data) -
// address and CRC are added/verified by the RTU transport layer
// (protocol/modbus.Transact -> BuildRTUFrame/ParseRTUFrame), exactly the
// same layering already used for VZLET's function 65 (BuildArchive65...
// PDU). This package deliberately does not build complete frames itself,
// unlike protocol/merkuriy (which has its own reasons, tracked
// separately) - conflating the two layers here previously caused every
// Akron request to be double-framed (address+CRC baked in twice) and
// silently fail on the wire.
const (
CmdIdentification = 101
CmdCurrentValues  = 102
CmdMaxValues      = 103
CmdHourlyArchive  = 104
CmdDailyArchive   = 105
CmdOnLog          = 106
CmdOffLog         = 107
)

// BuildRequestPDU builds a User-Defined command PDU: [code][params...].
// params must be 0-3 bytes per the document. No address, no CRC - those
// are the RTU transport's job (see package doc comment).
func BuildRequestPDU(code byte, params []byte) []byte {
pdu := make([]byte, 0, 1+len(params))
pdu = append(pdu, code)
pdu = append(pdu, params...)
return pdu
}

// ParseResponsePDU validates the byteCount-prefixed data field of a
// response PDU ([code][byteCount][data]) and returns code and data
// (without the byteCount byte). No address, no CRC to check here - the
// RTU transport already validated and stripped those.
func ParseResponsePDU(pdu []byte) (code byte, data []byte, err error) {
if len(pdu) < 2 {
return 0, nil, fmt.Errorf("akron response PDU too short")
}
byteCount := int(pdu[1])
data = pdu[2:]
if len(data) != byteCount {
return 0, nil, fmt.Errorf("akron response byte count mismatch: declared %d, got %d", byteCount, len(data))
}
return pdu[0], data, nil
}

// BuildIdentificationPDU builds a command-101 request PDU (device type,
// firmware version, serial number).
func BuildIdentificationPDU() []byte {
return BuildRequestPDU(CmdIdentification, nil)
}

// BuildCurrentValuesPDU builds a command-102 request PDU (V, Q,
// volume+Pu, runtime, fault code).
func BuildCurrentValuesPDU() []byte {
return BuildRequestPDU(CmdCurrentValues, nil)
}

// BuildMaxValuesPDU builds a command-103 request PDU (Hmax, Qmax).
func BuildMaxValuesPDU() []byte {
return BuildRequestPDU(CmdMaxValues, nil)
}

// buildArchiveParams encodes the 3-byte parameter block shared by both
// hourly (104) and daily (105) archive requests: big-endian 2-byte start
// index i, then 1-byte record count n. i=1 is the archive's "top" (most
// recent), i=M is its "base" (oldest) - per the document's own
// convention, passed through as-is (not reinterpreted here).
func buildArchiveParams(startIndex uint16, n byte) []byte {
return []byte{byte(startIndex >> 8), byte(startIndex & 0xFF), n}
}

// BuildHourlyArchivePDU builds a command-104 request PDU for n rows of
// the hourly archive starting at index startIndex (1 <= n <= 31,
// 1 <= startIndex <= 1925-n+1, per the document).
func BuildHourlyArchivePDU(startIndex uint16, n byte) []byte {
return BuildRequestPDU(CmdHourlyArchive, buildArchiveParams(startIndex, n))
}

// BuildDailyArchivePDU builds a command-105 request PDU for n rows of
// the daily archive starting at index startIndex (1 <= n <= 36,
// 1 <= startIndex <= 2200-n+1, per the document).
func BuildDailyArchivePDU(startIndex uint16, n byte) []byte {
return BuildRequestPDU(CmdDailyArchive, buildArchiveParams(startIndex, n))
}