package northbound

import (
	"encoding/binary"
	"testing"
)

// --- helpers to build request PDUs ---

func fc06(addr, val uint16) []byte {
	p := make([]byte, 5)
	p[0] = 0x06
	binary.BigEndian.PutUint16(p[1:3], addr)
	binary.BigEndian.PutUint16(p[3:5], val)
	return p
}

func fc03(addr, qty uint16) []byte {
	p := make([]byte, 5)
	p[0] = 0x03
	binary.BigEndian.PutUint16(p[1:3], addr)
	binary.BigEndian.PutUint16(p[3:5], qty)
	return p
}

// fc16 builds a block write of consecutive registers starting at addr.
func fc16(addr uint16, vals []uint16) []byte {
	qty := len(vals)
	p := make([]byte, 6+qty*2)
	p[0] = 0x10
	binary.BigEndian.PutUint16(p[1:3], addr)
	binary.BigEndian.PutUint16(p[3:5], uint16(qty))
	p[5] = byte(qty * 2)
	for i, v := range vals {
		binary.BigEndian.PutUint16(p[6+i*2:6+i*2+2], v)
	}
	return p
}

func readU16(resp []byte, t *testing.T) uint16 {
	t.Helper()
	if len(resp) != 4 || resp[1] != 2 {
		t.Fatalf("expected 2-byte register response, got % X", resp)
	}
	return binary.BigEndian.Uint16(resp[2:4])
}

const testString = "V01{Расход}=123.45 кг/с;V02{Масса}=678.90 кг;"

// writeFullRequestFC06 writes a complete 7900-7914 request via single-
// register writes, options LAST — the sequence mb_request_poll_string uses.
func writeFullRequestFC06(t *testing.T, r *VKMArchiveResponder) {
	t.Helper()
	// id=42, pipe=1, start 01.07.2026 00:00:00, end 02.07.2026 00:00:00
	writes := []struct {
		addr, val uint16
	}{
		{7900, 42}, {7901, 1},
		{7902, 1}, {7903, 7}, {7904, 2026}, {7905, 0}, {7906, 0}, {7907, 0},
		{7908, 2}, {7909, 7}, {7910, 2026}, {7911, 0}, {7912, 0}, {7913, 0},
		{7914, 32}, // options — triggers collection
	}
	for _, w := range writes {
		resp := r.Respond(fc06(w.addr, w.val))
		want := fc06(w.addr, w.val) // FC06 echoes the request
		if string(resp) != string(want) {
			t.Fatalf("FC06 write %d: echo = % X, want % X", w.addr, resp, want)
		}
	}
}

func TestVKM_HappyPath_FC06(t *testing.T) {
	r := NewVKMArchiveResponder(FixedVKMSource{Result: testString})
	writeFullRequestFC06(t, r)

	// First status poll → collecting (1).
	if s := readU16(r.Respond(fc03(8000, 1)), t); s != vkmStatusCollecting {
		t.Fatalf("first status = %d, want 1 (collecting)", s)
	}
	// Second status poll → ready (2).
	if s := readU16(r.Respond(fc03(8000, 1)), t); s != vkmStatusReady {
		t.Fatalf("second status = %d, want 2 (ready)", s)
	}

	// Request id echo at 8001.
	if id := readU16(r.Respond(fc03(8001, 1)), t); id != 42 {
		t.Fatalf("req id echo = %d, want 42", id)
	}

	// Length at 8002.
	gotLen := readU16(r.Respond(fc03(8002, 1)), t)
	if int(gotLen) != len([]byte(testString)) {
		t.Fatalf("len = %d, want %d", gotLen, len([]byte(testString)))
	}

	// Data at 8003: read enough registers to cover the string.
	qty := uint16((len([]byte(testString)) + 1) / 2)
	resp := r.Respond(fc03(8003, qty))
	if resp[0] != 0x03 {
		t.Fatalf("data resp fc = 0x%02X", resp[0])
	}
	got := string(resp[2 : 2+len([]byte(testString))])
	if got != testString {
		t.Fatalf("data = %q, want %q", got, testString)
	}
}

func TestVKM_HappyPath_FC16Block(t *testing.T) {
	r := NewVKMArchiveResponder(FixedVKMSource{Result: testString})

	// Whole 7900-7914 request written as ONE block (options included).
	vals := []uint16{42, 1, 1, 7, 2026, 0, 0, 0, 2, 7, 2026, 0, 0, 0, 32}
	resp := r.Respond(fc16(7900, vals))
	// FC16 echoes addr + qty.
	if len(resp) != 5 || resp[0] != 0x10 {
		t.Fatalf("FC16 echo = % X", resp)
	}
	if binary.BigEndian.Uint16(resp[1:3]) != 7900 || binary.BigEndian.Uint16(resp[3:5]) != 15 {
		t.Fatalf("FC16 echo addr/qty wrong: % X", resp)
	}

	// Collection should have been triggered by the block covering 7914.
	if s := readU16(r.Respond(fc03(8000, 1)), t); s != vkmStatusCollecting {
		t.Fatalf("status after block = %d, want 1", s)
	}
	if s := readU16(r.Respond(fc03(8000, 1)), t); s != vkmStatusReady {
		t.Fatalf("status = %d, want 2", s)
	}
	if id := readU16(r.Respond(fc03(8001, 1)), t); id != 42 {
		t.Fatalf("req id = %d, want 42", id)
	}
}

func TestVKM_NoRecords_Status3(t *testing.T) {
	// Empty source → after collecting, status must be 3 (no records).
	r := NewVKMArchiveResponder(FixedVKMSource{Result: ""})
	writeFullRequestFC06(t, r)

	if s := readU16(r.Respond(fc03(8000, 1)), t); s != vkmStatusCollecting {
		t.Fatalf("first status = %d, want 1", s)
	}
	if s := readU16(r.Respond(fc03(8000, 1)), t); s != vkmStatusNoRecords {
		t.Fatalf("status = %d, want 3 (no records)", s)
	}
	// Length must be 0 for an empty result.
	if l := readU16(r.Respond(fc03(8002, 1)), t); l != 0 {
		t.Fatalf("len = %d, want 0", l)
	}
}

func TestVKM_IdleStatus_BeforeRequest(t *testing.T) {
	r := NewVKMArchiveResponder(FixedVKMSource{Result: testString})
	// Polling status with no pending request → ready (idle), not collecting.
	if s := readU16(r.Respond(fc03(8000, 1)), t); s != vkmStatusReady {
		t.Fatalf("idle status = %d, want 2", s)
	}
}
