package pollcore

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"mbgw/internal/protocol/modbus"
	"mbgw/internal/transport"
)

// PhysicalIOLockKey returns the mutex key for the physical southbound
// channel represented by params.
//
// RTU serial is keyed by COM port because multiple Modbus unit IDs can
// share one RS-485 bus. TCP-serial is keyed by host:port because one
// converter endpoint normally represents one serial bus. Native Modbus
// TCP keeps the existing per-device isolation: separate devices use
// separate TCP connections and do not need to block each other.
func PhysicalIOLockKey(params transport.Params, deviceID string) string {
	switch params.Kind {
	case transport.KindRTUSerial:
		if com := strings.TrimSpace(params.COM); com != "" {
			return "rtu_serial:" + strings.ToUpper(com)
		}
	case transport.KindTCPSerial:
		host := strings.ToLower(strings.TrimSpace(params.Host))
		if host != "" && params.Port > 0 {
			return fmt.Sprintf("tcp_serial:%s:%d", host, params.Port)
		}
	case transport.KindModbusTCP:
		if deviceID != "" {
			return "modbus_tcp:" + deviceID
		}
	}

	if deviceID != "" {
		return "device:" + deviceID
	}
	return ""
}

// sharedIOLocks serializes access to the same physical channel even
// when different Reader instances are created for devices that share it.
var sharedIOLocks sync.Map // map[string]*sync.Mutex

func sharedIOMutex(lockKey string) *sync.Mutex {
	if lockKey == "" {
		return &sync.Mutex{}
	}
	actual, _ := sharedIOLocks.LoadOrStore(lockKey, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

// LockKey acquires the shared physical-I/O lock for lockKey and returns
// an unlock function. It is used for I/O that happens outside Reader,
// such as a session handshake.
//
// Callers must use the exact same lockKey that was passed to
// NewWithLockKey for the same physical channel.
func LockKey(lockKey string) func() {
	mu := sharedIOMutex(lockKey)
	mu.Lock()
	return mu.Unlock
}

// Reader provides point-level and raw-transaction access to a device over
// a transport, using protocol/modbus for framing/CRC/retries.
//
// ioMu covers the WHOLE transaction, including protocol retries. When a
// shared lock key is supplied, different Reader instances for the same
// physical channel use the same mutex, so current/archive/backfill/manual
// reload/diagnostic traffic cannot overlap on the wire.
type Reader struct {
	tr     transport.Transport
	isTCP  bool
	unitID uint8

	ioMu *sync.Mutex
}

// New creates a Reader with a private I/O lock. Kept for compatibility
// with standalone tools/tests that do not need cross-Reader
// serialization.
func New(tr transport.Transport, isTCP bool, unitID uint8) *Reader {
	return &Reader{
		tr:     tr,
		isTCP:  isTCP,
		unitID: unitID,
		ioMu:   &sync.Mutex{},
	}
}

// NewWithLockKey creates a Reader that shares one I/O mutex with every
// other Reader created with the same non-empty lockKey.
func NewWithLockKey(tr transport.Transport, isTCP bool, unitID uint8, lockKey string) *Reader {
	return &Reader{
		tr:     tr,
		isTCP:  isTCP,
		unitID: unitID,
		ioMu:   sharedIOMutex(lockKey),
	}
}

// withPhysicalChannel runs one complete protocol transaction while holding
// the shared physical-bus lock. Serial transports are opened immediately
// before the transaction and released immediately after it; native Modbus TCP
// keeps its existing persistent connection because ReleaseIdle is a no-op for
// that transport.
//
// Opening/closing is deliberately INSIDE ioMu. Two devices with different
// Modbus unit IDs on the same COM port therefore cannot race at Open() even
// though each device owns its own Transport object.
func (r *Reader) withPhysicalChannel(ctx context.Context, fn func() ([]byte, error)) (resp []byte, err error) {
	// Keep lock ownership in ONE place together with the physical lifecycle.
	// This prevents any present or future Reader operation from accidentally
	// doing Open/Release outside the shared bus mutex.
	r.ioMu.Lock()
	defer r.ioMu.Unlock()

	if r.tr == nil {
		return nil, fmt.Errorf("nil transport")
	}
	if err := r.tr.Open(ctx); err != nil {
		// Best-effort cleanup if an implementation allocated a resource before
		// returning its Open error. Do not call Close: Close is terminal, while
		// a later poll must be allowed to retry a temporarily busy COM port.
		_ = transport.ReleaseIdle(r.tr)
		return nil, err
	}
	defer func() {
		if releaseErr := transport.ReleaseIdle(r.tr); releaseErr != nil && err == nil {
			err = fmt.Errorf("release idle transport: %w", releaseErr)
		}
	}()
	return fn()
}

// ReadRaw reads a logical point (space/addr/dataType) and returns the raw
// data bytes (function code and byte count already stripped).
func (r *Reader) ReadRaw(ctx context.Context, space string, addr int, dataType string) ([]byte, error) {
	return r.withPhysicalChannel(ctx, func() ([]byte, error) {
		return modbus.ReadPoint(ctx, r.tr, r.isTCP, r.unitID, space, addr, dataType)
	})
}

// Transact sends an already-built PDU and returns the raw response PDU.
// The shared lock covers Open -> whole Modbus transaction (including retries)
// -> ReleaseIdle, so another device on the same COM port can only start after
// the previous one has physically released the port.
func (r *Reader) Transact(ctx context.Context, pduReq []byte) ([]byte, error) {
	return r.withPhysicalChannel(ctx, func() ([]byte, error) {
		return modbus.Transact(ctx, r.tr, r.isTCP, nextTxIDPublic(), r.unitID, pduReq)
	})
}

// nextTxIDPublic exposes modbus's internal tx id counter through a small
// wrapper, since Transact needs one but Reader.Transact's callers do not
// manage tx ids themselves.
func nextTxIDPublic() uint16 {
	return modbus.NextTxID()
}
