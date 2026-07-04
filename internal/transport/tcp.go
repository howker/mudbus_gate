package transport

import (
"context"
"fmt"
"io"
"net"
"sync"
"time"
)

type tcpTransport struct {
mu              sync.Mutex
addr            string
conn            net.Conn
responseTimeout time.Duration
interframeDelay time.Duration
lastInteraction time.Time
}

func newTCP(p Params) (Transport, error) {
if p.Host == "" || p.Port == 0 {
return nil, fmt.Errorf("invalid tcp address: %s:%d", p.Host, p.Port)
}
addr := fmt.Sprintf("%s:%d", p.Host, p.Port)

t := &tcpTransport{
addr:            addr,
responseTimeout: p.ResponseTimeout,
interframeDelay: p.InterframeDelay,
}

// ля MVP подключаемся сразу при создании (fail-fast)
if err := t.connect(context.Background()); err != nil {
return nil, err
}

return t, nil
}

func (t *tcpTransport) connect(ctx context.Context) error {
if t.conn != nil {
t.conn.Close()
t.conn = nil
}

var d net.Dialer
conn, err := d.DialContext(ctx, "tcp", t.addr)
if err != nil {
return fmt.Errorf("dial tcp %s: %w", t.addr, err)
}

t.conn = conn
return nil
}

func (t *tcpTransport) Read(ctx context.Context, req []byte) ([]byte, error) {
t.mu.Lock()
defer t.mu.Unlock()

// 1. Соблюдение Interframe Delay (пауза между запросами)
elapsed := time.Since(t.lastInteraction)
if elapsed < t.interframeDelay {
time.Sleep(t.interframeDelay - elapsed)
}

// 2. еконнект, если соединение отпало
if t.conn == nil {
if err := t.connect(ctx); err != nil {
return nil, err
}
}

// 3. станавливаем таймаут на запись
deadline, ok := ctx.Deadline()
if !ok {
deadline = time.Now().Add(t.responseTimeout)
}
t.conn.SetWriteDeadline(deadline)

// 4. тправка запроса
if _, err := t.conn.Write(req); err != nil {
t.conn.Close()
t.conn = nil
return nil, fmt.Errorf("write error: %w", err)
}

// 5. станавливаем таймаут на чтение
t.conn.SetReadDeadline(deadline)

// 6. тение заголовка MBAP (Modbus Application Protocol) - 7 байт
header := make([]byte, 7)
if _, err := io.ReadFull(t.conn, header); err != nil {
t.conn.Close()
t.conn = nil
return nil, fmt.Errorf("read header error: %w", err)
}

// 7. пределение длины оставшейся части пакета
// айты 4 и 5 в MBAP содержат длину (UnitID + PDU)
length := int(header[4])<<8 | int(header[5])
if length <= 0 || length > 260 {
t.conn.Close()
t.conn = nil
return nil, fmt.Errorf("invalid modbus tcp payload length: %d", length)
}

// 8. тение оставшейся части (length - 1, т.к. UnitID уже находится в header[6])
payload := make([]byte, length-1)
if _, err := io.ReadFull(t.conn, payload); err != nil {
t.conn.Close()
t.conn = nil
return nil, fmt.Errorf("read payload error: %w", err)
}

t.lastInteraction = time.Now()

// 9. Сборка полного ответа (аголовок + PDU)
return append(header, payload...), nil
}

func (t *tcpTransport) Close() error {
t.mu.Lock()
defer t.mu.Unlock()

if t.conn != nil {
err := t.conn.Close()
t.conn = nil
return err
}
return nil
}
