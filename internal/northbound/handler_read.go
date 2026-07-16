package northbound

import (
	"context"
	"fmt"
)

// handleReadRegisters serves function 03 (Read Holding Registers) and 04
// (Read Input Registers) against s.uspd.CurrentPoints, using the latest
// readings for s.uspd.DeviceID from storage.Repo — never touching a device
// transport (northbound holds no reference to one).
func (s *Server) handleReadRegisters(transID []byte, unitID, function byte, body []byte) []byte {
	if len(body) < 4 {
		return exceptionFrame(transID, unitID, function, excIllegalDataValue)
	}
	start := binaryUint16(body[0:2])
	qty := binaryUint16(body[2:4])
	if qty == 0 || qty > maxReadQuantity {
		return exceptionFrame(transID, unitID, function, excIllegalDataValue)
	}

	space := "holding"
	if function == funcReadInput {
		space = "input"
	}

	// context.Background() here matches the M1 MVP scope: a single
	// bounded local Repo read, not a caller-cancellable long operation.
	// If Repo reads ever become cancellable/slow, this should take a
	// per-request ctx threaded from handleConn instead.
	values, err := s.currentValues(context.Background())
	if err != nil {
		return exceptionFrame(transID, unitID, function, excSlaveDeviceFailur)
	}

	data, covered, err := ReadRegisters(s.uspd.CurrentPoints, space, start, qty, values)
	if err != nil {
		return exceptionFrame(transID, unitID, function, excSlaveDeviceFailur)
	}
	if !covered {
		return exceptionFrame(transID, unitID, function, excIllegalDataAddr)
	}

	resp := make([]byte, 9+len(data))
	copy(resp[0:2], transID)
	resp[2], resp[3] = 0, 0
	length := 1 /* unit */ + 1 /* func */ + 1 /* byte count */ + len(data)
	resp[4] = byte(length >> 8)
	resp[5] = byte(length)
	resp[6] = unitID
	resp[7] = function
	resp[8] = byte(len(data))
	copy(resp[9:], data)
	return resp
}

// currentValues fetches the latest readings for the served device and
// reduces them to the map[PointID]float64 shape ReadRegisters expects.
// Readings whose Value isn't numeric (e.g. a string/BCD-decoded field not
// meant for the northbound map) are silently skipped — they simply aren't
// available for this read, same as any other unmapped register.
func (s *Server) currentValues(ctx context.Context) (map[string]float64, error) {
	readings, err := s.repo.GetLatestReadings(ctx, s.uspd.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("northbound: get latest readings for %s: %w", s.uspd.DeviceID, err)
	}
	values := make(map[string]float64, len(readings))
	for _, r := range readings {
		if f, ok := toFloat64(r.Value); ok {
			values[r.PointID] = f
		}
	}
	return values, nil
}

// toFloat64 converts a storage.ReadingCurrent.Value (typed `any`) to
// float64 for the register-encoding pipeline, covering the numeric kinds
// codec/pointresolver are known to produce.
func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	default:
		return 0, false
	}
}
