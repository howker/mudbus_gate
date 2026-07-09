package archive

import (
    "context"
    "testing"
)

type fakeSession struct{ state string }

func (f fakeSession) State() string { return f.state }

func merkuriyTestLayout() []RecordLayoutField {
    return []RecordLayoutField{
        {Offset: 0, Name: "a", Type: "uint16"},
        {Offset: 2, Name: "b", Type: "uint16"},
    }
}

func TestMerkuriyLongResponse_MissingMemNumberParam(t *testing.T) {
    r := NewMerkuriyLongResponseReader()
    sess := fakeSession{state: "ready"}
    tx := &fakeTransactor{}
    q := ArchiveQuery{Params: map[string]any{}, RecordLayout: merkuriyTestLayout()}
    if _, err := r.Read(context.Background(), sess, tx, q); err == nil {
        t.Fatal("expected error when archive.params.mem_number is missing")
    }
}

func TestMerkuriyLongResponse_SessionNotReady(t *testing.T) {
    r := NewMerkuriyLongResponseReader()
    sess := fakeSession{state: "closed"}
    tx := &fakeTransactor{}
    q := ArchiveQuery{
        Params:       map[string]any{"mem_number": 3},
        RecordLayout: merkuriyTestLayout(),
    }
    if _, err := r.Read(context.Background(), sess, tx, q); err == nil {
        t.Fatal("expected error when session is not ready")
    }
}

func TestMerkuriyLongResponse_NilSession(t *testing.T) {
    r := NewMerkuriyLongResponseReader()
    tx := &fakeTransactor{}
    q := ArchiveQuery{
        Params:       map[string]any{"mem_number": 3},
        RecordLayout: merkuriyTestLayout(),
    }
    if _, err := r.Read(context.Background(), nil, tx, q); err == nil {
        t.Fatal("expected error when session is nil")
    }
}

func TestMerkuriyLongResponse_DeviceStatusError(t *testing.T) {
    r := NewMerkuriyLongResponseReader()
    sess := fakeSession{state: "ready"}
    // Bare data (no address, no CRC - RTU transport's job): a single
    // status byte (X3h - insufficient access level) instead of real data.
    tx := &fakeTransactor{resp: []byte{0x03}}
    q := ArchiveQuery{
        Params:       map[string]any{"mem_number": 3},
        RecordLayout: merkuriyTestLayout(),
    }
    _, err := r.Read(context.Background(), sess, tx, q)
    if err == nil {
        t.Fatal("expected error when device returns a status byte instead of data")
    }
}

func TestMerkuriyLongResponse_MultiRecordSplit(t *testing.T) {
    r := NewMerkuriyLongResponseReader()
    sess := fakeSession{state: "ready"}
    // Bare data (no address, no CRC): two 4-byte records {a:1,b:2} and
    // {a:3,b:4}.
    tx := &fakeTransactor{resp: []byte{0x00, 0x01, 0x00, 0x02, 0x00, 0x03, 0x00, 0x04}}
    q := ArchiveQuery{
        Params:       map[string]any{"mem_number": 3},
        RecordLayout: merkuriyTestLayout(),
        FromIndex:    0,
        ToIndex:      1,
    }
    recs, err := r.Read(context.Background(), sess, tx, q)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(recs) != 2 {
        t.Fatalf("expected 2 records, got %d", len(recs))
    }
    if recs[0].Fields["a"] != int64(1) || recs[0].Fields["b"] != int64(2) {
        t.Fatalf("record 0 mismatch: %+v", recs[0].Fields)
    }
    if recs[1].Fields["a"] != int64(3) || recs[1].Fields["b"] != int64(4) {
        t.Fatalf("record 1 mismatch: %+v", recs[1].Fields)
    }
    if !recs[0].CRCOK || !recs[1].CRCOK {
        t.Fatal("expected CRCOK=true for both records (RTU frame CRC already validated by the transport)")
    }
}

func TestMerkuriyLongResponse_MisalignedLengthIsError(t *testing.T) {
    r := NewMerkuriyLongResponseReader()
    sess := fakeSession{state: "ready"}
    // 6 bytes is not a multiple of the 4-byte record size.
    tx := &fakeTransactor{resp: []byte{0x00, 0x01, 0x00, 0x02, 0x00, 0x03}}
    q := ArchiveQuery{
        Params:       map[string]any{"mem_number": 3},
        RecordLayout: merkuriyTestLayout(),
    }
    if _, err := r.Read(context.Background(), sess, tx, q); err == nil {
        t.Fatal("expected error for a response length not a multiple of record size")
    }
}

