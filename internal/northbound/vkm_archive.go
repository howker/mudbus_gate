package northbound

import (
	"encoding/binary"
	"time"
)

// VKMArchiveResponder is the SLAVE side of the ЭЛЕМЕР УВП-280.01 / ВКМ-360
// string-archive protocol — the inverse of the mb_request_poll_string
// downstream strategy. It lets mbgw stand in front of Энергосфера as a
// УВП-280А-shaped device (that is how Энергосфера reads ВКМ, since it has
// no native ВКМ driver) and serve the ВКМ's hourly/interval archives out
// of mbgw's own store.
//
// Protocol (УВП-280.01 register map, "Чтение архивов"):
//  1. master WRITES the request into 7900-7914 (id, pipe, start/end time,
//     options); the OPTIONS register 7914 is written LAST and starts the
//     collection. Writes come as FC06 (single) or FC16 (block).
//  2. master POLLS status register 8000 (03) until it reads 2 (ready);
//     1 = collecting, 3 = no records for the period.
//  3. master READS length from 8002 and the result STRING from 8003+.
//
// The result string is device-formatted ("[tag]{header}=value unit;…").
// mbgw does not synthesise it field by field — it serves the string the
// real device produced (stored downstream), so whatever subset Энергосфера
// parses today (mass, temperature) is reproduced exactly. Until a real ВКМ
// is available, a fixed test string stands in via FixedVKMSource; the
// mechanism here is independent of the string's content.
//
// STATE: this type is STATEFUL across the multi-step exchange and holds
// per-connection request state, so ONE instance must be created per
// upstream connection (like the simulator's per-conn vkm360ArchiveState).
type VKMArchiveResponder struct {
	src VKMArchiveSource

	regs  map[int]uint16 // captured 7900-7914 request registers
	extra map[int]uint16 // static identification/status register stubs (non-archive reads)

	haveReq     bool
	statusPolls int
	haveResult  bool
	result      string
}

// VKMArchiveSource yields the УВП-format result string for a pipe and time
// window. ok=false means "no records for that period" (status 3). The DB-
// backed implementation comes later; FixedVKMSource covers the mechanism
// build and tests.
type VKMArchiveSource interface {
	Archive(pipe int, start, end time.Time) (result string, ok bool)
}

// FixedVKMSource returns one canned string regardless of pipe/time — the
// stand-in until real ВКМ archives are wired in.
type FixedVKMSource struct{ Result string }

func (f FixedVKMSource) Archive(pipe int, start, end time.Time) (string, bool) {
	if f.Result == "" {
		return "", false
	}
	return f.Result, true
}

func NewVKMArchiveResponder(src VKMArchiveSource) *VKMArchiveResponder {
	return &VKMArchiveResponder{src: src, regs: make(map[int]uint16), extra: defaultVKMIdentityRegs()}
}

// defaultVKMIdentityRegs stubs the УВП-280.01 registers a driver is likely
// to check BEFORE it ever touches the archive protocol:
//   - 100/101: byte-order registers, DOCUMENTED default values
//     (0123h / 01234567h per modbus_uvp280_01.pdf "Настройки интерфейса").
//   - 110/112/114: DOCUMENTED "control constant" registers — fixed,
//     spec'd values (1234567890 / 123.4567 / 123.4567890123456) a driver
//     can use as a handshake/sanity check that this really is a
//     УВП-280.01-family device. These are not guesses: they are the exact
//     values the document specifies.
//   - 1806-1811: identification block (pipe mask, firmware version, build
//     date, serial) — found necessary by LIVE testing against
//     Энергосфера: she reads this before proceeding, and silence here
//     made the connection look dead (see package notes / vkm_live.jsonl
//     from 19.07.2026). This is the VKM analogue of Akron's command-101
//     passport check.
//
// Everything here is either a documented constant or an explicitly-marked
// stub (identification fields) — mechanism first, real identity data
// plugged in once available. Extend this map as further live probing
// reveals more registers the driver checks before proceeding.
func defaultVKMIdentityRegs() map[int]uint16 {
	return map[int]uint16{
		100: 0x0123,              // byte order, 32-bit (documented default)
		101: 0x0123, 102: 0x4567, // byte order, 64-bit (documented default 01234567h, hi/lo words)
		110: 18838, 111: 722, // control constant int32 = 1234567890
		112: 17142, 113: 59861, // control constant float = 123.4567
		114: 16478, 115: 56636, 116: 2043, 117: 19603, // control constant double = 123.4567890123456

		1806: 0x0001, // pipe mask: pipe 1 present
		1807: 100,    // firmware version 1.00
		1808: 715,    // build date: month*100+day = 15 July
		1809: 2026,   // build year
		1810: 9,      // serial (int32 high word): 654321 = 0x0009FCB1
		1811: 64497,  // serial (int32 low word)
	}
}

