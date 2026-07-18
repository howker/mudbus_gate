package akron

import (
	"testing"
	"time"

	"mbgw/internal/codec"
)

// TestEncodeHourlyRow_DocExample checks the row against the documented
// worked example value (582.7 m³) and time (10:00 05.03.2008), the same
// fixture used elsewhere in the codebase. The exact (U, Pu) split differs
// from the device's original (our encoder picks Pu=0), so we assert on the
// DECODED value and on the BCD time bytes, not on raw U/Pu bytes.
func TestEncodeHourlyRow_DocExample(t *testing.T) {
	ts := time.Date(2008, 3, 5, 10, 0, 0, 0, time.UTC)
	row, err := EncodeHourlyRow(582.7, ts)
	if err != nil {
		t.Fatalf("EncodeHourlyRow: %v", err)
	}
	if len(row) != HourlyRowSize {
		t.Fatalf("row size = %d, want %d", len(row), HourlyRowSize)
	}

	// Volume (bytes 0..4) must decode back to 582.7.
	got, err := codec.DecodeAkronVolume(row[0:5], "3210")
	if err != nil {
		t.Fatalf("DecodeAkronVolume: %v", err)
	}
	if !approxEqual(got, 582.7, 1e-6) {
		t.Fatalf("decoded volume = %v, want 582.7", got)
	}

	// Time (bytes 5..8) must be packed BCD H,D,M,Y = 10,05,03,08.
	wantBCD := []byte{0x10, 0x05, 0x03, 0x08}
	if string(row[5:9]) != string(wantBCD) {
		t.Fatalf("time bytes = % X, want % X", row[5:9], wantBCD)
	}
}

// TestEncodeVolume_RoundTrip verifies encode→decode is value-preserving
// across magnitudes, including the precision-sensitive small values.
func TestEncodeVolume_RoundTrip(t *testing.T) {
	cases := []float64{0, 0.001, 1.5, 582.7, 12345.678, 2_000_000.0}
	for _, v := range cases {
		u, pu, err := EncodeVolume(v)
		if err != nil {
			t.Fatalf("EncodeVolume(%v): %v", v, err)
		}
		// Rebuild the 5-byte volume field and decode it.
		field := []byte{byte(u), byte(u >> 8), byte(u >> 16), byte(u >> 24), pu}
		got, err := codec.DecodeAkronVolume(field, "3210")
		if err != nil {
			t.Fatalf("DecodeAkronVolume(%v): %v", v, err)
		}
		// Pu=0 keeps 3 decimals; tolerance well under half a milli-unit.
		if !approxEqual(got, v, 5e-4) {
			t.Fatalf("round-trip %v → U=%d Pu=%d → %v (mismatch)", v, u, pu, got)
		}
	}
}

// TestEncodeVolume_PicksSmallestPu confirms the precision-first Pu choice:
// values that fit at Pu=0 must use Pu=0.
func TestEncodeVolume_PicksSmallestPu(t *testing.T) {
	_, pu, err := EncodeVolume(582.7)
	if err != nil {
		t.Fatal(err)
	}
	if pu != 0 {
		t.Fatalf("Pu = %d, want 0 (value fits at Pu=0)", pu)
	}
	// A value too large for Pu=0 (int32 max /1000 ≈ 2.147e6) must bump Pu.
	_, pu, err = EncodeVolume(5_000_000.0)
	if err != nil {
		t.Fatal(err)
	}
	if pu == 0 {
		t.Fatal("Pu should have increased for a value that overflows Pu=0")
	}
}

func TestEncodeHourlyRow_RejectsNonFinite(t *testing.T) {
	ts := time.Date(2026, 7, 17, 16, 0, 0, 0, time.UTC)
	if _, err := EncodeHourlyRow(mustInf(), ts); err == nil {
		t.Fatal("expected error for +Inf volume")
	}
}

func approxEqual(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

func mustInf() float64 {
	x := 1.0
	y := 0.0
	return x / y // +Inf
}
