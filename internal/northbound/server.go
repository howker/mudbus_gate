// Package northbound implements the upstream (northbound) Modbus TCP slave:
// mbgw presents itself to the upper system (ПК «Энергосфера») as an
// ordinary Modbus UPD, per TRD addendum §A.4/§12. This package is
// deliberately independent of internal/transport, which implements the
// *client* (master-downward) side of Modbus TCP (Open/Send/Receive against
// a dialed connection). Here we are the accepting/listening side of the
// same wire protocol, so the framing logic is small enough to duplicate
// rather than force an asymmetric reuse of a client-shaped interface.
//
// MVP invariants (TASK_M1 «Запрещено»):
//   - no net/http here — this is raw Modbus TCP, not REST;
//   - reads only from storage.Repo (never touches device transports);
//   - write requests (05/06/0F/10) are rejected with exception 1 and never
//     forwarded downstream — northbound cannot, even in principle, act on
//     a device, because it holds no reference to one.
package northbound

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"mbgw/internal/errs"
	"mbgw/internal/monitor"
	"mbgw/internal/storage"
)

// Modbus function codes this slave understands.
const (
	funcReadHolding            = 0x03
	funcReadInput              = 0x04
	funcWriteSingleCoil        = 0x05
	funcWriteSingleRegister    = 0x06
	funcWriteMultipleCoils     = 0x0F
	funcWriteMultipleRegisters = 0x10
)

// Modbus exception codes, per CONTRACTS.md section 2.2.
const (
	excIllegalFunction   = 0x01
	excIllegalDataAddr   = 0x02
	excIllegalDataValue  = 0x03
	excSlaveDeviceFailur = 0x04
)

// maxReadQuantity is the standard Modbus TCP limit for a single 03/04
// request (byte count field is 1 byte → 125 registers max).
const maxReadQuantity = 125

// defaultReadTimeout bounds how long a per-connection goroutine blocks
// waiting for the next frame's header before re-checking ctx. It is not the
// request/response latency (that is effectively instantaneous — Repo reads
// are in-memory/local SQLite) but the idle-connection poll interval, and
// therefore also the worst-case shutdown latency for an idle connection
// after ctx is cancelled. 30s idle is generous for a poll-driven upper
// system and keeps shutdown bounded for tests/operations.
const defaultReadTimeout = 30 * time.Second

// Server is the northbound Modbus TCP slave for one logical UPD (NorthUSPD).
// It is safe to Listen from one goroutine; Close may be called concurrently
// to trigger shutdown.
type Server struct {
	uspd NorthUSPD
	repo storage.Repo
	bus  *monitor.Bus // may be nil — write-rejection events are then skipped

	// ReadTimeout overrides defaultReadTimeout when non-zero. Exported so
	// tests can shorten idle-shutdown latency; must be set before Listen.
	ReadTimeout time.Duration

	mu       sync.Mutex
	listener net.Listener
	closed   bool
}

// NewServer creates a Server for the given logical UPD. bus may be nil (no
// write-rejection events published, e.g. in unit tests that don't care).
func NewServer(uspd NorthUSPD, repo storage.Repo, bus *monitor.Bus) *Server {
	return &Server{uspd: uspd, repo: repo, bus: bus}
}

// Listen binds uspd.Listen and blocking-accepts connections until ctx is
// cancelled, at which point it closes the listener and returns nil (clean
// shutdown). Each accepted connection is served in its own goroutine.
func (s *Server) Listen(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.uspd.Listen)
	if err != nil {
		return fmt.Errorf("northbound: listen %s: %w", s.uspd.Listen, err)
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
				return nil // shutdown-triggered close, not a real error
			default:
			}
			s.mu.Lock()
			closedByUs := s.closed
			s.mu.Unlock()
			if closedByUs {
				return nil
			}
			return fmt.Errorf("northbound: accept: %w", err)
		}
		go s.handleConn(ctx, conn)
	}
}

// Close stops accepting new connections (in-flight connections drain on
// their own idle timeout, bounded by ReadTimeout/defaultReadTimeout).
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// Addr returns the actual listening address (useful in tests using ":0").
// Returns nil if Listen has not yet bound.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *Server) readTimeout() time.Duration {
	if s.ReadTimeout > 0 {
		return s.ReadTimeout
	}
	return defaultReadTimeout
}

