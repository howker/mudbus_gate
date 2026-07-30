package session

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"mbgw/internal/codec"
	"mbgw/internal/protocol/modbus"
	"mbgw/internal/transport"
)

// ModbusByteOrderAuth is the dedicated session type for VKM-like devices.
//
// Target behavior required by the architecture:
// 1. Open() must enter initializing state.
// 2. Read control constants from registers 110 / 112 / 114.
// 3. Detect byte/word order using known golden constants, including double.
// 4. Perform authorization writes to registers 200 / 201.
// 5. Handle temporary BUSY responses with throttling/backoff.
// 6. Expose ready/error state to the poller via State().
//
// Current status:
// This implementation is still a staged skeleton.
// The lifecycle contract is already correct, and the internal phases are
// now split explicitly so the future VKM handshake can be added without
// turning Open() into one monolithic block again.
type ModbusByteOrderAuth struct {
	state State
	mu    sync.RWMutex
	tr    transport.Transport

	// detectedOrder stores the currently selected 32-bit byte/word order.
	// In the real implementation it will be discovered by comparing decoded
	// control constants from VKM registers against known golden values.
	detectedOrder string

	// txID is the rolling Modbus TCP transaction id used for protocol helpers.
	txID uint16

	// unitID is currently pinned to the default Modbus slave id used by the
	// early session skeleton. It can be moved to profile/device config later.
	unitID uint8
}

func NewModbusByteOrderAuth() *ModbusByteOrderAuth {
	return &ModbusByteOrderAuth{
		state:  StateClosed,
		unitID: 1,
	}
}

func (s *ModbusByteOrderAuth) Open(ctx context.Context, tr transport.Transport) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.state = StateInitializing
	s.tr = tr

	order, err := s.detectByteOrder(ctx)
	if err != nil {
		s.state = StateError
		return err
	}
	s.detectedOrder = order

	if err := s.authorize(ctx); err != nil {
		s.state = StateError
		return err
	}

	s.state = StateReady
	return nil
}

// supported32Orders defines the currently expected VKM 32-bit order candidates.
// This list is intentionally explicit so future byte-order detection does not
// spread magic strings across the codebase.
var supported32Orders = []string{
	"0123",
	"1032",
	"2301",
	"3210",
}

// supported64Orders defines the currently expected 64-bit order candidates.
// At this stage the list is intentionally conservative.
// It should only grow when profile/contracts and golden vectors confirm
// which 64-bit order variants must be supported in production.
var supported64Orders = []string{
	"01234567",
	"10325476",
	"76543210",
}

// VKM control constants expected during byte-order detection.
//
// The architecture review explicitly called out registers 110 / 112 / 114:
// - 110 and 112 are used as 32-bit control constants
// - 114 is used as a double control constant
//
// These values are not fully wired yet, but we pin their intent in code now
// so the upcoming implementation does not drift from the contract.
const (
	vkmControlRegister110 = 110
	vkmControlRegister112 = 112
	vkmControlRegister114 = 114

	vkmAuthRegister200 = 200
	vkmAuthRegister201 = 201
)

const (
	vkmGolden110Int32   = int32(1234567890)
	vkmGolden112Float32 = float32(123.4567)
	vkmGolden114Float64 = float64(123.4567890123456)

	authBusyMaxAttempts = 3
)

var (
	// Architect review explicitly referenced the double golden bytes:
	// 40 5E DD 3C 07 FB 4C 93
	vkmGolden114DoubleRaw = []byte{0x40, 0x5E, 0xDD, 0x3C, 0x07, 0xFB, 0x4C, 0x93}
)

// nextTxID returns the next transaction id for Modbus TCP requests.
func (s *ModbusByteOrderAuth) nextTxID() uint16 {
	s.txID++
	if s.txID == 0 {
		s.txID = 1
	}
	return s.txID
}

