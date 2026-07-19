package northbound

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

func startVKMTestServer(t *testing.T, newSrc func() VKMArchiveSource) (addr string, cancel func()) {
	t.Helper()
	srv := NewVKMServer("127.0.0.1:0", newSrc)
	srv.ReadTimeout = 2 * time.Second

	ctx, cancelCtx := context.WithCancel(context.Background())
	ready := make(chan struct{})
	errCh := make(chan error, 1)

	go func() {
		go func() {
			for i := 0; i < 100; i++ {
				if srv.Addr() != nil {
					close(ready)
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
		errCh <- srv.Listen(ctx)
	}()

	select {
	case <-ready:
	case err := <-errCh:
		t.Fatalf("server exited before binding: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("server did not bind in time")
	}

	return srv.Addr().String(), func() {
		cancelCtx()
		_ = srv.Close()
	}
}

// TestVKMServer_EndToEnd_FullArchiveCycle drives the real MBAP wire
// protocol over an actual TCP connection: write the 7900-7914 request via
// FC06 (as mb_request_poll_string does), poll status, read length, read
// the data string — proving the transport (MBAP framing) and the state
// machine work together, not just the state machine in isolation.
func TestVKMServer_EndToEnd_FullArchiveCycle(t *testing.T) {
	const testString = "V01{Расход}=123.45 кг/с;V02{Масса}=678.90 кг;"
	addr, cancel := startVKMTestServer(t, func() VKMArchiveSource {
		return FixedVKMSource{Result: testString}
	})
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()

	writeReg := func(txID uint16, addrReg, val uint16) {
		req := make([]byte, 6)
		binary.BigEndian.PutUint16(req[0:2], addrReg)
		binary.BigEndian.PutUint16(req[2:4], val)
		frame := buildTCPWriteFrame(t, txID, 1, 0x06, req)
		if _, err := conn.Write(frame); err != nil {
			t.Fatalf("write reg %d: %v", addrReg, err)
		}
		_ = readResponse(t, conn) // discard echo
	}

	// Request: id=7, pipe=1, start 01.07.2026, end 02.07.2026, options last.
	regs := []struct{ addr, val uint16 }{
		{7900, 7}, {7901, 1},
		{7902, 1}, {7903, 7}, {7904, 2026}, {7905, 0}, {7906, 0}, {7907, 0},
		{7908, 2}, {7909, 7}, {7910, 2026}, {7911, 0}, {7912, 0}, {7913, 0},
		{7914, 32},
	}
	for i, r := range regs {
		writeReg(uint16(100+i), r.addr, r.val)
	}

	// Status poll 1: collecting.
	req := buildReadRequest(200, 1, 0x03, 8000, 1)
	conn.Write(req)
	resp := readResponse(t, conn)
	if status := binary.BigEndian.Uint16(resp[9:11]); status != vkmStatusCollecting {
		t.Fatalf("first status = %d, want 1", status)
	}

	// Status poll 2: ready.
	req = buildReadRequest(201, 1, 0x03, 8000, 1)
	conn.Write(req)
	resp = readResponse(t, conn)
	if status := binary.BigEndian.Uint16(resp[9:11]); status != vkmStatusReady {
		t.Fatalf("second status = %d, want 2", status)
	}

	// Length.
	req = buildReadRequest(202, 1, 0x03, 8002, 1)
	conn.Write(req)
	resp = readResponse(t, conn)
	wantLen := len([]byte(testString))
	if l := binary.BigEndian.Uint16(resp[9:11]); int(l) != wantLen {
		t.Fatalf("len = %d, want %d", l, wantLen)
	}

	// Data.
	qty := uint16((wantLen + 1) / 2)
	req = buildReadRequest(203, 1, 0x03, 8003, qty)
	conn.Write(req)
	resp = readResponse(t, conn)
	got := string(resp[9 : 9+wantLen])
	if got != testString {
		t.Fatalf("data = %q, want %q", got, testString)
	}
}

// TestVKMServer_TwoConnections_IndependentState proves the per-connection
// isolation promised in the doc comment: connection A's request must not
// leak into connection B's status/result.
func TestVKMServer_TwoConnections_IndependentState(t *testing.T) {
	addr, cancel := startVKMTestServer(t, func() VKMArchiveSource {
		return FixedVKMSource{Result: "X=1;"}
	})
	defer cancel()

	connA := dial(t, addr)
	defer connA.Close()
	connB := dial(t, addr)
	defer connB.Close()

	// A starts a request (writes options register).
	req := make([]byte, 6)
	binary.BigEndian.PutUint16(req[0:2], 7914)
	binary.BigEndian.PutUint16(req[2:4], 1)
	frameA := buildTCPWriteFrame(t, 1, 1, 0x06, req)
	connA.Write(frameA)
	_ = readResponse(t, connA)

	// B, which never wrote anything, must see IDLE (ready), not A's
	// collecting/ready sequence.
	reqB := buildReadRequest(1, 1, 0x03, 8000, 1)
	connB.Write(reqB)
	respB := readResponse(t, connB)
	statusB := binary.BigEndian.Uint16(respB[9:11])
	if statusB != vkmStatusReady {
		t.Fatalf("B's status leaked A's state: got %d, want %d (idle/ready)", statusB, vkmStatusReady)
	}
}

// buildTCPWriteFrame assembles a full MBAP frame for an FC06/FC16 PDU.
func buildTCPWriteFrame(t *testing.T, transID uint16, unitID, funcCode byte, pduData []byte) []byte {
	t.Helper()
	pdu := append([]byte{funcCode}, pduData...)
	frame := make([]byte, 7+len(pdu))
	binary.BigEndian.PutUint16(frame[0:2], transID)
	binary.BigEndian.PutUint16(frame[2:4], 0) // protocol id
	binary.BigEndian.PutUint16(frame[4:6], uint16(1+len(pdu)))
	frame[6] = unitID
	copy(frame[7:], pdu)
	return frame
}
