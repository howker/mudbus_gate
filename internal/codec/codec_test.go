package codec

import (
	"bytes"
	"encoding/hex"
	"math"
	"strings"
	"testing"
)

func parseHex(s string) []byte {
	s = strings.ReplaceAll(s, " ", "")
	b, _ := hex.DecodeString(s)
	return b
}

func TestRegisterCount(t *testing.T) {
	tests := []struct {
		name     string
		dataType string
		want     int
	}{
		{name: "float", dataType: "float", want: 2},
		{name: "int32", dataType: "int32", want: 2},
		{name: "uint32", dataType: "uint32", want: 2},
		{name: "scaled_int", dataType: "scaled_int", want: 1}, // 16-bit per VZLET devices (0.01C/0.001MPa); confirmed via live TSRV-024 slice
		{name: "double", dataType: "double", want: 4},
		{name: "float64", dataType: "float64", want: 4},
		{name: "u32+float", dataType: "u32+float", want: 4},
		{name: "long+float", dataType: "long+float", want: 4},
		{name: "uint16", dataType: "uint16", want: 1},
		{name: "int16", dataType: "int16", want: 1},
		{name: "bitfield", dataType: "bitfield", want: 1},
		{name: "unknown defaults to 1", dataType: "string", want: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RegisterCount(tc.dataType); got != tc.want {
				t.Fatalf("RegisterCount(%q): want %d, got %d", tc.dataType, tc.want, got)
			}
		})
	}
}

func TestReorder32(t *testing.T) {
	src := parseHex("42 F6 E9 D5")

	tests := []struct {
		name    string
		order   string
		input   []byte
		want    []byte
		wantErr bool
	}{
		{name: "0123", order: "0123", input: src, want: parseHex("42 F6 E9 D5")},
		{name: "ABCD alias", order: "ABCD", input: src, want: parseHex("42 F6 E9 D5")},
		{name: "empty means canonical", order: "", input: src, want: parseHex("42 F6 E9 D5")},
		{name: "1032", order: "1032", input: parseHex("F6 42 D5 E9"), want: parseHex("42 F6 E9 D5")},
		{name: "BADC alias", order: "BADC", input: parseHex("F6 42 D5 E9"), want: parseHex("42 F6 E9 D5")},
		{name: "2301", order: "2301", input: parseHex("E9 D5 42 F6"), want: parseHex("42 F6 E9 D5")},
		{name: "CDAB alias", order: "CDAB", input: parseHex("E9 D5 42 F6"), want: parseHex("42 F6 E9 D5")},
		{name: "3210", order: "3210", input: parseHex("D5 E9 F6 42"), want: parseHex("42 F6 E9 D5")},
		{name: "DCBA alias", order: "DCBA", input: parseHex("D5 E9 F6 42"), want: parseHex("42 F6 E9 D5")},
		{name: "unknown", order: "9999", input: src, wantErr: true},
		{name: "short payload", order: "0123", input: parseHex("42 F6 E9"), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Reorder32(tc.input, tc.order)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("want %x, got %x", tc.want, got)
			}
		})
	}
}

func TestDecodeFloat32(t *testing.T) {
	tests := []struct {
		order string
		hex   string
		want  float32
	}{
		{"0123", "42 F6 E9 D5", 123.4567},
		{"3210", "D5 E9 F6 42", 123.4567},
		{"1032", "F6 42 D5 E9", 123.4567},
		{"2301", "E9 D5 42 F6", 123.4567},
	}

	for _, tc := range tests {
		t.Run(tc.order, func(t *testing.T) {
			b := parseHex(tc.hex)
			val, err := DecodeFloat32(b, tc.order)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if math.Abs(float64(val-tc.want)) > 0.0001 {
				t.Fatalf("expected %f, got %f", tc.want, val)
			}
		})
	}
}

func TestDecodeInt32(t *testing.T) {
	tests := []struct {
		order string
		hex   string
		want  int32
	}{
		{"0123", "49 96 02 D2", 1234567890},
		{"3210", "D2 02 96 49", 1234567890},
		{"1032", "96 49 D2 02", 1234567890},
		{"2301", "02 D2 49 96", 1234567890},
	}

	for _, tc := range tests {
		t.Run(tc.order, func(t *testing.T) {
			b := parseHex(tc.hex)
			val, err := DecodeInt32(b, tc.order)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if val != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, val)
			}
		})
	}
}

func TestReorder64(t *testing.T) {
	tests := []struct {
		name    string
		order   string
		input   []byte
		want    []byte
		wantErr bool
	}{
		{
			name:  "01234567",
			order: "01234567",
			input: parseHex("40 5E DD 3C 07 FB 4C 93"),
			want:  parseHex("40 5E DD 3C 07 FB 4C 93"),
		},
		{
			name:  "empty means canonical",
			order: "",
			input: parseHex("40 5E DD 3C 07 FB 4C 93"),
			want:  parseHex("40 5E DD 3C 07 FB 4C 93"),
		},
		{
			name:  "10325476",
			order: "10325476",
			input: parseHex("5E 40 3C DD FB 07 93 4C"),
			want:  parseHex("40 5E DD 3C 07 FB 4C 93"),
		},
		{
			name:  "76543210",
			order: "76543210",
			input: parseHex("93 4C FB 07 3C DD 5E 40"),
			want:  parseHex("40 5E DD 3C 07 FB 4C 93"),
		},
		{
			name:    "unknown",
			order:   "bad-order",
			input:   parseHex("40 5E DD 3C 07 FB 4C 93"),
			wantErr: true,
		},
		{
			name:    "short payload",
			order:   "01234567",
			input:   parseHex("40 5E DD 3C 07 FB 4C"),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Reorder64(tc.input, tc.order)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("want %x, got %x", tc.want, got)
			}
		})
	}
}