// readControlRegister reads one VKM control/golden-constant value through
// Modbus function 03 (Holding Registers) and returns only the response
// payload bytes (without function code / byte count).
//
// CONFIRMED against a real ВКМ-360 (2026-07-29, tools/vkmprobe
// --probe-control-regs): register 110 answers Modbus exception 0x02
// (illegal data address) on function 04 (Input Registers) — the space
// this code originally assumed — but function 03 (Holding Registers) at
// the same address 110 returns exactly the documented golden constant
// (int32 1234567890, byte order "0123"). The CONTRACTS.md contract text
// itself doesn't specify the register space explicitly; this was an
// unverified assumption baked into the "staged skeleton" implementation,
// caught only once real hardware was available to probe.
//
// Expected response shape for function 03: response PDU = [0x03][byteCount][data...]
func (s *ModbusByteOrderAuth) readControlRegister(ctx context.Context, addr int, dataType string) ([]byte, error) {
	if s.tr == nil {
		return nil, fmt.Errorf("transport is not initialized")
	}

	reqPDU, err := modbus.BuildReadPDU("HR", addr, dataType)
	if err != nil {
		return nil, err
	}

	respPDU, err := modbus.Transact(ctx, s.tr, true, s.nextTxID(), s.unitID, reqPDU)
	if err != nil {
		return nil, err
	}

	if len(respPDU) < 2 {
		return nil, fmt.Errorf("modbus response too short: %d", len(respPDU))
	}
	if respPDU[0] != 0x03 {
		return nil, fmt.Errorf("unexpected modbus function in response: 0x%02X", respPDU[0])
	}

	byteCount := int(respPDU[1])
	if len(respPDU) != 2+byteCount {
		return nil, fmt.Errorf("modbus byte count mismatch: declared=%d actual=%d", byteCount, len(respPDU)-2)
	}

	data := make([]byte, byteCount)
	copy(data, respPDU[2:])
	return data, nil
}

// writeSingleHoldingRegister writes one holding register through function 06
// and validates the standard echo response.
func (s *ModbusByteOrderAuth) writeSingleHoldingRegister(ctx context.Context, addr int, value uint16) error {
	if s.tr == nil {
		return fmt.Errorf("transport is not initialized")
	}

	reqPDU := modbus.BuildWriteSingleRegisterPDU(addr, value)

	respPDU, err := modbus.Transact(ctx, s.tr, true, s.nextTxID(), s.unitID, reqPDU)
	if err != nil {
		return err
	}

	if len(respPDU) != 5 {
		return fmt.Errorf("unexpected write-single response length: %d", len(respPDU))
	}
	if respPDU[0] != 0x06 {
		return fmt.Errorf("unexpected write-single function in response: 0x%02X", respPDU[0])
	}
	if respPDU[1] != reqPDU[1] || respPDU[2] != reqPDU[2] || respPDU[3] != reqPDU[3] || respPDU[4] != reqPDU[4] {
		return fmt.Errorf("write-single echo mismatch for register %d", addr)
	}

	return nil
}

// writeHoldingRegisters writes one or more contiguous holding registers
// and validates the function 06 / 16 response shape.
func (s *ModbusByteOrderAuth) writeHoldingRegisters(ctx context.Context, addr int, values []uint16) error {
	if len(values) == 0 {
		return fmt.Errorf("values must not be empty")
	}

	if len(values) == 1 {
		return s.writeSingleHoldingRegister(ctx, addr, values[0])
	}

	if s.tr == nil {
		return fmt.Errorf("transport is not initialized")
	}

	reqPDU, err := modbus.BuildWriteMultipleRegistersPDU(addr, values)
	if err != nil {
		return err
	}

	respPDU, err := modbus.Transact(ctx, s.tr, true, s.nextTxID(), s.unitID, reqPDU)
	if err != nil {
		return err
	}

	if len(respPDU) != 5 {
		return fmt.Errorf("unexpected write-multiple response length: %d", len(respPDU))
	}
	if respPDU[0] != 0x10 {
		return fmt.Errorf("unexpected write-multiple function in response: 0x%02X", respPDU[0])
	}

	gotAddr := int(respPDU[1])<<8 | int(respPDU[2])
	gotQty := int(respPDU[3])<<8 | int(respPDU[4])

	if gotAddr != addr {
		return fmt.Errorf("write-multiple address mismatch: want %d, got %d", addr, gotAddr)
	}
	if gotQty != len(values) {
		return fmt.Errorf("write-multiple quantity mismatch: want %d, got %d", len(values), gotQty)
	}

	return nil
}

func float32AlmostEqual(a, b float32) bool {
	return math.Abs(float64(a-b)) <= 0.0001
}

