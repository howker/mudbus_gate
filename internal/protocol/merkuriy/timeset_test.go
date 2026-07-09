package merkuriy

import (
	"bytes"
	"testing"
	"time"

	"mbgw/internal/protocol/modbus"
)

func TestBuildReadCurrentTime_Golden(t *testing.T) {
	got := BuildReadCurrentTime(0x80)
	want := []byte{0x80, 0x04, 0x00, 0x72, 0xE8}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X, want % X", got, want)
	}
}

func TestParseCurrentTimeResponse_Golden(t *testing.T) {
	// Synthetic response using section 3.10's own worked time value
	// (10:55:00, среда=3, 05 марта 2008, zima) as a round-trip check:
	// 80 00 55 10 03 05 03 08 01 1E 6F
	frame := []byte{0x80, 0x00, 0x55, 0x10, 0x03, 0x05, 0x03, 0x08, 0x01, 0x1E, 0x6F}
	got, dow, isWinter, err := ParseCurrentTimeResponse(frame)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2008, time.March, 5, 10, 55, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if dow != 3 {
		t.Fatalf("dow: want 3, got %d", dow)
	}
	if !isWinter {
		t.Fatal("expected isWinter=true (flag=1)")
	}
}

func TestParseCurrentTimeResponse_DeviceStatusError(t *testing.T) {
	// 1-byte status response (X3h - insufficient access level) instead of
	// 8-byte time data.
	frame := []byte{0x80, 0x03, 0x40, 0x21}
	if _, _, _, err := ParseCurrentTimeResponse(frame); err == nil {
		t.Fatal("expected error when device returns a status byte instead of time data")
	}
}

func TestBuildSetTime_Golden(t *testing.T) {
	// Section 3.10's worked example: set time to 10:55:00, среда (dow=3),
	// 05 марта 2008, zima (isWinter=true).
	tm := time.Date(2008, time.March, 5, 10, 55, 0, 0, time.Local)
	got, err := BuildSetTime(0x80, tm, 3, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []byte{0x80, 0x03, 0x0C, 0x00, 0x55, 0x10, 0x03, 0x05, 0x03, 0x08, 0x01, 0x7F, 0x70}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X, want % X", got, want)
	}
}

func TestBuildCorrectTime_Golden(t *testing.T) {
	// Section 3.11's worked example: correct time to 10:55:30.
	tm := time.Date(2008, time.March, 5, 10, 55, 30, 0, time.Local)
	got, err := BuildCorrectTime(0x80, tm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []byte{0x80, 0x03, 0x0D, 0x30, 0x55, 0x10, 0x67, 0xE4}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X, want % X", got, want)
	}
}

func TestSetTimeAndCorrectTime_RoundTripThroughReadParse(t *testing.T) {
	// Not a wire-format check - just verifies encodeTimeFields' BCD
	// encoding stays internally consistent with ParseCurrentTimeResponse's
	// decoding, for a value distinct from the manual's own example.
	tm := time.Date(2025, time.December, 31, 23, 59, 58, 0, time.Local)
	data, err := encodeTimeFields(tm, 5, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := append([]byte{0x01}, data...)
	crc := modbus.CRC16(body)
	frame := append(body, byte(crc&0xFF), byte(crc>>8))

	got, dow, isWinter, err := ParseCurrentTimeResponse(frame)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(tm) {
		t.Fatalf("got %v, want %v", got, tm)
	}
	if dow != 5 {
		t.Fatalf("dow: want 5, got %d", dow)
	}
	if isWinter {
		t.Fatal("expected isWinter=false")
	}
}
func TestBuildReadCurrentTimePDU_Golden(t *testing.T) {
    got := BuildReadCurrentTimePDU()
    want := []byte{0x04, 0x00}
    if !bytes.Equal(got, want) {
        t.Fatalf("got % X, want % X", got, want)
    }
}

func TestParseCurrentTimeData_Golden(t *testing.T) {
    // Bare data (no address, no CRC): same section 3.10 worked time
    // value (10:55:00, среда=3, 05 марта 2008, zima).
    data := []byte{0x00, 0x55, 0x10, 0x03, 0x05, 0x03, 0x08, 0x01}
    got, dow, isWinter, err := ParseCurrentTimeData(data)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    want := time.Date(2008, time.March, 5, 10, 55, 0, 0, time.Local)
    if !got.Equal(want) {
        t.Fatalf("got %v, want %v", got, want)
    }
    if dow != 3 {
        t.Fatalf("dow: want 3, got %d", dow)
    }
    if !isWinter {
        t.Fatal("expected isWinter=true")
    }
}

func TestBuildSetTimePDU_Golden(t *testing.T) {
    tm := time.Date(2008, time.March, 5, 10, 55, 0, 0, time.Local)
    got, err := BuildSetTimePDU(tm, 3, true)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    want := []byte{0x03, 0x0C, 0x00, 0x55, 0x10, 0x03, 0x05, 0x03, 0x08, 0x01}
    if !bytes.Equal(got, want) {
        t.Fatalf("got % X, want % X", got, want)
    }
}

func TestBuildCorrectTimePDU_Golden(t *testing.T) {
    tm := time.Date(2008, time.March, 5, 10, 55, 30, 0, time.Local)
    got, err := BuildCorrectTimePDU(tm)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    want := []byte{0x03, 0x0D, 0x30, 0x55, 0x10}
    if !bytes.Equal(got, want) {
        t.Fatalf("got % X, want % X", got, want)
    }
}
