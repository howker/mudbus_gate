package akron

import (
	"fmt"

	"mbgw/internal/protocol/modbus"
)

// Package akron implements the User-Defined Function Code protocol used
// by "Акрон-01" and "Акрон-02-1" ultrasonic flow meters (per
// "ВЗАИМОДЕЙСТВИЕ С КОНТРОЛЛЕРОМ СЕТИ MODBUS ПРИБОРОВ Акрон-01 и
// Акрон-02-1"): Modbus RTU framing, command codes 100-110 (decimal),
// multi-byte values little-endian on the wire.
//
// Request frame:  [addr 1B][code 1B][params 0-3B][CRC 2B]
// Response frame: [addr 1B][code 1B][byteCount 1B][data byteCount B][CRC 2B]
// (the response shape mirrors standard Modbus function 03/04 responses -
// same structure, just with a User-Defined function code instead of
// 03/04). Verified against the document's own worked example for command
// 102 (see akron_test.go).
//
// Since version 3.7, Akron devices also support standard Modbus function
// 03 for current values (see profiles/acron-01.yaml, not this package) -
// this package covers only the User-Defined commands (identification,
// max values, and critically the hourly/daily/on-off archives, which
// have no function-03 equivalent at all).

const (
	CmdIdentification = 101
	CmdCurrentValues  = 102
	CmdMaxValues      = 103
	CmdHourlyArchive  = 104
	CmdDailyArchive   = 105
	CmdOnLog          = 106
	CmdOffLog         = 107
)

// BuildRequest builds a User-Defined command request:
// [addr][code][params...][CRC]. params must be 0-3 bytes per the document.
func BuildRequest(addr byte, code byte, params []byte) []byte {
	body := make([]byte, 0, 2+len(params)+2)
	body = append(body, addr, code)
	body = append(body, params...)
	crc := modbus.CRC16(body)
	body = append(body, byte(crc&0xFF), byte(crc>>8))
	return body
}

// ParseResponse validates CRC and the byteCount-prefixed data field,
// returning addr, code, and data (without the byteCount byte itself).
func ParseResponse(frame []byte) (addr byte, code byte, data []byte, err error) {
	if len(frame) < 5 {
		return 0, 0, nil, fmt.Errorf("akron response too short")
	}
	body := frame[:len(frame)-2]
	calcCRC := modbus.CRC16(body)
	frameCRC := uint16(frame[len(frame)-2]) | (uint16(frame[len(frame)-1]) << 8)
	if calcCRC != frameCRC {
		return 0, 0, nil, fmt.Errorf("invalid CRC")
	}
	byteCount := int(frame[2])
	data = frame[3 : len(frame)-2]
	if len(data) != byteCount {
		return 0, 0, nil, fmt.Errorf("akron response byte count mismatch: declared %d, got %d", byteCount, len(data))
	}
	return frame[0], frame[1], data, nil
}

// BuildIdentification builds a command-101 request (device type, firmware
// version, serial number).
func BuildIdentification(addr byte) []byte {
	return BuildRequest(addr, CmdIdentification, nil)
}

// BuildCurrentValues builds a command-102 request (V, Q, volume+Pu,
// runtime, fault code).
func BuildCurrentValues(addr byte) []byte {
	return BuildRequest(addr, CmdCurrentValues, nil)
}

// BuildMaxValues builds a command-103 request (Hmax, Qmax).
func BuildMaxValues(addr byte) []byte {
	return BuildRequest(addr, CmdMaxValues, nil)
}

// buildArchiveParams encodes the 3-byte parameter block shared by both
// hourly (104) and daily (105) archive requests: big-endian 2-byte start
// index i, then 1-byte record count n. i=1 is the archive's "top" (most
// recent), i=M is its "base" (oldest) - per the document's own
// convention, passed through as-is (not reinterpreted here).
func buildArchiveParams(startIndex uint16, n byte) []byte {
	return []byte{byte(startIndex >> 8), byte(startIndex & 0xFF), n}
}

// BuildHourlyArchive builds a command-104 request for n rows of the
// hourly archive starting at index startIndex (1 ≤ n ≤ 31, 1 ≤ startIndex
// ≤ 1925-n+1, per the document).
func BuildHourlyArchive(addr byte, startIndex uint16, n byte) []byte {
	return BuildRequest(addr, CmdHourlyArchive, buildArchiveParams(startIndex, n))
}

// BuildDailyArchive builds a command-105 request for n rows of the daily
// archive starting at index startIndex (1 ≤ n ≤ 36, 1 ≤ startIndex ≤
// 2200-n+1, per the document).
func BuildDailyArchive(addr byte, startIndex uint16, n byte) []byte {
	return BuildRequest(addr, CmdDailyArchive, buildArchiveParams(startIndex, n))
}
