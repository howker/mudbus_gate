package transport

import (
    "context"
    "fmt"
    "io"
    "net"
    "sync"
    "time"

    "go.bug.st/serial"

    "mbgw/internal/errs"
)

// serialTransport implements Transport for RTU over a real COM port
// (KindRTUSerial) or RTU framing over a TCP socket via a serial-to-TCP
// converter (KindTCPSerial). Frame boundaries are determined by the
// interframe silence gap, per CONTRACTS.md section 1 ("for RTU, assembled
// by interframe pause").
type serialTransport struct {
    mu              sync.Mutex
    params          Params
    port            io.ReadWriteCloser // serial.Port or net.Conn depending on Kind
    interframeDelay time.Duration
    closed          bool
}

func newSerial(p Params) (Transport, error) {
    if p.Kind == KindTCPSerial {
        if p.Host == "" || p.Port == 0 {
            return nil, fmt.Errorf("invalid tcp-serial address: %s:%d", p.Host, p.Port)
        }
    } else {
        if p.COM == "" {
            return nil, fmt.Errorf("invalid rtu_serial: COM port not specified")
        }
    }

    interframe := p.InterframeDelay
    if interframe <= 0 {
        // Fallback per CONTRACTS.md section 6 NFRs: at 9600 baud,
        // interframe pause should be at least ~4ms (3.5 character times).
        interframe = 4 * time.Millisecond
    }

    return &serialTransport{
        params:          p,
        interframeDelay: interframe,
    }, nil
}

func (t *serialTransport) Open(ctx context.Context) error {
    t.mu.Lock()
    defer t.mu.Unlock()

    if t.closed {
        return fmt.Errorf("serial transport: %w", errs.ErrClosed)
    }
    if t.port != nil {
        return nil // idempotent
    }

    if t.params.Kind == KindTCPSerial {
        var d net.Dialer
        addr := fmt.Sprintf("%s:%d", t.params.Host, t.params.Port)
        conn, err := d.DialContext(ctx, "tcp", addr)
        if err != nil {
            return fmt.Errorf("dial tcp-serial %s: %w", addr, err)
        }
        t.port = conn
        return nil
    }

    mode := &serial.Mode{
        BaudRate: t.params.Baudrate,
        DataBits: 8,
    }
    if mode.BaudRate <= 0 {
        mode.BaudRate = 9600
    }
    switch t.params.Parity {
    case "even":
        mode.Parity = serial.EvenParity
    case "odd":
        mode.Parity = serial.OddParity
    default:
        mode.Parity = serial.NoParity
    }
    switch t.params.StopBits {
    case 2:
        mode.StopBits = serial.TwoStopBits
    default:
        mode.StopBits = serial.OneStopBit
    }

    port, err := serial.Open(t.params.COM, mode)
    if err != nil {
        return fmt.Errorf("open serial port %s: %w", t.params.COM, err)
    }
    t.port = port
    return nil
}

func (t *serialTransport) Send(ctx context.Context, frame []byte) error {
    t.mu.Lock()
    defer t.mu.Unlock()

    if t.closed {
        return fmt.Errorf("serial transport: %w", errs.ErrClosed)
    }
    if t.port == nil {
        return fmt.Errorf("serial transport: not open (call Open first)")
    }

    if _, err := t.port.Write(frame); err != nil {
        return fmt.Errorf("serial write: %w", errs.ErrTransport)
    }
    return nil
}

// Receive assembles one RTU frame by reading until an interframe silence
// gap is observed (no bytes arrive for interframeDelay), or the overall
// timeout expires. This is a simplified, polling-based implementation
// suitable for our use case; it does not require OS-level "read with
// inter-character timeout" support.
func (t *serialTransport) Receive(ctx context.Context, timeout time.Duration) ([]byte, error) {
    t.mu.Lock()
    defer t.mu.Unlock()

    if t.closed {
        return nil, fmt.Errorf("serial transport: %w", errs.ErrClosed)
    }
    if t.port == nil {
        return nil, fmt.Errorf("serial transport: not open (call Open first)")
    }

    deadline := time.Now().Add(timeout)
    buf := make([]byte, 0, 256)
    chunk := make([]byte, 256)

    if p, ok := t.port.(serial.Port); ok {
        _ = p.SetReadTimeout(t.interframeDelay)
    }

    for {
        // For KindTCPSerial, t.port is a net.Conn, not a serial.Port, so
        // the SetReadTimeout call above silently does nothing for it -
        // without an explicit per-iteration read deadline here, Read
        // would block forever on an unresponsive device instead of
        // honoring interframeDelay/timeout at all. This mirrors what
        // tcpTransport.Receive already does for KindModbusTCP.
        if c, ok := t.port.(net.Conn); ok {
            readDeadline := time.Now().Add(t.interframeDelay)
            if readDeadline.After(deadline) {
                readDeadline = deadline
            }
            _ = c.SetReadDeadline(readDeadline)
        }

        n, err := t.port.Read(chunk)
        if n > 0 {
            buf = append(buf, chunk[:n]...)
        }
        if err != nil {
            if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
                // A per-iteration read timeout on net.Conn just means no
                // bytes arrived within interframeDelay - that's either
                // the normal interframe silence gap (if we already have
                // data) or we should keep waiting (if we don't), not a
                // hard error by itself. Fall through to the existing
                // buf-length/deadline checks below.
            } else if len(buf) > 0 {
                break // treat other read errors after some data as end-of-frame
            } else {
                return nil, fmt.Errorf("serial read: %w", errs.ErrTransport)
            }
        }
        if n == 0 && len(buf) > 0 {
            break // silence gap after receiving some data = frame boundary
        }
        if time.Now().After(deadline) {
            if len(buf) == 0 {
                return nil, fmt.Errorf("serial receive: %w", errs.ErrTimeout)
            }
            break
        }
    }

    return buf, nil
}

// Release closes only the currently opened physical serial/TCP-serial
// handle and keeps this transport object reusable. Production polling uses
// this after every complete Modbus transaction so an idle COM port is not
// permanently owned by one configured device. Close remains the FINAL
// lifecycle operation and makes later Open calls fail with ErrClosed.
func (t *serialTransport) Release() error {
    t.mu.Lock()
    defer t.mu.Unlock()

    if t.closed {
        return fmt.Errorf("serial transport: %w", errs.ErrClosed)
    }
    if t.port == nil {
        return nil
    }

    err := t.port.Close()
    t.port = nil
    return err
}

func (t *serialTransport) Close() error {
    t.mu.Lock()
    defer t.mu.Unlock()

    t.closed = true
    if t.port != nil {
        err := t.port.Close()
        t.port = nil
        return err
    }
    return nil
}

// ReleaseIdle releases a reusable physical channel when the concrete
// transport supports it. RTU serial and TCP-serial implement Release;
// native Modbus TCP deliberately keeps its existing persistent connection.
//
// The Transport interface itself is not widened: existing transports,
// tools and tests keep the original Open/Close contract.
func ReleaseIdle(tr Transport) error {
    if tr == nil {
        return nil
    }
    releaser, ok := tr.(interface{ Release() error })
    if !ok {
        return nil
    }
    return releaser.Release()
}

func (t *serialTransport) Info() Params {
    return t.params
}