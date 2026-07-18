package akron

import "testing"

// buildIdentityResponse mirrors exactly how the simulator's
// akronIdentityPDU builds a command-101 reply, so this test pins the
// parser to the real wire layout:
//
//	[101][6][DeviceType][FirmwareBCD][Serial 4B little-endian]
func buildIdentityResponse(devType, fwBCD byte, serial uint32) []byte {
	return []byte{
		CmdIdentification, 6,
		devType, fwBCD,
		byte(serial), byte(serial >> 8), byte(serial >> 16), byte(serial >> 24),
	}
}

func TestParseIdentification_Simulator(t *testing.T) {
	// Simulator defaults: type 0x00, firmware 0x37 (v3.7), serial 12345.
	pdu := buildIdentityResponse(0x00, 0x37, 12345)

	id, err := ParseIdentification(pdu)
	if err != nil {
		t.Fatalf("ParseIdentification: %v", err)
	}
	if id.DeviceType != 0x00 {
		t.Errorf("DeviceType = 0x%02X, want 0x00", id.DeviceType)
	}
	if id.Firmware != "3.7" {
		t.Errorf("Firmware = %q, want \"3.7\"", id.Firmware)
	}
	if id.FirmwareBCD != 0x37 {
		t.Errorf("FirmwareBCD = 0x%02X, want 0x37", id.FirmwareBCD)
	}
	if id.Serial != 12345 {
		t.Errorf("Serial = %d, want 12345", id.Serial)
	}
}

func TestParseIdentification_SerialLittleEndian(t *testing.T) {
	// 0x01020304 on the wire little-endian is 04 03 02 01.
	pdu := buildIdentityResponse(0x00, 0x52, 0x01020304)
	id, err := ParseIdentification(pdu)
	if err != nil {
		t.Fatalf("ParseIdentification: %v", err)
	}
	if id.Serial != 0x01020304 {
		t.Errorf("Serial = 0x%08X, want 0x01020304", id.Serial)
	}
	if id.Firmware != "5.2" {
		t.Errorf("Firmware = %q, want \"5.2\"", id.Firmware)
	}
}

func TestParseIdentification_Errors(t *testing.T) {
	cases := map[string][]byte{
		"too short":                   {CmdIdentification},
		"wrong command":               {CmdCurrentValues, 6, 0, 0x37, 1, 2, 3, 4},
		"byte count 5":                {CmdIdentification, 5, 0, 0x37, 1, 2, 3},
		"byte count says 6 but short": {CmdIdentification, 6, 0, 0x37, 1, 2}, // declared 6, only 4 data bytes
	}
	for name, pdu := range cases {
		if _, err := ParseIdentification(pdu); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
