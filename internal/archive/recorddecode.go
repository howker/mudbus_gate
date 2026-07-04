package archive
import "time"
func decodeRecord(raw []byte) map[string]any {
    out := map[string]any{"raw_len": len(raw)}
    if len(raw) >= 1 {
        out["status"] = raw[0]
    }
    if len(raw) >= 4 {
        out["marker"] = string([]byte{raw[0], raw[1], raw[2], raw[3]})
    }
    if len(raw) >= 8 {
        ts := int64(raw[4])<<24 | int64(raw[5])<<16 | int64(raw[6])<<8 | int64(raw[7])
        out["ts_unix"] = ts
        out["ts"] = time.Unix(ts, 0).UTC()
    }
    return out
}
