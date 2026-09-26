package modbus

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"mbgw/internal/transport"
)

// parseHex removes spaces from golden vector literals.
func parseHex(s string) []byte {
	s = strings.ReplaceAll(s, " ", "")
	b, _ := hex.DecodeString(s)
	return b
}

type stubTransport struct {
	resp    []byte
	err     error
	lastReq []byte
}

func (s *stubTransport) Open(ctx context.Context) error { return nil }

func (s *stubTransport) Send(ctx context.Context, frame []byte) error {
	s.lastReq = append([]byte(nil), frame...)
	return nil
}

func (s *stubTransport) Receive(ctx context.Context, timeout time.Duration) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return append([]byte(nil), s.resp...), nil
}

func (s *stubTransport) Close() error {
	return nil
}

func (s *stubTransport) Info() transport.Params {
	return transport.Params{Retries: 1}
}

func TestCRC16_Golden(t *testing.T) {
	pdu := parseHex("01 04 07 D0 00 02")
	crc := CRC16(pdu)

	lo := byte(crc & 0xFF)
	hi := byte(crc >> 8)
	got := []byte{lo, hi}
	want := parseHex("71 46")

	if !bytes.Equal(got, want) {
		t.Fatalf("unexpected CRC: want %x, got %x", want, got)
	}
}

func TestBuildTCPFrame(t *testing.T) {
	pdu := parseHex("04 04 42 F6 E9 D5")
	got := BuildTCPFrame(0x1234, 0x01, pdu)
	want := parseHex("12 34 00 00 00 07 01 04 04 42 F6 E9 D5")

	if !bytes.Equal(got, want) {
		t.Fatalf("unexpected TCP frame: want %x, got %x", want, got)
	}
}

func TestParseTCPFrame(t *testing.T) {
	frame := parseHex("12 34 00 00 00 07 01 04 04 42 F6 E9 D5")

	txID, unitID, pdu, err := ParseTCPFrame(frame)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if txID != 0x1234 {
		t.Fatalf("unexpected txID: want 0x1234, got 0x%04X", txID)
	}
	if unitID != 0x01 {
		t.Fatalf("unexpected unit id: want 1, got %d", unitID)
	}

	wantPDU := parseHex("04 04 42 F6 E9 D5")
	if !bytes.Equal(pdu, wantPDU) {
		t.Fatalf("unexpected PDU: want %x, got %x", wantPDU, pdu)
	}
}

func TestParseTCPFrame_BadProtocolID(t *testing.T) {
	frame := parseHex("12 34 00 01 00 07 01 04 04 42 F6 E9 D5")

	_, _, _, err := ParseTCPFrame(frame)
	if err == nil {
		t.Fatal("expected protocol id error, got nil")
	}
}

func TestParseTCPFrame_BadLength(t *testing.T) {
	frame := parseHex("12 34 00 00 00 08 01 04 04 42 F6 E9 D5")

	_, _, _, err := ParseTCPFrame(frame)
	if err == nil {
		t.Fatal("expected length mismatch error, got nil")
	}
}

func TestTransactTCP(t *testing.T) {
	reqPDU := parseHex("04 00 70 00 02")
	respPDU := parseHex("04 04 42 F6 E9 D5")

	t.Run("happy path", func(t *testing.T) {
		st := &stubTransport{
			resp: BuildTCPFrame(0x1234, 0x01, respPDU),
		}

		got, err := Transact(context.Background(), st, true, 0x1234, 0x01, reqPDU)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !bytes.Equal(got, respPDU) {
			t.Fatalf("unexpected PDU: want %x, got %x", respPDU, got)
		}

		wantReq := BuildTCPFrame(0x1234, 0x01, reqPDU)
		if !bytes.Equal(st.lastReq, wantReq) {
			t.Fatalf("unexpected request frame: want %x, got %x", wantReq, st.lastReq)
		}
	})

	t.Run("txID mismatch", func(t *testing.T) {
		st := &stubTransport{
			resp: BuildTCPFrame(0x9999, 0x01, respPDU),
		}

		_, err := Transact(context.Background(), st, true, 0x1234, 0x01, reqPDU)
		if err == nil {
			t.Fatal("expected txID mismatch error, got nil")
		}
	})

	t.Run("unitID mismatch", func(t *testing.T) {
		st := &stubTransport{
			resp: BuildTCPFrame(0x1234, 0x02, respPDU),
		}

		_, err := Transact(context.Background(), st, true, 0x1234, 0x01, reqPDU)
		if err == nil {
			t.Fatal("expected unitID mismatch error, got nil")
		}
	})

	t.Run("exception busy typed", func(t *testing.T) {
		st := &stubTransport{
			resp: BuildTCPFrame(0x1234, 0x01, parseHex("84 06")),
		}

		_, err := Transact(context.Background(), st, true, 0x1234, 0x01, reqPDU)
		if err == nil {
			t.Fatal("expected modbus exception error, got nil")
		}

		exc, ok := err.(*ExceptionError)
		if !ok {
			t.Fatalf("expected *ExceptionError, got %T (%v)", err, err)
		}
		if exc.Code != ExceptionSlaveDeviceBusy {
			t.Fatalf("expected exception code 0x06, got 0x%02X", exc.Code)
		}
		if !IsBusyError(err) {
			t.Fatal("expected IsBusyError=true for BUSY exception")
		}
	})

	t.Run("exception non-busy typed", func(t *testing.T) {
		st := &stubTransport{
			resp: BuildTCPFrame(0x1234, 0x01, parseHex("84 02")),
		}

		_, err := Transact(context.Background(), st, true, 0x1234, 0x01, reqPDU)
		if err == nil {
			t.Fatal("expected modbus exception error, got nil")
		}

		exc, ok := err.(*ExceptionError)
		if !ok {
			t.Fatalf("expected *ExceptionError, got %T (%v)", err, err)
		}
		if exc.Code != ExceptionIllegalDataAddress {
			t.Fatalf("expected exception code 0x02, got 0x%02X", exc.Code)
		}
		if IsBusyError(err) {
			t.Fatal("expected IsBusyError=false for non-BUSY exception")
		}
	})
}