func float64AlmostEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-12
}

func isBusyModbusError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "code 0x06")
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// detectByteOrder is intentionally extracted before the real implementation exists.
//
// Current behavior:
//  1. Read IR 110 / 112 / 114.
//  2. Try every supported 32-bit order against int32 and float golden constants.
//  3. Try every supported 64-bit order against the double golden constant.
//  4. Confirm that the matched 64-bit order belongs to the same order family as
//     the matched 32-bit order.
//  5. Return the agreed 32-bit order.
func (s *ModbusByteOrderAuth) detectByteOrder(ctx context.Context) (string, error) {
	raw110, err := s.readControlRegister(ctx, vkmControlRegister110, "int32")
	if err != nil {
		return "", fmt.Errorf("read VKM control register 110: %w", err)
	}

	raw112, err := s.readControlRegister(ctx, vkmControlRegister112, "float")
	if err != nil {
		return "", fmt.Errorf("read VKM control register 112: %w", err)
	}

	raw114, err := s.readControlRegister(ctx, vkmControlRegister114, "double")
	if err != nil {
		return "", fmt.Errorf("read VKM control register 114: %w", err)
	}

	var matched32 string
	for _, order := range supported32Orders {
		got110, err := codec.DecodeInt32(raw110, order)
		if err != nil {
			continue
		}
		if got110 != vkmGolden110Int32 {
			continue
		}

		got112, err := codec.DecodeFloat32(raw112, order)
		if err != nil {
			continue
		}
		if !float32AlmostEqual(got112, vkmGolden112Float32) {
			continue
		}

		if matched32 != "" {
			return "", fmt.Errorf("ambiguous 32-bit byte order detection: both %s and %s match", matched32, order)
		}
		matched32 = order
	}

	if matched32 == "" {
		return "", fmt.Errorf("no supported 32-bit byte order matched VKM control constants")
	}

	var matched64 string
	for _, order := range supported64Orders {
		got114, err := codec.DecodeFloat64(raw114, order)
		if err != nil {
			continue
		}
		if !float64AlmostEqual(got114, vkmGolden114Float64) {
			continue
		}

		if matched64 != "" {
			return "", fmt.Errorf("ambiguous 64-bit byte order detection: both %s and %s match", matched64, order)
		}
		matched64 = order
	}

	if matched64 == "" {
		return "", fmt.Errorf("no supported 64-bit byte order matched VKM control constants")
	}

	expected64By32 := map[string]string{
		"0123": "01234567",
		"1032": "10325476",
		"3210": "76543210",
	}

	expected64, ok := expected64By32[matched32]
	if !ok {
		return "", fmt.Errorf("no mapped 64-bit order family for detected 32-bit order %s", matched32)
	}

	if matched64 != expected64 {
		return "", fmt.Errorf("byte order family mismatch: 32-bit=%s but 64-bit=%s", matched32, matched64)
	}

	return matched32, nil
}

// authorize performs the auth write path required by the session contract.
//
// Current behavior:
// - write a minimal placeholder credential payload into registers 200 / 201
// - retry temporary Modbus BUSY responses with a small backoff
func (s *ModbusByteOrderAuth) authorize(ctx context.Context) error {
	backoffs := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond}
	var lastErr error

	for attempt := 1; attempt <= authBusyMaxAttempts; attempt++ {
		err := s.writeHoldingRegisters(ctx, vkmAuthRegister200, []uint16{1, 0})
		if err == nil {
			return nil
		}
		lastErr = err

		if !isBusyModbusError(err) {
			return fmt.Errorf("write VKM auth registers 200/201: %w", err)
		}

		if attempt == authBusyMaxAttempts {
			break
		}

		if err := sleepWithContext(ctx, backoffs[attempt-1]); err != nil {
			return fmt.Errorf("write VKM auth registers 200/201: %w", err)
		}
	}

	return fmt.Errorf("write VKM auth registers 200/201: %w", lastErr)
}

func (s *ModbusByteOrderAuth) KeepAlive(ctx context.Context) error {
	_ = ctx
	// Placeholder for future VKM keepalive logic.
	return nil
}

func (s *ModbusByteOrderAuth) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = StateClosed
	return nil
}

func (s *ModbusByteOrderAuth) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}
