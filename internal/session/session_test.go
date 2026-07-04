package session

import (
    "bytes"
    "context"
    "testing"
    "time"

    "mbgw/internal/protocol/modbus"
    "mbgw/internal/transport"
)

type stubTransport struct {
    responses [][]byte
    requests  [][]byte
    readErr   error
}

func (s *stubTransport) Open(ctx context.Context) error { return nil }

func (s *stubTransport) Send(ctx context.Context, frame []byte) error {
    s.requests = append(s.requests, append([]byte(nil), frame...))
    return nil
}

func (s *stubTransport) Receive(ctx context.Context, timeout time.Duration) ([]byte, error) {
    if s.readErr != nil {
        return nil, s.readErr
    }
    if len(s.responses) == 0 {
        return nil, context.DeadlineExceeded
    }
    resp := s.responses[0]
    s.responses = s.responses[1:]
    return append([]byte(nil), resp...), nil
}

func (s *stubTransport) Close() error {
    return nil
}

func (s *stubTransport) Info() transport.Params {
    return transport.Params{Retries: 1}
}

func newVKMTransportForOrder(order string) *stubTransport {
	var raw110 []byte
	var raw112 []byte
	var raw114 []byte

	switch order {
	case "0123":
		raw110 = []byte{0x49, 0x96, 0x02, 0xD2}
		raw112 = []byte{0x42, 0xF6, 0xE9, 0xD5}
		raw114 = []byte{0x40, 0x5E, 0xDD, 0x3C, 0x07, 0xFB, 0x4C, 0x93}
	case "1032":
		raw110 = []byte{0x96, 0x49, 0xD2, 0x02}
		raw112 = []byte{0xF6, 0x42, 0xD5, 0xE9}
		raw114 = []byte{0x5E, 0x40, 0x3C, 0xDD, 0xFB, 0x07, 0x93, 0x4C}
	case "2301":
		raw110 = []byte{0x02, 0xD2, 0x49, 0x96}
		raw112 = []byte{0xE9, 0xD5, 0x42, 0xF6}
		raw114 = []byte{0xDD, 0x3C, 0x40, 0x5E, 0x4C, 0x93, 0x07, 0xFB}
	case "3210":
		raw110 = []byte{0xD2, 0x02, 0x96, 0x49}
		raw112 = []byte{0xD5, 0xE9, 0xF6, 0x42}
		raw114 = []byte{0x93, 0x4C, 0xFB, 0x07, 0x3C, 0xDD, 0x5E, 0x40}
	default:
		raw110 = []byte{0x49, 0x96, 0x02, 0xD2}
		raw112 = []byte{0x42, 0xF6, 0xE9, 0xD5}
		raw114 = []byte{0x40, 0x5E, 0xDD, 0x3C, 0x07, 0xFB, 0x4C, 0x93}
	}

	return &stubTransport{
		responses: [][]byte{
			modbus.BuildTCPFrame(1, 1, append([]byte{0x04, 0x04}, raw110...)),
			modbus.BuildTCPFrame(2, 1, append([]byte{0x04, 0x04}, raw112...)),
			modbus.BuildTCPFrame(3, 1, append([]byte{0x04, 0x08}, raw114...)),
		},
	}
}

func newVKMHappyTransport() *stubTransport {
	return newVKMTransportForOrder("0123")
}

func newVKMHappyTransportWithAuth() *stubTransport {
	return &stubTransport{
		responses: [][]byte{
			modbus.BuildTCPFrame(1, 1, []byte{0x04, 0x04, 0x49, 0x96, 0x02, 0xD2}),
			modbus.BuildTCPFrame(2, 1, []byte{0x04, 0x04, 0x42, 0xF6, 0xE9, 0xD5}),
			modbus.BuildTCPFrame(3, 1, []byte{0x04, 0x08, 0x40, 0x5E, 0xDD, 0x3C, 0x07, 0xFB, 0x4C, 0x93}),
			modbus.BuildTCPFrame(4, 1, []byte{0x10, 0x00, 0xC8, 0x00, 0x02}),
		},
	}
}

func TestNewSession_Noop(t *testing.T) {
	sess, err := New("none")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := sess.Open(context.Background(), nil); err != nil {
		t.Fatalf("unexpected open error: %v", err)
	}

	if got := sess.State(); got != StateReady {
		t.Fatalf("expected StateReady, got %v", got)
	}
}

func TestNewSession_Unknown(t *testing.T) {
	_, err := New("magic_auth")
	if err == nil {
		t.Fatal("expected error for unknown session type, got nil")
	}
}

