package northbound

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
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

// TestVKM_IdentityBlock_LiveObservedRequest reproduces the EXACT request
// captured from a live Энергосфера УВП-280А-driver probe (vkm_live.jsonl,
// 19.07.2026): reading 6 registers from 1806 — the identification block
// (pipe mask, firmware version, build date, serial). The carrier used to
// answer with silence (nil), which made the connection look dead and ЭС
// kept reconnecting every ~1s. This must now get a real answer.
func TestVKM_IdentityBlock_LiveObservedRequest(t *testing.T) {
	r := NewVKMArchiveResponder(FixedVKMSource{Result: testString})

	// Exact bytes from the live log: 03 07 0e 00 06 (addr=0x070E=1806, qty=6).
	resp := r.Respond([]byte{0x03, 0x07, 0x0E, 0x00, 0x06})
	if resp == nil {
		t.Fatal("identity block read returned nil (silence) — this is the live bug")
	}
	if resp[0] != 0x03 || resp[1] != 12 {
		t.Fatalf("resp header = % X, want func=03 byteCount=12", resp[0:2])
	}
	data := resp[2:]
	pipeMask := binary.BigEndian.Uint16(data[0:2])
	fwVersion := binary.BigEndian.Uint16(data[2:4])
	if pipeMask == 0 {
		t.Fatal("pipe mask is zero — device would look unconfigured")
	}
	if fwVersion == 0 {
		t.Fatal("firmware version is zero")
	}
}

// TestVKM_HandshakeConstants_MatchDocument verifies the documented
// control-constant registers (110/112) decode to the exact values
// specified in modbus_uvp280_01.pdf — these are not guesses, they are
// spec'd defaults a driver may use as a sanity check.
func TestVKM_HandshakeConstants_MatchDocument(t *testing.T) {
	r := NewVKMArchiveResponder(FixedVKMSource{Result: testString})

	// 110HR, int32 = 1234567890
	resp := r.Respond(fc03(110, 2))
	got := int32(binary.BigEndian.Uint32(resp[2:6]))
	if got != 1234567890 {
		t.Fatalf("110 (int32) = %d, want 1234567890", got)
	}

	// 112HR, float = 123.4567
	resp = r.Respond(fc03(112, 2))
	f := math.Float32frombits(binary.BigEndian.Uint32(resp[2:6]))
	if diff := f - 123.4567; diff < -0.001 || diff > 0.001 {
		t.Fatalf("112 (float) = %v, want ~123.4567", f)
	}
}

// TestVKM_LiveClock_ReplacesExceptionLoop reproduces the EXACT request
// captured from live Энергосфера traffic right after the identity block
// succeeded (vkm_live.jsonl, 19.07.2026 19:15): 03 07 08 00 06 (addr=1800,
// qty=6). This used to return ILLEGAL DATA ADDRESS, which she retried
// rapidly before reconnecting — never reaching the archive protocol.
func TestVKM_LiveClock_ReplacesExceptionLoop(t *testing.T) {
	r := NewVKMArchiveResponder(FixedVKMSource{Result: testString})

	resp := r.Respond([]byte{0x03, 0x07, 0x08, 0x00, 0x06})
	if resp == nil || resp[0] == (0x03|0x80) {
		t.Fatalf("clock read still exceptions/silences: % X", resp)
	}
	if resp[0] != 0x03 || resp[1] != 12 {
		t.Fatalf("resp header = % X, want func=03 byteCount=12", resp[0:2])
	}
	data := resp[2:]
	day := binary.BigEndian.Uint16(data[0:2])
	month := binary.BigEndian.Uint16(data[2:4])
	year := binary.BigEndian.Uint16(data[4:6])
	hour := binary.BigEndian.Uint16(data[6:8])

	now := time.Now()
	if int(year) != now.Year() {
		t.Fatalf("year = %d, want %d", year, now.Year())
	}
	if month < 1 || month > 12 {
		t.Fatalf("month = %d, out of range", month)
	}
	if day < 1 || day > 31 {
		t.Fatalf("day = %d, out of range", day)
	}
	if hour > 23 {
		t.Fatalf("hour = %d, out of range", hour)
	}
}

// TestVKM_CurrentBlock2020_NoExceptionLoop reproduces the EXACT request
// that caused the enormous "Основные интервалы" time skew in the ЭС console
// (vkm_live.jsonl, 21.07.2026): FC04 read of 2020 qty=4 (operating-time
// counters). It used to return ILLEGAL DATA ADDRESS in a tight loop; now it
// must return a valid block with a non-zero штатное время so the driver's
// archive time base stays sane.
func TestVKM_CurrentBlock2020_NoExceptionLoop(t *testing.T) {
	r := NewVKMArchiveResponder(FixedVKMSource{Result: testString})

	// 04 07 e4 00 04 = FC04 addr=0x07E4=2020 qty=4
	resp := r.Respond([]byte{0x04, 0x07, 0xE4, 0x00, 0x04})
	if resp == nil || resp[0] == (0x04|0x80) {
		t.Fatalf("2020 block still exceptions/silences: % X", resp)
	}
	if resp[0] != 0x04 || resp[1] != 8 {
		t.Fatalf("resp header = % X, want func=04 byteCount=8", resp[0:2])
	}
	// Operating time (2020:2021, int32) must be non-zero.
	opTime := binary.BigEndian.Uint32(resp[2:6])
	if opTime == 0 {
		t.Fatal("operating time is zero — this is what skewed the archive time base")
	}
}

// for truly unknown addresses, but also don't stay silent (which looked
// like a dead connection to a live Энергосфера — see
// TestVKM_IdentityBlock_LiveObservedRequest doc). A real Modbus exception
// (ILLEGAL DATA ADDRESS) keeps the connection alive with a fast, correct
// NACK.
func TestVKM_UnknownRegister_GetsException(t *testing.T) {
	r := NewVKMArchiveResponder(FixedVKMSource{Result: testString})
	resp := r.Respond(fc03(9500, 2))
	if resp == nil {
		t.Fatal("unknown register returned silence — should be a Modbus exception")
	}
	if len(resp) != 2 || resp[0] != (0x03|0x80) || resp[1] != 0x02 {
		t.Fatalf("resp = % X, want exception func=83 code=02", resp)
	}
}

func TestVKM_IdleStatus_BeforeRequest(t *testing.T) {
	r := NewVKMArchiveResponder(FixedVKMSource{Result: testString})
	// Polling status with no pending request → ready (idle), not collecting.
	if s := readU16(r.Respond(fc03(8000, 1)), t); s != vkmStatusReady {
		t.Fatalf("idle status = %d, want 2", s)
	}
}
