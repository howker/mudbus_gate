package northbound

import (
	"bytes"
	"testing"

	"mbgw/internal/codec"
)

// A note on golden values in this file:
//
// The Akron documentation's worked examples ("87,42 [куб.м/ч]") are the
// *decimal display* of a float32, rounded to 2 places for the reader — not
// the exact value that was encoded. The real wire bytes 42 AE D5 F4 decode
// to 87.41787719726562, not exactly 87.42. Asserting EncodeValue(87.42)
// equals those doc bytes is therefore wrong: it compares an exact input
// against an already-rounded reference and fails.
//
// So this file uses two kinds of checks:
//  1. Exact literal values with exact float32 representations (0.5, -12.25,
//     etc.) for straightforward encode/order tests.
//  2. Round-trip checks against the *actual* doc byte patterns: decode the
//     documented bytes with codec.DecodeFloat32 (the existing, already-
//     verified decoder), then re-encode that decoded value and assert we
//     get the identical bytes back. This validates EncodeValue against the
//     real Akron wire format without smuggling in decimal-rounding error.

func TestEncodeValue_FloatBigEndian(t *testing.T) {
	p := NorthPoint{Register: 0, Space: "holding", Type: "float", Order: "0123", Source: "x"}
	got, err := EncodeValue(p, -12.25) // exact in float32
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0xC1, 0x44, 0x00, 0x00}
	if !bytes.Equal(got, want) {
		t.Fatalf("float 0123: got % X, want % X", got, want)
	}
}

func TestEncodeValue_FloatOrder3210(t *testing.T) {
	p := NorthPoint{Type: "float", Order: "3210", Source: "x"}
	got, err := EncodeValue(p, -12.25)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x00, 0x00, 0x44, 0xC1} // full byte reversal of C1 44 00 00
	if !bytes.Equal(got, want) {
		t.Fatalf("float 3210: got % X, want % X", got, want)
	}
}

func TestEncodeValue_Int32BigEndian(t *testing.T) {
	// acc_time = 1710 min, per doc example (integers have no rounding issue).
	p := NorthPoint{Type: "int32", Order: "0123", Source: "acc_time"}
	got, err := EncodeValue(p, 1710)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x00, 0x00, 0x06, 0xAE}
	if !bytes.Equal(got, want) {
		t.Fatalf("int32 0123: got % X, want % X", got, want)
	}
}

func TestEncodeValue_ScaleBias(t *testing.T) {
	p := NorthPoint{Type: "float", Order: "0123", Scale: 10, Source: "x"}
	got, err := EncodeValue(p, -1.225) // *10 → -12.25, same golden as above
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0xC1, 0x44, 0x00, 0x00}
	if !bytes.Equal(got, want) {
		t.Fatalf("scaled float: got % X, want % X", got, want)
	}
}

func TestEncodeValue_Uint16(t *testing.T) {
	p := NorthPoint{Type: "uint16", Source: "x"}
	got, err := EncodeValue(p, 5827)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x16, 0xC3} // 5827 = 0x16C3
	if !bytes.Equal(got, want) {
		t.Fatalf("uint16: got % X, want % X", got, want)
	}
}

func TestEncodeValue_UnsupportedType(t *testing.T) {
	p := NorthPoint{Type: "double", Source: "x"}
	if _, err := EncodeValue(p, 1); err == nil {
		t.Fatal("expected error for unsupported type, got nil")
	}
}

// --- Round-trip against the real Akron doc byte patterns ---

func TestEncodeValue_RoundTripAgainstDocQ(t *testing.T) {
	// Doc bytes for the flow-rate example (Q), displayed as "87,42 куб.м/ч".
	docBytes := []byte{0x42, 0xAE, 0xD5, 0xF4}
	v, err := codec.DecodeFloat32(docBytes, "0123")
	if err != nil {
		t.Fatal(err)
	}

	p := NorthPoint{Type: "float", Order: "0123", Source: "Q"}
	got, err := EncodeValue(p, float64(v))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, docBytes) {
		t.Fatalf("round-trip Q: got % X, want % X", got, docBytes)
	}
}

