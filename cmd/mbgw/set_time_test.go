package main

import "testing"

func TestSameWriteMultipleRegistersAck(t *testing.T) {
	tests := []struct {
		name string
		resp []byte
		addr int
		qty  int
		want bool
	}{
		{name: "valid", resp: []byte{0x10, 0x80, 0x00, 0x00, 0x02}, addr: 0x8000, qty: 2, want: true},
		{name: "wrong function", resp: []byte{0x03, 0x80, 0x00, 0x00, 0x02}, addr: 0x8000, qty: 2, want: false},
		{name: "wrong address", resp: []byte{0x10, 0x80, 0x01, 0x00, 0x02}, addr: 0x8000, qty: 2, want: false},
		{name: "wrong quantity", resp: []byte{0x10, 0x80, 0x00, 0x00, 0x01}, addr: 0x8000, qty: 2, want: false},
		{name: "short", resp: []byte{0x10, 0x80}, addr: 0x8000, qty: 2, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameWriteMultipleRegistersAck(tt.resp, tt.addr, tt.qty); got != tt.want {
				t.Fatalf("sameWriteMultipleRegistersAck(% X, %d, %d) = %v, want %v", tt.resp, tt.addr, tt.qty, got, tt.want)
			}
		})
	}
}