// УВП-280.01 archive register addresses and status codes.
const (
	vkmReqIDReg   = 7900
	vkmClockReg   = 1800 // day,month,year,hour,minute,second (plain int16 each)
	vkmPipeReg    = 7901
	vkmStartReg   = 7902 // 7902..7907 = day,month,year,hour,min,sec (start)
	vkmEndReg     = 7908 // 7908..7913 = day,month,year,hour,min,sec (end)
	vkmOptsReg    = 7914 // written LAST; triggers collection
	vkmStatusReg  = 8000
	vkmReqEchoReg = 8001
	vkmLenReg     = 8002
	vkmDataReg    = 8003

	vkmStatusCollecting = 1
	vkmStatusReady      = 2
	vkmStatusNoRecords  = 3
)

// Respond maps one inbound Modbus PDU (function code + data, no framing) to
// a response PDU, mutating per-connection state. nil = no answer.
func (r *VKMArchiveResponder) Respond(pdu []byte) []byte {
	if len(pdu) < 5 {
		return nil
	}
	switch pdu[0] {
	case 0x06:
		return r.writeSingle(pdu)
	case 0x10:
		return r.writeBlock(pdu)
	case 0x03, 0x04:
		return r.read(pdu)
	}
	return nil
}

// writeSingle handles FC06 (Preset Single Register): [06][addr:2][val:2].
func (r *VKMArchiveResponder) writeSingle(pdu []byte) []byte {
	addr := int(binary.BigEndian.Uint16(pdu[1:3]))
	val := binary.BigEndian.Uint16(pdu[3:5])
	r.regs[addr] = val
	if addr == vkmOptsReg {
		r.startCollection()
	}
	// FC06 echoes the request (addr + value).
	return append([]byte{0x06}, pdu[1:5]...)
}

// writeBlock handles FC16 (Preset Multiple Registers):
// [10][addr:2][qty:2][byteCount:1][data:byteCount].
func (r *VKMArchiveResponder) writeBlock(pdu []byte) []byte {
	if len(pdu) < 6 {
		return nil
	}
	addr := int(binary.BigEndian.Uint16(pdu[1:3]))
	qty := int(binary.BigEndian.Uint16(pdu[3:5]))
	byteCount := int(pdu[5])
	if byteCount != qty*2 || len(pdu) < 6+byteCount {
		return nil
	}
	for i := 0; i < qty; i++ {
		v := binary.BigEndian.Uint16(pdu[6+i*2 : 6+i*2+2])
		r.regs[addr+i] = v
	}
	// The options register 7914 starting collection may fall anywhere in
	// the written block — trigger if the block covers it.
	if addr <= vkmOptsReg && vkmOptsReg < addr+qty {
		r.startCollection()
	}
	// FC16 echoes addr + qty.
	return append([]byte{0x10}, pdu[1:5]...)
}

// startCollection assembles the request and asks the source for the result
// string, latching status for the subsequent polling loop.
func (r *VKMArchiveResponder) startCollection() {
	pipe := int(r.regs[vkmPipeReg])
	start := r.assembleTime(vkmStartReg)
	end := r.assembleTime(vkmEndReg)

	res, ok := r.src.Archive(pipe, start, end)
	r.result = res
	r.haveResult = ok
	r.haveReq = true
	r.statusPolls = 0
}

// assembleTime builds a wall-clock time from six consecutive registers at
// base: day, month, year, hour, minute, second. Missing/zero registers
// yield a zero-ish time — fine for the fixed source, refined for the DB
// source later.
func (r *VKMArchiveResponder) assembleTime(base int) time.Time {
	day := int(r.regs[base])
	month := int(r.regs[base+1])
	year := int(r.regs[base+2])
	hour := int(r.regs[base+3])
	minute := int(r.regs[base+4])
	sec := int(r.regs[base+5])
	if year == 0 || month == 0 || day == 0 {
		return time.Time{}
	}
	return time.Date(year, time.Month(month), day, hour, minute, sec, 0, time.Local)
}

