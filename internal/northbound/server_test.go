package northbound

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"mbgw/internal/monitor"
	"mbgw/internal/storage"
)

// fakeRepo is a minimal in-memory storage.Repo for northbound tests. It
// does not need migrations/InitSchema semantics — only the two read/write
// paths the northbound server actually calls.
type fakeRepo struct {
	readings []storage.ReadingCurrent
}

func (f *fakeRepo) InitSchema(ctx context.Context) error { return nil }

func (f *fakeRepo) SaveReadingCurrent(ctx context.Context, r storage.ReadingCurrent) error {
	f.readings = append(f.readings, r)
	return nil
}

func (f *fakeRepo) GetLatestReadings(ctx context.Context, deviceID string) ([]storage.ReadingCurrent, error) {
	var out []storage.ReadingCurrent
	for _, r := range f.readings {
		if r.DeviceID == deviceID {
			out = append(out, r)
		}
	}
	return out, nil
}

// testUSPD is a small Akron-shaped logical UPD: V@0 and Q@2, both float,
// big-endian ("0123") on the wire toward Энергосфера.
func testUSPD(listen string) NorthUSPD {
	return NorthUSPD{
		ID:       "test-uspd",
		DeviceID: "acron-1",
		Listen:   listen,
		UnitID:   1,
		Interval: "60m",
		CurrentPoints: []NorthPoint{
			{Register: 0, Space: "holding", Type: "float", Order: "0123", Source: "V"},
			{Register: 2, Space: "holding", Type: "float", Order: "0123", Source: "Q"},
		},
	}
}

