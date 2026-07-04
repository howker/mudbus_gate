package client

import (
    "context"
    "fmt"

    "mbgw/internal/protocol/modbus"
    "mbgw/internal/transport"
)

// Client defines the interface for reading raw bytes for a logical data point,
// and for sending pre-built PDUs directly (needed by archive strategies such as
// mb_request_poll_string, which control the exact request shape themselves).
// Any type implementing both methods also satisfies archive.Transactor.
type Client interface {
    ReadRaw(ctx context.Context, space string, addr int, dataType string) ([]byte, error)
    Transact(ctx context.Context, req []byte) ([]byte, error)
}

// ModbusClient binds a transport with Modbus protocol framing rules.
type ModbusClient struct {
    tr     transport.Transport
    isTCP  bool
    unitID uint8
    txID   uint16
}

func NewModbus(tr transport.Transport, isTCP bool, unitID uint8) *ModbusClient {
    return &ModbusClient{
        tr:     tr,
        isTCP:  isTCP,
        unitID: unitID,
    }
}

// wrapAndSend wraps a PDU into the appropriate frame (TCP/RTU), sends it through
// the transport and returns the raw response PDU (function code + byte count + data),
// after checking for a Modbus exception.
func (c *ModbusClient) wrapAndSend(ctx context.Context, pduReq []byte) ([]byte, error) {
    if c.tr == nil {
        return nil, fmt.Errorf("transport is nil")
    }

    var reqFrame []byte
    if c.isTCP {
        c.txID++
        reqFrame = modbus.BuildTCPFrame(c.txID, c.unitID, pduReq)
    } else {
        reqFrame = modbus.BuildRTUFrame(c.unitID, pduReq)
    }

    respFrame, err := c.tr.Read(ctx, reqFrame)
    if err != nil {
        return nil, fmt.Errorf("transport read: %w", err)
    }

    var pduResp []byte
    if c.isTCP {
        if len(respFrame) < 9 {
            return nil, fmt.Errorf("tcp frame too short")
        }
        pduResp = respFrame[7:]
    } else {
        _, pduResp, err = modbus.ParseRTUFrame(respFrame)
        if err != nil {
            return nil, fmt.Errorf("parse rtu: %w", err)
        }
    }

    if isExc, code := modbus.IsException(pduResp); isExc {
        return nil, &modbus.ExceptionError{Code: code}
    }
    if len(pduResp) < 2 {
        return nil, fmt.Errorf("pdu response too short")
    }

    return pduResp, nil
}

// ReadRaw builds a read request for a logical point (space/addr/dataType) and
// returns only the raw data bytes (function code and byte count stripped).
func (c *ModbusClient) ReadRaw(ctx context.Context, space string, addr int, dataType string) ([]byte, error) {
    pduReq, err := modbus.BuildReadPDU(space, addr, dataType)
    if err != nil {
        return nil, fmt.Errorf("build pdu: %w", err)
    }

    pduResp, err := c.wrapAndSend(ctx, pduReq)
    if err != nil {
        return nil, err
    }

    // Skip function code [0] and byte count [1], return only data bytes.
    return pduResp[2:], nil
}

// Transact sends an already-built PDU (e.g. from archive strategies that need
// full control over the request shape) and returns the raw response PDU
// (function code + byte count/echo + data), still checked for Modbus exceptions.
// This implements archive.Transactor.
func (c *ModbusClient) Transact(ctx context.Context, pduReq []byte) ([]byte, error) {
    return c.wrapAndSend(ctx, pduReq)
}