package northbound

import (
	"fmt"
	"math"

	"mbgw/internal/codec"
)

// NorthPoint projects one engineering value from the local DB into the
// upstream (northbound) Modbus register image that the master (Энергосфера)
// reads via function 03/04.
//
// Encoding pipeline for a numeric value v:
//
//	raw = v*Scale + Bias        (Scale defaults to 1, Bias to 0)
//	bytes = encode(raw, Type, Order)
//
// Order is the codec byte/word order string (e.g. "0123" big-endian —
// "старшим вперёд" — which matches the default Энергосфера line setup).
// For 16-bit types Order is ignored (a single register has no word order;
// bytes are always big-endian within the register).
type NorthPoint struct {
	Register uint16  // start register (0-based) in the upstream image
	Space    string  // "holding" | "input" (aliases "HR" | "IR")
	Type     string  // "float" | "int32" | "uint32" | "int16" | "uint16"
	Order    string  // codec byte/word order, e.g. "0123"
	Source   string  // point name in the device reading (e.g. "Q", "V")
	Scale    float64 // pre-scale applied before encoding (0 means 1.0)
	Bias     float64 // pre-bias applied before encoding
}

// NorthUSPD is one logical UPD presented upstream. It is homogeneous by
// archive interval (see TRD addendum §A.4): meters → 30m, others → 60m.
type NorthUSPD struct {
	ID            string       // "mbgw-heat" | "mbgw-meters"
	Listen        string       // listener address, e.g. ":1502"
	UnitID        uint8        // modbus unit id of this logical UPD
	Interval      string       // "60m" | "30m"
	CurrentPoints []NorthPoint // current values served via func 03/04
	ArchivePlan   string       // OI archive strategy — filled after discovery (M3/M4)
}

// normSpace maps profile/vendor space aliases onto the two upstream spaces.
func normSpace(s string) string {
	switch s {
	case "holding", "HR", "hr", "":
		return "holding"
	case "input", "IR", "ir":
		return "input"
	default:
		return s
	}
}

// RegisterSpan returns how many 16-bit registers a point occupies.
func RegisterSpan(p NorthPoint) int {
	return codec.RegisterCount(p.Type)
}

// scaleOf returns the effective scale (0 is treated as identity 1.0).
func scaleOf(p NorthPoint) float64 {
	if p.Scale == 0 {
		return 1.0
	}
	return p.Scale
}

// EncodeValue encodes a single engineering value into its wire bytes,
// applying Scale/Bias and the point's Type/Order. Returned length is
// RegisterSpan(p)*2 bytes.
func EncodeValue(p NorthPoint, v float64) ([]byte, error) {
	x := v*scaleOf(p) + p.Bias

	switch p.Type {
	case "float":
		return codec.EncodeFloat32(float32(x), p.Order)
	case "int32":
		return codec.EncodeInt32(int32(math.Round(x)), p.Order)
	case "uint32":
		return codec.EncodeUint32(uint32(math.Round(x)), p.Order)
	case "int16":
		return codec.EncodeUint16(uint16(int16(math.Round(x)))), nil
	case "uint16":
		return codec.EncodeUint16(uint16(math.Round(x))), nil
	default:
		return nil, fmt.Errorf("northbound: unsupported point type %q", p.Type)
	}
}

// ReadRegisters builds the response payload for a read of `qty` registers
// starting at `start` in `space`. Registers covered by a NorthPoint are
// filled with that point's encoded value (using `values[Source]`); any
// register not covered (or whose source value is missing) stays 0x0000.
//
// anyCovered reports whether at least one requested register fell inside a
// mapped point — the handler uses it to decide between returning zeros and
// raising Modbus exception 02 (ILLEGAL DATA ADDRESS) for a fully-unmapped
// range.
func ReadRegisters(points []NorthPoint, space string, start, qty uint16, values map[string]float64) (data []byte, anyCovered bool, err error) {
	space = normSpace(space)
	data = make([]byte, int(qty)*2) // zero-filled

	reqStart := int(start)
	reqEnd := reqStart + int(qty) // exclusive

	for _, p := range points {
		if normSpace(p.Space) != space {
			continue
		}
		span := RegisterSpan(p)
		pStart := int(p.Register)
		pEnd := pStart + span // exclusive

		// overlap of [pStart,pEnd) with [reqStart,reqEnd)
		lo := pStart
		if reqStart > lo {
			lo = reqStart
		}
		hi := pEnd
		if reqEnd < hi {
			hi = reqEnd
		}
		if lo >= hi {
			continue // no overlap
		}
		anyCovered = true

		v, ok := values[p.Source]
		if !ok {
			// value not available yet → leave those registers at 0
			continue
		}
		full, encErr := EncodeValue(p, v)
		if encErr != nil {
			return nil, anyCovered, encErr
		}
		if len(full) != span*2 {
			return nil, anyCovered, fmt.Errorf("northbound: %s encoded %d bytes, expected %d", p.Source, len(full), span*2)
		}

		// copy the overlapping register slice into the response image
		for reg := lo; reg < hi; reg++ {
			srcOff := (reg - pStart) * 2 // byte offset inside `full`
			dstOff := (reg - reqStart) * 2
			data[dstOff] = full[srcOff]
			data[dstOff+1] = full[srcOff+1]
		}
	}

	return data, anyCovered, nil
}