// handleConn serves one accepted connection until it errors, is closed by
// the peer, or ctx is cancelled (checked once per idle-timeout tick — see
// defaultReadTimeout's doc comment for the resulting shutdown-latency bound).
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		frame, err := readFrame(conn, s.readTimeout())
		if err != nil {
			if errors.Is(err, io.EOF) {
				return // peer closed the connection
			}
			if errors.Is(err, errs.ErrTimeout) {
				continue // idle connection — re-check ctx and wait again
			}
			return // malformed frame or transport error — drop the connection
		}

		resp := s.handleFrame(frame)
		if resp == nil {
			continue // not addressed to us (unit id mismatch) — stay connected
		}
		if _, err := conn.Write(resp); err != nil {
			return
		}
	}
}

// readFrame reads exactly one Modbus TCP frame (MBAP header + PDU),
// mirroring internal/transport's client-side framing (length from the
// MBAP header) but from the accepting side of the connection.
func readFrame(conn net.Conn, timeout time.Duration) ([]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("northbound: set read deadline: %w", errs.ErrTransport)
	}

	header := make([]byte, 7)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, classifyReadErr(err, "header")
	}

	length := int(header[4])<<8 | int(header[5])
	if length <= 0 || length > 260 {
		return nil, fmt.Errorf("northbound: invalid mbap length %d: %w", length, errs.ErrFrame)
	}

	payload := make([]byte, length-1)
	if len(payload) > 0 {
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return nil, fmt.Errorf("northbound: set read deadline: %w", errs.ErrTransport)
		}
		if _, err := io.ReadFull(conn, payload); err != nil {
			return nil, classifyReadErr(err, "payload")
		}
	}

	return append(header, payload...), nil
}

func classifyReadErr(err error, what string) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return io.EOF
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return errs.ErrTimeout
	}
	return fmt.Errorf("northbound: read %s: %w", what, errs.ErrTransport)
}

// handleFrame dispatches one parsed request frame and returns the response
// frame to write back, or nil if this server must not respond at all (the
// frame was addressed to a different unit id — the normal behaviour of a
// device that is not the one being addressed on a shared line/connection).
func (s *Server) handleFrame(frame []byte) []byte {
	if len(frame) < 8 {
		return nil // too short to contain even a function code — drop silently
	}

	transID := frame[0:2]
	unitID := frame[6]
	function := frame[7]
	body := frame[8:]

	if unitID != s.uspd.UnitID {
		return nil
	}

	switch function {
	case funcReadHolding, funcReadInput:
		return s.handleReadRegisters(transID, unitID, function, body)
	case funcWriteSingleCoil, funcWriteSingleRegister, funcWriteMultipleCoils, funcWriteMultipleRegisters:
		s.publishWriteRejected(unitID, function, body)
		return exceptionFrame(transID, unitID, function, excIllegalFunction)
	default:
		return exceptionFrame(transID, unitID, function, excIllegalFunction)
	}
}

// exceptionFrame builds a standard Modbus TCP exception response: MBAP
// header (echoing the transaction id) + [function|0x80, exception code].
func exceptionFrame(transID []byte, unitID, function, code byte) []byte {
	resp := make([]byte, 9)
	copy(resp[0:2], transID)
	resp[2], resp[3] = 0, 0 // protocol id
	resp[4], resp[5] = 0, 3 // length = unit(1) + func(1) + exception code(1)
	resp[6] = unitID
	resp[7] = function | 0x80
	resp[8] = code
	return resp
}

// publishWriteRejected records a rejected write attempt on the monitor bus
// (no-op if bus is nil). This is also the hook M3's discovery mode extends
// with full raw payload logging (TRD addendum §B.4).
func (s *Server) publishWriteRejected(unitID, function byte, body []byte) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(monitor.Event{
		DeviceID: s.uspd.DeviceID,
		Stage:    "northbound_write_rejected",
		Detail:   fmt.Sprintf("unit=%d func=0x%02X body=% X", unitID, function, body),
	})
}

func binaryUint16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