func TestNoopSession_Close(t *testing.T) {
	sess, err := New("none")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := sess.Open(context.Background(), nil); err != nil {
		t.Fatalf("unexpected open error: %v", err)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("unexpected close error: %v", err)
	}

	if got := sess.State(); got != StateClosed {
		t.Fatalf("expected StateClosed, got %v", got)
	}
}

func TestModbusByteOrderAuth_LifecycleSkeleton(t *testing.T) {
	sess, err := New("modbus_byteorder_auth")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := newVKMHappyTransportWithAuth()
	if err := sess.Open(context.Background(), tr); err != nil {
		t.Fatalf("unexpected open error: %v", err)
	}

	if got := sess.State(); got != StateReady {
		t.Fatalf("expected StateReady, got %v", got)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("unexpected close error: %v", err)
	}

	if got := sess.State(); got != StateClosed {
		t.Fatalf("expected StateClosed, got %v", got)
	}
}

func TestModbusByteOrderAuth_OpenSetsDetectedOrder(t *testing.T) {
	raw := NewModbusByteOrderAuth()
	tr := newVKMHappyTransportWithAuth()

	if err := raw.Open(context.Background(), tr); err != nil {
		t.Fatalf("unexpected open error: %v", err)
	}

	if raw.detectedOrder == "" {
		t.Fatal("expected detectedOrder to be populated during Open")
	}
	if raw.detectedOrder != "0123" {
		t.Fatalf("expected detectedOrder 0123, got %s", raw.detectedOrder)
	}
}

func TestModbusByteOrderAuth_VKMDetectionAnchors(t *testing.T) {
	if vkmControlRegister110 != 110 {
		t.Fatalf("unexpected VKM control register 110 value: %d", vkmControlRegister110)
	}
	if vkmControlRegister112 != 112 {
		t.Fatalf("unexpected VKM control register 112 value: %d", vkmControlRegister112)
	}
	if vkmControlRegister114 != 114 {
		t.Fatalf("unexpected VKM control register 114 value: %d", vkmControlRegister114)
	}

	want114 := []byte{0x40, 0x5E, 0xDD, 0x3C, 0x07, 0xFB, 0x4C, 0x93}
	if !bytes.Equal(vkmGolden114DoubleRaw, want114) {
		t.Fatalf("unexpected VKM double golden bytes: want %x, got %x", want114, vkmGolden114DoubleRaw)
	}
}

func TestModbusByteOrderAuth_ReadInputRegisters(t *testing.T) {
	st := &stubTransport{
		responses: [][]byte{
			modbus.BuildTCPFrame(1, 1, []byte{0x04, 0x04, 0x42, 0xF6, 0xE9, 0xD5}),
		},
	}

	sess := NewModbusByteOrderAuth()
	sess.tr = st

	got, err := sess.readInputRegisters(context.Background(), vkmControlRegister112, "float")
	if err != nil {
		t.Fatalf("unexpected readInputRegisters error: %v", err)
	}

	want := []byte{0x42, 0xF6, 0xE9, 0xD5}
	if !bytes.Equal(got, want) {
		t.Fatalf("unexpected payload: want %x, got %x", want, got)
	}

	if len(st.requests) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(st.requests))
	}

	wantReqPDU, err := modbus.BuildReadPDU("IR", vkmControlRegister112, "float")
	if err != nil {
		t.Fatalf("unexpected BuildReadPDU error: %v", err)
	}
	wantReq := modbus.BuildTCPFrame(1, 1, wantReqPDU)

	if !bytes.Equal(st.requests[0], wantReq) {
		t.Fatalf("unexpected request frame: want %x, got %x", wantReq, st.requests[0])
	}
}

func TestModbusByteOrderAuth_ReadInputRegisters_BadByteCount(t *testing.T) {
	st := &stubTransport{
		responses: [][]byte{
			modbus.BuildTCPFrame(1, 1, []byte{0x04, 0x04, 0x42, 0xF6}),
		},
	}

	sess := NewModbusByteOrderAuth()
	sess.tr = st

	_, err := sess.readInputRegisters(context.Background(), vkmControlRegister112, "float")
	if err == nil {
		t.Fatal("expected byte count mismatch error, got nil")
	}
}

