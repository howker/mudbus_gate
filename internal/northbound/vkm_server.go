package northbound

import (
	"context"
	"encoding/hex"
	"log"
	"net"
	"sync"
	"time"

	"mbgw/internal/protocol/modbus"
)

// VKMServer is the MBAP (Modbus TCP) carrier that lets mbgw stand in front
// of Энергосфера as a УВП-280А-shaped device serving ВКМ-360 archives.
// Confirmed against the live ЭС config: the ВКМ record uses plain Modbus
// TCP (MBAP), not raw RTU — so this reuses the standard TCP framing
// (protocol/modbus.ParseTCPFrame/BuildTCPFrame), unlike the Akron carrier
// which speaks bare RTU frames.
//
// Per-connection state matters here: the real УВП-280.01 partitions
// archive-request state by client interface ("данные запрошенные по
// интерфейсу RS232-1 будут доступны только по интерфейсу RS232-1" — see
// modbus_uvp280_01.pdf, "Чтение архивов"). VKMServer mirrors that by
// creating a FRESH VKMArchiveResponder per accepted connection, never
// sharing one across connections — two simultaneous readers get
// independent request/status/result state, exactly like two interfaces on
// the real device.
type VKMServer struct {
	listen string
	newSrc func() VKMArchiveSource // factory: one source per connection

	// Log, if set, records every request/response frame as a
	// DiscoveryEntry (same JSONL shape the Akron carrier and discovery
	// modes use) — so `Get-Content *.jsonl` works the same way for VKM
	// as it already does for Akron. Optional: nil means no audit trail.
	Log *DiscoveryLog

	ReadTimeout time.Duration

	mu       sync.Mutex
	listener net.Listener
	closed   bool
}

// NewVKMServer builds a carrier listening on listen. newSrc is called once
// per accepted connection to build that connection's VKMArchiveSource —
// pass a factory returning a DB-backed source in production, or
// `func() VKMArchiveSource { return FixedVKMSource{Result: "..."} }` for
// the current fixed-string smoke stage.
func NewVKMServer(listen string, newSrc func() VKMArchiveSource) *VKMServer {
	return &VKMServer{listen: listen, newSrc: newSrc}
}

func (s *VKMServer) readTimeout() time.Duration {
	if s.ReadTimeout > 0 {
		return s.ReadTimeout
	}
	return 30 * time.Second
}

func (s *VKMServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *VKMServer) Listen(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.listen)
	if err != nil {
		return err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.listener = ln
	s.mu.Unlock()

	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
		case <-stopped:
		}
	}()
	defer close(stopped)

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			s.mu.Lock()
			closedByUs := s.closed
			s.mu.Unlock()
			if closedByUs {
				return nil
			}
			return err
		}
		go s.handleConn(conn)
	}
}

func (s *VKMServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func (s *VKMServer) handleConn(conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()

	// Fresh responder per connection — see the per-interface state note
	// above. This IS the isolation mechanism, not an incidental detail.
	resp := NewVKMArchiveResponder(s.newSrc())

	buf := make([]byte, 512)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.readTimeout()))
		n, err := conn.Read(buf)
		if err != nil {
			if err.Error() != "EOF" {
				log.Printf("[vkm-carrier %s] соединение закрыто: %v\n", remote, err)
			}
			return
		}
		if n < 8 {
			continue
		}
		frame := append([]byte(nil), buf[:n]...)

		txID, unitID, pdu, perr := modbus.ParseTCPFrame(frame)
		if perr != nil {
			log.Printf("[vkm-carrier %s] битый MBAP-кадр: %v\n", remote, perr)
			continue
		}

		s.writeLog(DiscoveryEntry{
			RemoteAddr: remote,
			Unit:       unitID,
			Function:   pdu[0],
			Direction:  "request",
			PayloadHex: hex.EncodeToString(pdu),
		})

		respPDU := resp.Respond(pdu)
		if respPDU == nil {
			continue // no answer, e.g. unhandled function/address
		}

		s.writeLog(DiscoveryEntry{
			RemoteAddr: remote,
			Unit:       unitID,
			Function:   pdu[0],
			Direction:  "response",
			PayloadHex: hex.EncodeToString(respPDU),
		})

		respFrame := modbus.BuildTCPFrame(txID, unitID, respPDU)
		if _, werr := conn.Write(respFrame); werr != nil {
			return
		}
	}
}

func (s *VKMServer) writeLog(e DiscoveryEntry) {
	if s.Log != nil {
		_ = s.Log.Write(e)
	}
}
