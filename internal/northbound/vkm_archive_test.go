package northbound

import (
	"encoding/binary"
	"math"
	"os"
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

// TestVKM_StatusRead_TwoRegisters reproduces the EXACT status read the live
// Энергосфера driver issues: 03 1f40 0002 — TWO registers (8000 status +
// 8001 request-id echo), not one. Serving only the status register made the
// reply malformed, so the driver never proceeded to read the result and
// restarted the request cycle with a fresh id (vkm_live.jsonl 22-23.07.2026).
func TestVKM_StatusRead_TwoRegisters(t *testing.T) {
	withVKMConfig(t, "min_ready_ms = 0\n")
	r := NewVKMArchiveResponder(FixedVKMSource{Result: testString})
	writeFullRequestFC06(t, r) // writes id=42

	// 03 1f40 0002 = read 2 registers from 8000
	resp := r.Respond([]byte{0x03, 0x1F, 0x40, 0x00, 0x02})
	if resp == nil || resp[0] == (0x03|0x80) {
		t.Fatalf("status read exceptions/silences: % X", resp)
	}
	if resp[1] != 4 || len(resp) != 6 {
		t.Fatalf("resp = % X, want byteCount=4 (2 registers)", resp)
	}
	// second register must echo the request id written at 7900 (42)
	gotID := binary.BigEndian.Uint16(resp[4:6])
	if gotID != 42 {
		t.Fatalf("8001 request-id echo = %d, want 42", gotID)
	}
}

func TestVKM_MinReadyDelay(t *testing.T) {
	withVKMConfig(t, "min_ready_ms = 150\n")

	r := NewVKMArchiveResponder(FixedVKMSource{Result: testString})
	writeFullRequestFC06(t, r)

	// Poll immediately — must still be "collecting" even past poll #1,
	// because min_ready_ms hasn't elapsed yet.
	if s := readU16(r.Respond(fc03(8000, 1)), t); s != vkmStatusCollecting {
		t.Fatalf("poll 1 = %d, want collecting", s)
	}
	if s := readU16(r.Respond(fc03(8000, 1)), t); s != vkmStatusCollecting {
		t.Fatalf("poll 2 (too soon) = %d, want still collecting", s)
	}

	time.Sleep(160 * time.Millisecond)
	if s := readU16(r.Respond(fc03(8000, 1)), t); s != vkmStatusReady {
		t.Fatalf("poll after delay = %d, want ready", s)
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

// TestVKM_YearModeEncoding verifies the config-file-selectable year
// encodings used to diagnose the ЭС century-scale time skew without
// rebuilds.
func TestVKM_YearModeEncoding(t *testing.T) {
	withVKMConfig(t, "year_mode = y2000\n")
	if got := encodeYearVKM(2026); got != 26 {
		t.Fatalf("y2000: got %d, want 26", got)
	}
	withVKMConfig(t, "year_mode = y1900\n")
	if got := encodeYearVKM(2026); got != 126 {
		t.Fatalf("y1900: got %d, want 126", got)
	}
	withVKMConfig(t, "year_mode = full\n")
	if got := encodeYearVKM(2026); got != 2026 {
		t.Fatalf("full: got %d, want 2026", got)
	}
}

// withVKMConfig points the config loader at a temp file with the given
// contents and forces a reload, restoring global state after the test.
func withVKMConfig(t *testing.T, contents string) {
	t.Helper()
	path := t.TempDir() + "/vkm_config.txt"
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	vkmCfgMu.Lock()
	vkmCfgPath = path
	vkmCfgLoaded = false
	vkmCfgChecked = time.Time{}
	vkmCfgModTime = time.Time{}
	vkmCfgMu.Unlock()
	t.Cleanup(func() {
		vkmCfgMu.Lock()
		vkmCfgPath = vkmConfigFileName
		vkmCfgLoaded = false
		vkmCfgChecked = time.Time{}
		vkmCfgModTime = time.Time{}
		vkmCfgMu.Unlock()
	})
}

// synchronization against its polling server (per operator feedback, this
// is the real source of the "разбежка времени"): 1008 must report zero
// drift and 1007 must report a healthy time-sync status, per
// modbus_uvp280_01.pdf. An exception here (as before) made ЭС treat the
// clock as wildly skewed.
func TestVKM_TimeSyncRegisters(t *testing.T) {
	r := NewVKMArchiveResponder(FixedVKMSource{Result: testString})

	// 1008 — difference in seconds vs reference; must be 0 (no drift).
	resp := r.Respond(fc03(1008, 1))
	if resp == nil || resp[0] == (0x03|0x80) {
		t.Fatalf("1008 exceptions/silences: % X", resp)
	}
	if diff := int16(binary.BigEndian.Uint16(resp[2:4])); diff != 0 {
		t.Fatalf("1008 time drift = %d, want 0", diff)
	}

	// 1007 — reference-time status bits; bit1 (fresh accurate time) must be
	// set, else ЭС considers the time stale.
	resp = r.Respond(fc03(1007, 1))
	if resp == nil || resp[0] == (0x03|0x80) {
		t.Fatalf("1007 exceptions/silences: % X", resp)
	}
	status := binary.BigEndian.Uint16(resp[2:4])
	if status&0x0002 == 0 {
		t.Fatalf("1007 status = 0x%04X, bit1 (fresh time) not set", status)
	}
}

// FC04 reads that blocked the driver from ever reaching the archive
// protocol: ТП1 at 2020, then ТП2 at 2110 (vkm_live.jsonl, 21.07.2026).
// Both must now return a valid (non-exception) block of the right length so
// the driver progresses. Content is zeros by design (see currentBlockVKM).
func TestVKM_CurrentBlock_AllPipes(t *testing.T) {
	r := NewVKMArchiveResponder(FixedVKMSource{Result: testString})

	cases := []struct {
		name           string
		addrHi, addrLo byte
	}{
		{"pipe1 2020", 0x07, 0xE4}, // 0x07E4 = 2020
		{"pipe2 2110", 0x08, 0x3E}, // 0x083E = 2110
	}
	for _, c := range cases {
		resp := r.Respond([]byte{0x04, c.addrHi, c.addrLo, 0x00, 0x04})
		if resp == nil || resp[0] == (0x04|0x80) {
			t.Fatalf("%s: still exceptions/silences: % X", c.name, resp)
		}
		if resp[0] != 0x04 || resp[1] != 8 || len(resp) != 10 {
			t.Fatalf("%s: resp = % X, want func=04 byteCount=8 (4 regs)", c.name, resp)
		}
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
