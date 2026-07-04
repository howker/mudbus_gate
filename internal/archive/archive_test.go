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
    if _, err := r.Read(context.Background(), tx, ArchiveQuery{}); err == nil {
        t.Fatal("expected error for short raw")
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
    recs, err := r.Read(context.Background(), tx, ArchiveQuery{})
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