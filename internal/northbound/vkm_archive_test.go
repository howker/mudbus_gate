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
	wantBytes := cp1251Encode(testString)
	gotLen := readU16(r.Respond(fc03(8002, 1)), t)
	if int(gotLen) != len(wantBytes) {
		t.Fatalf("len = %d, want %d", gotLen, len(wantBytes))
	}

	// Data at 8003: read enough registers to cover the string. The wire
	// format is Windows-1251 (single byte per Cyrillic char), not Go's
	// native UTF-8 — see cp1251Encode.
	qty := uint16((len(wantBytes) + 1) / 2)
	resp := r.Respond(fc03(8003, qty))
	if resp[0] != 0x03 {
		t.Fatalf("data resp fc = 0x%02X", resp[0])
	}
	got := resp[2 : 2+len(wantBytes)]
	if string(got) != string(wantBytes) {
		t.Fatalf("data = % X, want %X (cp1251 of %q)", got, wantBytes, testString)
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
	resp := r.Respond(fc03(6000, 2)) // вне диапазона 7900-9999, заведомо неизвестный регистр
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

// TestVKM_AssembleTime_DecodesYearWithSameMode reproduces the live bug: the
// master mirrors back whatever year encoding it saw on our clock, so with
// year_mode=y2000 she writes archive request years as 2-digit (26, not
// 2026). Building the reply string with the raw value produced "0026"
// (vkm_live.jsonl, 24.07.2026); decodeYearVKM must undo the same encoding
// used for the outgoing clock.
func TestVKM_AssembleTime_DecodesYearWithSameMode(t *testing.T) {
	withVKMConfig(t, "year_mode = y2000\n")

	r := NewVKMArchiveResponder(FixedVKMSource{})
	// Write a request with year=26 (as the live master does), day=15,
	// month=7 — mirroring the exact live scenario.
	regs := []struct{ addr, val uint16 }{
		{vkmReqIDReg, 1}, {vkmPipeReg, 1},
		{vkmStartReg, 15}, {vkmStartReg + 1, 7}, {vkmStartReg + 2, 26},
		{vkmStartReg + 3, 16}, {vkmStartReg + 4, 0}, {vkmStartReg + 5, 0},
		{vkmEndReg, 15}, {vkmEndReg + 1, 7}, {vkmEndReg + 2, 26},
		{vkmEndReg + 3, 16}, {vkmEndReg + 4, 30}, {vkmEndReg + 5, 0},
	}
	for _, reg := range regs {
		r.regs[int(reg.addr)] = reg.val
	}

	got := r.assembleTime(vkmStartReg)
	if got.Year() != 2026 {
		t.Fatalf("assembleTime year = %d, want 2026 (raw register was 26)", got.Year())
	}
	if got.Month() != 7 || got.Day() != 15 || got.Hour() != 16 {
		t.Fatalf("assembleTime = %v, want 15.07.2026 16:00", got)
	}
}

// TestVKM_CurrentBlock_KeyParameters verifies the documented current-value
// offsets carry real values (not zero): pressure (+0), temperature (+4),
// and — the whole point of this milestone — heat ENERGY (+18). The
// operator could only read mass+temperature from the real device because
// ЭS reads these current registers and anything left zero (energy,
// pressure) came through absent.
func TestVKM_CurrentBlock_KeyParameters(t *testing.T) {
	r := NewVKMArchiveResponder(FixedVKMSource{})

	readFloat := func(off int) float32 {
		addr := 2000 + off
		hi, lo := byte(addr>>8), byte(addr&0xFF)
		resp := r.Respond([]byte{0x04, hi, lo, 0x00, 0x02})
		if resp == nil || resp[0] == (0x04|0x80) {
			t.Fatalf("offset +%d read failed: % X", off, resp)
		}
		return math.Float32frombits(binary.BigEndian.Uint32(resp[2:6]))
	}

	if v := readFloat(4); v < 45.5 || v > 45.7 {
		t.Errorf("temperature (+4) = %v, want ~45.6", v)
	}
	if v := readFloat(0); v < 149000 || v > 151000 {
		t.Errorf("pressure (+0) = %v, want ~150000", v)
	}
	if v := readFloat(18); v < 5.4e9 || v > 5.6e9 {
		t.Errorf("heat energy (+18) = %v, want ~5.5e9 J", v)
	}
	if v := readFloat(10); v < 678 || v > 679 {
		t.Errorf("mass (+10) = %v, want ~678.9", v)
	}
}

// TestVKM_ParameterMap_NameAndValue covers the ЭЛЕМЕР-ВКМ-360 "Чтение
// карты параметров" mechanism (7600 write → 7700 name / 7800 value read),
// found in the real ВКМ-360 register map — a driver can enumerate a
// pipe's real parameter names/tags BEFORE building an archive request.
// We had implemented NOTHING for this before; any driver attempt got
// silence.
func TestVKM_ParameterMap_NameAndValue(t *testing.T) {
	r := NewVKMArchiveResponder(FixedVKMSource{})

	// Write 7600 = (pipe=1)<<8 | row=0 → select the first parameter (Масса).
	resp := r.Respond(fc06(7600, (1<<8)|0))
	if resp == nil {
		t.Fatal("7600 write got no response")
	}

	// Read name at 7700 — enough registers to cover "Масса" in cp1251.
	nameResp := r.Respond(fc03(7700, 10))
	name := nameResp[2 : 2+len(cp1251Encode("Масса"))]
	if string(name) != string(cp1251Encode("Масса")) {
		t.Fatalf("param name = % X, want cp1251(%q) = % X", name, "Масса", cp1251Encode("Масса"))
	}

	// Read value at 7800.
	valResp := r.Respond(fc03(7800, 10))
	wantVal := cp1251Encode("678.900000")
	val := valResp[2 : 2+len(wantVal)]
	if string(val) != string(wantVal) {
		t.Fatalf("param value = % X, want % X", val, wantVal)
	}
}

// TestVKM_ParameterMap_OutOfRangeRow verifies an unavailable row yields a
// zero-length STRING (first byte 0x00, "конец строки" per the doc) — the
// response still carries the full qty*2 bytes requested (a valid Modbus
// read can't return fewer registers than asked), just zero-padded.
func TestVKM_ParameterMap_OutOfRangeRow(t *testing.T) {
	r := NewVKMArchiveResponder(FixedVKMSource{})
	r.Respond(fc06(7600, (1<<8)|99)) // row 99 doesn't exist

	nameResp := r.Respond(fc03(7700, 5))
	if nameResp[1] != 10 {
		t.Fatalf("byteCount = %d, want 10 (qty*2, full read)", nameResp[1])
	}
	if nameResp[2] != 0 {
		t.Fatalf("first byte = 0x%02X, want 0x00 (empty C-string)", nameResp[2])
	}
}

// TestVKM_ContinuationRead_LongString — regression-тест на баг с продолжением
// чтения, найденный на живом захвате (2026-08-01, vkm_live.jsonl): ЭС
// читает 8002 разом на 120 регистров (длина + первые 238 байт), а если
// строка длиннее — тут же читает продолжение с адреса 8002+120=8122.
// Раньше это давало Modbus exception 0x02 (адрес не обслуживался вообще),
// и ЭС не могла дочитать строку длиннее одного 120-регистрового чтения.
func TestVKM_ContinuationRead_LongString(t *testing.T) {
	// Строка длиннее 238 байт (120 регистров - 2 байта на длину), чтобы
	// потребовалось продолжение чтения. Наращиваем по факту длины В CP1251
	// (1 байт на кириллический символ), а не по len(long) — это длина
	// Go-строки в UTF-8 (2 байта на кириллический символ), которая
	// заметно больше и вводит в заблуждение при подборе порога.
	long := "V01{Расход}=123.45 кг/с;"
	for len(cp1251Encode(long)) < 300 {
		long += "V02{Масса теплоносителя за интервал измерения}=678.90 кг;"
	}

	r := NewVKMArchiveResponder(FixedVKMSource{Result: long})
	writeFullRequestFC06(t, r)
	_ = readU16(r.Respond(fc03(8000, 1)), t) // collecting
	_ = readU16(r.Respond(fc03(8000, 1)), t) // ready

	wantBytes := cp1251Encode(long)

	// Первое чтение: 8002, 120 регистров (длина + первые 238 байт).
	first := r.Respond(fc03(8002, 120))
	if first[0] != 0x03 || int(first[1]) != 240 {
		t.Fatalf("первое чтение: fc=0x%02X byteCount=%d, ожидалось fc=03 byteCount=240", first[0], first[1])
	}
	gotLen := binary.BigEndian.Uint16(first[2:4])
	if int(gotLen) != len(wantBytes) {
		t.Fatalf("длина в ответе = %d, ожидалось %d", gotLen, len(wantBytes))
	}
	firstChunk := first[4:242] // 238 байт данных после 2 байт длины

	// Продолжение: адрес 8002+120=8122, читаем ещё сколько нужно.
	remaining := len(wantBytes) - len(firstChunk)
	contQty := uint16((remaining + 1) / 2)
	cont := r.Respond(fc03(8122, contQty))
	if cont == nil || cont[0] != 0x03 {
		t.Fatalf("продолжение (адрес 8122): получен exception вместо данных: % X", cont)
	}
	contChunk := cont[2 : 2+remaining]

	got := append(append([]byte{}, firstChunk...), contChunk...)
	if string(got) != string(wantBytes) {
		t.Fatalf("собранная строка не совпадает с исходной\nполучено: % X\nожидалось: % X", got, wantBytes)
	}
}
