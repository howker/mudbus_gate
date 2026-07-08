package archive
import (
    "fmt"
    "time"
    "mbgw/internal/codec"
)
// fieldByteSize returns the number of bytes occupied by a record_layout
// field of the given type, mirroring codec.RegisterCount (registers*2),
// with special cases for stInfoEvent/bcd (1 byte) and akron_volume
// (5 bytes: 4-byte raw + 1-byte Pu multiplier) which are not modeled in
// codec.RegisterCount.
func fieldByteSize(dataType string) int {
    if dataType == "stInfoEvent" || dataType == "bcd" {
        return 1
    }
    if dataType == "akron_volume" {
        return 5
    }
    return codec.RegisterCount(dataType) * 2
}
// decodeByLayout decodes a raw archive record according to the profile's
// record_layout: for each field, it slices raw[offset:offset+size] and
// calls the matching codec.Decode* function, keyed by field.Name in the
// result map. Fields with unsupported/unknown types are skipped (recorded
// under "<name>_error" instead of failing the whole record).
func decodeByLayout(raw []byte, layout []RecordLayoutField, order32 string, order64 string) map[string]any {
    out := make(map[string]any, len(layout))
    for _, f := range layout {
        size := fieldByteSize(f.Type)
        if f.Offset < 0 || f.Offset+size > len(raw) {
            out[f.Name+"_error"] = fmt.Sprintf("field out of range: offset=%d size=%d raw_len=%d", f.Offset, size, len(raw))
            continue
        }
        chunk := raw[f.Offset : f.Offset+size]
        var val any
        var err error
        switch f.Type {
        case "float":
            var v float32
            v, err = codec.DecodeFloat32(chunk, order32)
            val = float64(v)
        case "double":
            val, err = codec.DecodeFloat64(chunk, order64)
        case "int32":
            var v int32
            v, err = codec.DecodeInt32(chunk, order32)
            val = int64(v)
        case "uint32":
            var v uint32
            v, err = codec.DecodeUint32(chunk, order32)
            if err == nil && f.Epoch != "" {
                val = time.Unix(int64(v), 0).UTC()
            } else {
                val = int64(v)
            }
        case "int16":
            var v int16
            v, err = codec.DecodeInt16(chunk)
            val = int64(v)
        case "uint16", "bitfield":
            var v uint16
            v, err = codec.DecodeUint16(chunk)
            val = int64(v)
        case "scaled_int":
            var v uint16
            v, err = codec.DecodeUint16(chunk)
            scale := f.Scale
            if scale == 0 {
                scale = 1
            }
            val = float64(v) * scale
        case "u32+float":
            val, err = codec.DecodeU32Float(chunk, order32)
        case "long+float":
            val, err = codec.DecodeLongFloat(chunk, order32)
        case "bcd":
            var v int
            v, err = codec.DecodeBCDByte(chunk[0])
            val = int64(v)
        case "akron_volume":
            val, err = codec.DecodeAkronVolume(chunk, order32)
        case "stInfoEvent":
            b := chunk[0]
            val = map[string]any{
                "raw":       b,
                "is_clear":  b&0x80 != 0,
                "event_num": b & 0x1F,
            }
        default:
            err = fmt.Errorf("unsupported record_layout type: %s", f.Type)
        }
        if err != nil {
            out[f.Name+"_error"] = err.Error()
            continue
        }
        out[f.Name] = val
    }
    return out
}