func TestEncodeValue_RoundTripAgainstDocV(t *testing.T) {
	// Doc bytes for the flow-speed example (V), displayed as "0,483 м/с".
	docBytes := []byte{0x3E, 0xF7, 0x6D, 0xBD}
	v, err := codec.DecodeFloat32(docBytes, "0123")
	if err != nil {
		t.Fatal(err)
	}

	p := NorthPoint{Type: "float", Order: "0123", Source: "V"}
	got, err := EncodeValue(p, float64(v))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, docBytes) {
		t.Fatalf("round-trip V: got % X, want % X", got, docBytes)
	}
}

// --- ReadRegisters: map assembly, using exact-representable test values ---

func TestReadRegisters_TwoFloats(t *testing.T) {
	points := []NorthPoint{
		{Register: 0, Space: "holding", Type: "float", Order: "0123", Source: "V"},
		{Register: 2, Space: "holding", Type: "float", Order: "0123", Source: "Q"},
	}
	values := map[string]float64{"V": 0.5, "Q": -12.25}

	data, covered, err := ReadRegisters(points, "holding", 0, 4, values)
	if err != nil {
		t.Fatal(err)
	}
	if !covered {
		t.Fatal("expected covered=true")
	}
	want := []byte{0x3F, 0x00, 0x00, 0x00, 0xC1, 0x44, 0x00, 0x00}
	if !bytes.Equal(data, want) {
		t.Fatalf("two floats: got % X, want % X", data, want)
	}
}

func TestReadRegisters_Subrange(t *testing.T) {
	points := []NorthPoint{
		{Register: 0, Space: "holding", Type: "float", Order: "0123", Source: "V"},
		{Register: 2, Space: "holding", Type: "float", Order: "0123", Source: "Q"},
	}
	values := map[string]float64{"V": 0.5, "Q": -12.25}

	data, covered, err := ReadRegisters(points, "holding", 2, 2, values)
	if err != nil {
		t.Fatal(err)
	}
	if !covered {
		t.Fatal("expected covered=true")
	}
	want := []byte{0xC1, 0x44, 0x00, 0x00}
	if !bytes.Equal(data, want) {
		t.Fatalf("subrange: got % X, want % X", data, want)
	}
}

func TestReadRegisters_MissingValue(t *testing.T) {
	points := []NorthPoint{
		{Register: 0, Space: "holding", Type: "float", Order: "0123", Source: "Q"},
	}
	data, covered, err := ReadRegisters(points, "holding", 0, 2, map[string]float64{})
	if err != nil {
		t.Fatal(err)
	}
	if !covered {
		t.Fatal("expected covered=true (address is mapped even if value absent)")
	}
	if !bytes.Equal(data, []byte{0, 0, 0, 0}) {
		t.Fatalf("missing value: got % X, want all zero", data)
	}
}

func TestReadRegisters_OutsideMap(t *testing.T) {
	points := []NorthPoint{
		{Register: 0, Space: "holding", Type: "float", Order: "0123", Source: "Q"},
	}
	_, covered, err := ReadRegisters(points, "holding", 100, 2, map[string]float64{"Q": 1})
	if err != nil {
		t.Fatal(err)
	}
	if covered {
		t.Fatal("expected covered=false for out-of-map range")
	}
}

func TestReadRegisters_WrongSpace(t *testing.T) {
	points := []NorthPoint{
		{Register: 0, Space: "holding", Type: "float", Order: "0123", Source: "Q"},
	}
	_, covered, err := ReadRegisters(points, "input", 0, 2, map[string]float64{"Q": 1})
	if err != nil {
		t.Fatal(err)
	}
	if covered {
		t.Fatal("expected covered=false when reading a different space")
	}
}