func TestDecodeFloat64(t *testing.T) {
	tests := []struct {
		name  string
		order string
		hex   string
		want  float64
	}{
		{
			name:  "01234567",
			order: "01234567",
			hex:   "40 5E DD 3C 07 FB 4C 93",
			want:  123.4567890123456,
		},
		{
			name:  "10325476",
			order: "10325476",
			hex:   "5E 40 3C DD FB 07 93 4C",
			want:  123.4567890123456,
		},
		{
			name:  "76543210",
			order: "76543210",
			hex:   "93 4C FB 07 3C DD 5E 40",
			want:  123.4567890123456,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := parseHex(tc.hex)
			val, err := DecodeFloat64(b, tc.order)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if math.Abs(val-tc.want) > 0.0000000000001 {
				t.Fatalf("expected %.13f, got %.13f", tc.want, val)
			}
		})
	}
}

func TestDecodeUint16AndInt16(t *testing.T) {
	u16, err := DecodeUint16(parseHex("09 E9"))
	if err != nil {
		t.Fatalf("unexpected uint16 error: %v", err)
	}
	if u16 != 2537 {
		t.Fatalf("expected uint16 2537, got %d", u16)
	}

	i16, err := DecodeInt16(parseHex("FF 9C"))
	if err != nil {
		t.Fatalf("unexpected int16 error: %v", err)
	}
	if i16 != -100 {
		t.Fatalf("expected int16 -100, got %d", i16)
	}
}

func TestDecodeUint16Short(t *testing.T) {
	_, err := DecodeUint16([]byte{0x01})
	if err == nil {
		t.Fatal("expected error for short uint16 payload")
	}
}

func TestEncodeUint16(t *testing.T) {
	got := EncodeUint16(2537)
	want := parseHex("09 E9")
	if !bytes.Equal(got, want) {
		t.Fatalf("want %x, got %x", want, got)
	}
}

func TestEncodeUint32(t *testing.T) {
	tests := []struct {
		name    string
		value   uint32
		order   string
		want    []byte
		wantErr bool
	}{
		{name: "0123", value: 1234567890, order: "0123", want: parseHex("49 96 02 D2")},
		{name: "3210", value: 1234567890, order: "3210", want: parseHex("D2 02 96 49")},
		{name: "1032", value: 1234567890, order: "1032", want: parseHex("96 49 D2 02")},
		{name: "2301", value: 1234567890, order: "2301", want: parseHex("02 D2 49 96")},
		{name: "ABCD alias", value: 1234567890, order: "ABCD", want: parseHex("49 96 02 D2")},
		{name: "bad order", value: 1234567890, order: "bad", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeUint32(tc.value, tc.order)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("want %x, got %x", tc.want, got)
			}
		})
	}
}

func TestEncodeInt32(t *testing.T) {
	tests := []struct {
		name  string
		value int32
		order string
		want  []byte
	}{
		{name: "0123", value: 1234567890, order: "0123", want: parseHex("49 96 02 D2")},
		{name: "3210", value: 1234567890, order: "3210", want: parseHex("D2 02 96 49")},
		{name: "1032", value: 1234567890, order: "1032", want: parseHex("96 49 D2 02")},
		{name: "2301", value: 1234567890, order: "2301", want: parseHex("02 D2 49 96")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeInt32(tc.value, tc.order)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("want %x, got %x", tc.want, got)
			}
		})
	}
}

func TestEncodeFloat32(t *testing.T) {
	tests := []struct {
		name  string
		value float32
		order string
		want  []byte
	}{
		{name: "0123", value: 123.4567, order: "0123", want: parseHex("42 F6 E9 D5")},
		{name: "3210", value: 123.4567, order: "3210", want: parseHex("D5 E9 F6 42")},
		{name: "1032", value: 123.4567, order: "1032", want: parseHex("F6 42 D5 E9")},
		{name: "2301", value: 123.4567, order: "2301", want: parseHex("E9 D5 42 F6")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeFloat32(tc.value, tc.order)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("want %x, got %x", tc.want, got)
			}
		})
	}
}

func TestEncodeFloat64(t *testing.T) {
	tests := []struct {
		name  string
		value float64
		order string
		want  []byte
	}{
		{name: "01234567", value: 123.4567890123456, order: "01234567", want: parseHex("40 5E DD 3C 07 FB 4C 93")},
		{name: "10325476", value: 123.4567890123456, order: "10325476", want: parseHex("5E 40 3C DD FB 07 93 4C")},
		{name: "76543210", value: 123.4567890123456, order: "76543210", want: parseHex("93 4C FB 07 3C DD 5E 40")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeFloat64(tc.value, tc.order)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("want %x, got %x", tc.want, got)
			}
		})
	}
}
func TestRegisterCount_BCD(t *testing.T) {
    if got := RegisterCount("bcd"); got != 1 {
        t.Fatalf("RegisterCount(\"bcd\"): want 1, got %d", got)
    }
}
