package akron

import (
	"bytes"
	"testing"
)

func TestBuildCurrentValues_Golden(t *testing.T) {
	// Document's own worked example: "Запрос текущих значений у прибора
	// с адресом 01: 01 66 80 0А" (0x66 = 102 decimal).
	got := BuildCurrentValues(0x01)
	want := []byte{0x01, 0x66, 0x80, 0x0A}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X, want % X", got, want)
	}
}

func TestParseResponse_CurrentValues_Golden(t *testing.T) {
	// Same worked example's response: 01 66 12 [18 data bytes] 44 74.
	frame := []byte{
		0x01, 0x66, 0x12,
		0xBD, 0x6D, 0xF7, 0x3E, 0x3B, 0xBB, 0xE7, 0x41,
		0xC3, 0x16, 0x00, 0x00, 0x02, 0xAE, 0x06, 0x00, 0x00, 0x00,
		0x44, 0x74,
	}
	addr, code, data, err := ParseResponse(frame)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr != 0x01 {
		t.Fatalf("want addr 0x01, got 0x%02X", addr)
	}
	if code != CmdCurrentValues {
		t.Fatalf("want code %d, got %d", CmdCurrentValues, code)
	}
	if len(data) != 18 {
		t.Fatalf("want 18 data bytes, got %d", len(data))
	}
}

func TestParseResponse_BadCRC(t *testing.T) {
	frame := []byte{0x01, 0x66, 0x00, 0x00, 0x00}
	if _, _, _, err := ParseResponse(frame); err == nil {
		t.Fatal("expected error for corrupted CRC")
	}
}

func TestParseResponse_ByteCountMismatch(t *testing.T) {
	// Declares byteCount=5 but only 2 data bytes follow before CRC.
	body := []byte{0x01, 0x66, 0x05, 0xAA, 0xBB}
	crcVal := crcForTest(body)
	frame := append(append([]byte(nil), body...), byte(crcVal&0xFF), byte(crcVal>>8))
	if _, _, _, err := ParseResponse(frame); err == nil {
		t.Fatal("expected error for byte count mismatch")
	}
}

func TestBuildHourlyArchive_ParamEncoding(t *testing.T) {
	// startIndex=1 (top of archive), n=5 rows.
	got := BuildHourlyArchive(0x01, 1, 5)
	// addr, code=104(0x68), i_hi=0x00, i_lo=0x01, n=0x05, then CRC (computed below).
	wantPrefix := []byte{0x01, 0x68, 0x00, 0x01, 0x05}
	if !bytes.Equal(got[:5], wantPrefix) {
		t.Fatalf("got prefix % X, want % X", got[:5], wantPrefix)
	}
	if len(got) != 7 {
		t.Fatalf("want 7-byte frame (5 body + 2 CRC), got %d", len(got))
	}
}

func TestBuildDailyArchive_ParamEncoding(t *testing.T) {
	got := BuildDailyArchive(0x01, 100, 10)
	wantPrefix := []byte{0x01, 0x69, 0x00, 0x64, 0x0A} // 105=0x69, index 100=0x0064
	if !bytes.Equal(got[:5], wantPrefix) {
		t.Fatalf("got prefix % X, want % X", got[:5], wantPrefix)
	}
}

// crcForTest computes a real CRC16-modbus for ad-hoc test fixtures.
func crcForTest(body []byte) uint16 {
	var crc uint16 = 0xFFFF
	for _, b := range body {
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
