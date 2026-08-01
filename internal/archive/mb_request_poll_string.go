package archive

import (
	"context"
	"encoding/binary"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mbgw/internal/protocol/modbus"
)

// MBRequestPollString implements the VKM-360 stateful archive strategy
// (CONTRACTS.md §6.1): write request registers 7900-7914 (options register
// 7914 written last triggers the collection), poll status register 8000
// until ready, then read length (8002) and the tagged ANSI string (8003+).

const (
	vkmArchIDReg        = 7900
	vkmArchPipeReg      = 7901
	vkmArchStartTimeReg = 7902 // 6 registers: day,month,year,hour,minute,second
	vkmArchEndTimeReg   = 7908 // 6 registers
	vkmArchOptionsReg   = 7914 // written last, triggers collection

	vkmArchStatusReg = 8000
	vkmArchEchoIDReg = 8001
	vkmArchLenReg    = 8002
	vkmArchDataStart = 8003
	vkmArchDataEnd   = 9999

	vkmArchStatusExpired    = 0
	vkmArchStatusCollecting = 1
	vkmArchStatusReady      = 2
	vkmArchStatusNoRecords  = 3
	vkmArchStatusBadStart   = 4
	vkmArchStatusBadEnd     = 5
	vkmArchStatusBadPipe    = 6
	vkmArchStatusTooLarge   = 7

	vkmArchPollInterval   = 200 * time.Millisecond
	vkmArchCollectTimeout = 15 * time.Second
	vkmArchBusyRetryDelay = 500 * time.Millisecond
	vkmArchBusyMaxWait    = 300 * time.Second
)

type MBRequestPollString struct{}

func NewMBRequestPollString() *MBRequestPollString { return &MBRequestPollString{} }

func (r *MBRequestPollString) Strategy() string { return "mb_request_poll_string" }

func (r *MBRequestPollString) Read(ctx context.Context, sess ArchiveSession, tx Transactor, q ArchiveQuery) ([]ArchiveRecord, error) {
	if err := r.startRequest(ctx, tx, q); err != nil {
		return nil, err
	}

	if err := r.waitReady(ctx, tx); err != nil {
		return nil, err
	}

	raw, err := r.readResultString(ctx, tx)
	if err != nil {
		return nil, err
	}

	return parseTaggedString(raw)
}

// startRequest writes the request registers. The options register (7914) is
// written LAST, which is what actually triggers the device to start collecting.
// If a previous request is still running, the device answers with Modbus
// exception 06 (BUSY); this is retried with backoff up to vkmArchBusyMaxWait.
func (r *MBRequestPollString) startRequest(ctx context.Context, tx Transactor, q ArchiveQuery) error {
	// Year is 2-digit (year-2000), matching every other date field in this
	// project (control constants, Akron's BCD year). CONFIRMED by
	// observation (2026-08-01, tools/vkmprobe --probe-archive-regs): when
	// tested writing the full year (2026) to this same register block via
	// function 16, the device echoed back 26 on read — its own year
	// arithmetic clearly expects/normalizes to 2 digits, so send that
	// directly rather than relying on undocumented device-side
	// normalization (which is untested for the DOWNSTREAM time-range
	// validation this feeds, unlike the write itself which we confirmed
	// doesn't error either way).
	timeRegs := func(t time.Time) []uint16 {
		return []uint16{
			uint16(t.Day()), uint16(t.Month()), uint16(t.Year() % 100),
			uint16(t.Hour()), uint16(t.Minute()), uint16(t.Second()),
		}
	}

	writes := []struct {
		addr   int
		values []uint16
	}{
		{vkmArchIDReg, []uint16{1}},
		{vkmArchPipeReg, []uint16{uint16(q.Instance)}},
		{vkmArchStartTimeReg, timeRegs(q.From)},
		{vkmArchEndTimeReg, timeRegs(q.To)},
	}

	deadline := time.Now().Add(vkmArchBusyMaxWait)
	for _, w := range writes {
		if err := r.writeRegistersWithBusyRetry(ctx, tx, w.addr, w.values, deadline); err != nil {
			return fmt.Errorf("write archive request register %d: %w", w.addr, err)
		}
	}

	// Options register is written last and triggers collection.
	// Bit 2 (include_tags) and bit 1 (include_header) are set so the
	// returned string includes tags, per CONTRACTS §6.1 format.
	options := uint16(0b0110)
	if err := r.writeRegistersWithBusyRetry(ctx, tx, vkmArchOptionsReg, []uint16{options}, deadline); err != nil {
		return fmt.Errorf("write archive options register: %w", err)
	}

	return nil
}

