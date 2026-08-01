package archive

import (
	"context"
	"fmt"
	"testing"
	"time"

	"mbgw/internal/protocol/modbus"
)

// sequentialTransactor returns pre-programmed responses in call order,
// regardless of request content. Sufficient for testing the happy-path
// sequencing of a multi-step archive strategy like mb_request_poll_string.
type sequentialTransactor struct {
	responses [][]byte
	errs      []error
	calls     int
	requests  [][]byte
}

func (s *sequentialTransactor) Transact(ctx context.Context, req []byte) ([]byte, error) {
	s.requests = append(s.requests, append([]byte(nil), req...))
	idx := s.calls
	s.calls++

	var err error
	if idx < len(s.errs) {
		err = s.errs[idx]
	}
	if err != nil {
		return nil, err
	}
	if idx >= len(s.responses) {
		return nil, context.DeadlineExceeded
	}
	return s.responses[idx], nil
}

// echoWritePDU builds a fake PDU response for a write request, mimicking
// what a real device (and client.ModbusClient.wrapAndSend) would return:
// function code + echoed data, exception bit clear.
func echoWriteSingleResp() []byte {
	return []byte{0x06, 0x00, 0xC8, 0x00, 0x01}
}

func echoWriteMultipleResp(addr int, qty int) []byte {
	return []byte{0x10, byte(addr >> 8), byte(addr), byte(qty >> 8), byte(qty)}
}

func statusResp(status uint16) []byte {
	return []byte{0x04, 0x02, byte(status >> 8), byte(status)}
}

func lenResp(n uint16) []byte {
	return []byte{0x04, 0x02, byte(n >> 8), byte(n)}
}

func dataResp(payload []byte) []byte {
	return append([]byte{0x04, byte(len(payload))}, payload...)
}

// cp1251EncodeForTest builds cp1251-encoded fixture bytes for test payload
// strings containing Cyrillic — mirroring cp1251Decode's table in reverse,
// so tests exercise the same encoding readResultString now assumes on the
// wire (CONFIRMED against real hardware, see cp1251.go). Test-local rather
// than importing internal/northbound's cp1251Encode, to keep this package's
// tests self-contained.
func cp1251EncodeForTest(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r < 0x80:
			out = append(out, byte(r))
		case r == 'Ё':
			out = append(out, 0xA8)
		case r == 'ё':
			out = append(out, 0xB8)
		case r == '°':
			out = append(out, 0xB0)
		case r >= 'А' && r <= 'Я':
			out = append(out, byte(r-'А'+0xC0))
		case r >= 'а' && r <= 'я':
			out = append(out, byte(r-'а'+0xE0))
		default:
			out = append(out, '?')
		}
	}
	return out
}

func TestMBRequestPollString_HappyPath(t *testing.T) {
	payload := "V01{асход}=123.45 кг/с;V02{асса}=678.90 кг;"

	tx := &sequentialTransactor{
		responses: [][]byte{
			echoWriteSingleResp(),                              // write id (7900)
			echoWriteSingleResp(),                              // write pipe (7901)
			echoWriteMultipleResp(7902, 6),                     // write start time
			echoWriteMultipleResp(7908, 6),                     // write end time
			echoWriteSingleResp(),                              // write options (7914)
			statusResp(vkmArchStatusCollecting),                // poll status: collecting
			statusResp(vkmArchStatusReady),                     // poll status: ready
			lenResp(uint16(len(cp1251EncodeForTest(payload)))), // read length (cp1251 byte length, not UTF-8)
			dataResp(cp1251EncodeForTest(payload)),             // read string (cp1251 on the wire)
		},
	}

	r := NewMBRequestPollString()
	q := ArchiveQuery{
		DeviceID:  "vkm_1",
		ArchiveID: "main",
		Instance:  1,
		From:      time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC),
	}

	recs, err := r.Read(context.Background(), nil, tx, q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}

	fields := recs[0].Fields
	if v, ok := fields["V01"].(float64); !ok || v != 123.45 {
		t.Fatalf("expected V01=123.45, got %v", fields["V01"])
	}
	if u, ok := fields["V01_unit"].(string); !ok || u != "кг/с" {
		t.Fatalf("expected V01_unit='кг/с', got %v", fields["V01_unit"])
	}
	if v, ok := fields["V02"].(float64); !ok || v != 678.90 {
		t.Fatalf("expected V02=678.90, got %v", fields["V02"])
	}

	if tx.calls != 9 {
		t.Fatalf("expected exactly 9 transactions, got %d", tx.calls)
	}

	// Regression for the 2026-08-01 incident: status/length/data-string
	// reads must use HR (function 0x03), not IR (0x04) — confirmed
	// against real hardware, every archive request failed at the very
	// first status poll before this fix. Indices: 0-4 are the five
	// writes, 5-6 are the two status polls, 7 is the length read, 8 is
	// the data-string read.
	hrReadIndices := map[int]string{5: "status poll (collecting)", 6: "status poll (ready)", 7: "length read", 8: "data-string read"}
	for idx, label := range hrReadIndices {
		if idx >= len(tx.requests) {
			t.Fatalf("missing request %d (%s)", idx, label)
		}
		if got := tx.requests[idx][0]; got != 0x03 {
			t.Fatalf("%s: expected function code 0x03 (HR), got 0x%02X", label, got)
		}
	}
}

