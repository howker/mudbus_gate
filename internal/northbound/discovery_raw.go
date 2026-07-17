// Raw discovery mode: for upstream links that are NOT Modbus TCP (no MBAP
// header) but a raw byte stream over TCP — e.g. ПК «Энергосфера» talking to
// an "АКРОН-01-1"-type УСПД over "Raw TCP", which passes bare Modbus RTU
// frames (address + PDU + CRC16) transparently.
//
// The responder behaviour (identity, command 110, live clock, per-command
// overrides) is configured at runtime via simulator.SetAkronSimConfig, so
// it can be changed by editing a YAML file on the server and restarting —
// no rebuild. See simulator/akron_responder.go.
//
// Safety: like DiscoveryServer, this type holds no storage.Repo and no
// device transport — it cannot touch real equipment.
package northbound

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"time"

	"mbgw/internal/protocol/modbus"
	"mbgw/internal/simulator"
)

const rawReadTimeout = 30 * time.Second
const rawReassembleWindow = 80 * time.Millisecond

// RawResponder turns an inbound RTU PDU into a response PDU (nil = no answer).
type RawResponder func(pdu []byte) []byte

type RawDiscoveryServer struct {
	listen    string
	log       *DiscoveryLog
	responder RawResponder

	ReadTimeout time.Duration

	mu       sync.Mutex
	listener net.Listener
	closed   bool
}

func NewRawDiscoveryServer(listen string, log *DiscoveryLog) *RawDiscoveryServer {
	return &RawDiscoveryServer{
		listen:    listen,
		log:       log,
		responder: simulator.AkronResponsePDU,
	}
}

func (s *RawDiscoveryServer) readTimeout() time.Duration {
	if s.ReadTimeout > 0 {
		return s.ReadTimeout
	}
	return rawReadTimeout
}

func (s *RawDiscoveryServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *RawDiscoveryServer) Listen(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.listen)
	if err != nil {
		return fmt.Errorf("northbound: raw discovery listen %s: %w", s.listen, err)
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
			return fmt.Errorf("northbound: raw discovery accept: %w", err)
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *RawDiscoveryServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func (s *RawDiscoveryServer) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	buf := make([]byte, 1024)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(s.readTimeout()))
		n, err := conn.Read(buf)
		if n > 0 {
			frame := append([]byte(nil), buf[:n]...)
			frame = s.reassemble(conn, frame)
			s.onFrame(remote, conn, frame)
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
	}
}

func (s *RawDiscoveryServer) reassemble(conn net.Conn, first []byte) []byte {
	frame := first
	for {
		_ = conn.SetReadDeadline(time.Now().Add(rawReassembleWindow))
		more := make([]byte, 512)
		n, err := conn.Read(more)
		if n > 0 {
			frame = append(frame, more[:n]...)
		}
		if err != nil || n == 0 {
			break
		}
	}
	return frame
}

func (s *RawDiscoveryServer) onFrame(remote string, conn net.Conn, frame []byte) {
	s.writeLog(DiscoveryEntry{
		RemoteAddr: remote,
		Direction:  "request",
		PayloadHex: hex.EncodeToString(frame),
	})

	unitID, pdu, err := modbus.ParseRTUFrame(frame)
	if err != nil || len(pdu) < 1 {
		return
	}

	s.writeLog(DiscoveryEntry{
		RemoteAddr: remote,
		Unit:       unitID,
		Function:   pdu[0],
		Direction:  "request-decoded",
		PayloadHex: hex.EncodeToString(pdu),
	})

	respPDU := s.responder(pdu)
	if respPDU == nil {
		return
	}
	respFrame := modbus.BuildRTUFrame(unitID, respPDU)

	s.writeLog(DiscoveryEntry{
		RemoteAddr: remote,
		Unit:       unitID,
		Function:   pdu[0],
		Direction:  "response",
		PayloadHex: hex.EncodeToString(respFrame),
	})

	_, _ = conn.Write(respFrame)
}

func (s *RawDiscoveryServer) writeLog(e DiscoveryEntry) {
	if s.log != nil {
		_ = s.log.Write(e)
	}
}
