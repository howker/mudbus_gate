package akron

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"mbgw/internal/codec"
)

// This file is the ENCODE (respond) side of the hourly/daily archive row —
// the inverse of codec.DecodeAkronVolume + the record_layout BCD decode.
// The downstream side reads real rows off a device and normalises them to a
// float value + timestamp (stored via storage.HourlyArchiveRecord); when
// mbgw itself plays the Akron slave upstream (M4 carrier), it must turn
// those stored values back into the exact wire shape Энергосфера expects
// for command 104:
//
//	row = U(4B, little-endian) + Pu(1B) + H,D,M,Y (4B, packed BCD)   // 9 bytes
//
// with value = U * 10^(Pu-3), per "ВЗАИМОДЕЙСТВИЕ С КОНТРОЛЛЕРОМ СЕТИ
// MODBUS ПРИБОРОВ Акрон-01 и Акрон-02-1", section 2 table 1. Multi-byte
// values are little-endian on the wire ("младшим байтом вперёд").
//
// HourlyRowSize is fixed at 9; the daily row drops the hour byte (8 bytes),
// handled separately if/when daily serving is added.
const HourlyRowSize = 9

// EncodeVolume packs a normalised volume value into Akron's (U, Pu) pair.
//
// It chooses the SMALLEST Pu (0..5) whose resulting U still fits a signed
// 32-bit counter, which maximises retained precision: Pu=0 keeps three
// decimal places (U = value*1000), each higher Pu trades one decimal for
// more headroom. The device's own original Pu is not preserved (we store a
// normalised float, not raw U/Pu) — but the decoded value is identical, and
// Энергосфера cares about the value, not how U/Pu were split.
//
// U is treated as signed (int32) because DecodeAkronVolume decodes it
// signed; negative volumes are supported for symmetry though hourly volume
// is normally non-negative.
func EncodeVolume(value float64) (u int32, pu byte, err error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, 0, fmt.Errorf("akron: cannot encode non-finite volume %v", value)
	}
	for p := 0; p <= 5; p++ {
		scaled := math.Round(value * math.Pow(10, float64(3-p)))
		if scaled >= math.MinInt32 && scaled <= math.MaxInt32 {
			return int32(scaled), byte(p), nil
		}
	}
	return 0, 0, fmt.Errorf("akron: volume %v does not fit any Pu in 0..5", value)
}

// EncodeHourlyRow builds one 9-byte command-104 archive row from a
// normalised value and its wall-clock hour timestamp. The timestamp's
// calendar fields are taken as-is (no timezone conversion): H, D, M, and
// the two low digits of the year, each packed BCD — matching how the row is
// read back and what fixes the archive-acceptance timestamp condition.
func EncodeHourlyRow(value float64, ts time.Time) ([]byte, error) {
	u, pu, err := EncodeVolume(value)
	if err != nil {
		return nil, err
	}

	row := make([]byte, 0, HourlyRowSize)

	var ub [4]byte
	binary.LittleEndian.PutUint32(ub[:], uint32(u)) // signed→two's-complement LE
	row = append(row, ub[:]...)
	row = append(row, pu)

	for _, v := range []int{ts.Hour(), ts.Day(), int(ts.Month()), ts.Year() % 100} {
		b, err := codec.EncodeBCDByte(v)
		if err != nil {
			return nil, fmt.Errorf("akron: encode BCD time field %d: %w", v, err)
		}
		row = append(row, b)
	}

	return row, nil
}
