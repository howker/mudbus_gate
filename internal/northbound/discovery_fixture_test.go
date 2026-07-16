package northbound

import (
	"bytes"
	"testing"
)

func TestDiscoveryFixture_StubRead(t *testing.T) {
	f := DiscoveryFixture{ReadFillByte: 0xAB}
	// func 03, start=0, qty=2
	body := []byte{0x00, 0x00, 0x00, 0x02}
	resp := f.Respond(1, funcReadHolding, body)

	want := []byte{funcReadHolding, 0x04, 0xAB, 0xAB, 0xAB, 0xAB}
	if !bytes.Equal(resp, want) {
		t.Fatalf("stub read: got % X, want % X", resp, want)
	}
}

func TestDiscoveryFixture_StubReadInput(t *testing.T) {
	f := DiscoveryFixture{}
	body := []byte{0x00, 0x05, 0x00, 0x01}
	resp := f.Respond(1, funcReadInput, body)

	want := []byte{funcReadInput, 0x02, 0x00, 0x00}
	if !bytes.Equal(resp, want) {
		t.Fatalf("stub read input: got % X, want % X", resp, want)
	}
}

func TestDiscoveryFixture_StubWriteSingleRegister_EchoesRequest(t *testing.T) {
	f := DiscoveryFixture{}
	body := []byte{0x00, 0x05, 0x00, 0x2A} // addr=5, value=42
	resp := f.Respond(1, funcWriteSingleRegister, body)

	want := []byte{funcWriteSingleRegister, 0x00, 0x05, 0x00, 0x2A}
	if !bytes.Equal(resp, want) {
		t.Fatalf("stub write single: got % X, want % X", resp, want)
	}
}

func TestDiscoveryFixture_StubWriteMultiple_DropsByteCountAndData(t *testing.T) {
	f := DiscoveryFixture{}
	// addr=10, qty=2, byteCount=4, data=4 bytes
	body := []byte{0x00, 0x0A, 0x00, 0x02, 0x04, 0x11, 0x22, 0x33, 0x44}
	resp := f.Respond(1, funcWriteMultipleRegisters, body)

	want := []byte{funcWriteMultipleRegisters, 0x00, 0x0A, 0x00, 0x02}
	if !bytes.Equal(resp, want) {
		t.Fatalf("stub write multiple: got % X, want % X", resp, want)
	}
}

func TestDiscoveryFixture_UnknownFunction_ExceptionIllegalFunction(t *testing.T) {
	f := DiscoveryFixture{}
	resp := f.Respond(1, 0x17, []byte{0, 0, 0, 1})

	want := []byte{0x17 | 0x80, excIllegalFunction}
	if !bytes.Equal(resp, want) {
		t.Fatalf("unknown function: got % X, want % X", resp, want)
	}
}

func TestDiscoveryFixture_Override_ExactMatch(t *testing.T) {
	f := DiscoveryFixture{
		Overrides: []DiscoveryOverride{
			{Unit: 1, Function: funcReadHolding, Address: 8000, ResponseHex: "0001"},
		},
	}
	body := []byte{0x1F, 0x40, 0x00, 0x01} // addr=8000 (0x1F40), qty=1
	resp := f.Respond(1, funcReadHolding, body)

	want := []byte{funcReadHolding, 0x00, 0x01}
	if !bytes.Equal(resp, want) {
		t.Fatalf("override: got % X, want % X", resp, want)
	}
}

func TestDiscoveryFixture_Override_WrongAddress_FallsBackToStub(t *testing.T) {
	f := DiscoveryFixture{
		ReadFillByte: 0x00,
		Overrides: []DiscoveryOverride{
			{Unit: 1, Function: funcReadHolding, Address: 8000, ResponseHex: "0001"},
		},
	}
	body := []byte{0x00, 0x00, 0x00, 0x01} // addr=0, does not match override's 8000
	resp := f.Respond(1, funcReadHolding, body)

	want := []byte{funcReadHolding, 0x02, 0x00, 0x00} // generic stub, not the override
	if !bytes.Equal(resp, want) {
		t.Fatalf("fallback: got % X, want % X", resp, want)
	}
}

func TestDiscoveryFixture_StubRead_QtyZero_DefaultsToOne(t *testing.T) {
	f := DiscoveryFixture{}
	body := []byte{0x00, 0x00, 0x00, 0x00} // qty=0 is invalid on the wire, but discovery must not crash
	resp := f.Respond(1, funcReadHolding, body)

	want := []byte{funcReadHolding, 0x02, 0x00, 0x00}
	if !bytes.Equal(resp, want) {
		t.Fatalf("qty=0 fallback: got % X, want % X", resp, want)
	}
}