func TestModbusByteOrderAuth_DetectByteOrder_StagedReads(t *testing.T) {
	st := newVKMHappyTransport()

	sess := NewModbusByteOrderAuth()
	sess.tr = st

	order, err := sess.detectByteOrder(context.Background())
	if err != nil {
		t.Fatalf("unexpected detectByteOrder error: %v", err)
	}
	if order != "0123" {
		t.Fatalf("expected staged detect result 0123, got %s", order)
	}

	if len(st.requests) != 3 {
		t.Fatalf("expected 3 staged requests, got %d", len(st.requests))
	}

	want1PDU, _ := modbus.BuildReadPDU("IR", vkmControlRegister110, "int32")
	want2PDU, _ := modbus.BuildReadPDU("IR", vkmControlRegister112, "float")
	want3PDU, _ := modbus.BuildReadPDU("IR", vkmControlRegister114, "double")

	want1 := modbus.BuildTCPFrame(1, 1, want1PDU)
	want2 := modbus.BuildTCPFrame(2, 1, want2PDU)
	want3 := modbus.BuildTCPFrame(3, 1, want3PDU)

	if !bytes.Equal(st.requests[0], want1) {
		t.Fatalf("unexpected first request: want %x, got %x", want1, st.requests[0])
	}
	if !bytes.Equal(st.requests[1], want2) {
		t.Fatalf("unexpected second request: want %x, got %x", want2, st.requests[1])
	}
	if !bytes.Equal(st.requests[2], want3) {
		t.Fatalf("unexpected third request: want %x, got %x", want3, st.requests[2])
	}
}

func TestModbusByteOrderAuth_DetectByteOrder_Selects1032(t *testing.T) {
	st := newVKMTransportForOrder("1032")

	sess := NewModbusByteOrderAuth()
	sess.tr = st

	order, err := sess.detectByteOrder(context.Background())
	if err != nil {
		t.Fatalf("unexpected detectByteOrder error: %v", err)
	}
	if order != "1032" {
		t.Fatalf("expected detected order 1032, got %s", order)
	}
}

func TestModbusByteOrderAuth_DetectByteOrder_NoMatch(t *testing.T) {
	st := &stubTransport{
		responses: [][]byte{
			modbus.BuildTCPFrame(1, 1, []byte{0x04, 0x04, 0x00, 0x00, 0x00, 0x00}),
			modbus.BuildTCPFrame(2, 1, []byte{0x04, 0x04, 0x00, 0x00, 0x00, 0x00}),
			modbus.BuildTCPFrame(3, 1, []byte{0x04, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}),
		},
	}

	sess := NewModbusByteOrderAuth()
	sess.tr = st

	_, err := sess.detectByteOrder(context.Background())
	if err == nil {
		t.Fatal("expected no-match detectByteOrder error, got nil")
	}
}

func TestModbusByteOrderAuth_WriteHoldingRegisters_Single(t *testing.T) {
	st := &stubTransport{
		responses: [][]byte{
			modbus.BuildTCPFrame(1, 1, []byte{0x06, 0x00, 0xC8, 0x00, 0x01}),
		},
	}

	sess := NewModbusByteOrderAuth()
	sess.tr = st

	err := sess.writeHoldingRegisters(context.Background(), vkmAuthRegister200, []uint16{1})
	if err != nil {
		t.Fatalf("unexpected writeHoldingRegisters(single) error: %v", err)
	}

	wantReq := modbus.BuildTCPFrame(1, 1, modbus.BuildWriteSingleRegisterPDU(vkmAuthRegister200, 1))
	if len(st.requests) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(st.requests))
	}
	if !bytes.Equal(st.requests[0], wantReq) {
		t.Fatalf("unexpected single-write request: want %x, got %x", wantReq, st.requests[0])
	}
}

func TestModbusByteOrderAuth_WriteHoldingRegisters_Multiple(t *testing.T) {
	st := &stubTransport{
		responses: [][]byte{
			modbus.BuildTCPFrame(1, 1, []byte{0x10, 0x00, 0xC8, 0x00, 0x02}),
		},
	}

	sess := NewModbusByteOrderAuth()
	sess.tr = st

	err := sess.writeHoldingRegisters(context.Background(), vkmAuthRegister200, []uint16{1, 0})
	if err != nil {
		t.Fatalf("unexpected writeHoldingRegisters(multiple) error: %v", err)
	}

	wantPDU, err := modbus.BuildWriteMultipleRegistersPDU(vkmAuthRegister200, []uint16{1, 0})
	if err != nil {
		t.Fatalf("unexpected BuildWriteMultipleRegistersPDU error: %v", err)
	}
	wantReq := modbus.BuildTCPFrame(1, 1, wantPDU)

	if len(st.requests) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(st.requests))
	}
	if !bytes.Equal(st.requests[0], wantReq) {
		t.Fatalf("unexpected multi-write request: want %x, got %x", wantReq, st.requests[0])
	}
}

