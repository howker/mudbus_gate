package northbound

import "testing"

func TestCP1251Encode_KnownBytes(t *testing.T) {
	cases := []struct {
		in   string
		want []byte
	}{
		{"кг", []byte{0xEA, 0xE3}},
		{"Расход", []byte{0xD0, 0xE0, 0xF1, 0xF5, 0xEE, 0xE4}},
		{"V01", []byte{'V', '0', '1'}}, // ASCII passthrough
		{"ё", []byte{0xB8}},
		{"Ё", []byte{0xA8}},
	}
	for _, c := range cases {
		got := cp1251Encode(c.in)
		if string(got) != string(c.want) {
			t.Errorf("cp1251Encode(%q) = % X, want % X", c.in, got, c.want)
		}
	}
}

func TestCP1251Encode_FullArchiveString(t *testing.T) {
	s := "V01{Расход}=123.45 кг/с;V02{Масса}=678.90 кг;"
	got := cp1251Encode(s)
	// Every byte must be a single byte per input rune — no UTF-8 multi-byte
	// sequences leaking through. Rune count (not byte count) must equal
	// output length.
	runeCount := 0
	for range s {
		runeCount++
	}
	if len(got) != runeCount {
		t.Fatalf("cp1251 output length = %d, want %d (one byte per rune)", len(got), runeCount)
	}
}