func TestBuildRTUFrame_Golden(t *testing.T) {
	address := uint8(0x01)
	pdu := parseHex("04 04 42 F6 E9 D5")
	expectedFrame := parseHex("01 04 04 42 F6 E9 D5 81 C1")

	frame := BuildRTUFrame(address, pdu)
	if !bytes.Equal(frame, expectedFrame) {
		t.Fatalf("unexpected RTU frame: want %x, got %x", expectedFrame, frame)
	}
}

func TestParseRTUFrame_Golden(t *testing.T) {
	expectedFrame := parseHex("01 04 04 42 F6 E9 D5 81 C1")
	expectedPDU := parseHex("04 04 42 F6 E9 D5")

	addr, pdu, err := ParseRTUFrame(expectedFrame)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if addr != 0x01 {
		t.Fatalf("unexpected address: want 1, got %d", addr)
	}
	if !bytes.Equal(pdu, expectedPDU) {
		t.Fatalf("unexpected PDU: want %x, got %x", expectedPDU, pdu)
	}
}

func TestParseRTUFrame_BadCRC(t *testing.T) {
	badCRCFrame := parseHex("01 04 04 42 F6 E9 D5 00 00")
	_, _, err := ParseRTUFrame(badCRCFrame)
	if err == nil {
		t.Fatal("expected CRC error, got nil")
	}
}

func TestIsException(t *testing.T) {
	pdu := parseHex("84 06")
	isExc, code := IsException(pdu)
	if !isExc {
		t.Fatal("expected exception=true")
	}
	if code != 0x06 {
		t.Fatalf("expected exception code 0x06, got 0x%02X", code)
	}
}

func TestBuildWriteMultipleRegistersPDU(t *testing.T) {
	pdu, err := BuildWriteMultipleRegistersPDU(200, []uint16{0x1234, 0x5678})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := parseHex("10 00 C8 00 02 04 12 34 56 78")
	if !bytes.Equal(pdu, want) {
		t.Fatalf("unexpected PDU: want %x, got %x", want, pdu)
	}
}

func TestBuildArchive65IndexPDU_Golden(t *testing.T) {
	// prtkl_Modbus_pril_1.pdf, Приложение А: запрос по индексу 6 записей
	// массива 1, начиная со 100-й, устройство 17.
	// Full frame incl. device addr+CRC: 11 41 00 01 00 06 00 00 64
	// (addr/CRC are framing, not part of the PDU) - PDU is: 41 00 01 00 06 00 00 64
	got := BuildArchive65IndexPDU(1, 6, 100)
	want := []byte{0x41, 0x00, 0x01, 0x00, 0x06, 0x00, 0x00, 0x64}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X, want % X", got, want)
	}
}

func TestBuildArchive65TimePDU_Golden(t *testing.T) {
	// prtkl_Modbus_pril_1.pdf: запрос по времени 6 записей массива 1 с
	// 10-12-1998 13:12:00, устройство 17.
	// PDU: 41 00 01 00 06 01 00 0C 0D 0A 0C 62
	tm := time.Date(1998, time.December, 10, 13, 12, 0, 0, time.UTC)
	got := BuildArchive65TimePDU(1, 6, tm)
	want := []byte{0x41, 0x00, 0x01, 0x00, 0x06, 0x01, 0x00, 0x0C, 0x0D, 0x0A, 0x0C, 0x62}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X, want % X", got, want)
	}
}

