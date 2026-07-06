package archive

import (
    "context"
    "testing"
    "time"
)

// fakeTransactor - тестовый Transactor, который всегда возвращает заранее заданный ответ,
// имитируя обмен с прибором без реального транспорта/сессии.
type fakeTransactor struct {
    resp []byte
    err  error
}

func (f *fakeTransactor) Transact(ctx context.Context, req []byte) ([]byte, error) {
    return f.resp, f.err
}

func TestRegistryContainsArchiveStrategies(t *testing.T) {
    if r, ok := Get("mb_indexed_binary"); !ok || r == nil {
        t.Fatal("expected mb_indexed_binary strategy to be registered")
    }
    if r, ok := Get("mb_func65"); !ok || r == nil {
        t.Fatal("expected mb_func65 strategy to be registered")
    }
}

func TestDecodeRecordSkeleton(t *testing.T) {
    got := decodeRecord([]byte{0x7F, 0x02, 0x03, 0x04, 0x00, 0x00, 0x00, 0x2A})
    if got["raw_len"] != 8 {
        t.Fatalf("expected raw_len=8, got %v", got["raw_len"])
    }
    if got["status"] != byte(0x7F) {
        t.Fatalf("expected status=0x7F, got %v", got["status"])
    }
    if got["ts_unix"] != int64(42) {
        t.Fatalf("expected ts_unix=42, got %v", got["ts_unix"])
    }
    ts, ok := got["ts"].(time.Time)
    if !ok || !ts.Equal(time.Unix(42, 0).UTC()) {
        t.Fatalf("expected ts=unix 42, got %v", got["ts"])
    }
}

func TestMBIndexedBinaryReadShort(t *testing.T) {
    r := NewMBIndexedBinary()
    // Full PDU: funcCode + byteCount + 3 bytes of data (too short after stripping header).
    tx := &fakeTransactor{resp: []byte{0x04, 3, 1, 2, 3}}
    q := ArchiveQuery{Params: map[string]any{"base_addr": 200, "record_regs": 4}}
    if _, err := r.Read(context.Background(), nil, tx, q); err == nil {
        t.Fatal("expected error for short raw")
    }
}

func TestMBIndexedBinaryRead_MissingParamsIsError(t *testing.T) {
    r := NewMBIndexedBinary()
    tx := &fakeTransactor{resp: []byte{0x04, 8, 0x7F, 2, 3, 4, 0, 0, 0, 42}}
    _, err := r.Read(context.Background(), nil, tx, ArchiveQuery{})
    if err == nil {
        t.Fatal("expected error when archive.params.base_addr/record_regs are missing from the profile")
    }
}

func TestDecodeByLayout_StInfoEvent(t *testing.T) {
    // Golden vectors: occurrence of NS#5 = 0x05, clearing of NS#5 = 0x85.
    layout := []RecordLayoutField{
        {Offset: 0, Name: "event", Type: "stInfoEvent"},
    }

    got := decodeByLayout([]byte{0x05}, layout, "0123", "01234567")
    ev, ok := got["event"].(map[string]any)
    if !ok {
        t.Fatalf("expected event field to be a map, got %T", got["event"])
    }
    if ev["is_clear"] != false {
        t.Fatalf("expected is_clear=false for 0x05, got %v", ev["is_clear"])
    }
    if ev["event_num"] != byte(5) {
        t.Fatalf("expected event_num=5 for 0x05, got %v", ev["event_num"])
    }

    got2 := decodeByLayout([]byte{0x85}, layout, "0123", "01234567")
    ev2 := got2["event"].(map[string]any)
    if ev2["is_clear"] != true {
        t.Fatalf("expected is_clear=true for 0x85, got %v", ev2["is_clear"])
    }
    if ev2["event_num"] != byte(5) {
        t.Fatalf("expected event_num=5 for 0x85, got %v", ev2["event_num"])
    }
}
func TestMBIndexedBinaryRead(t *testing.T) {
    r := NewMBIndexedBinary()
    // Full PDU: funcCode(0x04) + byteCount(8) + 8 bytes of record data.
    tx := &fakeTransactor{resp: []byte{0x04, 8, 0x7F, 2, 3, 4, 0, 0, 0, 42}}
    q := ArchiveQuery{Params: map[string]any{"base_addr": 200, "record_regs": 4}}
    recs, err := r.Read(context.Background(), nil, tx, q)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(recs) != 1 {
        t.Fatalf("expected 1 record, got %d", len(recs))
    }
    rec := recs[0]
    if rec.Fields["status"] != byte(0x7F) {
        t.Fatalf("expected status 0x7F, got %v", rec.Fields["status"])
    }
    if rec.Fields["ts_unix"] != int64(42) {
        t.Fatalf("expected ts_unix 42, got %v", rec.Fields["ts_unix"])
    }
}
func TestMBFunc65_MissingParamsIsError(t *testing.T) {
    r := NewMBFunc65()
    tx := &fakeTransactor{resp: []byte{0x41, 8, 0, 0, 0, 1, 0, 42, 0xD0, 0x04}}
    _, err := r.Read(context.Background(), nil, tx, ArchiveQuery{})
    if err == nil {
        t.Fatal("expected error when archive.params.archive_type is missing from the profile")
    }
}

