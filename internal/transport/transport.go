package transport

import (
"context"
"fmt"
"time"
)

// Kind определяет тип транспорта
type Kind string

const (
KindModbusTCP Kind = "modbus_tcp"
KindRTUSerial Kind = "rtu_serial"
KindTCPSerial Kind = "tcp_serial"
KindGSM       Kind = "gsm"   // post-MVP
KindVPN       Kind = "vpn"   // post-MVP
)

// Params описывает параметры подключения
type Params struct {
Kind            Kind
Host            string
Port            int
COM             string
Baudrate        int
Parity          string // "none"|"even"|"odd"
StopBits        int    // 1|2
ResponseTimeout time.Duration
InterframeDelay time.Duration
}

// Transport определяет контракт для обмена байтами (HAL)
type Transport interface {
// Read отправляет запрос и ждет ответ или ошибку таймаута
Read(ctx context.Context, req []byte) (resp []byte, err error)
Close() error
}

// New создает транспорт в зависимости от типа
func New(p Params) (Transport, error) {
switch p.Kind {
case KindModbusTCP:
return newTCP(p)
case KindRTUSerial, KindTCPSerial:
return nil, fmt.Errorf("serial transport not implemented yet")
default:
return nil, fmt.Errorf("unsupported transport kind: %s", p.Kind)
}
}
