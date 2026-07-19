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

	regs map[int]uint16 // captured 7900-7914 request registers

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
	return &VKMArchiveResponder{src: src, regs: make(map[int]uint16)}
}

// УВП-280.01 archive register addresses and status codes.
const (
	vkmReqIDReg   = 7900
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
	return nil
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