func TestMBFunc65_Read(t *testing.T) {
    r := NewMBFunc65()
    // funcCode(0x41) + byteCount(8) + record: time=1 (not the nonexistent
    // marker), data=42 (uint16 0x002A), CRC16-modbus over first 6 bytes
    // (0x04D0, little-endian on wire: D0 04) - same fixture the tsrv024
    // simulator uses. record_layout declares a crc:true field, so this
    // strategy for this profile is treated as CRC-checked (TSRV-024 style).
    tx := &fakeTransactor{resp: []byte{0x41, 8, 0, 0, 0, 1, 0, 42, 0xD0, 0x04}}
    q := ArchiveQuery{
        Params:       map[string]any{"archive_type": 1},
        RecordLayout: []RecordLayoutField{{Offset: 6, Name: "crc", Type: "uint16", CRC: true}},
    }
    recs, err := r.Read(context.Background(), nil, tx, q)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(recs) != 1 {
        t.Fatalf("expected 1 record, got %d", len(recs))
    }
    if !recs[0].CRCOK {
        t.Fatal("expected CRCOK=true for a valid CRC")
    }
}

func TestMBFunc65_NonexistentRecordMarker(t *testing.T) {
    r := NewMBFunc65()
    // time = 0xFFFFFFFF marker -> no record, regardless of remaining bytes/CRC.
    tx := &fakeTransactor{resp: []byte{0x41, 4, 0xFF, 0xFF, 0xFF, 0xFF}}
    q := ArchiveQuery{Params: map[string]any{"archive_type": 1}}
    recs, err := r.Read(context.Background(), nil, tx, q)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(recs) != 0 {
        t.Fatalf("expected 0 records for nonexistent-record marker, got %d", len(recs))
    }
}

func TestMBFunc65_BadCRCIsFlagged(t *testing.T) {
    r := NewMBFunc65()
    // Same record as TestMBFunc65_Read but with a corrupted CRC byte.
    tx := &fakeTransactor{resp: []byte{0x41, 8, 0, 0, 0, 1, 0, 42, 0x00, 0x00}}
    q := ArchiveQuery{
        Params:       map[string]any{"archive_type": 1},
        RecordLayout: []RecordLayoutField{{Offset: 6, Name: "crc", Type: "uint16", CRC: true}},
    }
    recs, err := r.Read(context.Background(), nil, tx, q)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(recs) != 1 {
        t.Fatalf("expected 1 record, got %d", len(recs))
    }
    if recs[0].CRCOK {
        t.Fatal("expected CRCOK=false for a corrupted CRC")
    }
}

func TestMBFunc65_NoCRCFieldSkipsCheck(t *testing.T) {
    // A device profile whose record_layout has no crc:true field (e.g.
    // IVK-TER) must not have its records flagged CRCOK=false just because
    // the last two bytes happen not to match a CRC16 that was never
    // supposed to be there.
    r := NewMBFunc65()
    tx := &fakeTransactor{resp: []byte{0x41, 8, 0, 0, 0, 1, 0, 42, 0x00, 0x00}}
    q := ArchiveQuery{Params: map[string]any{"archive_type": 0}}
    recs, err := r.Read(context.Background(), nil, tx, q)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if !recs[0].CRCOK {
        t.Fatal("expected CRCOK=true (no CRC check) when record_layout has no crc:true field")
    }
}

func TestDecodeByLayout_BCD(t *testing.T) {
    // Merkuriy timedate structure (dow-hh-mm-ss-dd-mon-yy), section 4.6's
    // worked example: 10:00, 5 марта 2008 (day-of-week not specified in
    // that example, using Wednesday=3 as a synthetic value here).
    layout := []RecordLayoutField{
        {Offset: 0, Name: "dow", Type: "bcd"},
        {Offset: 1, Name: "hour", Type: "bcd"},
        {Offset: 2, Name: "minute", Type: "bcd"},
        {Offset: 3, Name: "second", Type: "bcd"},
        {Offset: 4, Name: "day", Type: "bcd"},
        {Offset: 5, Name: "month", Type: "bcd"},
        {Offset: 6, Name: "year", Type: "bcd"},
    }
    raw := []byte{0x03, 0x10, 0x00, 0x00, 0x05, 0x03, 0x08}

    got := decodeByLayout(raw, layout, "0123", "01234567")

    want := map[string]int64{
        "dow": 3, "hour": 10, "minute": 0, "second": 0,
        "day": 5, "month": 3, "year": 8,
    }
    for name, wantVal := range want {
        if got[name] != wantVal {
            t.Fatalf("field %q: want %d, got %v", name, wantVal, got[name])
        }
    }
}

func TestDecodeByLayout_BCD_InvalidNibbleIsError(t *testing.T) {
    layout := []RecordLayoutField{
        {Offset: 0, Name: "hour", Type: "bcd"},
    }
    raw := []byte{0xFA} // invalid BCD (both nibbles out of 0-9 range)

    got := decodeByLayout(raw, layout, "0123", "01234567")

    if _, ok := got["hour"]; ok {
        t.Fatal("expected no valid 'hour' field for invalid BCD byte")
    }
    if _, ok := got["hour_error"]; !ok {
        t.Fatal("expected 'hour_error' to be set for invalid BCD byte")
    }
}