type queuedTransport struct {
	responses [][]byte
	lastReq   []byte
	flushed   int
}

func (q *queuedTransport) Open(context.Context) error { return nil }
func (q *queuedTransport) Close() error               { return nil }
func (q *queuedTransport) Info() transport.Params {
	return transport.Params{Retries: 1, ResponseTimeout: 50 * time.Millisecond}
}
func (q *queuedTransport) Send(_ context.Context, frame []byte) error {
	q.lastReq = append([]byte(nil), frame...)
	return nil
}
func (q *queuedTransport) Receive(_ context.Context, _ time.Duration) ([]byte, error) {
	if len(q.responses) == 0 {
		return nil, fmt.Errorf("test timeout")
	}
	f := q.responses[0]
	q.responses = q.responses[1:]
	return append([]byte(nil), f...), nil
}
func (q *queuedTransport) ResetInputBuffer() error { q.flushed++; return nil }

func TestTransactRTUDiscardsForeignAddressThenAcceptsOwn(t *testing.T) {
	req := BuildReadPDUWithQtyMust(t, "HR", 100, 2)
	foreign := BuildRTUFrame(1, []byte{0x03, 0x04, 0, 1, 0, 2})
	own := BuildRTUFrame(2, []byte{0x03, 0x04, 0, 3, 0, 4})
	tr := &queuedTransport{responses: [][]byte{foreign, own}}

	got, err := Transact(context.Background(), tr, false, 0, 2, req)
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
	if !bytes.Equal(got, []byte{0x03, 0x04, 0, 3, 0, 4}) {
		t.Fatalf("accepted PDU=%x", got)
	}
	if tr.flushed != 1 {
		t.Fatalf("input flushes=%d, want 1", tr.flushed)
	}
}

func TestTransactRTURejectsWrongReadByteCount(t *testing.T) {
	req := BuildReadPDUWithQtyMust(t, "HR", 100, 2)
	bad := BuildRTUFrame(2, []byte{0x03, 0x02, 0, 3})
	tr := &queuedTransport{responses: [][]byte{bad}}
	if _, err := Transact(context.Background(), tr, false, 0, 2, req); err == nil || !strings.Contains(err.Error(), "byte count mismatch") {
		t.Fatalf("err=%v, want byte count mismatch", err)
	}
}

func TestTransactRTUDiscardsWrongFunctionThenAcceptsOwn(t *testing.T) {
	req := BuildReadPDUWithQtyMust(t, "HR", 100, 2)
	wrongFunc := BuildRTUFrame(2, []byte{0x04, 0x04, 0, 1, 0, 2})
	own := BuildRTUFrame(2, []byte{0x03, 0x04, 0, 3, 0, 4})
	tr := &queuedTransport{responses: [][]byte{wrongFunc, own}}
	got, err := Transact(context.Background(), tr, false, 0, 2, req)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 0x03 {
		t.Fatalf("accepted wrong function: %x", got)
	}
}

func TestTransactRTUCustomFunctionBindsResponseFunction(t *testing.T) {
	req := []byte{0x41, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00}
	wrongFunc := BuildRTUFrame(2, []byte{0x65, 0x00})
	own := BuildRTUFrame(2, []byte{0x41, 0x00})
	tr := &queuedTransport{responses: [][]byte{wrongFunc, own}}
	got, err := Transact(context.Background(), tr, false, 0, 2, req)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte{0x41, 0x00}) {
		t.Fatalf("accepted PDU=%x, want custom function 0x41 response", got)
	}
}

func TestTransactRTUOnlyForeignFramesEndsWithError(t *testing.T) {
	req := BuildReadPDUWithQtyMust(t, "HR", 100, 2)
	foreign := BuildRTUFrame(1, []byte{0x03, 0x04, 0, 1, 0, 2})
	tr := &queuedTransport{responses: [][]byte{foreign}}
	if got, err := Transact(context.Background(), tr, false, 0, 2, req); err == nil || got != nil {
		t.Fatalf("got=%x err=%v, want no accepted response", got, err)
	}
}

func BuildReadPDUWithQtyMust(t *testing.T, space string, addr int, qty uint16) []byte {
	t.Helper()
	pdu, err := BuildReadPDUWithQty(space, addr, qty)
	if err != nil {
		t.Fatal(err)
	}
	return pdu
}