func (r *MBRequestPollString) writeRegistersWithBusyRetry(ctx context.Context, tx Transactor, addr int, values []uint16, deadline time.Time) error {
	var pduReq []byte
	var err error
	if len(values) == 1 {
		pduReq = modbus.BuildWriteSingleRegisterPDU(addr, values[0])
	} else {
		pduReq, err = modbus.BuildWriteMultipleRegistersPDU(addr, values)
		if err != nil {
			return err
		}
	}

	for {
		_, err := tx.Transact(ctx, pduReq)
		if err == nil {
			return nil
		}
		if !isBusyErr(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("busy retry deadline exceeded: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(vkmArchBusyRetryDelay):
		}
	}
}

// waitReady polls the status register until the archive is ready to be read,
// or a terminal error/timeout condition is reached.
//
// Status register 8000 is read via HR (function 03), NOT IR (function 04).
// CONFIRMED against real hardware (2026-08-01, tools/vkmprobe
// --probe-archive-regs): HR 8000 answers normally; IR 8000 (what this
// used before the fix) answers Modbus exception 0x02 (illegal data
// address) on every attempt — the same register-space mistake already
// found and fixed for the byte-order control constants
// (internal/session/modbus_byteorder_auth.go). This is why every real
// archive request failed immediately: waitReady's very first status poll
// hit the wrong space and returned an unwrapped "modbus exception: code
// 0x02" (no "poll archive status:" prefix existed here, unlike every
// other step in this file — added below for future diagnosability).
func (r *MBRequestPollString) waitReady(ctx context.Context, tx Transactor) error {
	deadline := time.Now().Add(vkmArchCollectTimeout)
	for {
		pduReq, err := modbus.BuildReadPDU("HR", vkmArchStatusReg, "int16")
		if err != nil {
			return err
		}
		pduResp, err := tx.Transact(ctx, pduReq)
		if err != nil {
			if isBusyErr(err) {
				if time.Now().After(deadline) {
					return fmt.Errorf("status poll busy timeout")
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(vkmArchPollInterval):
				}
				continue
			}
			return fmt.Errorf("poll archive status: %w", err)
		}
		if len(pduResp) < 4 {
			return fmt.Errorf("status response too short")
		}
		status := binary.BigEndian.Uint16(pduResp[2:4])

		switch status {
		case vkmArchStatusReady:
			return nil
		case vkmArchStatusCollecting:
			if time.Now().After(deadline) {
				return fmt.Errorf("archive collection timeout")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(vkmArchPollInterval):
			}
		case vkmArchStatusNoRecords:
			return fmt.Errorf("no records in requested period")
		case vkmArchStatusExpired:
			return fmt.Errorf("archive request expired or not accepted")
		case vkmArchStatusBadStart:
			return fmt.Errorf("invalid start time")
		case vkmArchStatusBadEnd:
			return fmt.Errorf("invalid end time")
		case vkmArchStatusBadPipe:
			return fmt.Errorf("invalid pipe number")
		case vkmArchStatusTooLarge:
			return fmt.Errorf("requested period too large")
		default:
			return fmt.Errorf("unknown archive status: %d", status)
		}
	}
}

// readResultString reads the string length and then the tagged ANSI payload.
//
// Both registers are HR (function 03), not IR — same confirmed fix as
// waitReady's status register (see its doc comment for the incident).
func (r *MBRequestPollString) readResultString(ctx context.Context, tx Transactor) (string, error) {
	lenPDU, err := modbus.BuildReadPDU("HR", vkmArchLenReg, "int16")
	if err != nil {
		return "", err
	}
	lenResp, err := tx.Transact(ctx, lenPDU)
	if err != nil {
		return "", fmt.Errorf("read archive length: %w", err)
	}
	if len(lenResp) < 4 {
		return "", fmt.Errorf("length response too short")
	}
	strLen := int(binary.BigEndian.Uint16(lenResp[2:4]))
	if strLen <= 0 {
		return "", fmt.Errorf("invalid archive string length: %d", strLen)
	}

	maxRegs := vkmArchDataEnd - vkmArchDataStart + 1
	neededRegs := (strLen + 1) / 2
	if neededRegs > maxRegs {
		return "", fmt.Errorf("archive string length %d exceeds available register range", strLen)
	}

	// Try the whole payload in one read first — this is the fast path and
	// matches every device that doesn't impose a tighter per-request cap.
	//
	// If it fails, fall back to adaptive chunked reads instead of giving
	// up outright. CONFIRMED against real hardware (2026-08-01): reading
	// ANY quantity (tested up to 100 registers) at this address succeeds
	// when no request is bound (status = no records / expired), but a
	// one-shot read of the exact computed size failed with Modbus
	// exception 0x03 (illegal VALUE) while a real result WAS bound
	// (status = ready). The device likely enforces something tighter than
	// "any quantity" specifically while serving an active/bound result —
	// exactly what, isn't pinned down yet (the ~300s result-cache window
	// expired before it could be isolated further; see
	// tools/vkmprobe --probe-archive-read). Chunking is the robust fix
	// regardless of the precise limit: it degrades gracefully to whatever
	// size actually works instead of failing the whole read.
	payload, err := readRegistersChunked(ctx, tx, vkmArchDataStart, neededRegs)
	if err != nil {
		return "", fmt.Errorf("read archive string: %w", err)
	}
	if len(payload) > strLen {
		payload = payload[:strLen]
	}
	// cp1251, not raw UTF-8 — CONFIRMED against real hardware (2026-08-01):
	// naively treating these bytes as UTF-8 produced unreadable tag headers
	// and units ("?????"), the classic signature of ANSI/cp1251 content
	// misread as UTF-8. See cp1251.go's doc comment.
	return cp1251Decode(payload), nil
}

// modbusMaxRegistersPerRead is the Modbus protocol's hard ceiling for a
// function-03/04 read (255-byte PDU limit / 2 bytes per register, minus
// the 2-byte function+count header — the spec caps it at 125 regardless
// of any device-specific behaviour). Requesting more than this is
// guaranteed to fail on ANY compliant device, not just this one — CONFIRMED
// live 2026-08-01: a real archive string needed 235 registers (469 bytes),
// so the naive "read it all in one shot" fast path was a wasted,
// always-failing round-trip on every single read, not just an occasional
// edge case.
const modbusMaxRegistersPerRead = 125

// readRegistersChunked reads `count` HR registers starting at `addr`,
// falling back to progressively smaller chunks if a read fails. Returns
// the concatenated raw bytes (2 bytes per register, so len(result) ==
// count*2 on success).
func readRegistersChunked(ctx context.Context, tx Transactor, addr, count int) ([]byte, error) {
	// Fast path: try it all in one read — but only when that's even
	// legal per the Modbus spec. Above modbusMaxRegistersPerRead, skip
	// straight to chunking instead of burning a round-trip on a request
	// no compliant device could ever satisfy.
	if count <= modbusMaxRegistersPerRead {
		if data, err := readRegisterBlock(ctx, tx, addr, count); err == nil {
			return data, nil
		}
	}

	// Fall back to shrinking chunk sizes. The largest tier is capped at
	// the Modbus max (125) rather than some arbitrary smaller number, so
	// long strings still complete in as few round-trips as the protocol
	// allows; smaller tiers below that are for whatever tighter limit
	// this specific device/connection turns out to enforce (CONFIRMED
	// live: a real result read failed at the device's own computed size
	// while status was "ready", cause not fully pinned down yet — see
	// readResultString's doc comment).
	var result []byte
	remaining := count
	pos := addr
	for _, chunkSize := range []int{modbusMaxRegistersPerRead, 20, 10, 5, 2, 1} {
		for remaining > 0 {
			n := chunkSize
			if n > remaining {
				n = remaining
			}
			data, err := readRegisterBlock(ctx, tx, pos, n)
			if err != nil {
				if chunkSize == 1 {
					return nil, fmt.Errorf("chunked read failed even at 1 register (addr=%d): %w", pos, err)
				}
				break // try a smaller chunk size from the current position
			}
			result = append(result, data...)
			pos += n
			remaining -= n
		}
		if remaining == 0 {
			return result, nil
		}
	}
	return nil, fmt.Errorf("chunked read: unreachable state (remaining=%d)", remaining)
}

// readRegisterBlock issues one HR read for `count` registers at `addr` and
// returns the raw payload bytes (response minus function code/byte count).
func readRegisterBlock(ctx context.Context, tx Transactor, addr, count int) ([]byte, error) {
	pdu, err := modbus.BuildReadPDUWithQty("HR", addr, uint16(count))
	if err != nil {
		return nil, err
	}
	resp, err := tx.Transact(ctx, pdu)
	if err != nil {
		return nil, err
	}
	if len(resp) < 2 {
		return nil, fmt.Errorf("response too short")
	}
	return resp[2:], nil
}

// numericPrefixRe matches a leading number (optional sign, decimal point,
// scientific-notation exponent) at the start of a string — used to split
// "4.2229e+05<unit>" (value and unit concatenated with no separator, the
// real VKM-360 wire format) into its numeric and unit parts.
var numericPrefixRe = regexp.MustCompile(`^[+-]?\d+(\.\d+)?([eE][+-]?\d+)?`)

// parseTaggedString parses the VKM archive string format.
//
// Two header placements are supported, tried in this order:
//
//  1. тег=<шапка>значениеЕДИНИЦА;  or  тег={шапка}значениеЕДИНИЦА;  —
//     CONFIRMED against real hardware (2026-08-01): the header sits AFTER
//     '=', and the value/unit are concatenated with NO separating space
//     (e.g. "Pi=<Изб.давление *>4.2229e+05кг/ч;"). The bracket STYLE is
//     inconsistent across tags in the SAME response: Pi/Pbar/T/dP used
//     angle brackets, while S/ST/H/Time/Twrk/Tnss/NSS/S_ns/ST_ns — the
//     very fields the hourly integration depends on — used curly braces.
//     A version that only stripped '<...>' left every curly-header tag
//     unparsed for a full 24-hour live backfill (0 hours saved) before
//     this was caught; stripHeaderBlock now handles both. A
//     numeric-prefix regex splits the value from the trailing unit text.
//  2. тег{шапка}=значение ед.изм;  — the format originally assumed from
//     CONTRACTS.md's spec text (header BEFORE '=' in curly braces, value
//     and unit space-separated). Kept for compatibility in case a
//     different firmware/tag uses this shape; the fast-path branch below
//     only fires when a bracket-free value actually contains a space.
//
// Non-numeric fields (e.g. "Time", a date RANGE like
// "01/08/26 14:32:00-01/08/26 15:31:00", not a single value) are kept as
// the full trimmed string rather than forced through the numeric split —
// fixes a latent bug where the fallback stored the (possibly wrongly
// truncated by the space-split) valueStr instead of the untouched
// valuePart.
func parseTaggedString(raw string) ([]ArchiveRecord, error) {
	raw = strings.TrimRight(raw, "\x00")
	entries := strings.Split(raw, ";")

	fields := make(map[string]any)
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		eq := strings.Index(entry, "=")
		if eq < 0 {
			continue
		}
		tagPart := entry[:eq]
		valuePart := strings.TrimSpace(entry[eq+1:])

		tag := tagPart
		if br := strings.Index(tagPart, "{"); br >= 0 {
			tag = strings.TrimSpace(tagPart[:br])
		}

		// Real format: header AFTER '=' in <...> OR {...}. Strip it
		// before splitting value/unit. No-op if valuePart doesn't start
		// with either bracket (format 2 with the header already stripped
		// via tagPart's '{', or a tag with no header at all).
		valuePart = stripHeaderBlock(valuePart)

		valueStr := valuePart
		unit := ""
		switch {
		case strings.Contains(valuePart, " "):
			// Format 2: "value unit" space-separated.
			sp := strings.LastIndex(valuePart, " ")
			valueStr = valuePart[:sp]
			unit = strings.TrimSpace(valuePart[sp+1:])
		default:
			// Format 1: "valueunit" concatenated, no space — extract the
			// leading numeric token, the rest (if any) is the unit.
			if loc := numericPrefixRe.FindStringIndex(valuePart); loc != nil {
				valueStr = valuePart[:loc[1]]
				unit = strings.TrimSpace(valuePart[loc[1]:])
			}
		}

		if f, err := strconv.ParseFloat(strings.TrimSpace(valueStr), 64); err == nil {
			fields[tag] = f
			if unit != "" {
				fields[tag+"_unit"] = unit
			}
		} else {
			// Non-numeric (e.g. Time's date range) — keep the full,
			// untouched value text, not the (possibly mis-split) valueStr.
			fields[tag] = valuePart
		}
	}

	rec := ArchiveRecord{
		RecordTS: time.Now().UTC(),
		Fields:   fields,
		CRCOK:    true,
		Raw:      []byte(raw),
	}
	return []ArchiveRecord{rec}, nil
}

// stripHeaderBlock removes a leading "<...>" or "{...}" block (the real
// device's header/description placement — CONFIRMED live to use BOTH
// styles inconsistently across different tags in the same response
// string) and returns the trimmed remainder. If valuePart doesn't start
// with either opening bracket, or has no matching closing bracket, it's
// returned unchanged.
func stripHeaderBlock(valuePart string) string {
	var closer byte
	switch {
	case strings.HasPrefix(valuePart, "<"):
		closer = '>'
	case strings.HasPrefix(valuePart, "{"):
		closer = '}'
	default:
		return valuePart
	}
	if end := strings.IndexByte(valuePart, closer); end >= 0 {
		return strings.TrimSpace(valuePart[end+1:])
	}
	return valuePart
}

// isBusyErr reports whether err represents a Modbus exception 06 (SLAVE DEVICE BUSY).
// Delegates to modbus.IsBusyError, which matches via errors.As on *modbus.ExceptionError
// (see internal/protocol/modbus/exception.go) instead of string matching.
func isBusyErr(err error) bool {
	return modbus.IsBusyError(err)
}

func init() { Register(NewMBRequestPollString()) }