func (r *VKMArchiveResponder) read(pdu []byte) []byte {
	addr := int(binary.BigEndian.Uint16(pdu[1:3]))
	qty := int(binary.BigEndian.Uint16(pdu[3:5]))
	fc := pdu[0]

	switch addr {
	case vkmStatusReg:
		return r.statusResponse(fc)
	case vkmReqEchoReg:
		return word(fc, r.regs[vkmReqIDReg])
	case vkmLenReg:
		return word(fc, uint16(len(r.result)))
	case vkmDataReg:
		return r.dataResponse(fc, qty)
	}

	// Fall back to the static identification/status register map for any
	// other address range the driver reads before/around the archive
	// protocol (e.g. 1806-1811, 100-117 handshake constants).
	if resp, ok := r.extraBlockResponse(fc, addr, qty); ok {
		return resp
	}

	// Live clock (1800-1805: day, month, year, hour, minute, second — plain
	// decimal int16 each, NOT BCD like Akron's clock). Found by LIVE
	// testing: right after the identity block (1806-1811), the driver
	// reads this block and got ILLEGAL DATA ADDRESS, then retried it
	// rapidly before eventually reconnecting and restarting the whole
	// sequence — never reaching the archive protocol (vkm_live.jsonl,
	// 19.07.2026 19:15). This mirrors the Akron finding that a device's
	// clock must read as live/current before a driver proceeds (see
	// M3_DISCOVERY_FINDINGS_akron.md §4) — same requirement, different
	// device family.
	if resp, ok := liveClockBlockVKM(fc, addr, qty); ok {
		return resp
	}

	// A truly unhandled register gets a proper Modbus exception (ILLEGAL
	// DATA ADDRESS) instead of silence. This matters operationally: a real
	// device NACKs an unsupported register immediately and the connection
	// stays open; staying silent makes a Modbus master TIME OUT and
	// reconnect, which is exactly what was observed live against
	// Энергосфера before any register handling existed here (see
	// defaultVKMIdentityRegs doc comment / vkm_live.jsonl 19.07.2026).
	// Returning a fast, correct NACK for registers we simply haven't
	// enumerated yet is far more robust than trying to guess every
	// register the driver might ever ask for.
	return exceptionResponse(fc, 0x02) // ExceptionIllegalDataAddress
}

// exceptionResponse builds a standard Modbus exception PDU:
// [funcCode|0x80][exceptionCode].
func exceptionResponse(funcCode, code byte) []byte {
	return []byte{funcCode | 0x80, code}
}

// liveClockBlockVKM answers a read anchored at 1800 (day/month/year/
// hour/minute/second, per modbus_uvp280_01.pdf "Встроенные часы реального
// времени прибора") with the gateway's real current time. Only serves
// reads starting exactly at 1800 with 1-6 registers, matching how a real
// device's contiguous register block behaves; qty=6 covers the whole
// block (the exact shape the driver was observed requesting).
func liveClockBlockVKM(fc byte, addr, qty int) ([]byte, bool) {
	if addr != vkmClockReg || qty < 1 || qty > 6 {
		return nil, false
	}
	now := time.Now()
	fields := [6]uint16{
		uint16(now.Day()), uint16(now.Month()), uint16(now.Year()),
		uint16(now.Hour()), uint16(now.Minute()), uint16(now.Second()),
	}
	data := make([]byte, 0, qty*2)
	for i := 0; i < qty; i++ {
		data = append(data, byte(fields[i]>>8), byte(fields[i]))
	}
	resp := make([]byte, 0, 2+len(data))
	resp = append(resp, fc, byte(len(data)))
	return append(resp, data...), true
}

// extraBlockResponse serves a contiguous multi-register read out of the
// extra map, only if EVERY requested register in [addr, addr+qty) is
// declared — a partial/unknown range still returns ok=false (silence),
// which is safer than fabricating data for registers we know nothing
// about.
func (r *VKMArchiveResponder) extraBlockResponse(fc byte, addr, qty int) ([]byte, bool) {
	if qty <= 0 {
		return nil, false
	}
	data := make([]byte, 0, qty*2)
	for a := addr; a < addr+qty; a++ {
		v, ok := r.extra[a]
		if !ok {
			return nil, false
		}
		data = append(data, byte(v>>8), byte(v))
	}
	resp := make([]byte, 0, 2+len(data))
	resp = append(resp, fc, byte(len(data)))
	return append(resp, data...), true
}

// statusResponse mirrors a real device's collect-then-ready timing: the
// first poll after a request returns "collecting", subsequent polls return
// "ready" (or "no records" if the period was empty). With no pending
// request it reports ready (idle).
func (r *VKMArchiveResponder) statusResponse(fc byte) []byte {
	status := uint16(vkmStatusReady)
	if r.haveReq {
		r.statusPolls++
		switch {
		case r.statusPolls == 1:
			status = vkmStatusCollecting
		case r.haveResult:
			status = vkmStatusReady
		default:
			status = vkmStatusNoRecords
		}
	}
	return word(fc, status)
}

// dataResponse returns the result string bytes, padded or truncated to the
// requested register quantity (qty*2 bytes), matching how a Modbus string
// block read behaves.
func (r *VKMArchiveResponder) dataResponse(fc byte, qty int) []byte {
	payload := []byte(r.result)
	maxBytes := qty * 2
	if len(payload) > maxBytes {
		payload = payload[:maxBytes]
	} else {
		padded := make([]byte, maxBytes)
		copy(padded, payload)
		payload = padded
	}
	resp := make([]byte, 2+len(payload))
	resp[0] = fc
	resp[1] = byte(len(payload))
	copy(resp[2:], payload)
	return resp
}

// word builds a single-register (2-byte) read response: [fc][02][hi][lo].
func word(fc byte, v uint16) []byte {
	return []byte{fc, 2, byte(v >> 8), byte(v)}
}
