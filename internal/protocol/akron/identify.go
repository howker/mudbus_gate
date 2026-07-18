package akron

import (
	"encoding/binary"
	"fmt"
)

// Identification is the decoded payload of a command-101 response — the
// device's static "passport": what kind of meter it is, its firmware
// version, and its factory serial number. mbgw reads this once at startup
// (it never changes) and stores it, so the upstream Akron carrier can
// answer command 101 with the real device's identity instead of a stub.
type Identification struct {
	DeviceType  byte   // 0x00 for Akron-01, per the protocol document
	Firmware    string // human form of the version byte, e.g. "3.7"
	FirmwareBCD byte   // raw version byte as received (0x37 == v3.7)
	Serial      uint32 // factory serial, little-endian on the wire
}

// ParseIdentification decodes a command-101 response PDU.
//
// Wire shape (mirrors the device and simulator, per "ВЗАИМОДЕЙСТВИЕ С
// КОНТРОЛЛЕРОМ СЕТИ MODBUS ПРИБОРОВ Акрон-01 и Акрон-02-1", table 1):
//
//	[101][byteCount=6][DeviceType 1B][Firmware 1B][Serial 4B, little-endian]
//
// The RTU address and CRC are already stripped/validated by the transport
// layer before this sees the PDU (same layering as the rest of this
// package). The firmware byte is packed as two nibbles — high = major,
// low = minor — so 0x37 renders as "3.7" (this is NOT the decimal 37;
// it's a version, not a BCD count).
//
// Note: for firmware ≥ 5.2 the document says the 4 serial bytes are ASCII
// rather than a little-endian word. This parser targets the ≥3.7 word
// form (the profile's firmware_compat is ">=3.7"); an ASCII-serial variant
// would branch on FirmwareBCD here if such a device ever appears.
func ParseIdentification(pdu []byte) (Identification, error) {
	code, data, err := ParseResponsePDU(pdu)
	if err != nil {
		return Identification{}, err
	}
	if code != CmdIdentification {
		return Identification{}, fmt.Errorf("akron: expected command %d, got %d", CmdIdentification, code)
	}
	if len(data) != 6 {
		return Identification{}, fmt.Errorf("akron: identification data must be 6 bytes, got %d", len(data))
	}

	fwByte := data[1]
	return Identification{
		DeviceType:  data[0],
		Firmware:    fmt.Sprintf("%d.%d", fwByte>>4, fwByte&0x0F),
		FirmwareBCD: fwByte,
		Serial:      binary.LittleEndian.Uint32(data[2:6]),
	}, nil
}