// startTestServer boots a Server on 127.0.0.1:0, seeds the fake repo, and
// returns the dial address plus a cancel func for cleanup. ReadTimeout is
// shortened so tests that rely on idle-timeout behaviour stay fast.
func startTestServer(t *testing.T, uspd NorthUSPD, repo *fakeRepo, bus *monitor.Bus) (addr string, cancel func()) {
	t.Helper()
	srv := NewServer(uspd, repo, bus)
	srv.ReadTimeout = 300 * time.Millisecond

	ctx, cancelCtx := context.WithCancel(context.Background())
	ready := make(chan struct{})
	errCh := make(chan error, 1)

	go func() {
		// Listen binds synchronously inside itself before accepting; poll
		// Addr() until it's non-nil so the test doesn't race the bind.
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

// buildReadRequest builds a raw Modbus TCP func 03/04 request frame.
func buildReadRequest(transID uint16, unitID, function byte, start, qty uint16) []byte {
	frame := make([]byte, 12)
	binary.BigEndian.PutUint16(frame[0:2], transID)
	frame[2], frame[3] = 0, 0 // protocol id
	binary.BigEndian.PutUint16(frame[4:6], 6)
	frame[6] = unitID
	frame[7] = function
	binary.BigEndian.PutUint16(frame[8:10], start)
	binary.BigEndian.PutUint16(frame[10:12], qty)
	return frame
}

// buildWriteSingleRegisterRequest builds a raw func 06 request frame.
func buildWriteSingleRegisterRequest(transID uint16, unitID byte, addr, value uint16) []byte {
	frame := make([]byte, 12)
	binary.BigEndian.PutUint16(frame[0:2], transID)
	frame[2], frame[3] = 0, 0
	binary.BigEndian.PutUint16(frame[4:6], 6)
	frame[6] = unitID
	frame[7] = funcWriteSingleRegister
	binary.BigEndian.PutUint16(frame[8:10], addr)
	binary.BigEndian.PutUint16(frame[10:12], value)
	return frame
}

// readResponse reads one full Modbus TCP response frame off conn using the
// same MBAP-length framing the server itself uses.
func readResponse(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	header := make([]byte, 7)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatalf("read response header: %v", err)
	}
	length := int(header[4])<<8 | int(header[5])
	payload := make([]byte, length-1)
	if len(payload) > 0 {
		if _, err := io.ReadFull(conn, payload); err != nil {
			t.Fatalf("read response payload: %v", err)
		}
	}
	return append(header, payload...)
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	return conn
}

func TestServer_ReadHoldingRegisters_Success(t *testing.T) {
	repo := &fakeRepo{}
	repo.SaveReadingCurrent(context.Background(), storage.ReadingCurrent{DeviceID: "acron-1", PointID: "V", Value: 0.5})
	repo.SaveReadingCurrent(context.Background(), storage.ReadingCurrent{DeviceID: "acron-1", PointID: "Q", Value: -12.25})

	addr, cancel := startTestServer(t, testUSPD("127.0.0.1:0"), repo, nil)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()

	req := buildReadRequest(0x0001, 1, funcReadHolding, 0, 4)
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	resp := readResponse(t, conn)

	// MBAP: transID echoed, unit=1, func=0x03, byteCount=8, data = golden
	// bytes for V=0.5 and Q=-12.25 (same values proven in regmap_test.go).
	wantData := []byte{0x3F, 0x00, 0x00, 0x00, 0xC1, 0x44, 0x00, 0x00}
	if resp[0] != 0x00 || resp[1] != 0x01 {
		t.Fatalf("transaction id not echoed: % X", resp[0:2])
	}
	if resp[6] != 1 || resp[7] != funcReadHolding || resp[8] != byte(len(wantData)) {
		t.Fatalf("unexpected header: unit=%d func=0x%02X byteCount=%d", resp[6], resp[7], resp[8])
	}
	if got := resp[9:]; string(got) != string(wantData) {
		t.Fatalf("data: got % X, want % X", got, wantData)
	}
}

func TestServer_ReadInputRegisters_WrongSpace_ExceptionIllegalDataAddress(t *testing.T) {
	// CurrentPoints in testUSPD are all "holding" — an input-register
	// read must find nothing mapped and return exception 02.
	repo := &fakeRepo{}
	repo.SaveReadingCurrent(context.Background(), storage.ReadingCurrent{DeviceID: "acron-1", PointID: "V", Value: 0.5})

	addr, cancel := startTestServer(t, testUSPD("127.0.0.1:0"), repo, nil)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()

	req := buildReadRequest(0x0002, 1, funcReadInput, 0, 2)
	conn.Write(req)
	resp := readResponse(t, conn)

	if resp[7] != funcReadInput|0x80 || resp[8] != excIllegalDataAddr {
		t.Fatalf("expected exception 02 on func 0x84, got func=0x%02X code=%d", resp[7], resp[8])
	}
}

func TestServer_UnknownFunction_ExceptionIllegalFunction(t *testing.T) {
	repo := &fakeRepo{}
	addr, cancel := startTestServer(t, testUSPD("127.0.0.1:0"), repo, nil)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()

	req := buildReadRequest(0x0003, 1, 0x17, 0, 1) // 0x17 is not implemented
	conn.Write(req)
	resp := readResponse(t, conn)

	if resp[7] != 0x17|0x80 || resp[8] != excIllegalFunction {
		t.Fatalf("expected exception 01, got func=0x%02X code=%d", resp[7], resp[8])
	}
}

func TestServer_WriteSingleRegister_RejectedAndPublished(t *testing.T) {
	repo := &fakeRepo{}
	bus := monitor.NewBus(nil)
	events, unsubscribe := bus.Subscribe()
	defer unsubscribe()

	addr, cancel := startTestServer(t, testUSPD("127.0.0.1:0"), repo, bus)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()

	req := buildWriteSingleRegisterRequest(0x0004, 1, 5, 42)
	conn.Write(req)
	resp := readResponse(t, conn)

	if resp[7] != funcWriteSingleRegister|0x80 || resp[8] != excIllegalFunction {
		t.Fatalf("expected exception 01 on write, got func=0x%02X code=%d", resp[7], resp[8])
	}

	select {
	case ev := <-events:
		if ev.Stage != "northbound_write_rejected" {
			t.Fatalf("unexpected event stage: %q", ev.Stage)
		}
		if ev.DeviceID != "acron-1" {
			t.Fatalf("unexpected event device id: %q", ev.DeviceID)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("expected a northbound_write_rejected event, got none")
	}
}

func TestServer_WrongUnitID_NoResponse_ConnectionStaysAlive(t *testing.T) {
	repo := &fakeRepo{}
	repo.SaveReadingCurrent(context.Background(), storage.ReadingCurrent{DeviceID: "acron-1", PointID: "V", Value: 0.5})

	addr, cancel := startTestServer(t, testUSPD("127.0.0.1:0"), repo, nil)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()

	// unit id 9, server is configured for unit id 1 — must get no response.
	req := buildReadRequest(0x0005, 9, funcReadHolding, 0, 2)
	conn.Write(req)

	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected no response for mismatched unit id, got data")
	}

	// The connection must still be usable for a subsequent, correctly
	// addressed request (mismatched unit id must not drop the connection).
	req2 := buildReadRequest(0x0006, 1, funcReadHolding, 0, 2)
	conn.Write(req2)
	resp := readResponse(t, conn)
	if resp[7] != funcReadHolding {
		t.Fatalf("connection did not recover after unit-id mismatch: func=0x%02X", resp[7])
	}
}

func TestServer_QuantityOutOfRange_ExceptionIllegalDataValue(t *testing.T) {
	repo := &fakeRepo{}
	addr, cancel := startTestServer(t, testUSPD("127.0.0.1:0"), repo, nil)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()

	req := buildReadRequest(0x0007, 1, funcReadHolding, 0, 0) // qty=0 is invalid
	conn.Write(req)
	resp := readResponse(t, conn)

	if resp[7] != funcReadHolding|0x80 || resp[8] != excIllegalDataValue {
		t.Fatalf("expected exception 03, got func=0x%02X code=%d", resp[7], resp[8])
	}
}

func TestServer_OutsideMap_ExceptionIllegalDataAddress(t *testing.T) {
	repo := &fakeRepo{}
	repo.SaveReadingCurrent(context.Background(), storage.ReadingCurrent{DeviceID: "acron-1", PointID: "V", Value: 0.5})

	addr, cancel := startTestServer(t, testUSPD("127.0.0.1:0"), repo, nil)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()

	req := buildReadRequest(0x0008, 1, funcReadHolding, 500, 2) // far outside the 2 mapped points
	conn.Write(req)
	resp := readResponse(t, conn)

	if resp[7] != funcReadHolding|0x80 || resp[8] != excIllegalDataAddr {
		t.Fatalf("expected exception 02, got func=0x%02X code=%d", resp[7], resp[8])
	}
}
