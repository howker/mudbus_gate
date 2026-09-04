package pollcore

import (
	"context"
	"sync"

	"mbgw/internal/protocol/modbus"
	"mbgw/internal/transport"
)

// sharedIOLocks serializes access to the same physical device/channel even
// when different Reader instances are created for it (for example the
// long-lived server reader and a one-shot diagnostic probe).
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
// NewWithLockKey for the same physical device.
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
// physical device use the same mutex, so current/archive/backfill/manual
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

// ReadRaw reads a logical point (space/addr/dataType) and returns the raw
// data bytes (function code and byte count already stripped).
func (r *Reader) ReadRaw(ctx context.Context, space string, addr int, dataType string) ([]byte, error) {
	r.ioMu.Lock()
	defer r.ioMu.Unlock()

	return modbus.ReadPoint(ctx, r.tr, r.isTCP, r.unitID, space, addr, dataType)
}

// Transact sends an already-built PDU and returns the raw response PDU.
// The shared lock covers the whole Modbus transaction, including retries.
func (r *Reader) Transact(ctx context.Context, pduReq []byte) ([]byte, error) {
	r.ioMu.Lock()
	defer r.ioMu.Unlock()

	return modbus.Transact(ctx, r.tr, r.isTCP, nextTxIDPublic(), r.unitID, pduReq)
}

// nextTxIDPublic exposes modbus's internal tx id counter through a small
// wrapper, since Transact needs one but Reader.Transact's callers do not
// manage tx ids themselves.
func nextTxIDPublic() uint16 {
	return modbus.NextTxID()
}
