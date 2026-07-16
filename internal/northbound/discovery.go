// Discovery mode (TRD addendum §B.4, MVP-план §6): a "black box" Modbus TCP
// slave used to safely observe how ПК «Энергосфера» talks to a Modbus УСПД,
// on a live but isolated instance, without touching any real device.
package northbound

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"mbgw/internal/errs"
)

// DiscoveryServer answers every inbound Modbus TCP frame, regardless of
// unit id or function code, with a fixture-driven stub response, and logs
// each request/response pair in full.
//
// Structural safety guarantee: this type holds no reference to a device
// transport and no storage.Repo — there are no such fields below, by
// design. It is therefore architecturally impossible for DiscoveryServer
// to forward anything it receives to real equipment, no matter what a
// peer sends it. If a future change ever needs this type to touch a
// device or Repo, that is no longer discovery mode and deserves a new,
// clearly-named type instead of a field added here.
type DiscoveryServer struct {
	listen  string
	fixture DiscoveryFixture
	log     *DiscoveryLog // may be nil (logging disabled — not recommended, but not required to run)

	// ReadTimeout overrides defaultReadTimeout when non-zero (see
	// server.go's doc comment on the same field on Server for the
	// idle-shutdown-latency rationale, identical here).
	ReadTimeout time.Duration

	mu       sync.Mutex
	listener net.Listener
	closed   bool
}

// NewDiscoveryServer creates a DiscoveryServer. log may be nil to disable
// logging (frames are still answered, just not recorded — mainly useful
// in tests).
func NewDiscoveryServer(listen string, fixture DiscoveryFixture, log *DiscoveryLog) *DiscoveryServer {
	return &DiscoveryServer{listen: listen, fixture: fixture, log: log}
}

func (s *DiscoveryServer) readTimeout() time.Duration {
	if s.ReadTimeout > 0 {
		return s.ReadTimeout
	}
	return defaultReadTimeout
}

// Addr returns the actual listening address (useful in tests using ":0").
func (s *DiscoveryServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Listen binds `listen` and blocking-accepts connections until ctx is
// cancelled (mirrors Server.Listen's shape exactly — see server.go).
func (s *DiscoveryServer) Listen(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.listen)
	if err != nil {
		return fmt.Errorf("northbound: discovery listen %s: %w", s.listen, err)
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
			return fmt.Errorf("northbound: discovery accept: %w", err)
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *DiscoveryServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func (s *DiscoveryServer) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		frame, err := readFrame(conn, s.readTimeout())
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			if errors.Is(err, errs.ErrTimeout) {
				continue
			}
			return
		}

		resp := s.handleFrame(remote, frame)
		if resp == nil {
			continue // frame too short to even contain a function code
		}
		if _, err := conn.Write(resp); err != nil {
			return
		}
	}
}

// handleFrame logs the request, builds a stub response via the fixture,
// logs the response, and returns the MBAP-wrapped bytes to write back.
// Unlike Server.handleFrame (M1), this never filters by unit id — seeing
// everything a peer sends, regardless of addressing, is the entire point
// of discovery mode.
func (s *DiscoveryServer) handleFrame(remote string, frame []byte) []byte {
	if len(frame) < 8 {
		return nil
	}
	transID := frame[0:2]
	unitID := frame[6]
	function := frame[7]
	body := frame[8:]   // PDU data only (no function code) — used for address/quantity parsing
	reqPDU := frame[7:] // function code + data — logged as-is, symmetric with the response PDU below

	entry := DiscoveryEntry{
		RemoteAddr: remote,
		Unit:       unitID,
		Function:   function,
		Direction:  "request",
		PayloadHex: hex.EncodeToString(reqPDU),
	}
	if len(body) >= 4 {
		entry.Address = binary.BigEndian.Uint16(body[0:2])
		entry.Quantity = binary.BigEndian.Uint16(body[2:4])
	}
	if s.log != nil {
		_ = s.log.Write(entry)
	}

	respPDU := s.fixture.Respond(unitID, function, body)

	if s.log != nil {
		_ = s.log.Write(DiscoveryEntry{
			RemoteAddr: remote,
			Unit:       unitID,
			Function:   function,
			Direction:  "response",
			PayloadHex: hex.EncodeToString(respPDU),
		})
	}

	out := make([]byte, 7+len(respPDU))
	copy(out[0:2], transID)
	out[2], out[3] = 0, 0
	length := 1 + len(respPDU) // unit(1) + PDU
	out[4] = byte(length >> 8)
	out[5] = byte(length)
	out[6] = unitID
	copy(out[7:], respPDU)
	return out
}