func appendCRC(body []byte) []byte {
    var crc uint16 = 0xFFFF
    for _, b := range body {
        crc ^= uint16(b)
        for i := 0; i < 8; i++ {
            if crc&1 != 0 {
                crc = (crc >> 1) ^ 0xA001
            } else {
                crc >>= 1
            }
        }
    }
    return append(append([]byte(nil), body...), byte(crc&0xFF), byte(crc>>8))
}
func akronHourlyLayout() []RecordLayoutField {
    return []RecordLayoutField{
        {Offset: 0, Name: "volume", Type: "akron_volume"},
        {Offset: 5, Name: "hour", Type: "bcd"},
        {Offset: 6, Name: "day", Type: "bcd"},
        {Offset: 7, Name: "month", Type: "bcd"},
        {Offset: 8, Name: "year", Type: "bcd"},
    }
}

func akronDailyLayout() []RecordLayoutField {
    return []RecordLayoutField{
        {Offset: 0, Name: "volume", Type: "akron_volume"},
        {Offset: 5, Name: "day", Type: "bcd"},
        {Offset: 6, Name: "month", Type: "bcd"},
        {Offset: 7, Name: "year", Type: "bcd"},
    }
}

func TestAkronArchive_MissingArchiveKindParam(t *testing.T) {
    r := NewAkronArchiveReader()
    sess := fakeSession{state: "ready"}
    tx := &fakeTransactor{}
    q := ArchiveQuery{Params: map[string]any{}, RecordLayout: akronHourlyLayout()}
    if _, err := r.Read(context.Background(), sess, tx, q); err == nil {
        t.Fatal("expected error when archive.params.archive_kind is missing")
    }
}

func TestAkronArchive_InvalidArchiveKindParam(t *testing.T) {
    r := NewAkronArchiveReader()
    sess := fakeSession{state: "ready"}
    tx := &fakeTransactor{}
    q := ArchiveQuery{Params: map[string]any{"archive_kind": "weekly"}, RecordLayout: akronHourlyLayout()}
    if _, err := r.Read(context.Background(), sess, tx, q); err == nil {
        t.Fatal("expected error for an archive_kind that is neither hourly nor daily")
    }
}

func TestAkronArchive_HourlyRead(t *testing.T) {
    r := NewAkronArchiveReader()
    sess := fakeSession{state: "ready"}
    // Bare PDU (no address, no CRC - the RTU transport layer's job, not
    // this strategy's): code=104(0x68), byteCount=9, row: U=5827 raw
    // (C3 16 00 00, '3210' order), Pu=02 -> 582.7 m3; hour=10(BCD),
    // day=05, month=03, year=08 (2008-03-05 10:00) - same values as
    // today's earlier codec.DecodeAkronVolume golden test.
    tx := &fakeTransactor{resp: []byte{0x68, 0x09, 0xC3, 0x16, 0x00, 0x00, 0x02, 0x10, 0x05, 0x03, 0x08}}
    q := ArchiveQuery{
        Params:       map[string]any{"archive_kind": "hourly"},
        RecordLayout: akronHourlyLayout(),
        WordOrder32:  "3210",
    }
    recs, err := r.Read(context.Background(), sess, tx, q)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(recs) != 1 {
        t.Fatalf("expected 1 record, got %d", len(recs))
    }
    rec := recs[0]
    vol, ok := rec.Fields["volume"].(float64)
    if !ok || vol < 582.69 || vol > 582.71 {
        t.Fatalf("expected volume ~582.7, got %v", rec.Fields["volume"])
    }
    if rec.Fields["hour"] != int64(10) || rec.Fields["day"] != int64(5) || rec.Fields["month"] != int64(3) || rec.Fields["year"] != int64(8) {
        t.Fatalf("date fields mismatch: %+v", rec.Fields)
    }
    if !rec.CRCOK {
        t.Fatal("expected CRCOK=true")
    }
}

func TestAkronArchive_MisalignedLengthIsError(t *testing.T) {
    r := NewAkronArchiveReader()
    sess := fakeSession{state: "ready"}
    // Declares byteCount=8 (not a multiple of the 9-byte hourly row size).
    tx := &fakeTransactor{resp: []byte{0x68, 0x08, 0xC3, 0x16, 0x00, 0x00, 0x02, 0x10, 0x05, 0x03}}
    q := ArchiveQuery{
        Params:       map[string]any{"archive_kind": "hourly"},
        RecordLayout: akronHourlyLayout(),
        WordOrder32:  "3210",
    }
    if _, err := r.Read(context.Background(), sess, tx, q); err == nil {
        t.Fatal("expected error for a response length not a multiple of row size")
    }
}
