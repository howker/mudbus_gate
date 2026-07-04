package codec

import (
	"encoding/binary"
	"fmt"
	"math"
)

// RegisterCount returns the number of 16-bit Modbus registers required
// for a given logical data type.
//
// Important architectural note:
// register sizing belongs to the codec/type system, not to Modbus framing.
// This function exists specifically to keep protocol code free from
// hardcoded assumptions like "float=2, everything else=1".
func RegisterCount(dataType string) int {
	switch dataType {
	case "float", "int32", "uint32":
			return 2
		case "scaled_int":
			return 1
	case "double", "float64", "u32+float", "long+float":
		return 4
	case "uint16", "int16", "bitfield":
		return 1
	default:
		// Strings/asciiz and other variable-size types must be resolved
		// by profile metadata (for example, explicit length in registers).
		return 1
	}
}

// DecodeUint16 decodes a 16-bit unsigned integer from big-endian bytes.
func DecodeUint16(data []byte) (uint16, error) {
	if len(data) < 2 {
		return 0, fmt.Errorf("need at least 2 bytes, got %d", len(data))
	}
	return binary.BigEndian.Uint16(data), nil
}

// DecodeInt16 decodes a 16-bit signed integer from big-endian bytes.
func DecodeInt16(data []byte) (int16, error) {
	v, err := DecodeUint16(data)
	if err != nil {
		return 0, err
	}
	return int16(v), nil
}

// Reorder32 normalizes 4-byte payload into canonical ABCD order before decode.
func Reorder32(data []byte, order string) ([]byte, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("32-bit value requires 4 bytes")
	}

	res := make([]byte, 4)
	switch order {
	case "1032", "BADC":
		res[0], res[1], res[2], res[3] = data[1], data[0], data[3], data[2]
	case "2301", "CDAB":
		res[0], res[1], res[2], res[3] = data[2], data[3], data[0], data[1]
	case "3210", "DCBA":
		res[0], res[1], res[2], res[3] = data[3], data[2], data[1], data[0]
	case "0123", "ABCD", "":
		copy(res, data[:4])
	default:
		return nil, fmt.Errorf("unknown 32-bit byte order: %s", order)
	}
	return res, nil
}

// reorder32FromCanonical converts canonical ABCD bytes into the target wire order.
func reorder32FromCanonical(data []byte, order string) ([]byte, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("32-bit value requires 4 bytes")
	}

	res := make([]byte, 4)
	switch order {
	case "0123", "ABCD", "":
		copy(res, data[:4])
	case "1032", "BADC":
		res[0], res[1], res[2], res[3] = data[1], data[0], data[3], data[2]
	case "2301", "CDAB":
		res[0], res[1], res[2], res[3] = data[2], data[3], data[0], data[1]
	case "3210", "DCBA":
		res[0], res[1], res[2], res[3] = data[3], data[2], data[1], data[0]
	default:
		return nil, fmt.Errorf("unknown 32-bit byte order: %s", order)
	}
	return res, nil
}

func DecodeUint32(data []byte, order string) (uint32, error) {
	ordered, err := Reorder32(data, order)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(ordered), nil
}

func DecodeInt32(data []byte, order string) (int32, error) {
	v, err := DecodeUint32(data, order)
	if err != nil {
		return 0, err
	}
	return int32(v), nil
}

func DecodeFloat32(data []byte, order string) (float32, error) {
	v, err := DecodeUint32(data, order)
	if err != nil {
		return 0, err
	}
	return math.Float32frombits(v), nil
}

// Reorder64 normalizes 8-byte payload into canonical order before float64 decode.
//
// Important note for architecture reviewers:
// this function is intentionally strict.
// Unlike the previous fallback behavior, unknown order must return an error.
// Silent fallback is dangerous because VKM byte-order detection depends on
// comparing decoded control constants, and a hidden fallback can mask bugs.
func Reorder64(data []byte, order string) ([]byte, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("64-bit value requires 8 bytes")
	}

	res := make([]byte, 8)
	switch order {
	case "01234567", "":
		copy(res, data[:8])
	case "10325476":
		res[0], res[1], res[2], res[3] = data[1], data[0], data[3], data[2]
		res[4], res[5], res[6], res[7] = data[5], data[4], data[7], data[6]
	case "76543210":
		for i := 0; i < 8; i++ {
			res[i] = data[7-i]
		}
	default:
		return nil, fmt.Errorf("unknown 64-bit byte order: %s", order)
	}
	return res, nil
}

