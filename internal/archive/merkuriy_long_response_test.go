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

func TestMerkuriyLongResponse_MissingAddrParam(t *testing.T) {
    r := NewMerkuriyLongResponseReader()
    sess := fakeSession{state: "ready"}
    tx := &fakeTransactor{}
    q := ArchiveQuery{Params: map[string]any{"mem_number": 3}, RecordLayout: merkuriyTestLayout()}
    if _, err := r.Read(context.Background(), sess, tx, q); err == nil {
        t.Fatal("expected error when archive.params.addr is missing")
    }
}

func TestMerkuriyLongResponse_MissingMemNumberParam(t *testing.T) {
    r := NewMerkuriyLongResponseReader()
    sess := fakeSession{state: "ready"}
    tx := &fakeTransactor{}
    q := ArchiveQuery{Params: map[string]any{"addr": 1}, RecordLayout: merkuriyTestLayout()}
    if _, err := r.Read(context.Background(), sess, tx, q); err == nil {
        t.Fatal("expected error when archive.params.mem_number is missing")
    }
}

func TestMerkuriyLongResponse_SessionNotReady(t *testing.T) {
    r := NewMerkuriyLongResponseReader()
    sess := fakeSession{state: "closed"}
    tx := &fakeTransactor{}
    q := ArchiveQuery{
        Params:       map[string]any{"addr": 1, "mem_number": 3},
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
        Params:       map[string]any{"addr": 1, "mem_number": 3},
        RecordLayout: merkuriyTestLayout(),
    }
    if _, err := r.Read(context.Background(), nil, tx, q); err == nil {
        t.Fatal("expected error when session is nil")
    }
}

func TestMerkuriyLongResponse_DeviceStatusError(t *testing.T) {
    r := NewMerkuriyLongResponseReader()
    sess := fakeSession{state: "ready"}
    tx := &fakeTransactor{resp: []byte{0x01, 0x03, 0x40, 0x21}}
    q := ArchiveQuery{
        Params:       map[string]any{"addr": 1, "mem_number": 3},
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
    tx := &fakeTransactor{resp: []byte{0x01, 0x00, 0x01, 0x00, 0x02, 0x00, 0x03, 0x00, 0x04, 0x9F, 0xA3}}
    q := ArchiveQuery{
        Params:       map[string]any{"addr": 1, "mem_number": 3},
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
        t.Fatal("expected CRCOK=true for both records (frame CRC already validated)")
    }
}

func TestMerkuriyLongResponse_MisalignedLengthIsError(t *testing.T) {
    r := NewMerkuriyLongResponseReader()
    sess := fakeSession{state: "ready"}
    body := []byte{0x01, 0x00, 0x01, 0x00, 0x02, 0x00}
    tx := &fakeTransactor{resp: appendCRC(body)}
    q := ArchiveQuery{
        Params:       map[string]any{"addr": 1, "mem_number": 3},
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