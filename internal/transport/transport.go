package transport

import (
    "context"
    "fmt"
    "time"
)

// Kind определяет тип транспорта.
type Kind string

const (
    KindModbusTCP Kind = "modbus_tcp"
    KindRTUSerial Kind = "rtu_serial"
    KindTCPSerial Kind = "tcp_serial"
    KindGSM       Kind = "gsm" // post-MVP
    KindVPN       Kind = "vpn" // post-MVP
)

// Params описывает параметры подключения.
type Params struct {
    Kind            Kind
    Host            string // tcp/tcp_serial
    Port            int
    COM             string // rtu_serial: "COM3" / "/dev/ttyUSB0"
    Baudrate        int
    Parity          string // "none"|"even"|"odd"
    StopBits        int    // 1|2
    ResponseTimeout time.Duration
    InterframeDelay time.Duration
    Retries         int
}

// Transport is the byte-level HAL contract (CONTRACTS.md section 1).
// A single Transport instance represents one physical channel, is NOT
// thread-safe (external serialization via lease/channel), and Open is
// idempotent. After Close, any call must return an error wrapping
// errs.ErrClosed.
type Transport interface {
    Open(ctx context.Context) error
    Close() error
    Send(ctx context.Context, frame []byte) error
    Receive(ctx context.Context, timeout time.Duration) ([]byte, error)
    Info() Params
}

// New creates a transport for the given params' Kind.
func New(p Params) (Transport, error) {
    switch p.Kind {
    case KindModbusTCP:
        return newTCP(p)
    case KindRTUSerial, KindTCPSerial:
        return newSerial(p)
    default:
        return nil, fmt.Errorf("unsupported transport kind: %s", p.Kind)
    }
}