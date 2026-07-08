package codec

import (
	"math"
	"testing"
)

func TestDecodeAkronVolume_Golden(t *testing.T) {
	// Manufacturer's own worked example (command 102): raw counter=5827
	// (wire bytes C3 16 00 00, little-endian), Pu=0x02 -> 582.7 m3.
	// Wire order matches command-102's own worked example: on-wire bytes
	// C3 16 00 00, decode order "3210" (full byte reversal).
	data := []byte{0xC3, 0x16, 0x00, 0x00, 0x02}
	got, err := DecodeAkronVolume(data, "3210")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(got-582.7) > 0.0001 {
		t.Fatalf("want 582.7, got %v", got)
	}
}

func TestDecodeAkronVolume_ScaleRange(t *testing.T) {
	// Pu documented range is 0-5; verify the scale formula across it for
	// a fixed raw value of 1000.
	tests := []struct {
		pu   byte
		want float64
	}{
		{0, 1.0},
		{1, 10.0},
		{2, 100.0},
		{3, 1000.0},
		{4, 10000.0},
		{5, 100000.0},
	}
	for _, tc := range tests {
		data := []byte{0xE8, 0x03, 0x00, 0x00, tc.pu} // 1000 little-endian (3210 order) + Pu
		got, err := DecodeAkronVolume(data, "3210")
		if err != nil {
			t.Fatalf("unexpected error for Pu=%d: %v", tc.pu, err)
		}
		if math.Abs(got-tc.want) > 0.0001 {
			t.Fatalf("Pu=%d: want %v, got %v", tc.pu, tc.want, got)
		}
	}
}

func TestDecodeAkronVolume_ShortPayload(t *testing.T) {
	if _, err := DecodeAkronVolume([]byte{0x01, 0x02, 0x03, 0x04}, "3210"); err == nil {
		t.Fatal("expected error for payload shorter than 5 bytes")
	}
}