// reorder64FromCanonical converts canonical 64-bit bytes into the target wire order.
func reorder64FromCanonical(data []byte, order string) ([]byte, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("64-bit value requires 8 bytes")
	}

	res := make([]byte, 8)
	switch order {
	case "01234567", "":
		copy(res, data[:8])
	case "10325476":
		res[0], res[1], res[2], res[3] = data[1], data[0], data[3], data[2]
		res[4], res[5], res[6], res[7] = data[5], data[4], data[7], data[6]
	case "76543210":
		for i := 0; i < 8; i++ {
			res[i] = data[7-i]
		}
	default:
		return nil, fmt.Errorf("unknown 64-bit byte order: %s", order)
	}
	return res, nil
}

func DecodeFloat64(data []byte, order string) (float64, error) {
	ordered, err := Reorder64(data, order)
	if err != nil {
		return 0, err
	}
	bits := binary.BigEndian.Uint64(ordered)
	return math.Float64frombits(bits), nil
}

// EncodeUint16 is currently used for simple write requests.
// More encode helpers will be added as write/session/archive support grows.
// DecodeString decodes a fixed-length ASCII/ANSI string from raw bytes,
// trimming trailing NUL bytes and spaces (common padding in Modbus string
// registers). Works for both plain "string" and "asciiz" record_layout
// types - asciiz's terminator is naturally handled by the NUL trim.
func DecodeString(data []byte) string {
    trimmed := data
    for len(trimmed) > 0 && (trimmed[len(trimmed)-1] == 0x00 || trimmed[len(trimmed)-1] == ' ') {
        trimmed = trimmed[:len(trimmed)-1]
    }
    return string(trimmed)
}
func EncodeUint16(val uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, val)
	return b
}

func EncodeUint32(val uint32, order string) ([]byte, error) {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, val)
	return reorder32FromCanonical(b, order)
}

func EncodeInt32(val int32, order string) ([]byte, error) {
	return EncodeUint32(uint32(val), order)
}

func EncodeFloat32(val float32, order string) ([]byte, error) {
	return EncodeUint32(math.Float32bits(val), order)
}


// DecodeU32Float decodes the VZLET-style composite type: an unsigned 32-bit
// integer part followed by a 32-bit float fractional part, each 4 bytes,
// each independently subject to the given 32-bit byte order. Result is
// float64(intPart) + float64(fracPart), per CONTRACTS.md section 5.3.
func DecodeU32Float(data []byte, order string) (float64, error) {
    if len(data) < 8 {
        return 0, fmt.Errorf("u32+float requires 8 bytes, got %d", len(data))
    }
    intPart, err := DecodeUint32(data[0:4], order)
    if err != nil {
        return 0, err
    }
    fracPart, err := DecodeFloat32(data[4:8], order)
    if err != nil {
        return 0, err
    }
    return float64(intPart) + float64(fracPart), nil
}

// DecodeLongFloat decodes the TSRV-style composite type: a signed 32-bit
// integer part followed by a 32-bit float fractional part, per
// CONTRACTS.md section 5.3.
func DecodeLongFloat(data []byte, order string) (float64, error) {
    if len(data) < 8 {
        return 0, fmt.Errorf("long+float requires 8 bytes, got %d", len(data))
    }
    intPart, err := DecodeInt32(data[0:4], order)
    if err != nil {
        return 0, err
    }
    fracPart, err := DecodeFloat32(data[4:8], order)
    if err != nil {
        return 0, err
    }
    return float64(intPart) + float64(fracPart), nil
}
func EncodeFloat64(val float64, order string) ([]byte, error) {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, math.Float64bits(val))
	return reorder64FromCanonical(b, order)
}