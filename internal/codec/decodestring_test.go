package codec

import "testing"

func TestDecodeString_TrimsNulAndSpaces(t *testing.T) {
    got := DecodeString([]byte{'A', 'B', 'C', 0x00, 0x00})
    if got != "ABC" {
        t.Fatalf("expected ABC, got %q", got)
    }
}

func TestDecodeString_TrimsTrailingSpaces(t *testing.T) {
    got := DecodeString([]byte{'X', 'Y', ' ', ' '})
    if got != "XY" {
        t.Fatalf("expected XY, got %q", got)
    }
}

func TestDecodeString_Empty(t *testing.T) {
    got := DecodeString([]byte{0x00, 0x00, 0x00, 0x00})
    if got != "" {
        t.Fatalf("expected empty string, got %q", got)
    }
}

func TestDecodeString_NoTrailingPadding(t *testing.T) {
    got := DecodeString([]byte{'H', 'I'})
    if got != "HI" {
        t.Fatalf("expected HI, got %q", got)
    }
}