package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"mbgw/internal/errs"
)

// tcpTransport implements Transport for Modbus TCP, per CONTRACTS.md
// section 1: Open is idempotent, Send/Receive are separate operations
// (allowing Protocol.Transact to control retries between them), and
// Receive frames by the MBAP length field.
type tcpTransport struct {
	mu              sync.Mutex
	params          Params
	addr            string
	conn            net.Conn
	interframeDelay time.Duration
	lastInteraction time.Time
	closed          bool
}

func newTCP(p Params) (Transport, error) {
	if p.Host == "" || p.Port == 0 {
		return nil, fmt.Errorf("invalid tcp address: %s:%d", p.Host, p.Port)
	}
	return &tcpTransport{
		params:          p,
		addr:            fmt.Sprintf("%s:%d", p.Host, p.Port),
		interframeDelay: p.InterframeDelay,
	}, nil
}

// Open is idempotent: calling it again while already connected is a no-op.
func (t *tcpTransport) Open(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return fmt.Errorf("tcp transport: %w", errs.ErrClosed)
	}
	if t.conn != nil {
		return nil // already open, idempotent per contract
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return fmt.Errorf("dial tcp %s: %w", t.addr, err)
	}
	t.conn = conn
	return nil
}

// Send transmits a fully-built frame. Does not wait for or read a
// response - that is Receive's job, allowing the protocol layer to control
// timing/retries between the two independently.
func (t *tcpTransport) Send(ctx context.Context, frame []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return fmt.Errorf("tcp transport: %w", errs.ErrClosed)
	}
	if err := t.ensureConnLocked(ctx); err != nil {
		return err
	}

	elapsed := time.Since(t.lastInteraction)
	if elapsed < t.interframeDelay {
		time.Sleep(t.interframeDelay - elapsed)
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(t.params.ResponseTimeout)
	}
	if err := t.conn.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}

	if _, err := t.conn.Write(frame); err != nil {
		t.conn.Close()
		t.conn = nil
		return fmt.Errorf("tcp write: %w", errs.ErrTransport)
	}
	return nil
}

// Receive reads one Modbus TCP frame, using the MBAP header's length field
// to know how many bytes to read (per CONTRACTS.md section 1: "for TCP, by
// length from MBAP").
func (t *tcpTransport) Receive(ctx context.Context, timeout time.Duration) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil, fmt.Errorf("tcp transport: %w", errs.ErrClosed)
	}
	if t.conn == nil {
		return nil, fmt.Errorf("tcp transport: not open (call Open first)")
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(timeout)
	}
	if err := t.conn.SetReadDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set read deadline: %w", err)
	}

	header := make([]byte, 7)
	if _, err := io.ReadFull(t.conn, header); err != nil {
		if netErr, ok2 := err.(net.Error); ok2 && netErr.Timeout() {
			// A read timeout does not necessarily mean the connection is
			// broken (e.g. the device simply does not support this
			// function and never replies) - keep the connection open for
			// the next attempt instead of forcing a costly reconnect.
			return nil, fmt.Errorf("read header: %w", errs.ErrTimeout)
		}
		t.conn.Close()
		t.conn = nil
		return nil, fmt.Errorf("read header: %w", errs.ErrTransport)
	}

	length := int(header[4])<<8 | int(header[5])
	if length <= 0 || length > 260 {
		t.conn.Close()
		t.conn = nil
		return nil, fmt.Errorf("invalid modbus tcp payload length %d: %w", length, errs.ErrFrame)
	}

	payload := make([]byte, length-1)
	if _, err := io.ReadFull(t.conn, payload); err != nil {
		if netErr, ok2 := err.(net.Error); ok2 && netErr.Timeout() {
			return nil, fmt.Errorf("read payload: %w", errs.ErrTimeout)
		}
		t.conn.Close()
		t.conn = nil
		return nil, fmt.Errorf("read payload: %w", errs.ErrTransport)
	}

	t.lastInteraction = time.Now()
	return append(header, payload...), nil
}

func (t *tcpTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.closed = true
	if t.conn != nil {
		err := t.conn.Close()
		t.conn = nil
		return err
	}
	return nil
}

// ensureConnLocked dials a fresh connection if the current one was dropped
// (t.conn == nil but the transport hasn't been explicitly Close()'d).
// Called by Send/Receive instead of failing outright on "not open" — a
// genuine socket error (as opposed to a mere read timeout, which does NOT
// clear t.conn — see Receive's comment) previously left the transport
// permanently dead until the whole process was restarted. CONFIRMED live
// 2026-08-01: a real VKM-360 connection dropped mid-backfill and stayed
// unreachable for hours afterward (every subsequent archive/current-value
// attempt failing with "not open"), since nothing ever called Open()
// again after startup. Caller must hold t.mu.
func (t *tcpTransport) ensureConnLocked(ctx context.Context) error {
	if t.conn != nil {
		return nil
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return fmt.Errorf("tcp reconnect: dial %s: %w", t.addr, err)
	}
	t.conn = conn
	return nil
}

func (t *tcpTransport) Info() Params {
	return t.params
}
