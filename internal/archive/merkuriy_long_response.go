package archive

import (
    "context"
    "fmt"

    "mbgw/internal/protocol/merkuriy"
)

// MerkuriyLongResponseReader is the archive-read strategy for Merkuriy
// (150/203.2TD/204/208/230/231/234/236/238/350, per "писание системы
// команд приборов учета еркурий") power profiles and event logs, using
// command 0x16 "relative addressing mode" (section 4.6) - the documented
// mechanism for reading ring-buffer arrays, matching CONTRACTS.md §6.4's
// algorithm (open channel via session, read a long response of up to 255
// bytes, decode records by record_layout).
//
// Requests/responses here are bare PDUs (code+params / raw data bytes) -
// no device address, no CRC. Those are RTU transport framing concerns
// (address = unitID at the transport/pollcore.Reader level, CRC =
// protocol/modbus.Transact's BuildRTUFrame/ParseRTUFrame), matching how
// VZLET's function 65 and Akron's User-Defined commands already work. An
// earlier version of this strategy built a complete frame itself
// (BuildReadRelative: address+code+params+CRC) and passed it to
// pollcore.Reader.Transact, which double-frames it - discovered by
// inspection while fixing the identical bug in Akron's archive strategy
// (this session's Akron live-RTU test caught it there first; Merkuriy's
// own live RTU archive read was never actually tested until now, per
// backlog).
//
// Merkuriy's response has no byteCount prefix (unlike Akron's): once the
// RTU transport strips address+CRC, what remains IS the data - no
// further response-shape parsing needed here.
//
// Required profile archive.params:
//   - "mem_number" (int): which memory/array number to read (e.g. 3 for
//     the main average-power-profile array, per the profile's own
//     documentation of its memory layout - not something this strategy
//     can know generically).
//
// Record size is derived from record_layout itself (sum of field
// offsets/sizes via fieldByteSize), not a separate param, so the profile
// remains the single source of truth for record shape.
type MerkuriyLongResponseReader struct{}

func NewMerkuriyLongResponseReader() *MerkuriyLongResponseReader {
    return &MerkuriyLongResponseReader{}
}

func (r *MerkuriyLongResponseReader) Strategy() string {
    return "merkuriy_long_response"
}

func merkuriyMemNumberParam(params map[string]any) (byte, error) {
    return byteParam(params, "mem_number")
}

func byteParam(params map[string]any, key string) (byte, error) {
    raw, ok := params[key]
    if !ok {
        return 0, fmt.Errorf("merkuriy_long_response: missing required archive param %q in profile", key)
    }
    switch v := raw.(type) {
    case int:
        return byte(v), nil
    case int64:
        return byte(v), nil
    case float64:
        return byte(v), nil
    default:
        return 0, fmt.Errorf("merkuriy_long_response: archive param %q has unsupported type %T", key, raw)
    }
}

// recordLayoutSize returns the total byte span of one record per
// record_layout: the highest offset+fieldsize among all fields. This is
// how the combined multi-record long response is split into individual
// records for decodeByLayout, without needing a separate "record_size"
// param that could drift out of sync with the layout itself.
func recordLayoutSize(layout []RecordLayoutField) int {
    size := 0
    for _, f := range layout {
        end := f.Offset + fieldByteSize(f.Type)
        if end > size {
            size = end
        }
    }
    return size
}

func (r *MerkuriyLongResponseReader) Read(ctx context.Context, sess ArchiveSession, tx Transactor, q ArchiveQuery) ([]ArchiveRecord, error) {
    if sess == nil || sess.State() != "ready" {
        return nil, fmt.Errorf("merkuriy_long_response: session is not ready (channel must be open before reading)")
    }

    memNumber, err := merkuriyMemNumberParam(q.Params)
    if err != nil {
        return nil, err
    }

    recordSize := recordLayoutSize(q.RecordLayout)
    if recordSize <= 0 {
        return nil, fmt.Errorf("merkuriy_long_response: record_layout is empty or has zero total size")
    }

    count := 1
    if q.ToIndex > q.FromIndex {
        count = q.ToIndex - q.FromIndex + 1
    }
    maxCount := 255 / recordSize
    if maxCount < 1 {
        return nil, fmt.Errorf("merkuriy_long_response: record size %d exceeds the 255-byte long-response limit", recordSize)
    }
    if count > maxCount {
        count = maxCount
    }

    reqPDU := merkuriy.BuildReadRelativePDU(memNumber, uint16(q.FromIndex), byte(count))

    data, err := tx.Transact(ctx, reqPDU)
    if err != nil {
        return nil, err
    }

    if len(data) == 1 {
        return nil, fmt.Errorf("merkuriy_long_response: device returned status %s instead of data", merkuriy.ParseStatus(data[0]))
    }

    if len(data)%recordSize != 0 {
        return nil, fmt.Errorf("merkuriy_long_response: response length %d is not a multiple of record size %d", len(data), recordSize)
    }

    numRecords := len(data) / recordSize
    records := make([]ArchiveRecord, 0, numRecords)
    for i := 0; i < numRecords; i++ {
        raw := data[i*recordSize : (i+1)*recordSize]
        fields := decodeByLayout(raw, q.RecordLayout, q.WordOrder32, q.WordOrder64)
        records = append(records, ArchiveRecord{
            Raw:    append([]byte(nil), raw...),
            Fields: fields,
            CRCOK:  true,
        })
    }

    return records, nil
}

func init() { Register(NewMerkuriyLongResponseReader()) }