func TestMBRequestPollString_BusyThenSucceeds(t *testing.T) {
	payload := "V01{асход}=1.00 кг/с;"

	tx := &sequentialTransactor{
		responses: [][]byte{
			nil, // idx0: forced error below
		},
		errs: []error{
			&modbus.ExceptionError{Code: 6}, // first write attempt: BUSY
		},
	}
	// After the forced busy error at call 0, subsequent calls succeed normally.
	tx.responses = [][]byte{
		nil,                            // idx0 unused (err forced)
		echoWriteSingleResp(),          // idx1: retry of write id succeeds
		echoWriteSingleResp(),          // write pipe
		echoWriteMultipleResp(7902, 6), // write start time
		echoWriteMultipleResp(7908, 6), // write end time
		echoWriteSingleResp(),          // write options
		statusResp(vkmArchStatusReady), // poll status: ready immediately
		lenResp(uint16(len(cp1251EncodeForTest(payload)))), // read length (cp1251 byte length, not UTF-8)
		dataResp(cp1251EncodeForTest(payload)),             // read string (cp1251 on the wire)
	}

	r := NewMBRequestPollString()
	q := ArchiveQuery{Instance: 1, From: time.Now(), To: time.Now()}

	recs, err := r.Read(context.Background(), nil, tx, q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
}

func TestMBRequestPollString_NoRecords(t *testing.T) {
	tx := &sequentialTransactor{
		responses: [][]byte{
			echoWriteSingleResp(),
			echoWriteSingleResp(),
			echoWriteMultipleResp(7902, 6),
			echoWriteMultipleResp(7908, 6),
			echoWriteSingleResp(),
			statusResp(vkmArchStatusNoRecords),
		},
	}

	r := NewMBRequestPollString()
	q := ArchiveQuery{Instance: 1, From: time.Now(), To: time.Now()}

	_, err := r.Read(context.Background(), nil, tx, q)
	if err == nil {
		t.Fatal("expected error for no_records status, got nil")
	}
}

// TestMBRequestPollString_ChunkedReadFallback is a regression test for the
// 2026-08-01 incident: a one-shot read of the full result string can fail
// with Modbus exception 0x03 (illegal value) while the device's status
// genuinely reports "ready" — confirmed against real hardware, cause not
// fully pinned down (see readRegistersChunked's doc comment), but reading
// in smaller chunks is the robust fallback regardless of the precise
// limit. This simulates a device that rejects any 7-register read (the
// one-shot attempt, then the chunkSize=20 and chunkSize=10 fallback tiers
// — all of which try the same 7 registers at once since 7 < both those
// tier sizes) but accepts 5-register reads, forcing a real split into two
// sub-reads (5 + 2) at the chunkSize=5 tier.
func TestMBRequestPollString_ChunkedReadFallback(t *testing.T) {
	payload := "V01{X}=1.5 m;"                 // 13 ASCII bytes (odd -> needs 7 registers/14 wire bytes)
	wireBytes := append([]byte(payload), 0x00) // pad to the even register boundary; readResultString truncates back to strLen

	tx := &sequentialTransactor{
		responses: [][]byte{
			echoWriteSingleResp(),          // 0: write id (7900)
			echoWriteSingleResp(),          // 1: write pipe (7901)
			echoWriteMultipleResp(7902, 6), // 2: write start time
			echoWriteMultipleResp(7908, 6), // 3: write end time
			echoWriteSingleResp(),          // 4: write options (7914)
			statusResp(vkmArchStatusReady), // 5: poll status: ready
			lenResp(uint16(len(payload))),  // 6: read length
			nil,                            // 7: fast-path one-shot (7 regs) — FAILS
			nil,                            // 8: chunk tier 125 (still 7 regs, remaining<125) — FAILS
			nil,                            // 9: chunk tier 20 (still 7 regs) — FAILS
			nil,                            // 10: chunk tier 10 (still 7 regs) — FAILS
			dataResp(wireBytes[0:10]),      // 11: chunk tier 5, first sub-read (5 regs = 10 bytes) — succeeds
			dataResp(wireBytes[10:14]),     // 12: chunk tier 5, second sub-read (2 regs = 4 bytes) — succeeds
		},
		errs: []error{
			nil, nil, nil, nil, nil, nil, nil,
			fmt.Errorf("modbus exception: code 0x03"),
			fmt.Errorf("modbus exception: code 0x03"),
			fmt.Errorf("modbus exception: code 0x03"),
			fmt.Errorf("modbus exception: code 0x03"),
			nil, nil,
		},
	}

	r := NewMBRequestPollString()
	q := ArchiveQuery{
		DeviceID:  "vkm_1",
		ArchiveID: "main",
		Instance:  1,
		From:      time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC),
	}

	recs, err := r.Read(context.Background(), nil, tx, q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if v, ok := recs[0].Fields["V01"].(float64); !ok || v != 1.5 {
		t.Fatalf("expected V01=1.5 (reconstructed from chunked reads), got %v", recs[0].Fields["V01"])
	}
	if tx.calls != 13 {
		t.Fatalf("expected exactly 13 transactions (7 setup + 4 failed attempts + 2 successful chunks), got %d", tx.calls)
	}
}

// TestParseTaggedString_RealDeviceFormat locks in the real wire format
// confirmed live against hardware (2026-08-01): header AFTER '=' in
// <...>, value and unit concatenated with NO space, and non-numeric
// fields (like a date range) kept whole rather than mangled by a wrong
// value/unit split.
func TestParseTaggedString_RealDeviceFormat(t *testing.T) {
	raw := "Time=<Диапазон>01/08/26 14:32:00-01/08/26 15:31:00;" +
		"Pi=<Расход. Массовый *>4.2229e+05кг/ч;" +
		"T=<Температура *>202.9°C;"

	recs, err := parseTaggedString(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	fields := recs[0].Fields

	// Numeric field with concatenated unit (the real format).
	if v, ok := fields["Pi"].(float64); !ok || v != 4.2229e+05 {
		t.Fatalf("expected Pi=4.2229e+05, got %v (%T)", fields["Pi"], fields["Pi"])
	}
	if u, ok := fields["Pi_unit"].(string); !ok || u != "кг/ч" {
		t.Fatalf("expected Pi_unit='кг/ч', got %v", fields["Pi_unit"])
	}

	if v, ok := fields["T"].(float64); !ok || v != 202.9 {
		t.Fatalf("expected T=202.9, got %v", fields["T"])
	}
	if u, ok := fields["T_unit"].(string); !ok || u != "°C" {
		t.Fatalf("expected T_unit='°C', got %v", fields["T_unit"])
	}

	// Non-numeric field (date range) must survive whole, not be mangled
	// by a wrong space-based value/unit split.
	want := "01/08/26 14:32:00-01/08/26 15:31:00"
	if got, ok := fields["Time"].(string); !ok || got != want {
		t.Fatalf("expected Time=%q, got %v", want, fields["Time"])
	}
}

// TestParseTaggedString_OldAssumedFormat confirms the originally-assumed
// format ({header} before '=', "value unit" space-separated) still parses
// correctly — kept for compatibility, not just the newly-confirmed format.
func TestParseTaggedString_OldAssumedFormat(t *testing.T) {
	raw := "V01{асход}=123.45 кг/с;"
	recs, err := parseTaggedString(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fields := recs[0].Fields
	if v, ok := fields["V01"].(float64); !ok || v != 123.45 {
		t.Fatalf("expected V01=123.45, got %v", fields["V01"])
	}
	if u, ok := fields["V01_unit"].(string); !ok || u != "кг/с" {
		t.Fatalf("expected V01_unit='кг/с', got %v", fields["V01_unit"])
	}
}

// TestReadRegistersChunked_SkipsOneShotAboveModbusMax confirms a read
// request larger than the Modbus protocol's 125-register ceiling never
// attempts a doomed one-shot read — it goes straight to chunking. Without
// this, every real archive result needing more than 125 registers (a
// realistic size — the first live result needed 235) would waste a
// guaranteed-failing round-trip on every single read.
func TestReadRegistersChunked_SkipsOneShotAboveModbusMax(t *testing.T) {
	count := 200 // > modbusMaxRegistersPerRead (125)
	want := make([]byte, count*2)
	for i := range want {
		want[i] = byte(i)
	}

	tx := &sequentialTransactor{
		responses: [][]byte{
			dataResp(want[0:250]),   // tier 125 (125 regs = 250 bytes)
			dataResp(want[250:400]), // tier 125 remainder (75 regs = 150 bytes)
		},
	}

	got, err := readRegistersChunked(context.Background(), tx, 8003, count)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tx.requests) != 2 {
		t.Fatalf("expected exactly 2 requests (no wasted one-shot attempt above the Modbus max), got %d", len(tx.requests))
	}
	if string(got) != string(want) {
		t.Fatalf("reconstructed payload mismatch")
	}
}

// TestParseTaggedString_MixedBracketStyles is a regression test for the
// 2026-08-01 incident: a real 24-hour VKM backfill saved ZERO hours
// because S and ST — the two fields the whole hourly integration depends
// on — use {...} for their header while Pi/Pbar/T/dP use <...>, all in
// the SAME response string. An angle-only stripper left S/ST as
// unparsed raw text forever.
func TestParseTaggedString_MixedBracketStyles(t *testing.T) {
	// Trimmed-down real fragment (2026-08-01, tools/mbgw.log): S and ST
	// use curly braces, Pi uses angle brackets, in the same string.
	raw := "Pi=<Избыт. давление *>4.2097e+05Па;" +
		"S={Масса теплонос. }2049.8782кг;" +
		"ST={Тепловая энергия }5.888087e+09Дж;" +
		"Time={Время  }31/07/26 16:00:00-31/07/26 17:00:00;" +
		"Twrk={Время штатной работы}1ч00м00с;"

	recs, err := parseTaggedString(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fields := recs[0].Fields

	if v, ok := fields["Pi"].(float64); !ok || v != 4.2097e+05 {
		t.Fatalf("expected Pi=4.2097e+05 (angle-bracket header), got %v", fields["Pi"])
	}
	if v, ok := fields["S"].(float64); !ok || v != 2049.8782 {
		t.Fatalf("expected S=2049.8782 (curly-brace header) — the exact field the hourly integration depends on, got %v (%T)", fields["S"], fields["S"])
	}
	if u, ok := fields["S_unit"].(string); !ok || u != "кг" {
		t.Fatalf("expected S_unit='кг', got %v", fields["S_unit"])
	}
	if v, ok := fields["ST"].(float64); !ok || v != 5.888087e+09 {
		t.Fatalf("expected ST=5.888087e+09 (curly-brace header), got %v", fields["ST"])
	}
	// Time and Twrk are non-numeric (date range / duration text) — must
	// survive as readable strings, not raw unparsed "{header}value" junk.
	if got, ok := fields["Time"].(string); !ok || got != "31/07/26 16:00:00-31/07/26 17:00:00" {
		t.Fatalf("expected Time to be the plain date range, got %v", fields["Time"])
	}
}
