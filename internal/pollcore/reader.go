package pollcore

import (
    "context"

    "mbgw/internal/protocol/modbus"
    "mbgw/internal/transport"
)

// Reader provides point-level and raw-transaction access to a device over
// a transport, using protocol/modbus for framing/CRC/retries. This
// replaces the old internal/client package, which duplicated framing
// logic already present in protocol/modbus (see backlog note resolved by
// this refactor).
type Reader struct {
    tr     transport.Transport
    isTCP  bool
    unitID uint8
}

// New creates a Reader bound to a transport. isTCP selects MBAP framing
// (true) vs RTU framing (false); unitID is the Modbus slave id.
func New(tr transport.Transport, isTCP bool, unitID uint8) *Reader {
    return &Reader{tr: tr, isTCP: isTCP, unitID: unitID}
}

// ReadRaw reads a logical point (space/addr/dataType) and returns the raw
// data bytes (function code and byte count already stripped).
func (r *Reader) ReadRaw(ctx context.Context, space string, addr int, dataType string) ([]byte, error) {
    return modbus.ReadPoint(ctx, r.tr, r.isTCP, r.unitID, space, addr, dataType)
}

// Transact sends an already-built PDU (used by archive strategies that
// need full control over the request shape, e.g. mb_request_poll_string)
// and returns the raw response PDU (function code + byte count/echo +
// data), still checked for Modbus exceptions. Implements archive.Transactor.
func (r *Reader) Transact(ctx context.Context, pduReq []byte) ([]byte, error) {
    return modbus.Transact(ctx, r.tr, r.isTCP, nextTxIDPublic(), r.unitID, pduReq)
}

// nextTxIDPublic exposes modbus's internal tx id counter through a small
// wrapper, since Transact needs one but Reader.Transact's callers (archive
// strategies) do not manage tx ids themselves.
func nextTxIDPublic() uint16 {
    return modbus.NextTxID()
}