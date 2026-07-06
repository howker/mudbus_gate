package codec

import "testing"

func TestDecodeBCDByte(t *testing.T) {
	tests := []struct {
		name    string
		b       byte
		want    int
		wantErr bool
	}{
		// Real golden vector: Меркурий command 0x16 worked example
		// (section 4.6 of "Описание системы команд приборов учета
		// Меркурий"), time field "10:00, 5 марта 2008":
		//   hour=0x10 -> 10, minute=0x00 -> 0, day=0x05 -> 5,
		//   month=0x03 -> 3, year=0x08 -> 8 (2008)
		{name: "merkuriy hour 10", b: 0x10, want: 10},
		{name: "merkuriy minute 00", b: 0x00, want: 0},
		{name: "merkuriy day 05", b: 0x05, want: 5},
		{name: "merkuriy month 03", b: 0x03, want: 3},
		{name: "merkuriy year 08 (2008)", b: 0x08, want: 8},
		{name: "max valid 99", b: 0x99, want: 99},
		{name: "invalid high nibble", b: 0xA0, wantErr: true},
		{name: "invalid low nibble", b: 0x0B, wantErr: true},
		{name: "invalid both nibbles", b: 0xFF, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeBCDByte(tc.b)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for byte 0x%02X, got nil", tc.b)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("DecodeBCDByte(0x%02X): want %d, got %d", tc.b, tc.want, got)
			}
		})
	}
}

func TestEncodeBCDByte(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		want    byte
		wantErr bool
	}{
		{name: "10", val: 10, want: 0x10},
		{name: "0", val: 0, want: 0x00},
		{name: "5", val: 5, want: 0x05},
		{name: "99", val: 99, want: 0x99},
		{name: "negative", val: -1, wantErr: true},
		{name: "over 99", val: 100, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeBCDByte(tc.val)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for value %d, got nil", tc.val)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("EncodeBCDByte(%d): want 0x%02X, got 0x%02X", tc.val, tc.want, got)
			}
		})
	}
}

func TestBCDByte_RoundTrip(t *testing.T) {
	for v := 0; v <= 99; v++ {
		enc, err := EncodeBCDByte(v)
		if err != nil {
			t.Fatalf("EncodeBCDByte(%d): unexpected error: %v", v, err)
		}
		dec, err := DecodeBCDByte(enc)
		if err != nil {
			t.Fatalf("DecodeBCDByte(0x%02X) for value %d: unexpected error: %v", enc, v, err)
		}
		if dec != v {
			t.Fatalf("round-trip mismatch: %d -> 0x%02X -> %d", v, enc, dec)
		}
	}
}