func TestModbusByteOrderAuth_Authorize(t *testing.T) {
	st := &stubTransport{
		responses: [][]byte{
			modbus.BuildTCPFrame(1, 1, []byte{0x10, 0x00, 0xC8, 0x00, 0x02}),
		},
	}

	sess := NewModbusByteOrderAuth()
	sess.tr = st

	if err := sess.authorize(context.Background()); err != nil {
		t.Fatalf("unexpected authorize error: %v", err)
	}

	wantPDU, err := modbus.BuildWriteMultipleRegistersPDU(vkmAuthRegister200, []uint16{1, 0})
	if err != nil {
		t.Fatalf("unexpected BuildWriteMultipleRegistersPDU error: %v", err)
	}
	wantReq := modbus.BuildTCPFrame(1, 1, wantPDU)

	if len(st.requests) != 1 {
		t.Fatalf("expected exactly 1 authorize request, got %d", len(st.requests))
	}
	if !bytes.Equal(st.requests[0], wantReq) {
		t.Fatalf("unexpected authorize request: want %x, got %x", wantReq, st.requests[0])
	}
}

func TestModbusByteOrderAuth_Authorize_RetriesOnBusyThenSucceeds(t *testing.T) {
	st := &stubTransport{
		responses: [][]byte{
			modbus.BuildTCPFrame(1, 1, []byte{0x90, 0x06}),
			modbus.BuildTCPFrame(2, 1, []byte{0x90, 0x06}),
			modbus.BuildTCPFrame(3, 1, []byte{0x10, 0x00, 0xC8, 0x00, 0x02}),
		},
	}

	sess := NewModbusByteOrderAuth()
	sess.tr = st

	if err := sess.authorize(context.Background()); err != nil {
		t.Fatalf("unexpected authorize error after busy retries: %v", err)
	}

	if len(st.requests) != 3 {
		t.Fatalf("expected 3 authorize attempts, got %d", len(st.requests))
	}

	wantPDU, err := modbus.BuildWriteMultipleRegistersPDU(vkmAuthRegister200, []uint16{1, 0})
	if err != nil {
		t.Fatalf("unexpected BuildWriteMultipleRegistersPDU error: %v", err)
	}
	wantReq1 := modbus.BuildTCPFrame(1, 1, wantPDU)
	wantReq2 := modbus.BuildTCPFrame(2, 1, wantPDU)
	wantReq3 := modbus.BuildTCPFrame(3, 1, wantPDU)

	if !bytes.Equal(st.requests[0], wantReq1) {
		t.Fatalf("unexpected first authorize request: want %x, got %x", wantReq1, st.requests[0])
	}
	if !bytes.Equal(st.requests[1], wantReq2) {
		t.Fatalf("unexpected second authorize request: want %x, got %x", wantReq2, st.requests[1])
	}
	if !bytes.Equal(st.requests[2], wantReq3) {
		t.Fatalf("unexpected third authorize request: want %x, got %x", wantReq3, st.requests[2])
	}
}

func TestModbusByteOrderAuth_Authorize_BusyExhausted(t *testing.T) {
	st := &stubTransport{
		responses: [][]byte{
			modbus.BuildTCPFrame(1, 1, []byte{0x90, 0x06}),
			modbus.BuildTCPFrame(2, 1, []byte{0x90, 0x06}),
			modbus.BuildTCPFrame(3, 1, []byte{0x90, 0x06}),
		},
	}

	sess := NewModbusByteOrderAuth()
	sess.tr = st

	err := sess.authorize(context.Background())
	if err == nil {
		t.Fatal("expected authorize error after BUSY exhaustion, got nil")
	}

	if len(st.requests) != 3 {
		t.Fatalf("expected 3 authorize attempts, got %d", len(st.requests))
	}
}

func TestModbusByteOrderAuth_Authorize_NonBusyDoesNotRetry(t *testing.T) {
	st := &stubTransport{
		responses: [][]byte{
			modbus.BuildTCPFrame(1, 1, []byte{0x90, 0x02}),
		},
	}

	sess := NewModbusByteOrderAuth()
	sess.tr = st

	err := sess.authorize(context.Background())
	if err == nil {
		t.Fatal("expected authorize error on non-BUSY exception, got nil")
	}

	if len(st.requests) != 1 {
		t.Fatalf("expected 1 authorize attempt for non-BUSY exception, got %d", len(st.requests))
	}
}
